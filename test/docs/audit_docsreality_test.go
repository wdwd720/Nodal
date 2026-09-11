package docsref

// Adversarial audit of the docs-vs-reality surface (goal §54, wave B area 10).
//
// Every test in this file is a REPRODUCTION of a finding, not a proposed
// invariant. Each one fails on `productization` at 6e620f9. Nothing here
// changes product code or a document; the fixes belong to whoever the
// orchestrator assigns them to.
//
// The area's rule, from the brief: a finding is a sentence, table row, count,
// route, file path, test name, flag or number in a document that the code, the
// schema, the tests or the configuration does not bear out -- or a mechanism
// the code has that the documents say does not exist. The second half is what
// most of this file is about, because the existing checks in this package all
// look for the first.
//
// The findings, in the order they appear below:
//
//	F-docs-1   POLICY_AUTHORITY §1 condition 4 names LIVE_* and WITHDRAWALS as
//	           the capabilities that need the four evidence references;
//	           gates.IsHighRisk is true for eighteen of twenty, CREDIT_PURCHASE
//	           and both payout capabilities among them. §1's state machine also
//	           omits SANDBOX, which has been a gate state since 00755
//	F-docs-2   VERIFICATION_AND_WITHDRAWAL §2 sends a caller to
//	           POST /v1/me/terms/accept; no such route exists anywhere
//	F-docs-3   USER_JOURNEY §1 says onboarding calls PUT /v1/me/profile; the
//	           route is POST and PUT is not served
//	F-docs-4   SECURITY.md tells a reviewer that test/security, test/contract,
//	           infra/ and docs/runbooks/ do not exist, that the security CI step
//	           passes vacuously, and lists as "Planned" seven adversarial suites
//	           that are written and pass
//	F-docs-5   THREAT_MODEL's top-ten residual risks 9 and 10 name as unbuilt
//	           the two test trees, the Terraform, the git remote and
//	           internal/{reconciliation,settlement,execution}
//	F-docs-6   .env.example gives the settlement pair as `solana` + the MAINNET
//	           USDC mint; scripts/seed registers `solana-devnet` + the devnet
//	           mint, and cmd/api refuses to start on a pair it cannot resolve
//	F-docs-7   PROVIDER_BOUNDARY §4 calls PAYOUT_KYC a capability that is
//	           DISABLED; internal/gates declares twenty and that is not one
//	F-docs-8   CREDIT_ECONOMY §7 says the activity feed carries no verification,
//	           profile or agent events and that they are "the documented
//	           extension point"; internal/activity/doc.go says the opposite and
//	           eleven such Sources exist. CURRENT_SYSTEM_INVENTORY says seven
//	           kinds; there are eighteen
//	F-docs-9   UI_UX_SYSTEM §10 says Buy Credits is not a shell action, that no
//	           endpoint sells Credits, and that the shell offers "Add funds" --
//	           the phrase USER_JOURNEY forbids by name
//	F-docs-10  PRODUCT_ARCHITECTURE's domain table and ADR-0027 name
//	           internal/credits and internal/payments; neither exists
//	F-docs-11  README calls apps/web a Next.js app and reports the e2e, UI-e2e
//	           and chaos tiers as pending
//	F-docs-12  BACKUP_RESTORE marks DISASTER_RECOVERY.md and
//	           docs/runbooks/database-corruption.md "(pending)"; both exist
//	F-docs-13  CURRENT_SYSTEM_INVENTORY says "Routes added (nine)" over a table
//	           of ten rows
//	F-docs-14  the decision register's test citations are checked by nothing,
//	           and one of them names a function that does not exist

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/nodal/controlplane/internal/gates"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// read returns a document's bytes as a string, failing the test if it is gone.
// A check that silently skips a document it cannot open is a check that stops
// running the day somebody renames the file.
func read(t *testing.T, root, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	require(t, err == nil, "reading %s: %v", rel, err)
	return string(body)
}

// lineOf returns the 1-based line number of the first occurrence of needle, or
// 0. Findings are reported with a file:line, so the tests carry one too.
func lineOf(body, needle string) int {
	idx := strings.Index(body, needle)
	if idx < 0 {
		return 0
	}
	return 1 + strings.Count(body[:idx], "\n")
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	return err == nil
}

// ---------------------------------------------------------------------------
// F-docs-2, F-docs-3: a route a document tells a caller to use
// ---------------------------------------------------------------------------

// servedRoutes reads the route table out of the generated chi server. That
// table -- not openapi.yaml -- is what the process actually mounts, which is
// the thing a documented call meets.
func servedRoutes(t *testing.T, root string) map[string][]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash("internal/gen/api/api.gen.go")))
	require(t, err == nil, "reading the generated server: %v", err)
	re := regexp.MustCompile(`r\.(Get|Post|Put|Delete|Patch|Head|Options)\(options\.BaseURL\+"([^"]+)"`)
	out := map[string][]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out[strings.ToUpper(m[1])] = append(out[strings.ToUpper(m[1])], m[2])
	}
	require(t, len(out) >= 4 && len(out["GET"]) > 30,
		"only %d methods and %d GET routes found; the generated router changed shape and this check stopped seeing it",
		len(out), len(out["GET"]))
	return out
}

// routeMatches is chi's matching rule, narrowed to what a document can write: a
// {param} segment eats exactly one non-empty segment. It is what makes
// `POST /v1/webhooks/stripe_credit` a true citation of `/webhooks/{provider}`
// and `POST /v1/me/terms/accept` a false one.
func routeMatches(template, concrete string) bool {
	tp := strings.Split(strings.Trim(template, "/"), "/")
	cp := strings.Split(strings.Trim(concrete, "/"), "/")
	if len(tp) != len(cp) {
		return false
	}
	for i := range tp {
		if strings.HasPrefix(tp[i], "{") {
			if cp[i] == "" {
				return false
			}
			continue
		}
		if tp[i] != cp[i] {
			return false
		}
	}
	return true
}

// routeDocs are the documents that tell somebody which call to make. A page, a
// client or a reviewer follows these; a route named here that the process does
// not mount is an instruction that fails on the first attempt.
var routeDocs = []string{
	"docs/product/USER_JOURNEY.md",
	"docs/product/VERIFICATION_AND_WITHDRAWAL.md",
	"docs/product/CREDIT_ECONOMY.md",
	"docs/product/PRODUCT_ARCHITECTURE.md",
	"docs/product/STAGING_E2E.md",
	"docs/product/PROVIDER_BOUNDARY.md",
}

var docRoute = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE) ` + "`?" + `(/v1/[A-Za-z0-9_{}:/.-]+)`)

func TestAuditDocs_EveryRouteAProductDocumentNamesIsServed(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	served := servedRoutes(t, root)

	var problems []string
	cited := 0
	for _, rel := range routeDocs {
		body := read(t, root, rel)
		for _, line := range strings.Split(body, "\n") {
			for _, m := range docRoute.FindAllStringSubmatch(line, -1) {
				method, p := m[1], strings.TrimRight(m[2], "`.,;)")
				p = strings.TrimPrefix(p, "/v1")
				if p == "" {
					continue
				}
				cited++
				hit, anyMethod := false, false
				for meth, tmpls := range served {
					for _, tmpl := range tmpls {
						if !routeMatches(tmpl, p) {
							continue
						}
						anyMethod = true
						if meth == method {
							hit = true
						}
					}
				}
				if hit {
					continue
				}
				why := "no route matches that path"
				if anyMethod {
					why = "the path is served, but not with " + method
				}
				problems = append(problems, rel+":"+strconv.Itoa(lineOf(body, m[0]))+
					" tells a caller to use "+method+" /v1"+p+" -- "+why)
			}
		}
	}
	require(t, cited > 40, "only %d routes cited across the product documents; this check stopped seeing them", cited)
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d documented call(s) the process does not answer:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-10: a package a document names
// ---------------------------------------------------------------------------

// The existing path check next door resolves only citations that name a Go TEST
// file or something under test/, because those are cheap to resolve exactly. A
// domain table that points a reader at the package owning a domain is the other
// citation a reader follows, and nothing resolved it: PRODUCT_ARCHITECTURE has
// pointed at `internal/credits` and `internal/payments` since it was written,
// and the packages are `internal/credit` and -- for payments -- no package at
// all, because PaymentIntents live in internal/credit beside the mint they
// cause.
var packageDocs = []string{
	"docs/product/PRODUCT_ARCHITECTURE.md",
	"docs/product/CREDIT_ECONOMY.md",
	"docs/product/VERIFICATION_AND_WITHDRAWAL.md",
	"docs/product/PROVIDER_BOUNDARY.md",
	"docs/adr/0022-one-identity-source-of-truth.md",
	"docs/adr/0023-the-sandbox-tier.md",
	"docs/adr/0024-how-a-principal-becomes-an-operator.md",
	"docs/adr/0025-verification-is-provider-hosted-and-evidence-based.md",
	"docs/adr/0026-the-conversion-request-is-payout-requests.md",
	"docs/adr/0027-a-position-is-the-sum-of-its-fills.md",
	"docs/adr/0028-notifications-and-realtime-on-one-instance.md",
	"docs/adr/0029-agents-as-a-constrained-authority-management-surface.md",
}

// pkgRef matches a backticked Go package directory: `internal/x`, `cmd/y`,
// `internal/x/y`. A selector (`internal/gates.Checker`) and a file
// (`internal/gates/sandbox.go`) are deliberately out: the first is a symbol and
// the second is already covered next door.
var pkgRef = regexp.MustCompile("`((?:internal|cmd)/[a-z0-9]+(?:/[a-z0-9]+)*)`")

func TestAuditDocs_EveryPackageAProductDocumentNamesExists(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	var problems []string
	cited := 0
	for _, rel := range packageDocs {
		body := read(t, root, rel)
		seen := map[string]bool{}
		for _, m := range pkgRef.FindAllStringSubmatch(body, -1) {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			cited++
			if exists(root, m[1]) {
				continue
			}
			problems = append(problems, rel+":"+strconv.Itoa(lineOf(body, m[0]))+" names "+m[1])
		}
	}
	require(t, cited > 50, "only %d package citations found; this check stopped seeing them", cited)
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d package(s) a document points a reader at do not exist:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-7: a capability a document names
// ---------------------------------------------------------------------------

// A capability name is not decoration: it is the thing an operator types into
// `POST /v1/admin/gates/{capability}/{action}` and the thing a reviewer looks
// for in `capability_gates`. A document that reports a capability as DISABLED
// when no such capability is declared describes a control that is not there --
// and PROVIDER_BOUNDARY does, for PAYOUT_KYC, which is a verification LEVEL.
var capabilityDocs = []string{
	"docs/product/PROVIDER_BOUNDARY.md",
	"docs/product/PRODUCT_ARCHITECTURE.md",
	"docs/product/CREDIT_ECONOMY.md",
	"docs/audit/LAUNCH_GATE_MATRIX.md",
}

// capabilityClaim matches "the `A`, `B` and `C` capabilities" and "the `A`
// capability": the shape that asserts the names ARE capabilities. Prose that
// merely mentions a name is not caught, and should not be. `(?s)` because the
// list that carries the defect is wrapped across two lines.
var capabilityClaim = regexp.MustCompile("(?s)((?:`[A-Z][A-Z_]{2,}`[\\s,]*(?:and[\\s]*)?)+)capabilit(?:y|ies)")

var backtickedUpper = regexp.MustCompile("`([A-Z][A-Z_]{2,})`")

func TestAuditDocs_EveryCapabilityADocumentNamesIsDeclared(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	declared := map[string]bool{}
	for _, c := range gates.AllCapabilities() {
		declared[string(c)] = true
	}
	require(t, len(declared) > 10, "only %d capabilities declared; this check is looking at the wrong thing", len(declared))

	var problems []string
	claims := 0
	for _, rel := range capabilityDocs {
		body := read(t, root, rel)
		for _, m := range capabilityClaim.FindAllStringSubmatch(body, -1) {
			for _, n := range backtickedUpper.FindAllStringSubmatch(m[1], -1) {
				claims++
				if declared[n[1]] {
					continue
				}
				problems = append(problems, rel+":"+strconv.Itoa(lineOf(body, m[0]))+
					" calls "+n[1]+" a capability; internal/gates declares "+
					strconv.Itoa(len(declared))+" and that is not one")
			}
		}
	}
	require(t, claims > 3, "only %d capability claims found; this check stopped seeing them", claims)
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d name(s) a document calls a capability are not:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-4, F-docs-5, F-docs-12: an absence the tree contradicts
// ---------------------------------------------------------------------------

// A document may name an absent thing only while saying it is absent -- that is
// the rule references_test.go already states, and it is the right one. Its
// converse was never checked: a document may not say a thing is absent when it
// is there.
//
// Empirically that is the direction these documents decayed in, and it is not a
// smaller defect. SECURITY.md and THREAT_MODEL.md are the two documents a
// reviewer scores security posture from, and between them they report four
// directories as missing, the security CI step as vacuous, and the
// reconciliation, settlement and execution code as unbuilt. §14 then hands an
// operator incident fallbacks "until they exist" while nineteen runbooks sit in
// docs/runbooks. THREAT_MODEL's own residual risk 8 states the rule this
// breaks: "Listing a closed control among the top ten residual risks
// understates the system in a document a reviewer uses to score its posture,
// which is a truthfulness defect in the same way an overstatement is."
//
// Each row is one sentence, quoted, and the path it is wrong about.
type absenceClaim struct {
	doc      string
	sentence string // quoted from the document; must still be present
	path     string // what it says is missing
}

func absenceClaims() []absenceClaim {
	// Empty, and that is the finding: every stated absence this audit found in
	// these three documents was contradicted by the tree. The rows live on in
	// retiredAbsenceClaims below, where they say the claim may not come back.
	// A new row belongs here the moment a document states an absence that is
	// load-bearing for a reader -- the check is the list, not the emptiness.
	return []absenceClaim{}
}

// retiredAbsenceClaims are the sentences above that have been FIXED. A fixed
// claim leaves this file as a row saying it may not come back: the check that
// caught it is worth more than the one afternoon it took to correct the
// document, and a document that decayed once decays again.
func retiredAbsenceClaims() []absenceClaim {
	return []absenceClaim{
		{"docs/security/SECURITY.md",
			"ls test/security test/contract infra docs/runbooks   # each is absent or empty today", "test/security"},
		{"docs/security/SECURITY.md", "DESIGNED, pending Terraform (`infra/` is empty; EB-012)", "infra/terraform"},
		{"docs/security/SECURITY.md", "compiler DESIGNED (`internal/strategy` absent)", "internal/strategy"},
		{"docs/security/SECURITY.md", "DESIGNED (`internal/model` absent)", "internal/model"},
		{"docs/security/SECURITY.md",
			"`./test/contract/...` — the directory does not exist, so the step is vacuous today", "test/contract"},
		{"docs/security/SECURITY.md",
			"**the directory does not exist, so the security step passes vacuously**", "test/security"},
		{"docs/security/SECURITY.md", "**Runbooks**: `docs/runbooks/` does not exist.", "docs/runbooks"},
		{"docs/security/SECURITY.md",
			"**no writer exists** — nothing on disk records a security event yet", "security_events"},
		{"docs/security/SECURITY.md", "much of it is now wrong in the UNDERSTATING direction", "docs/security/SECURITY.md"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`make security`, `make contract` and `make iac-scan` pass on empty directories; no git remote", "test/security"},
		{"docs/threat-model/THREAT_MODEL.md",
			"the reconciliation, settlement and execution code that must keep running does not exist", "internal/reconciliation"},
		{"docs/threat-model/THREAT_MODEL.md", "no Terraform, IAM task roles", "infra/terraform"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`internal/{signing, wallet, execution, reconciliation, settlement, quote, instruments, intent, agent, strategy, model, prediction}`, every worker binary |",
			"internal/signing"},
		{"docs/operations/BACKUP_RESTORE.md",
			"`docs/operations/DISASTER_RECOVERY.md` (pending)", "docs/operations/DISASTER_RECOVERY.md"},
		{"docs/operations/BACKUP_RESTORE.md",
			"`docs/runbooks/database-corruption.md` (pending)", "docs/runbooks/database-corruption.md"},
		{"docs/operations/BACKUP_RESTORE.md",
			"## 2. Production design (AWS, pending Terraform)", "infra/terraform/modules/rds"},
	}
}

// presenceClaims are the corrected sentences: each names something as PRESENT,
// and each is now the thing that would go stale if the tree lost it. Same
// discipline in the other direction -- the sentence is pinned, so a claim that
// moves is a claim nobody is checking any more.
func presenceClaims() []absenceClaim {
	return []absenceClaim{
		{"docs/security/SECURITY.md",
			"go test -count=1 ./test/security/... ./test/contract/...", "test/security"},
		{"docs/security/SECURITY.md",
			"go test -count=1 ./test/security/... ./test/contract/...", "test/contract"},
		{"docs/security/SECURITY.md",
			"IMPLEMENTED in `infra/terraform/modules/ecs-service`", "infra/terraform/modules/ecs-service"},
		{"docs/security/SECURITY.md",
			"compiler IMPLEMENTED (`internal/strategy/compiler.go`", "internal/strategy/compiler.go"},
		{"docs/security/SECURITY.md",
			"IMPLEMENTED (`internal/model/prompt.go`", "internal/model/prompt.go"},
		{"docs/security/SECURITY.md",
			"`docs/runbooks/` holds nineteen runbooks and an index", "docs/runbooks/README.md"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`test/security` holds twenty test files and `test/contract` eight over recorded provider fixtures",
			"test/security"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`test/security` holds twenty test files and `test/contract` eight over recorded provider fixtures",
			"test/contract"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`internal/reconciliation`, `internal/settlement` and `internal/execution` hold 67 Go files",
			"internal/reconciliation"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`internal/reconciliation`, `internal/settlement` and `internal/execution` hold 67 Go files",
			"internal/settlement"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`internal/reconciliation`, `internal/settlement` and `internal/execution` hold 67 Go files",
			"internal/execution"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`infra/terraform` carries per-service task roles (`modules/ecs-service`)",
			"infra/terraform/modules/ecs-service"},
		{"docs/threat-model/THREAT_MODEL.md",
			"`nativemarket.ConservativeSafetyPolicy()` sets `circuit_breaker_move_bps: 0`",
			"internal/nativemarket/safety.go"},
		{"docs/operations/BACKUP_RESTORE.md",
			"`docs/operations/DISASTER_RECOVERY.md`, `docs/runbooks/database-corruption.md`, BLOCKERS EB-012.",
			"docs/operations/DISASTER_RECOVERY.md"},
		{"docs/operations/BACKUP_RESTORE.md",
			"`docs/operations/DISASTER_RECOVERY.md`, `docs/runbooks/database-corruption.md`, BLOCKERS EB-012.",
			"docs/runbooks/database-corruption.md"},
		{"docs/operations/BACKUP_RESTORE.md",
			"The design above is in `infra/terraform/modules/rds`", "infra/terraform/modules/rds"},
	}
}

// writerClaims are claims about CODE rather than about a directory. The defect
// was the sharpest instance of the one above: SECURITY.md §12 said in bold that
// nothing on disk records a security event, while REQUIREMENTS_TRACEABILITY's
// R-130-1 -- IN_PROGRESS, in the other document a reviewer reads -- listed the
// four packages that do. Two documents in the same tree, opposite answers, and
// the security one is the one somebody scores posture from.
//
// Corrected, the row runs the other way: §12 now names the four writers, and
// each named file has to still contain the INSERT. A document that lists its
// evidence is only better than one that does not if the evidence is checked.
type writerClaim struct {
	doc, sentence, what string
	writers             []string // files that must contain `INSERT INTO <what>`
}

func writerClaims() []writerClaim {
	return []writerClaim{
		{"docs/security/SECURITY.md",
			"Four packages write rows: `internal/identity/login.go`",
			"security_events",
			[]string{"internal/identity/login.go", "internal/funding/service.go",
				"internal/signing/repository.go", "internal/webhook/handler.go"}},
	}
}

func TestAuditDocs_NoDocumentDeclaresAnAbsenceTheTreeContradicts(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	var problems []string
	for _, c := range writerClaims() {
		body := read(t, root, c.doc)
		if !strings.Contains(body, c.sentence) {
			problems = append(problems, c.doc+" no longer contains the sentence this check reads: "+
				strconv.Quote(c.sentence))
			continue
		}
		for _, f := range c.writers {
			src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
			if err != nil || !strings.Contains(string(src), "INSERT INTO "+c.what) {
				problems = append(problems, c.doc+":"+strconv.Itoa(lineOf(body, c.sentence))+
					" names "+f+" as a writer of "+c.what+"; it does not write one")
			}
		}
	}
	for _, c := range absenceClaims() {
		body := read(t, root, c.doc)
		if !strings.Contains(body, c.sentence) {
			// The sentence moved. That is not a pass: a claim that moved is a
			// claim nobody is checking any more (the rule counts_test.go states).
			problems = append(problems, c.doc+" no longer contains the sentence this check reads: "+
				strconv.Quote(c.sentence))
			continue
		}
		if !exists(root, c.path) {
			continue // the document is right
		}
		problems = append(problems, c.doc+":"+strconv.Itoa(lineOf(body, c.sentence))+
			" says "+c.path+" is absent; it is in the repository")
	}
	for _, c := range retiredAbsenceClaims() {
		body := read(t, root, c.doc)
		if strings.Contains(body, c.sentence) {
			problems = append(problems, c.doc+":"+strconv.Itoa(lineOf(body, c.sentence))+
				" states again that "+c.path+" is absent: "+strconv.Quote(c.sentence))
		}
	}
	for _, c := range presenceClaims() {
		body := read(t, root, c.doc)
		if !strings.Contains(body, c.sentence) {
			problems = append(problems, c.doc+" no longer contains the sentence this check reads: "+
				strconv.Quote(c.sentence))
			continue
		}
		if !exists(root, c.path) {
			problems = append(problems, c.doc+":"+strconv.Itoa(lineOf(body, c.sentence))+
				" names "+c.path+" as present; it is not in the repository")
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d stated absence(s) the tree contradicts:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// TestAuditDocs_NothingListedAsPlannedIsAlreadyWritten answers the question the
// brief asks about SECURITY.md's PART 155 matrix directly: is the "Planned
// (traceability)" column honest, or is it F-111's claim-dressed-as-a-plan?
//
// It was honest in FORM and false in SUBSTANCE. The column named eight suites by
// the filename they were going to take; the suites landed under different
// filenames in the same directory and pass today. And because references_test.go
// exempted any PARAGRAPH containing the word "planned" from its path check, and a
// markdown table is one paragraph, the whole matrix was invisible to the control
// that would otherwise have caught the left-hand column going stale with it --
// which is how "IDOR ... (primitive only; no HTTP handlers exist)" survived
// cmd/api. That exemption is now per-cell and scoped to the planned COLUMN
// (F-238), and this check holds the rows themselves.
//
// The rule, in both directions:
//
//   - a row whose subject already has a passing proof must NAME that proof in
//     the "Exists today" column and plan nothing;
//   - a row whose Planned cell is still honest must name a suite that is not on
//     disk, and say what would prove it.
func provenNotPlanned() []struct{ item, retiredPlan, proof string } {
	return []struct{ item, retiredPlan, proof string }{
		{"IDOR / cross-tenant reads / cross-tenant writes", "test/security/{idor,cross_tenant}_test.go",
			"TestIDOR_CoversEveryAccountScopedRoute"},
		{"IDOR / cross-tenant reads / cross-tenant writes", "test/security/{idor,cross_tenant}_test.go",
			"TestIDOR_CrossTenantAccountReadsAreRefused"},
		{"Prompt injection", "test/security/prompt_injection_test.go",
			"TestPromptInjection_UntrustedTextNeverEntersTheInstructionChannel"},
		{"Agent withdrawal attempt", "test/security/agent_withdrawal_attempt_test.go",
			"TestAgentPrincipalPermissionSetIsClosed"},
		{"Admin privilege misuse", "test/security/admin_privilege_misuse_test.go",
			"TestDualControl_AProposerCannotApproveItsOwnAction"},
		{"Production capability bypass", "test/security/production_capability_bypass_test.go",
			"TestProductionRefusesFakeProviders"},
		{"Fake provider activation in production", "test/security/fake_provider_in_prod_test.go",
			"TestLocalDefaultsAreNotProductionValid"},
		{"Stolen session scenarios", "test/security/stolen_session_test.go",
			"TestReplay_ForgedSessionCookiesAreRefused"},
	}
}

// genuinelyPlanned are the four rows whose Planned cell is still true. Each must
// name a suite that is NOT on disk -- the day one of them lands, this check is
// what says the matrix is now out of date.
func genuinelyPlanned() []struct{ item, plan string } {
	return []struct{ item, plan string }{
		{"CSRF", "test/security/csrf_test.go"},
		{"SSRF", "test/security/ssrf_test.go"},
		{"Webhook forgery", "test/security/webhook_forgery_test.go"},
		{"Dependency compromise", "test/security/dependency_compromise_test.go"},
	}
}

// part155Row is one row of the matrix, split into the two columns that carry a
// claim.
type part155Row struct{ exists, planned string }

// part155Rows reads the matrix out of the document. Reading the table rather
// than searching the whole file is the point: a proof named in the Planned
// column and a proof named in the Exists column are opposite claims, and a check
// that only asked "does this string appear somewhere" could not tell them apart.
func part155Rows(t *testing.T, body string) map[string]part155Row {
	t.Helper()
	const header = "| PART 155 item | Exists today (file → test) | Planned (traceability) |"
	idx := strings.Index(body, header)
	require(t, idx >= 0, "SECURITY.md no longer carries the PART 155 matrix this check reads")
	out := map[string]part155Row{}
	for _, line := range strings.Split(body[idx:], "\n") {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := tableCells(line)
		if len(cells) != 3 || strings.HasPrefix(strings.TrimSpace(cells[0]), "---") {
			continue
		}
		out[strings.TrimSpace(cells[0])] = part155Row{
			exists:  strings.TrimSpace(cells[1]),
			planned: strings.TrimSpace(cells[2]),
		}
	}
	require(t, len(out) > 12, "only %d PART 155 rows parsed; this check stopped seeing the matrix", len(out))
	return out
}

func TestAuditDocs_NothingListedAsPlannedIsAlreadyWritten(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	const doc = "docs/security/SECURITY.md"
	body := read(t, root, doc)
	rows := part155Rows(t, body)
	declared := declaredTests(t, root)

	var problems []string
	for _, r := range provenNotPlanned() {
		row, ok := rows[r.item]
		if !ok {
			problems = append(problems, doc+" no longer has a PART 155 row for "+strconv.Quote(r.item))
			continue
		}
		if !declared[r.proof] {
			problems = append(problems, doc+" row "+strconv.Quote(r.item)+" is held to "+r.proof+
				", which is not declared anywhere: either the proof was renamed or it was deleted")
			continue
		}
		if !strings.Contains(row.exists, r.proof) {
			problems = append(problems, doc+" row "+strconv.Quote(r.item)+
				" does not name its passing proof "+r.proof+" in the Exists column")
		}
		if strings.Contains(row.planned, r.retiredPlan) {
			problems = append(problems, doc+" row "+strconv.Quote(r.item)+" still plans "+r.retiredPlan+
				"; "+r.proof+" is written and passes")
		}
	}
	for _, r := range genuinelyPlanned() {
		row, ok := rows[r.item]
		if !ok {
			problems = append(problems, doc+" no longer has a PART 155 row for "+strconv.Quote(r.item))
			continue
		}
		if !strings.Contains(row.planned, r.plan) {
			problems = append(problems, doc+" row "+strconv.Quote(r.item)+
				" no longer names "+r.plan+" as the suite it plans")
			continue
		}
		if exists(root, r.plan) {
			problems = append(problems, doc+" row "+strconv.Quote(r.item)+" plans "+r.plan+
				", which is already in the repository")
		}
	}
	// The other half of the rule, over every row rather than a list: a Planned
	// cell may not name a path that is on disk.
	for item, row := range rows {
		for _, m := range pathRef.FindAllStringSubmatch(row.planned, -1) {
			if isTestCitation(m[1]) && exists(root, m[1]) {
				problems = append(problems, doc+" row "+strconv.Quote(item)+" plans "+m[1]+
					", which is already in the repository")
			}
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d PART 155 row(s) the matrix describes wrongly:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-8: the activity feed
// ---------------------------------------------------------------------------

// CREDIT_ECONOMY §7 exists to say what is NOT built, "so nobody has to discover
// it". One of its five bullets is false in the understating direction, and the
// package it cites as the authority contradicts it in as many words.
//
// The count is derived from internal/activity rather than recalled, for the
// reason counts_test.go already gives: a number somebody verified once is a
// number that will be wrong within a week and nobody will know which week.
func activityKinds(t *testing.T, root string) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash("internal/activity/activity.go")))
	require(t, err == nil, "reading internal/activity/activity.go: %v", err)
	re := regexp.MustCompile(`Kind[A-Za-z0-9]+ Kind = "([A-Z_]+)"`)
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out = append(out, m[1])
	}
	require(t, len(out) > 5, "only %d activity kinds parsed; this check stopped seeing them", len(out))
	return out
}

func TestAuditDocs_TheActivityFeedIsDescribedAsItIsBuilt(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	kinds := activityKinds(t, root)
	have := map[string]bool{}
	for _, k := range kinds {
		have[k] = true
	}

	var problems []string

	// (a) The "not built" bullet. The document may state ONE absence here --
	// security events -- and every other kind it used to deny has to be
	// declared, derived from the package rather than recalled.
	const ce = "docs/product/CREDIT_ECONOMY.md"
	ceBody := read(t, root, ce)
	const bullet = "**No security events in the activity feed.**"
	require(t, strings.Contains(ceBody, bullet), "%s no longer carries the bullet this check reads", ce)
	at := ce + ":" + strconv.Itoa(lineOf(ceBody, bullet))
	for _, k := range []string{"VERIFICATION_UPDATED", "TERMS_ACCEPTED", "ACCOUNT_CLOSURE_REQUESTED",
		"ACCOUNT_CLOSURE_DECIDED", "AGENT_CREATED", "AGENT_PAUSED", "AGENT_RESUMED", "AGENT_DISABLED",
		"PAYOUT_DESTINATION_ADDED", "PAYOUT_DESTINATION_DISABLED"} {
		if !have[k] {
			problems = append(problems, at+" says only security events are missing from the feed; "+
				k+" is not declared either")
		}
	}
	for _, k := range kinds {
		if strings.HasPrefix(k, "SECURITY_") || k == "LOGIN_ANOMALY" || k == "SESSION_REVOKED" {
			problems = append(problems, at+" says the feed carries no security events; internal/activity declares "+k)
		}
	}

	// (b) The package it cites as the authority has to still say it. The
	// defect was a document citing doc.go for the opposite of what doc.go said.
	docGo, err := os.ReadFile(filepath.Join(root, filepath.FromSlash("internal/activity/doc.go")))
	require(t, err == nil, "reading internal/activity/doc.go: %v", err)
	if !strings.Contains(string(docGo), "Security events are still absent") {
		problems = append(problems, ce+" cites internal/activity/doc.go for the security absence; "+
			"doc.go no longer states it")
	}
	if !strings.Contains(string(docGo), "have landed (D-081)") {
		problems = append(problems, ce+" says the verification, profile and agent kinds landed with D-081; "+
			"internal/activity/doc.go no longer says so")
	}

	// (c) The inventory's count, read out of the sentence and compared with the
	// package. A number recalled beside the rows that would have produced it is
	// the F-111 shape; this one is derived on both sides.
	const inv = "docs/build/CURRENT_SYSTEM_INVENTORY.md"
	invBody := read(t, root, inv)
	countRe := regexp.MustCompile(`the §16 timeline: ([a-z]+) kinds`)
	m := countRe.FindStringSubmatch(invBody)
	require(t, m != nil, "%s no longer carries the §16 timeline count this check reads", inv)
	if m[1] != numberWord(len(kinds)) {
		problems = append(problems, inv+":"+strconv.Itoa(lineOf(invBody, m[0]))+
			" says the timeline has "+m[1]+" kinds; internal/activity declares "+numberWord(len(kinds)))
	}

	// (d) The traceability row. It was held at IN_PROGRESS by an evidence cell
	// that was simply out of date, and the summary counts are computed from
	// statuses, so a row kept open by a stale sentence understates the matrix.
	const trace = "docs/build/REQUIREMENTS_TRACEABILITY.md"
	traceBody := read(t, root, trace)
	var row string
	for _, line := range strings.Split(traceBody, "\n") {
		if strings.HasPrefix(line, "| R-PG-016-1 |") {
			row = line
		}
	}
	require(t, row != "", "%s no longer carries R-PG-016-1", trace)
	if strings.Contains(row, "| IN_PROGRESS |") || strings.Contains(row, "| NOT_STARTED |") {
		problems = append(problems, trace+":"+strconv.Itoa(lineOf(traceBody, row))+
			" keeps R-PG-016-1 open; internal/activity declares "+strconv.Itoa(len(kinds))+" kinds")
	}
	declared := declaredTests(t, root)
	cites := false
	for _, name := range distinct(refPattern.FindAllStringSubmatch(row, -1)) {
		if declared[name] {
			cites = true
		}
	}
	if !cites {
		problems = append(problems, trace+":"+strconv.Itoa(lineOf(traceBody, row))+
			" names no test that exists; a VERIFIED row without one is the rule this document states")
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d claim(s) about the activity feed the code does not bear out:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-9: the shell
// ---------------------------------------------------------------------------

// UI_UX_SYSTEM §10 is the design system's list of where it departs from the
// brief. Three of its entries describe a shell that is not the one apps/web
// ships, and one of them puts the exact phrase USER_JOURNEY forbids -- "Add
// funds", never that, always "Buy Credits" -- into the design system as what
// the product says. A copywriter reading the design system for the product's
// own vocabulary is told to use the banned word.
func TestAuditDocs_TheDesignSystemDescribesTheShellThatShipped(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	const doc = "docs/product/UI_UX_SYSTEM.md"
	body := read(t, root, doc)
	shell, err := os.ReadFile(filepath.Join(root, filepath.FromSlash("apps/web/src/components/AppShell.tsx")))
	require(t, err == nil, "reading AppShell.tsx: %v", err)
	shellSrc := string(shell)

	var problems []string

	// §10.8 and §10.9, corrected. Each sentence is pinned, and each is checked
	// against the shell rather than against the brief it was written from.
	claims := []struct{ sentence, why string }{
		{"**8. Search is not in the shell; notifications are.**", "notifications"},
		{"`AppShell.tsx` declares `PRIMARY_ACTIONS` as **Buy Credits**", "buycredits"},
		{"`POST /v1/payments`\nsells Credits and `GET /v1/credits/pricing` publishes the rate", "sells"},
	}
	for _, c := range claims {
		require(t, strings.Contains(body, c.sentence),
			"%s no longer contains %q; the claim moved", doc, c.sentence)
		at := doc + ":" + strconv.Itoa(lineOf(body, c.sentence))
		switch c.why {
		case "notifications":
			if !strings.Contains(shellSrc, `label: "Notifications"`) {
				problems = append(problems, at+" says notifications are in the shell; "+
					"AppShell.tsx declares no Notifications destination")
			}
			if strings.Contains(shellSrc, `label: "Search"`) {
				problems = append(problems, at+" says search is not in the shell; AppShell.tsx declares it")
			}
		case "buycredits":
			if !strings.Contains(shellSrc, `label: "Buy Credits"`) {
				problems = append(problems, at+` says the shell offers "Buy Credits"; AppShell.tsx does not`)
			}
		case "sells":
			mounted := false
			for _, p := range servedRoutes(t, root)["POST"] {
				if p == "/payments" {
					mounted = true
				}
			}
			if !mounted {
				problems = append(problems, at+" says POST /v1/payments sells Credits; it is not mounted")
			}
		}
	}

	// The phrase USER_JOURNEY forbids by name may appear in docs/product only in
	// the sentence that forbids it. The design system is where a copywriter
	// looks the product's vocabulary up, which is what made this one expensive.
	const forbidding = `**Buy Credits** (never "add funds")`
	uj := read(t, root, "docs/product/USER_JOURNEY.md")
	require(t, strings.Contains(uj, forbidding),
		"USER_JOURNEY.md no longer forbids the phrase on one line, so no grep can find it")
	err = filepath.WalkDir(filepath.Join(root, "docs", "product"), func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return werr
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(strings.ToLower(line), "add funds") || strings.Contains(line, forbidding) {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			problems = append(problems, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+
				` uses the phrase "add funds", which USER_JOURNEY forbids by name`)
		}
		return nil
	})
	require(t, err == nil, "walking docs/product: %v", err)

	// §11's remaining work: two items that were done stayed on the list, and the
	// screen count it opens with predates most of the app.
	done := []struct{ sentence, evidence, why string }{
		{"put `SegmentedBar` on Home", "apps/web/src/pages/home/Home.tsx", "SegmentedBar"},
		{"replace the generic `Explanation` with `Refusal`", "apps/web/src/components/Refusal.tsx", "Refusal"},
	}
	for _, d := range done {
		if !strings.Contains(body, d.sentence) {
			continue // struck from the list, which is the fix
		}
		src, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(d.evidence)))
		if rerr != nil {
			continue
		}
		switch d.why {
		case "SegmentedBar":
			if strings.Contains(string(src), "SegmentedBar") {
				problems = append(problems, doc+":"+strconv.Itoa(lineOf(body, d.sentence))+
					" lists putting SegmentedBar on Home as still to do; Home.tsx renders one")
			}
		case "Refusal":
			if !exists(root, "apps/web/src/components/Explanation.tsx") {
				problems = append(problems, doc+":"+strconv.Itoa(lineOf(body, d.sentence))+
					" lists replacing the generic Explanation as still to do; Explanation.tsx is gone and Refusal.tsx is there")
			}
		}
	}
	const screens = "`apps/web/src/pages` now holds forty-one page\nmodules"
	require(t, strings.Contains(body, screens), "%s no longer states the page count this check reads", doc)
	pages := 0
	require(t, filepath.WalkDir(filepath.Join(root, "apps", "web", "src", "pages"),
		func(path string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".tsx") ||
				strings.Contains(d.Name(), ".test.") {
				return werr
			}
			pages++
			return nil
		}) == nil, "walking apps/web/src/pages")
	if numberWord(pages) != "forty-one" {
		problems = append(problems, doc+":"+strconv.Itoa(lineOf(body, screens))+
			" says forty-one page modules; apps/web/src/pages holds "+strconv.Itoa(pages))
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d claim(s) in the design system the shipped app contradicts:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-1: the authority model
// ---------------------------------------------------------------------------

// POLICY_AUTHORITY is the contract for who may do what. Two of its statements
// about the capability gate are now wrong, both in the direction that makes the
// control look smaller than it is:
//
//   - condition 4 parenthesises the capabilities that need the four evidence
//     references as "LIVE_* and WITHDRAWALS". gates.IsHighRisk -- which is what
//     Propose and cp_gate_is_high_risk actually consult -- is true for eighteen
//     of twenty, CREDIT_PURCHASE, PAYOUT_RESERVE, PAYOUT_SETTLE, MARKETPLACE and
//     NATIVE_MARKET_TRADING among them. A reviewer scoping the ceremony from this
//     document would under-plan every one of them, and CREDIT_PURCHASE is the
//     gate LAUNCH_GATE_MATRIX says holds the money path shut;
//   - the state machine it prints has seven states. SANDBOX has been the eighth
//     since migration 00755, and ADR-0023 cites this very section as the thing
//     it constrains.
func TestAuditDocs_ThePolicyAuthorityDescribesTheGateTheCodeEnforces(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	const doc = "docs/architecture/POLICY_AUTHORITY.md"
	body := read(t, root, doc)

	var problems []string

	// Condition 4's scope. The defect was a closed list -- "LIVE_* and
	// WITHDRAWALS" -- beside a predicate that is true for eighteen of twenty.
	// The fix names gates.IsHighRisk as the authority and writes the only two
	// parts of the list that a reader can hold in their head: the count, and
	// the EXCEPTIONS. Both are derived here, so adding a capability or flipping
	// IsHighRisk fails this check instead of quietly under-scoping the ceremony.
	var highRisk, exceptions []string
	for _, c := range gates.AllCapabilities() {
		if gates.IsHighRisk(c) {
			highRisk = append(highRisk, string(c))
			continue
		}
		exceptions = append(exceptions, string(c))
	}
	const cond4 = "every capability `gates.IsHighRisk` returns true for requires all four"
	require(t, strings.Contains(body, cond4), "%s no longer states condition 4 the way this check reads it", doc)
	para := paragraphOf(body, cond4)
	at := doc + ":" + strconv.Itoa(lineOf(body, cond4))
	count := numberWord(len(highRisk)) + " of the " + numberWord(len(gates.AllCapabilities())) + " declared"
	if !strings.Contains(para, count) {
		problems = append(problems, at+" does not say condition 4 applies to "+count+
			" capabilities; that is what gates.IsHighRisk answers today")
	}
	for _, e := range exceptions {
		if !strings.Contains(para, "`"+e+"`") {
			problems = append(problems, at+" does not name "+e+
				", which is one of the capabilities gates.IsHighRisk returns false for")
		}
	}
	if phrase := "The " + numberWord(len(exceptions)) + " exceptions are"; !strings.Contains(para, phrase) {
		problems = append(problems, at+" does not say there are "+numberWord(len(exceptions))+
			" exceptions; gates.IsHighRisk returns false for exactly "+strings.Join(exceptions, ", "))
	}

	const machine = "State machine: `DISABLED → PENDING_APPROVAL → APPROVED → ACTIVE`"
	require(t, strings.Contains(body, machine), "%s no longer prints the state machine this check reads", doc)
	// The section that prints the machine, up to the next heading.
	section := body[strings.Index(body, machine):]
	if i := strings.Index(section, "\n## "); i > 0 {
		section = section[:i]
	}
	for _, s := range []gates.GateState{gates.StateSandbox} {
		if !strings.Contains(section, string(s)) {
			problems = append(problems, doc+":"+strconv.Itoa(lineOf(body, machine))+
				" prints a gate state machine with no "+string(s)+
				" state; internal/gates declares it and migration 00755 created it")
		}
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d claim(s) in the authority contract the gate code does not bear out:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-6: the example environment
// ---------------------------------------------------------------------------

// .env.example is the file README tells a developer to copy. Its settlement
// pair is `solana` plus the MAINNET USDC mint. scripts/seed -- the only thing
// that registers a settlement asset locally -- registers `solana-devnet` plus
// the devnet mint, and cmd/api's composition calls repo.GetByMint on the
// configured pair and returns an error when it does not resolve. So the
// documented local configuration does not boot.
//
// It is also the claim render.yaml refuses in as many words: "Devnet, not
// mainnet ... pointing at mainnet USDC would claim a settlement path this
// deployment does not have."
func TestAuditDocs_TheExampleEnvironmentNamesTheSettlementAssetTheSeedRegisters(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	env := read(t, root, ".env.example")
	seed := read(t, root, "scripts/seed/main.go")

	value := regexp.MustCompile(`(?m)^CP_API_SETTLEMENT_(CHAIN|MINT)=(.+)$`)
	got := map[string]string{}
	for _, m := range value.FindAllStringSubmatch(env, -1) {
		got[m[1]] = strings.TrimSpace(m[2])
	}
	require(t, got["CHAIN"] != "" && got["MINT"] != "",
		".env.example no longer sets CP_API_SETTLEMENT_CHAIN and CP_API_SETTLEMENT_MINT")

	constant := regexp.MustCompile(`(?m)^\s*(chain|usdcDevMint)\s+= "([^"]+)"`)
	want := map[string]string{}
	for _, m := range constant.FindAllStringSubmatch(seed, -1) {
		want[m[1]] = m[2]
	}
	require(t, want["chain"] != "" && want["usdcDevMint"] != "",
		"scripts/seed no longer declares the chain and mint constants this check reads")

	var problems []string
	if got["CHAIN"] != want["chain"] {
		problems = append(problems, ".env.example:"+strconv.Itoa(lineOf(env, "CP_API_SETTLEMENT_CHAIN="))+
			" sets CP_API_SETTLEMENT_CHAIN="+got["CHAIN"]+"; scripts/seed registers assets on "+want["chain"])
	}
	if got["MINT"] != want["usdcDevMint"] {
		problems = append(problems, ".env.example:"+strconv.Itoa(lineOf(env, "CP_API_SETTLEMENT_MINT="))+
			" sets CP_API_SETTLEMENT_MINT="+got["MINT"]+"; scripts/seed registers "+want["usdcDevMint"]+
			" and cmd/api refuses to start on a pair it cannot resolve")
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("the documented local configuration does not resolve against the documented seed:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-11: the README
// ---------------------------------------------------------------------------

// The README is the first document anybody reads. Its architecture summary
// names a framework the repository does not use, and its test-strategy
// paragraph reports three whole tiers as pending while CI runs all three.
func TestAuditDocs_TheReadmeDescribesTheTreeItShipsWith(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	body := read(t, root, "README.md")
	pkg := read(t, root, "apps/web/package.json")
	ci := read(t, root, ".github/workflows/ci.yml")

	var problems []string

	// The framework. The README named one the repository has never depended on,
	// so this reads the manifest rather than the sentence.
	const stack = "React 19 + Vite single-page app in `apps/web`"
	require(t, strings.Contains(body, stack), "README no longer contains %q; the claim moved", stack)
	at := "README.md:" + strconv.Itoa(lineOf(body, stack))
	if strings.Contains(pkg, `"next"`) {
		problems = append(problems, at+" calls apps/web a Vite app; apps/web/package.json declares next")
	}
	for _, dep := range []string{`"vite"`, `"react"`, `"react-router-dom"`} {
		if !strings.Contains(pkg, dep) {
			problems = append(problems, at+" names the stack as React + Vite + react-router; package.json has no "+dep)
		}
	}

	// The test tiers. Three whole tiers were reported as pending while CI ran
	// all three; the sentence now names each directory, and each has to be there
	// with the job that runs it.
	const tiers = "API e2e (`test/e2e`), Playwright UI e2e (`apps/web/e2e`), chaos (`test/chaos`)"
	require(t, strings.Contains(body, tiers), "README no longer contains %q; the claim moved", tiers)
	at = "README.md:" + strconv.Itoa(lineOf(body, tiers))
	for _, tier := range []struct{ dir, job string }{
		{"test/e2e", "  e2e:"},
		{"apps/web/e2e", "  web-e2e:"},
		{"test/chaos", "  chaos:"},
		{"test/security", "  integration:"},
		{"test/contract", "  contract:"},
	} {
		if !exists(root, tier.dir) {
			problems = append(problems, at+" names "+tier.dir+"; it is not in the repository")
		}
		if !strings.Contains(ci, tier.job) {
			problems = append(problems, at+" says CI runs "+tier.dir+"; there is no"+
				strings.TrimSuffix(tier.job, ":")+" job")
		}
	}

	// The load tier. It really is unmeasured -- the defect was the REASON,
	// which named an absent API binary that has been on disk and deployed for
	// days. A true claim with a false reason is a claim nobody can act on.
	const load = "no measured run against a deployed target has been recorded"
	require(t, strings.Contains(body, load), "README no longer contains %q; the claim moved", load)
	if !exists(root, "cmd/api/main.go") {
		problems = append(problems, "README.md:"+strconv.Itoa(lineOf(body, load))+
			" implies cmd/api exists; it does not")
	}
	if strings.Contains(body, "unmeasured until an API binary exists") {
		problems = append(problems, "README.md:"+strconv.Itoa(lineOf(body, "unmeasured until an API binary exists"))+
			" says the load tier is unmeasured until an API binary exists; cmd/api exists and is deployed")
	}

	// What V1 is not. The list outlived two of its own entries.
	const marketplace = "the **marketplace** is built (`internal/commerce`"
	require(t, strings.Contains(body, marketplace), "README no longer contains %q; the claim moved", marketplace)
	if !exists(root, "internal/commerce") {
		problems = append(problems, "README.md:"+strconv.Itoa(lineOf(body, marketplace))+
			" says internal/commerce is built; it is not in the repository")
	}

	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d README claim(s) the tree contradicts:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-13: a count over a table in the same document
// ---------------------------------------------------------------------------

// The inventory states how many routes each wave added and then lists them. The
// agent wave's count and its table disagree by one. Same defect as F-111's
// "this table lists N capabilities" -- a number recalled beside the rows that
// would have produced it.
func TestAuditDocs_EveryRoutesAddedCountMatchesItsTable(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	const doc = "docs/build/CURRENT_SYSTEM_INVENTORY.md"
	body := read(t, root, doc)
	lines := strings.Split(body, "\n")

	words := map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
		"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12}
	// Both spellings the document uses: "**Routes added (10).**" and
	// "**Routes added** (nine; ...".
	header := regexp.MustCompile(`Routes added\**\s*\(?(\d+|[a-z]+)`)
	row := regexp.MustCompile("^\\| ?`?(GET|POST|PUT|PATCH|DELETE)")

	var problems []string
	counted := 0
	for i, line := range lines {
		m := header.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		claimed, err := strconv.Atoi(m[1])
		if err != nil {
			var ok bool
			if claimed, ok = words[m[1]]; !ok {
				continue // "Routes added (all under /v1 ...)" -- no count claimed
			}
		}
		counted++
		// The rows of the first table after the header, and only that table: a
		// run of table rows, ending at the first line that is not one.
		rows, started := 0, false
		for j := i + 1; j < len(lines); j++ {
			if row.MatchString(lines[j]) {
				rows++
				started = true
				continue
			}
			if started {
				break
			}
			if strings.HasPrefix(lines[j], "## ") || strings.HasPrefix(lines[j], "### ") {
				break
			}
		}
		if rows > 0 && rows != claimed {
			problems = append(problems, doc+":"+strconv.Itoa(i+1)+" says "+strconv.Itoa(claimed)+
				" routes were added; the table under it has "+strconv.Itoa(rows)+" rows")
		}
	}
	require(t, counted >= 2, "only %d 'Routes added (n)' headers found; this check stopped seeing them", counted)
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("%d route count(s) disagree with their own table:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// F-docs-14: the decision register's citations
// ---------------------------------------------------------------------------

// references_test.go's inScope list is the documents "a reviewer would use to
// decide whether the system is ready". DECISION_REGISTER.md is not on it, and
// it is the document that records WHY every control is shaped the way it is --
// the one a fixer opens before changing one. Its citations have never been
// resolved by anything, and one of them does not resolve: D-024's "Test change
// (PART 235)" line names TestIntegration_RelayRetriesWithBackoff, which has
// never existed under that name. The test is
// TestIntegration_RelayFailedPublishBacksOffAndRetries.
//
// This is the F-25 shape -- a citation pointing at nothing -- surviving four
// audits because it lives one document outside the check's scope.
func TestAuditDocs_TheDecisionRegisterCitesTestsThatExist(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	const doc = "docs/build/DECISION_REGISTER.md"
	body := read(t, root, doc)
	declared := declaredTests(t, root)
	require(t, len(declared) > 500, "expected the repository's tests, found %d", len(declared))

	var problems []string
	cited := 0
	for _, para := range paragraphs(body) {
		excused := absencePhrase.MatchString(para)
		for _, name := range distinct(refPattern.FindAllStringSubmatch(para, -1)) {
			cited++
			if satisfied(name, declared) || excused {
				continue
			}
			problems = append(problems, doc+":"+strconv.Itoa(lineOf(body, name))+" names "+name)
		}
	}
	require(t, cited > 200, "only %d test citations found in the register; this check stopped seeing them", cited)
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("the decision register cites %d test(s) that do not exist:\n  %s",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// helpers the fixes added
// ---------------------------------------------------------------------------

// numberWord spells a small number the way these documents write one. A
// document says "eighteen of the twenty declared", not "18 of the 20", and a
// check that derives the number from the code has to be able to write it the
// same way.
func numberWord(n int) string {
	words := []string{"zero", "one", "two", "three", "four", "five", "six", "seven",
		"eight", "nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen",
		"sixteen", "seventeen", "eighteen", "nineteen", "twenty"}
	if n >= 0 && n < len(words) {
		return words[n]
	}
	tens := map[int]string{2: "twenty", 3: "thirty", 4: "forty", 5: "fifty",
		6: "sixty", 7: "seventy", 8: "eighty", 9: "ninety"}
	if n < 100 {
		if t, ok := tens[n/10]; ok {
			if n%10 == 0 {
				return t
			}
			return t + "-" + words[n%10]
		}
	}
	return strconv.Itoa(n)
}

// paragraphOf returns the blank-line-delimited paragraph containing needle, so
// a check can say "this sentence names X" rather than "the document does
// somewhere".
func paragraphOf(body, needle string) string {
	idx := strings.Index(body, needle)
	if idx < 0 {
		return ""
	}
	start := 0
	if i := strings.LastIndex(body[:idx], "\n\n"); i >= 0 {
		start = i + 2
	}
	end := len(body)
	if i := strings.Index(body[idx:], "\n\n"); i >= 0 {
		end = idx + i
	}
	return body[start:end]
}
