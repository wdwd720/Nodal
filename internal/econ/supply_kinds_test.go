package econ_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/ledger"
)

// Every advanced journal kind (internal/ledger/advanced.go), against what
// migration 00763 does with it for a supply-controlled asset -- a platform
// stable asset or a bridged twin -- and what stands in front of it in Go.
//
// A kind the database refuses for such an asset must be either guarded in Go
// at every entry point that could lead such an asset into it (the entry
// refuses it through econ.RefuseSupplyControlled before anything is written),
// or unreachable for such an asset, with the reason. A kind the database
// allows may still carry guards: SIM_TRADE is allowed only when the trade
// moves the asset's holdings by nothing (SU002), and an allowed collateral
// move exists only to be sold by a refused liquidation.
//
// TestSupplyKinds_EveryKindHasARow keeps the table complete;
// TestSupplyKinds_EveryGuardRefusesSupplyControlledAssets proves, from the
// source, that every entry point a row names calls the shared check (directly
// or through a helper of its package); the integration test
// TestIntegration_SupplyKindsAgreeWithTheDatabase holds every row's rule
// against cp_supply_controlled_kind_allowed itself; and each guard has an
// integration test in its own package showing the coded refusal.

// supplyRule is what 00763 does with a kind for a supply-controlled asset
// (cp_supply_controlled_kind_allowed).
type supplyRule string

const (
	// allowedForAll: allowed for every supply-controlled asset (SU001), in a
	// transaction that conserves its holdings (SU002).
	allowedForAll supplyRule = "ALLOWED"
	// allowedByRole: allowed only for the asset's own controller's kinds, or
	// where the asset has the role the kind needs -- a stable asset's reserve,
	// a bridge route's source, a prediction market's collateral.
	allowedByRole supplyRule = "BY_ROLE"
	// refusedForAll: refused for every supply-controlled asset (SU001).
	refusedForAll supplyRule = "REFUSED"
)

// guard names an entry point that refuses a supply-controlled asset: a
// package directory under the repository root and a function, written
// "(*Type).Method" or "function".
type guard struct {
	pkg string
	fn  string
}

type supplyKindRow struct {
	kind   ledger.Kind
	rule   supplyRule
	guards []guard
	// reason says why the guards are all the kind needs: why every other
	// path to it is unreachable for a supply-controlled asset, or why it is
	// allowed.
	reason string
}

// supplyKindTable is the table. Its rows follow ledger.AdvancedKinds.
func supplyKindTable() []supplyKindRow {
	const (
		sandboxfund = "internal/sandboxfund"
		margin      = "internal/margin"
		derivatives = "internal/derivatives"
		equities    = "internal/equities"
		treasury    = "internal/treasury"
		orders      = "internal/orders"
		marketplace = "internal/marketplace"
		feeledger   = "internal/feeledger"
		delegation  = "internal/delegation"
	)
	numeraire := "margin posts it only in the margin account's numeraire, which OpenAccount refuses when it is supply-controlled; " +
		"an asset supply-controlled then stays so, and one that was not never becomes so once value moved in it (SU003)"
	derivFlow := "derivatives posts it only in an instrument's settlement asset and only for a position that exists; OpenPosition, " +
		"the one way a position comes into being, refuses a supply-controlled settlement asset, and a settlement asset never changes"
	return []supplyKindRow{
		{ledger.KindSandboxFunding, refusedForAll, []guard{{sandboxfund, "(*Faucet).Fund"}},
			"the faucet is the one poster of SANDBOX_FUNDING"},
		{ledger.KindFacilityFunding, refusedForAll, []guard{{margin, "(*Service).SeedFacilities"}, {derivatives, "(*Service).FundClearingHouse"}},
			"margin's lending pool and insurance fund and the derivatives clearing house are the only facilities ever funded; " +
				"a controller's own facility and a reserve, bridge or prediction facility book the asset only in their own kinds"},
		{ledger.KindHouseFunding, refusedForAll, []guard{{treasury, "(*Service).Fund"}, {delegation, "(*admitter).RequestSwap"}},
			"treasury funds a treasury book; a delegation venue's house book is funded only outside the services, so a swap that " +
				"would need its venue funded in a supply-controlled asset is refused at its admission"},
		{ledger.KindP2PTransfer, allowedForAll, nil,
			"a customer-to-customer transfer: what one customer sends the other receives"},
		{ledger.KindSimTrade, allowedForAll, []guard{
			{orders, "(*PaperVenue).market"}, {margin, "(*Service).OpenLeveragedSpot"}, {treasury, "(*Service).ExecutePaper"},
			{treasury, "(*Service).Rebalance"}, {equities, "(*Service).ListEquity"}, {equities, "(*Service).SubmitOrderIntent"},
			{marketplace, "(*Service).InstallVersion"}, {marketplace, "(*Service).follow"},
		}, "allowed between two booked parties -- the internal book (customers and house books) and a delegated swap with a " +
			"venue's house book; a fill against an external, unbooked venue (the orders paper venue, a margin leveraged buy, the " +
			"treasury paper venue, an equity trade date, a marketplace follow's dispatch) changes the holdings (SU002) and is " +
			"refused at its entry"},
		{ledger.KindEquitySettlement, refusedForAll, []guard{{equities, "(*Service).ListEquity"}, {equities, "(*Service).SubmitOrderIntent"}},
			"a settlement follows the fill of an order; a listing's settlement asset never changes"},
		{ledger.KindCorporateAction, refusedForAll, []guard{{equities, "(*Service).ListEquity"}, {equities, "(*Service).AnnounceCorporateAction"}},
			"an application follows the announcement of its action"},
		{ledger.KindMarginCollateralMove, allowedForAll, []guard{{margin, "(*Service).PostCollateral"}},
			"the move is allowed, the liquidation it exists for is not; ReleaseCollateral stays allowed"},
		{ledger.KindMarginBorrow, refusedForAll, []guard{{margin, "(*Service).OpenAccount"}}, numeraire},
		{ledger.KindMarginRepay, refusedForAll, []guard{{margin, "(*Service).OpenAccount"}}, numeraire},
		{ledger.KindMarginInterest, refusedForAll, []guard{{margin, "(*Service).OpenAccount"}}, numeraire},
		{ledger.KindLiquidation, refusedForAll, []guard{
			{margin, "(*Service).OpenAccount"}, {margin, "(*Service).PostCollateral"}, {margin, "(*Service).OpenLeveragedSpot"},
		}, "a liquidation sells what the account holds as collateral or bought on margin, into its numeraire"},
		{ledger.KindDeficitCoverage, refusedForAll, []guard{{margin, "(*Service).OpenAccount"}}, numeraire},
		{ledger.KindDerivativeMarginMove, allowedForAll, []guard{{derivatives, "(*Service).OpenPosition"}},
			"the move is allowed, the flows it secures are not"},
		{ledger.KindVariationMargin, refusedForAll, []guard{{derivatives, "(*Service).OpenPosition"}}, derivFlow},
		{ledger.KindFundingPayment, refusedForAll, []guard{{derivatives, "(*Service).OpenPosition"}}, derivFlow},
		{ledger.KindOptionPremium, refusedForAll, []guard{{derivatives, "(*Service).OpenPosition"}}, derivFlow},
		{ledger.KindOptionSettlement, refusedForAll, []guard{{derivatives, "(*Service).OpenPosition"}}, derivFlow},
		{ledger.KindFutureSettlement, refusedForAll, []guard{{derivatives, "(*Service).OpenPosition"}}, derivFlow},
		{ledger.KindStableIssuance, allowedByRole, nil,
			"internal/stableasset books only its registered stable asset (the controller's own kind) and that asset's " +
				"registered reserve assets (the reserve role); unreachable for any other supply-controlled asset"},
		{ledger.KindStableRedemption, allowedByRole, nil,
			"the inverse of STABLE_ISSUANCE, booking the same two assets"},
		{ledger.KindBridgeSourceFinalized, allowedByRole, nil,
			"internal/bridge books only its route's source asset (the source role) and destination twin (the controller's " +
				"own kind); unreachable for any other supply-controlled asset"},
		{ledger.KindBridgeDestinationFinalized, allowedByRole, nil,
			"the release that mints the route's twin (the controller's own kind) and consumes the source units in flight " +
				"(the source role)"},
		{ledger.KindBridgeRefund, allowedByRole, nil, "returns the route's source asset (the source role)"},
		{ledger.KindPredictionSetIssuance, allowedByRole, nil,
			"internal/predmarket books only a market's collateral (the collateral role) and the outcome shares it creates " +
				"with the market, which no controller owns; unreachable for any other supply-controlled asset"},
		{ledger.KindPredictionSetRedemption, allowedByRole, nil, "the inverse of PREDICTION_SET_ISSUANCE"},
		{ledger.KindPredictionPayout, allowedByRole, nil, "pays a market's collateral against its shares"},
		{ledger.KindPredictionRefund, allowedByRole, nil, "returns a voided market's collateral against its shares"},
		{ledger.KindPerformanceFeeAccrual, refusedForAll, []guard{{feeledger, "(*Service).OpenPeriod"}},
			"posted only in a period's fee asset, which never changes; opening the period is the one entry"},
		{ledger.KindPerformanceFeeCrystallization, refusedForAll, []guard{{feeledger, "(*Service).OpenPeriod"}},
			"posted only in a period's fee asset, which never changes; opening the period is the one entry"},
		{ledger.KindSandboxWithdrawal, refusedForAll, []guard{{delegation, "(*admitter).ProposeWithdrawal"}},
			"a delegated withdrawal is the one poster of SANDBOX_WITHDRAWAL, and a proposal is its one entry"},
	}
}

// TestSupplyKinds_EveryKindHasARow: one row per advanced kind, in declaration
// order, and a refused kind is never left without a guard.
func TestSupplyKinds_EveryKindHasARow(t *testing.T) {
	table := supplyKindTable()
	kinds := ledger.AdvancedKinds()
	require.Len(t, table, len(kinds), "one row per kind of internal/ledger/advanced.go")
	for i, k := range kinds {
		row := table[i]
		assert.Equal(t, k, row.kind, "row %d", i)
		assert.NotEmpty(t, row.reason, "%s: every row says why", row.kind)
		switch row.rule {
		case refusedForAll:
			assert.NotEmpty(t, row.guards, "%s: the database refuses it, so an entry point must refuse first", row.kind)
		case allowedForAll, allowedByRole:
		default:
			t.Errorf("%s: unknown rule %q", row.kind, row.rule)
		}
	}
}

// TestSupplyKinds_EveryGuardRefusesSupplyControlledAssets reads each guarded
// package's source: every entry point a row names must exist and must call
// the shared check -- econ.RefuseSupplyControlled, econ.RefuseFacilityFunding,
// econ.SupplyControlledRefusal -- itself or through a function of its own
// package that does.
func TestSupplyKinds_EveryGuardRefusesSupplyControlledAssets(t *testing.T) {
	root := repoRoot(t)
	refusing := map[string]map[string]bool{}
	for _, row := range supplyKindTable() {
		for _, g := range row.guards {
			fns, ok := refusing[g.pkg]
			if !ok {
				fns = refusingFunctions(t, filepath.Join(root, filepath.FromSlash(g.pkg)))
				refusing[g.pkg] = fns
			}
			refuses, exists := fns[g.fn]
			assert.True(t, exists, "%s: %s.%s does not exist", row.kind, g.pkg, g.fn)
			assert.True(t, refuses, "%s: %s.%s does not refuse a supply-controlled asset", row.kind, g.pkg, g.fn)
		}
	}
	// The check can fail: entry points that deliberately let such an asset
	// through -- releasing collateral (risk-reducing, an allowed move), a
	// house order on the internal book (a trade between two books), a P2P
	// transfer -- are not counted as refusing it.
	for _, deliberate := range []guard{
		{"internal/margin", "(*Service).ReleaseCollateral"},
		{"internal/orders", "(*Service).SubmitHouseOrder"},
		{"internal/orders", "(*Service).Accept"},
		{"internal/orders", "(*Service).Execute"},
		{"internal/transfers", "(*Service).Send"},
	} {
		fns := refusingFunctions(t, filepath.Join(root, filepath.FromSlash(deliberate.pkg)))
		refuses, exists := fns[deliberate.fn]
		require.True(t, exists, "%s.%s does not exist", deliberate.pkg, deliberate.fn)
		assert.False(t, refuses, "%s.%s lets a supply-controlled asset through on purpose", deliberate.pkg, deliberate.fn)
	}
}

// supplyChecks are the shared check's refusing functions.
var supplyChecks = map[string]bool{"RefuseSupplyControlled": true, "RefuseFacilityFunding": true, "SupplyControlledRefusal": true}

// refusingFunctions returns every function of the package in dir, keyed
// "(*Type).Method" or "function", mapped to whether it calls the shared check,
// directly or through other functions of the package.
func refusingFunctions(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	type fn struct {
		direct bool
		// calls are the keys of the package's functions it calls: a plain
		// call of a function, or a call of a method on its own receiver.
		calls []string
	}
	fns := map[string]*fn{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		require.NoError(t, err)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			recvName, recvPrefix := receiverOf(fd)
			rec := &fn{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch c := call.Fun.(type) {
				case *ast.SelectorExpr:
					x, ok := c.X.(*ast.Ident)
					switch {
					case ok && x.Name == "econ" && supplyChecks[c.Sel.Name]:
						rec.direct = true
					case ok && recvName != "" && x.Name == recvName:
						rec.calls = append(rec.calls, recvPrefix+c.Sel.Name)
					}
				case *ast.Ident:
					rec.calls = append(rec.calls, c.Name)
				}
				return true
			})
			fns[funcKey(fd)] = rec
		}
	}
	out := map[string]bool{}
	for k, f := range fns {
		out[k] = f.direct
	}
	for changed := true; changed; {
		changed = false
		for k, f := range fns {
			if out[k] {
				continue
			}
			for _, callee := range f.calls {
				if out[callee] {
					out[k], changed = true, true
				}
			}
		}
	}
	return out
}

// receiverOf returns a method's receiver variable and the key prefix of its
// type's methods ("(*Type)." or "(Type)."), or "" for a function.
func receiverOf(fd *ast.FuncDecl) (string, string) {
	if fd.Recv == nil || len(fd.Recv.List) == 0 || len(fd.Recv.List[0].Names) == 0 {
		return "", ""
	}
	key := funcKey(fd)
	return fd.Recv.List[0].Names[0].Name, strings.TrimSuffix(key, fd.Name.Name)
}

// funcKey is "(*Type).Method", "(Type).Method" or "function".
func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	switch r := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := r.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fd.Name.Name
		}
	case *ast.Ident:
		return "(" + r.Name + ")." + fd.Name.Name
	}
	return fd.Name.Name
}

// repoRoot is the directory holding go.mod, above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.mod above the test")
		dir = parent
	}
}

// TestSupplyKinds_TheTableCoversEveryGuardedPackage keeps the documentation
// honest in the other direction: every package that calls the shared check
// (outside tests) appears in the table, so a new guard is not a guard nobody
// listed.
func TestSupplyKinds_TheTableCoversEveryGuardedPackage(t *testing.T) {
	root := repoRoot(t)
	listed := map[string]bool{"internal/econ": true}
	for _, row := range supplyKindTable() {
		for _, g := range row.guards {
			listed[g.pkg] = true
		}
	}
	var unlisted []string
	require.NoError(t, filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for name := range supplyChecks {
			if strings.Contains(string(body), "econ."+name+"(") {
				rel, err := filepath.Rel(root, filepath.Dir(path))
				if err != nil {
					return err
				}
				if pkg := filepath.ToSlash(rel); !listed[pkg] {
					unlisted = append(unlisted, pkg)
				}
			}
		}
		return nil
	}))
	sort.Strings(unlisted)
	assert.Empty(t, unlisted, "packages that refuse supply-controlled assets but are named by no row of the table")
}
