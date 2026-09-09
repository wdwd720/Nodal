// Package reachability answers the question that keeps finding defects: can
// anybody actually reach this?
//
// F-26, F-28 and F-29 were all the same shape. A control or a capability was
// correct, tested, and unreachable — no HTTP route, no administrative action,
// no worker, nothing outside a test could invoke it. Every one of them looked
// finished from inside the test suite, because the test suite was the only
// caller.
//
//	F-26  the whole internal economy: two compiler inputs cmd/api never supplied
//	F-28  launching a native market: the "separate act" nothing implemented
//	F-29  cancelling your own payout: money reserved with no path to release it
//
// Three incidents is a class. This test is that class made mechanical: every
// exported method on a financial service that takes a transaction — that is,
// every method that CHANGES money — must have a caller somewhere a deployment
// can reach, or an entry below saying in words why not.
//
// # What it cannot do
//
// It matches on names and shapes, not on a call graph, because a call graph
// would mean a new module dependency for one test. A call is recognised as
// `.Method(ctx, tx` — the second argument being a transaction is what makes the
// match specific enough to be useful. The first version matched `.Method(ctx`
// alone and could not be made to fail: `Cancel`, `Create` and `Execute` are
// names half a dozen unrelated types use.
//
// A call only counts when the file also IMPORTS the declaring package. That
// second condition is what makes common names usable: `Create`, `Execute`,
// `Activate` and `SetStatus` are each declared by four of the five packages
// here, and without it every one of them looked reachable from every file in
// the repository.
//
// It can still be fooled where one file imports two of these packages and calls
// the same method name on both — and by a method reached only through an
// interface whose method name differs. Both failure modes point the same way:
// it can say a method IS called when the caller it found belongs to something
// else. So a passing entry is weaker evidence than a failing one, which is the
// right way round for a check whose job is to find the unreachable.
package reachability

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// financialPackages are the ones whose mutators move value, or decide whether
// value may move. Adding a package here is how a new financial service joins
// the check.
//
// internal/risk is in the second category and was added after F-34, where the
// whole kernel turned out to be unreachable: nothing had ever written a policy
// row, so nothing had ever been evaluated. A control that decides is worth the
// same question as a control that moves.
var financialPackages = []string{
	"internal/nativeasset",
	"internal/nativemarket",
	"internal/commerce",
	"internal/credit",
	"internal/payout",
	"internal/risk",
}

// reachableFrom are the trees a deployment can actually run. A caller in
// test/ does not count: that is the whole point.
var reachableFrom = []string{"internal", "cmd", "scripts"}

// unreachableOnPurpose lists mutators with no deployment caller, each with the
// reason. An entry here is a claim somebody made deliberately; an absence is a
// defect this test reports.
//
// Every reason below is an EXTERNAL dependency or a decision recorded in
// BLOCKERS.md. "Nobody has got round to it" is not a reason and must not be
// added: that is precisely what F-26, F-28 and F-29 were.
var unreachableOnPurpose = map[string]string{
	// Payout destinations. Registering one means collecting bank details, and
	// verifying one requires a payout provider to accept them (B-01, B-06).
	// Collecting that data with nowhere to send it is worse than not offering
	// it, so the endpoint is deliberately absent rather than merely missing.
	"internal/payout.CreateDestination":    "B-01/B-06: no payout provider exists to verify a destination or receive value",
	"internal/payout.SetDestinationStatus": "B-01/B-06: verification is a provider's decision, and there is no provider",

	// Identity verification for a payout. The level it would record is
	// PAYOUT_KYC, which by definition comes from a provider.
	"internal/payout.CompleteVerification": "B-06: PAYOUT_KYC is performed by, or accepted by, a payout provider",
}

// reachedThroughInterface lists mutators whose only caller reaches them through
// an interface, which the name-and-import matching below cannot see.
//
// This is a DIFFERENT claim from unreachableOnPurpose and is kept separate on
// purpose. That map says "no deployment can run this, and here is the external
// reason". This one says "a deployment does run this, and here is the interface
// and the file that wires it" -- a claim a reader can check in about a minute,
// which is the bar an exemption has to clear to be worth anything.
//
// The whole Credit funding family lives here as of the Stripe workstream. It
// used to be in unreachableOnPurpose citing B-04, and that reason stopped being
// true when the provider was written: every one of these is now called by
// credit.PurchaseService, whose own entry points are the webhook Dispatcher
// interface and the CreditsPort the HTTP layer holds.
var reachedThroughInterface = map[string]string{
	"internal/credit.Dispatch": "webhook.Dispatcher[credit.PurchaseEvent]; wired in cmd/api/wire_credit.go and mounted at POST /v1/webhooks/stripe_credit",
	"internal/credit.Refund":   "credit.PurchaseService.apply, on a charge.refunded event arriving through Dispatch",
	"internal/credit.Reverse":  "credit.PurchaseService.apply, on a chargeback arriving through Dispatch",

	"internal/credit.CreateFunding":  "credit.PurchaseService.StartPurchase, through httpapi.CreditsPort at POST /v1/payments",
	"internal/credit.AdvanceFunding": "credit.PurchaseService.apply and StartPurchase",
	"internal/credit.SettleFunding":  "credit.PurchaseService.apply and SettleDue, the latter run by cmd/reconciliation-worker",
	"internal/credit.DisputeFunding": "credit.PurchaseService.apply, on charge.dispute.created arriving through Dispatch",
	"internal/credit.MintFrom":       "credit.PurchaseService.apply, on the CAPTURED edge",
	"internal/credit.SetFinality":    "credit.Service.SettleFunding and DisputeFunding, both reached as above",
}

var (
	// mutator matches an exported method on an EXPORTED type taking a
	// transaction: the shape of every call that changes money in this codebase.
	//
	// The receiver type must be exported because the question this test asks --
	// "does anything outside this package call it" -- cannot be asked of a
	// method on an unexported type. Nothing outside can name that type at all;
	// such a method is reached only through an interface the package itself
	// hands out, and its caller is by construction inside the package. Matching
	// them produced exactly one finding, and it was false: the adapter that
	// writes a risk decision, called by the service two files away.
	mutator = regexp.MustCompile(`(?m)^func \(\w+ \*?[A-Z]\w*\) ([A-Z]\w*)\(ctx context\.Context, tx pgx\.Tx`)
	// callSite matches an invocation of such a method. It requires the SECOND
	// argument to be a transaction, which is what makes the match specific:
	// `.Cancel(ctx` matches half a dozen unrelated types, and the first
	// version of this test could not be made to fail because of it.
	// `.Cancel(ctx, tx` matches a call to a transactional mutator.
	callSite = regexp.MustCompile(`\.([A-Z]\w*)\(ctx, tx[,)]`)
)

func TestReachability_EveryFinancialMutatorHasADeploymentCaller(t *testing.T) {
	root := repoRoot(t)

	declared := map[string][]string{} // package -> method names
	for _, pkg := range financialPackages {
		declared[pkg] = mutatorsIn(t, filepath.Join(root, filepath.FromSlash(pkg)))
		if len(declared[pkg]) == 0 {
			t.Fatalf("%s declares no transactional mutators; the pattern or the package moved", pkg)
		}
	}

	var problems, staleExemptions []string
	for _, pkg := range financialPackages {
		for _, name := range declared[pkg] {
			key := pkg + "." + name
			reachable := calledOutside(t, root, pkg, name)
			reason, exempt := unreachableOnPurpose[key]
			viaIface, indirect := reachedThroughInterface[key]
			switch {
			case exempt && indirect:
				staleExemptions = append(staleExemptions,
					key+" is listed both as unreachable ("+reason+") and as reached through an interface ("+viaIface+"); it cannot be both")
			case reachable && exempt:
				staleExemptions = append(staleExemptions,
					key+" is listed as unreachable ("+reason+") but something outside its package calls it")
			case reachable && indirect:
				staleExemptions = append(staleExemptions,
					key+" is listed as reached through an interface ("+viaIface+") but a direct caller now exists; drop the entry")
			case !reachable && indirect:
				// Claimed, and named specifically enough to be checked.
			case !reachable && !exempt:
				problems = append(problems, key+
					" changes money and nothing a deployment can run calls it; "+
					"give it a caller, or add it to unreachableOnPurpose with the external reason")
			}
		}
	}
	sort.Strings(problems)
	sort.Strings(staleExemptions)
	if len(problems) > 0 {
		t.Errorf("%d financial mutator(s) no deployment can reach:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
	if len(staleExemptions) > 0 {
		t.Errorf("%d exemption(s) are no longer true:\n  %s",
			len(staleExemptions), strings.Join(staleExemptions, "\n  "))
	}
}

// TestReachability_EveryExemptionNamesABlocker: an exemption whose reason does
// not point at a recorded external blocker is somebody's opinion. The goal
// document is explicit that BLOCKED_EXTERNAL is never for missing code.
func TestReachability_EveryExemptionNamesABlocker(t *testing.T) {
	root := repoRoot(t)
	blockers, err := os.ReadFile(filepath.Join(root, "docs", "build", "BLOCKERS.md"))
	if err != nil {
		t.Fatalf("read BLOCKERS.md: %v", err)
	}
	body := string(blockers)
	ids := regexp.MustCompile(`B-\d\d`)
	for key, reason := range unreachableOnPurpose {
		named := ids.FindAllString(reason, -1)
		if len(named) == 0 {
			t.Errorf("%s is exempt for the reason %q, which names no blocker; "+
				"an exemption without an external cause is a defect wearing a comment", key, reason)
			continue
		}
		for _, id := range named {
			if !strings.Contains(body, "## "+id+" ") {
				t.Errorf("%s cites %s, which BLOCKERS.md does not define", key, id)
			}
		}
	}
}

func mutatorsIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		for _, m := range mutator.FindAllStringSubmatch(string(src), -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				out = append(out, m[1])
			}
		}
	}
	sort.Strings(out)
	return out
}

// calledOutside reports whether any non-test file outside the declaring
// package, in a tree a deployment runs, invokes the method.
func calledOutside(t *testing.T, root, pkg, method string) bool {
	t.Helper()
	found := false
	for _, tree := range reachableFrom {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, err error) error {
			if err != nil || found {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel := filepath.ToSlash(mustRel(t, root, filepath.Dir(path)))
			if rel == pkg {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			// The file must IMPORT the declaring package. Without this,
			// `.Create(ctx, tx` in any file counts as a call to every
			// `Create` in the codebase, and `Create`, `Execute`, `Activate`
			// and `SetStatus` are names four packages share.
			if !strings.Contains(string(src), `"github.com/nodal/controlplane/`+pkg+`"`) {
				return nil
			}
			for _, m := range callSite.FindAllStringSubmatch(string(src), -1) {
				if m[1] == method {
					found = true
					return filepath.SkipAll
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
	return found
}

func mustRel(t *testing.T, base, target string) string {
	t.Helper()
	rel, err := filepath.Rel(base, target)
	if err != nil {
		t.Fatalf("relative path: %v", err)
	}
	return rel
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
