package httpapi

// Reproductions for the FOURTH round of the withdrawal-verification audit
// (goal §54). This round is narrow: it audits the THIRD round's fix rather than
// the area. Nothing here changes product code.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// ---------------------------------------------------------------------------
// F-272's fix, exhaustively: `withdrawable_now` must equal `payout_eligible`
// for EVERY provenance an account can hold, and the page must not refuse.
//
// The fix keys buckets on (origin, origin floor, root set, finality) and asserts
// the two figures agree, returning an error rather than rendering a payload that
// contradicts itself. That turns a wrong number into a 500, so the fix is only
// sound if the key really is every input `Permits` reads about the value. The
// fixer named ONE that it is not: `HeldDays`, which the fold takes from the
// YOUNGEST lot in a bucket, and recorded that neither shipped policy has a
// positive `MinHoldDays`.
//
// This sweeps every single holding and every ordered PAIR of holdings drawn
// from a universe of 4 origins x 6 root sets x 5 finalities x 3 ages -- 129,960
// cases per policy -- under both policies this build can load, and under a third
// policy with a 30-day hold to show that the recorded residual is real and that
// nothing else is.
// ---------------------------------------------------------------------------

type wv4Holding struct {
	origin valuedomain.CreditOrigin
	roots  []valuedomain.CreditOrigin
	fin    valuedomain.FundingFinality
	ageH   int
}

// lot builds the lot the database would return for this provenance: the floor
// is the most restricted root, which is the CHECK 00819 adds
// (credit_lot_state_floor_is_a_root) and the rule its trigger computes.
func (w wv4Holding) lot(now time.Time, qty int64) credit.Lot {
	return credit.Lot{
		ID: credit.NewLotID(), Origin: w.origin,
		OriginFloor: valuedomain.MostRestrictedOrigin(w.roots...),
		RootOrigins: append([]valuedomain.CreditOrigin(nil), w.roots...),
		Finality:    w.fin,
		Quantity:    money.QuantityFromInt64(qty), Remaining: money.QuantityFromInt64(qty),
		CreatedAt: now.Add(-time.Duration(w.ageH) * time.Hour),
	}
}

func wv4Universe() []wv4Holding {
	origins := []valuedomain.CreditOrigin{
		valuedomain.OriginPromotional,
		valuedomain.OriginPurchased,
		valuedomain.OriginMarketTradingProceeds,
		valuedomain.OriginCreatorEarning,
	}
	rootSets := [][]valuedomain.CreditOrigin{
		{valuedomain.OriginPromotional},
		{valuedomain.OriginPurchased},
		{valuedomain.OriginMarketTradingProceeds},
		{valuedomain.OriginPurchased, valuedomain.OriginMarketTradingProceeds},
		{valuedomain.OriginPromotional, valuedomain.OriginPurchased},
		{valuedomain.OriginCreatorEarning, valuedomain.OriginPurchased},
	}
	ages := []int{1, 25, 24 * 40}

	var out []wv4Holding
	for _, o := range origins {
		for _, r := range rootSets {
			for _, f := range valuedomain.AllFinalities() {
				for _, a := range ages {
					out = append(out, wv4Holding{origin: o, roots: r, fin: f, ageH: a})
				}
			}
		}
	}
	return out
}

// wv4Sweep reports how many combinations made the page refuse and how many made
// its two figures disagree.
func wv4Sweep(t *testing.T, policy valuedomain.Policy) (cases, refusals, disagreements int, firstRefusal, firstDisagreement string) {
	t.Helper()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	caps := map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true}
	universe := wv4Universe()

	run := func(lots []credit.Lot) {
		cases++
		// What credit.Balances computes, per lot: exactly what POST /v1/payouts
		// will reserve.
		gross, eligible := money.Quantity{}, money.Quantity{}
		for _, l := range lots {
			gross = gross.Add(l.Remaining)
			ok, _ := policy.Permits(valuedomain.PermitInput{
				Origin: l.Origin, OriginFloor: l.OriginFloor, RootOrigins: l.RootOrigins,
				Finality: l.Finality, Domain: valuedomain.InternalCredit,
				Verified: valuedomain.VerificationPayoutKYC, HeldDays: l.AgeDays(now),
				ActiveCaps: caps, PolicyValid: true,
			})
			if ok {
				eligible = eligible.Add(l.Remaining)
			}
		}
		out, err := eligibility.ExplainWithdrawal(eligibility.WithdrawalInput{
			Policy: policy, Verified: valuedomain.VerificationPayoutKYC, ActiveCaps: caps,
			Holdings: foldHoldings(lots, now), PolicyValid: true,
			Gross: gross, Spendable: gross, PayoutEligible: eligible,
			JurisdictionSupported: true, ProviderAvailable: true,
			DestinationConfigured: true, DisclosureAccepted: true,
		})
		if err != nil {
			refusals++
			if firstRefusal == "" {
				firstRefusal = wv4Describe(now, lots) + " -> " + err.Error()
			}
			return
		}
		if out.WithdrawableNow.Cmp(eligible) != 0 {
			disagreements++
			if firstDisagreement == "" {
				firstDisagreement = fmt.Sprintf("%s -> withdrawable_now=%s payout_eligible=%s",
					wv4Describe(now, lots), out.WithdrawableNow, eligible)
			}
		}
	}

	for i := range universe {
		run([]credit.Lot{universe[i].lot(now, 100_000_000)})
	}
	for i := range universe {
		for j := range universe {
			run([]credit.Lot{universe[i].lot(now, 100_000_000), universe[j].lot(now, 200_000_000)})
		}
	}
	return cases, refusals, disagreements, firstRefusal, firstDisagreement
}

func wv4Describe(now time.Time, lots []credit.Lot) string {
	s := ""
	for _, l := range lots {
		s += fmt.Sprintf("{%s floor=%s roots=%v fin=%s age=%dd qty=%s}",
			l.Origin, l.OriginFloor, l.RootOrigins, l.Finality, l.AgeDays(now), l.Remaining)
	}
	return s
}

func TestAuditWV4_TheEligibilityInvariantHoldsForEveryProvenanceAndEveryPair(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy valuedomain.Policy
	}{
		{"the sandbox tier's policy", valuedomain.SandboxPolicy()},
		{"the fail-closed default every other deployment runs", valuedomain.DefaultPolicy()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cases, refusals, disagreements, firstRefusal, firstDis := wv4Sweep(t, tc.policy)
			require.Greater(t, cases, 100_000, "the sweep must actually be a sweep")
			assert.Zero(t, refusals,
				"F-272's fix turns a contradictory payload into an error, so GET /v1/me/eligibility "+
					"now 500s wherever a bucket is not homogeneous in every input Permits reads. "+
					"%d of %d combinations refused; first: %s", refusals, cases, firstRefusal)
			assert.Zero(t, disagreements,
				"and withdrawable_now must equal payout_eligible by construction under no "+
					"account-level block. %d of %d disagreed; first: %s", disagreements, cases, firstDis)
			t.Logf("%d combinations, %d refusals, %d disagreements", cases, refusals, disagreements)
		})
	}
}

// The residual the fixer recorded, measured rather than trusted: a policy with a
// positive MinHoldDays DOES make a bucket heterogeneous, and the page refuses
// rather than rendering. It is recorded here as unreachable in this build —
// `config.NormalizePayoutPolicy` accepts only CLOSED and SANDBOX, and both set
// MinHoldDays to zero for every rule — so the failure is latent in the fix and
// not a defect of it, and this test states exactly how much of the space it
// would cost the day a policy with a hold period is persisted.
func TestAuditWV4_AHoldPeriodIsTheOneThingThatStillBreaksTheBucketKey(t *testing.T) {
	base := valuedomain.SandboxPolicy()
	rules := make(map[valuedomain.CreditOrigin]valuedomain.OriginRule, len(base.Rules))
	for o, r := range base.Rules {
		if r.PayoutAllowed {
			r.MinHoldDays = 30
		}
		rules[o] = r
	}
	held := valuedomain.Policy{Version: base.Version + "-held30", Rules: rules}
	require.NoError(t, held.Validate(), "the hold-period policy must be a policy this type accepts")

	cases, refusals, disagreements, firstRefusal, _ := wv4Sweep(t, held)
	assert.Positive(t, refusals,
		"the fixer's own residual says a positive MinHoldDays can make a bucket heterogeneous, "+
			"because the bucket's age is that of its youngest lot; if nothing refuses, the "+
			"residual is not the one that was recorded")
	assert.Zero(t, disagreements,
		"and when it does, it refuses rather than rendering a smaller number: a page that says "+
			"'you may withdraw nothing' over a request the engine approves is worse than an error")
	t.Logf("with a 30-day hold: %d combinations, %d refusals (%.3f%%), %d disagreements. First: %s",
		cases, refusals, 100*float64(refusals)/float64(cases), disagreements, firstRefusal)
}

// Two lots of one origin and one floor whose ROOT SETS differ get two verdicts
// under a policy this build does not ship, and the page keys its buckets on the
// set, so it renders two buckets and two answers. This is D-138's fix holding at
// the read surface, and it is the counterpart of the allocation record's gap
// that internal/payout's fourth-round file reproduces.
func TestAuditWV4_TwoRootSetsBehindOneFloorAreTwoBucketsAndTwoAnswers(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	// The policy B-02 can come back with: the closed-loop float is stored value
	// and may not leave; the gains earned inside the economy may.
	base := valuedomain.SandboxPolicy()
	rules := make(map[valuedomain.CreditOrigin]valuedomain.OriginRule, len(base.Rules))
	for o, r := range base.Rules {
		rules[o] = r
	}
	closed := valuedomain.OriginRule{PayoutAllowed: false, RequiredVerification: valuedomain.VerificationNone}
	rules[valuedomain.OriginPurchased] = closed
	policy := valuedomain.Policy{Version: "audit-wv4-gains-not-float", Rules: rules}
	require.NoError(t, policy.Validate())

	// Same origin, same floor (CREATOR_EARNING sorts before PURCHASED at the
	// same restriction rank, so it is the floor of both), different root sets.
	onlyEarned := credit.Lot{
		ID: credit.NewLotID(), Origin: valuedomain.OriginMarketTradingProceeds,
		OriginFloor: valuedomain.OriginCreatorEarning,
		RootOrigins: []valuedomain.CreditOrigin{valuedomain.OriginCreatorEarning},
		Finality:    valuedomain.FinalitySettled,
		Quantity:    money.QuantityFromInt64(100_000_000), Remaining: money.QuantityFromInt64(100_000_000),
		CreatedAt: now.Add(-24 * time.Hour),
	}
	halfPurchased := credit.Lot{
		ID: credit.NewLotID(), Origin: valuedomain.OriginMarketTradingProceeds,
		OriginFloor: valuedomain.OriginCreatorEarning,
		RootOrigins: []valuedomain.CreditOrigin{
			valuedomain.OriginCreatorEarning, valuedomain.OriginPurchased},
		Finality: valuedomain.FinalitySettled,
		Quantity: money.QuantityFromInt64(100_000_000), Remaining: money.QuantityFromInt64(100_000_000),
		CreatedAt: now.Add(-24 * time.Hour),
	}
	require.Equal(t, onlyEarned.OriginFloor, halfPurchased.OriginFloor,
		"fixture check: the two provenances must be indistinguishable by (origin, floor), "+
			"which is what payout_allocations records")

	holdings := foldHoldings([]credit.Lot{onlyEarned, halfPurchased}, now)
	require.Len(t, holdings, 2,
		"D-138: the fold keys on the root set, so one floor over two provenances is two buckets")

	out, err := eligibility.ExplainWithdrawal(eligibility.WithdrawalInput{
		Policy: policy, Verified: valuedomain.VerificationPayoutKYC,
		ActiveCaps: map[valuedomain.CapabilityKey]bool{valuedomain.CapPayoutReserve: true},
		Holdings:   holdings, PolicyValid: true,
		Gross:                 money.QuantityFromInt64(200_000_000),
		Spendable:             money.QuantityFromInt64(200_000_000),
		PayoutEligible:        money.QuantityFromInt64(100_000_000),
		JurisdictionSupported: true, ProviderAvailable: true,
		DestinationConfigured: true, DisclosureAccepted: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "100000000", out.WithdrawableNow.String(),
		"only the half that no purchase funded may leave")

	var refused *eligibility.OriginBucket
	for i := range out.Buckets {
		if len(out.Buckets[i].RootOrigins) == 2 && out.Buckets[i].Quantity.IsPositive() {
			refused = &out.Buckets[i]
		}
	}
	require.NotNil(t, refused, "the refused provenance must be a bucket of its own")
	assert.Equal(t, valuedomain.OriginPurchased, refused.RefusedRoot,
		"D-138: the reason a person can act on is the root THIS policy refuses, which is not "+
			"the one this build's rank calls the most restricted; the floor of this bucket is "+
			"CREATOR_EARNING and the policy releases it")
	assert.Contains(t, refused.Reasons, eligibility.WithdrawalOriginNotWithdrawable)
	assert.False(t, refused.VerificationWouldSuffice,
		"and verifying does not fix a provenance the policy refuses")
}

// ---------------------------------------------------------------------------
// F-wv4-3 — the Withdraw page renders the finer buckets F-272 produced with
// nothing on them that says which provenance each one is, and keys the React
// list on the one field that is no longer unique.
//
// F-272's record says "The page renders the finer buckets; a floor differing
// from the origin is still rendered, as F-261 requires." The first half is true:
// the API returns one bucket per (origin, floor, root set, finality) and the
// table has a row for each. The second half is not. `apps/web/src` contains no
// reference to `origin_floor`, `root_origins`, `refused_root` or `finality`, and
// the bucket table's columns are rank, origin, held, may-leave, permitted and
// why-not. So an account holding trading proceeds out of a purchase and trading
// proceeds out of a grant sees two rows both labelled MARKET_TRADING_PROCEEDS,
// one saying 200.00 may leave and one saying 0.00, and nothing anywhere on the
// page saying why they differ.
//
// That is the sentence D-131 wrote the floor for: "what the person needs to read
// is 'this came from a promotional grant', not a word about the trade". D-138
// added `refused_root` for the same reason and the page does not read it either.
//
// The second half is mechanical. `rowKey={(bucket) => bucket.origin}` is a React
// key, and the generated schema's own description says in as many words: "More
// than one bucket may therefore carry the same `origin` … A client that keys on
// `origin` alone must key on (origin, origin_floor, finality, root_origins)
// instead." The page polls, so the list is re-rendered from new data against
// stale keys, and duplicate keys are how a row's money figure ends up on a
// different row's provenance.
//
// The client's own contract spec is the third piece: `withdrawalOriginBucketSpec`
// does not name the new fields at all, in either its required or its optional
// set, so nothing would notice if the API stopped sending them.
// ---------------------------------------------------------------------------

func TestAuditWV4_TheWithdrawPageCannotTellTwoBucketsOfOneOriginApart(t *testing.T) {
	root := wv4RepoRoot(t)
	page := wv4Read(t, filepath.Join(root, "apps", "web", "src", "pages", "withdraw", "Withdraw.tsx"))
	contract := wv4Read(t, filepath.Join(root, "apps", "web", "src", "api", "contract.ts"))

	require.True(t, strings.Contains(page, "Where this value came from"),
		"fixture check: this is the bucket table F-272 made finer")
	require.True(t, strings.Contains(page, "rowKey={(bucket: WithdrawalOriginBucket) => bucket.origin}"),
		"fixture check: the table's React key")

	// The generated schema says the origin is no longer unique.
	api := wv4Read(t, filepath.Join(root, "internal", "gen", "api", "api.gen.go"))
	require.True(t, strings.Contains(api, "More than one bucket may therefore carry the same `origin`"),
		"fixture check: the contract states it")
	require.True(t, strings.Contains(api, "RefusedRoot        *CreditOrigin `json:\"refused_root,omitempty\"`"),
		"fixture check: the API sends the field D-138 added for the person to read")

	for _, field := range []string{"origin_floor", "root_origins", "refused_root"} {
		assert.True(t, strings.Contains(page, field),
			"F-wv4-3: the Withdraw page's bucket table renders %q for no bucket. F-272 split one "+
				"bucket per origin into one per (origin, floor, root set, finality) and its record "+
				"says 'a floor differing from the origin is still rendered, as F-261 requires'. "+
				"Two rows now read MARKET_TRADING_PROCEEDS, one with a positive 'May leave' and "+
				"one with zero, and the page carries nothing that says which is which", field)
	}

	assert.False(t, strings.Contains(page, "rowKey={(bucket: WithdrawalOriginBucket) => bucket.origin}"),
		"F-wv4-3: and the list is keyed on `bucket.origin`, which the generated schema's own "+
			"description says is no longer unique — 'A client that keys on `origin` alone must key "+
			"on (origin, origin_floor, finality, root_origins) instead'. `useEligibility` sets "+
			"staleTime 0 and refetchOnWindowFocus, and four mutations invalidate it, so the list "+
			"is re-rendered from new data against duplicate keys")

	assert.True(t, strings.Contains(contract, "origin_floor"),
		"F-wv4-3: `withdrawalOriginBucketSpec` names neither the floor nor the root set nor the "+
			"refused root, in required or optional, so the browser contract check cannot notice "+
			"the day the API stops sending them")
}

func wv4Read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- a path built from the repository root
	require.NoError(t, err, "reading %s", path)
	return string(b)
}

func wv4RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("no module root above the working directory")
	return ""
}
