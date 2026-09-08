package commerce

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuedomain"
	"github.com/nodal/controlplane/migrations"
)

func q(n int64) money.Quantity { return money.QuantityFromInt64(n) }

// ---------------------------------------------------------------------------
// The provenance table
// ---------------------------------------------------------------------------

// TestEarningOrigin_EveryKindHasOne. A kind with no declared provenance would
// reach Purchase and fail there, which is the right failure but far too late:
// the mapping is the package's whole reason to exist and it must be total.
func TestEarningOrigin_EveryKindHasOne(t *testing.T) {
	for _, k := range AllKinds() {
		origin, ok := EarningOrigin(k)
		require.True(t, ok, "kind %s has no declared earning provenance", k)
		require.True(t, origin.Valid(), "kind %s maps to unknown origin %q", k, origin)
	}
	require.Len(t, earningOrigin, len(AllKinds()),
		"the map and the kind list must not drift apart")
}

// TestEarningOrigin_AnUnknownKindGetsNoDefault. A default would mean a new
// product kind silently inherited a payout treatment nobody chose for it, which
// is exactly how an origin ends up withdrawable by accident.
func TestEarningOrigin_AnUnknownKindGetsNoDefault(t *testing.T) {
	origin, ok := EarningOrigin(Kind("NFT_MYSTERY_BOX"))
	require.False(t, ok)
	require.Empty(t, string(origin))
	require.False(t, Kind("NFT_MYSTERY_BOX").Valid())
}

// TestEarningOrigin_ProducesOnlyCommerceProvenances is the guard that keeps a
// future edit from routing a sale into an origin a payout policy already
// permits for a different reason. Commerce may only mint these three.
func TestEarningOrigin_ProducesOnlyCommerceProvenances(t *testing.T) {
	allowed := map[valuedomain.CreditOrigin]bool{
		valuedomain.OriginCreatorEarning:      true,
		valuedomain.OriginDataSaleEarning:     true,
		valuedomain.OriginAgentServiceEarning: true,
	}
	for _, k := range AllKinds() {
		origin, _ := EarningOrigin(k)
		require.True(t, allowed[origin],
			"kind %s produces %s, which is not a commerce provenance", k, origin)
	}

	// And specifically never the speculative one, which is the distinction the
	// whole payout architecture rests on.
	for _, k := range AllKinds() {
		origin, _ := EarningOrigin(k)
		require.NotEqual(t, valuedomain.OriginMarketTradingProceeds, origin)
		require.NotEqual(t, valuedomain.OriginPromotional, origin)
		require.NotEqual(t, valuedomain.OriginPurchased, origin)
	}
}

// TestEarningOrigin_DataAndAgentServiceAreNotCollapsed. Selling a dataset and
// running an agent for somebody are different activities with different
// regulatory shapes. Collapsing them into one origin would force any future
// determination to cover both or neither.
func TestEarningOrigin_DataAndAgentServiceAreNotCollapsed(t *testing.T) {
	data, _ := EarningOrigin(KindData)
	agent, _ := EarningOrigin(KindAgentService)
	other, _ := EarningOrigin(KindResearch)

	require.Equal(t, valuedomain.OriginDataSaleEarning, data)
	require.Equal(t, valuedomain.OriginAgentServiceEarning, agent)
	require.Equal(t, valuedomain.OriginCreatorEarning, other)
	require.NotEqual(t, data, agent)
	require.NotEqual(t, data, other)
	require.NotEqual(t, agent, other)
}

// ---------------------------------------------------------------------------
// Go and SQL must declare the same vocabulary
// ---------------------------------------------------------------------------

// tableBlock returns one CREATE TABLE body. The lists below are looked up per
// table because `status` exists on two of them, and an earlier version of this
// helper happily compared the product statuses against the seller ones.
func tableBlock(t *testing.T, sql, table string) string {
	t.Helper()
	start := strings.Index(sql, "CREATE TABLE "+table+" (")
	require.GreaterOrEqual(t, start, 0, "no CREATE TABLE %s", table)
	end := strings.Index(sql[start:], "\n);")
	require.Greater(t, end, 0, "unterminated CREATE TABLE %s", table)
	return sql[start : start+end]
}

// checkList pulls the members of a `CHECK (col IN ('A','B',...))` out of one
// table definition so the SQL side of an enum can be compared with the Go side.
func checkList(t *testing.T, sql, table, column string) []string {
	t.Helper()
	block := tableBlock(t, sql, table)
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(column) + `\s+text NOT NULL CHECK \(` +
		regexp.QuoteMeta(column) + ` IN\s*\((.*?)\)\)`)
	m := re.FindStringSubmatch(block)
	require.Len(t, m, 2, "could not find a CHECK list for %s.%s", table, column)
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		part = strings.Trim(strings.TrimSpace(part), "'")
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func commerceMigration(t *testing.T) string {
	t.Helper()
	b, err := migrations.FS.ReadFile("00715_internal_commerce.sql")
	require.NoError(t, err)
	return string(b)
}

// TestMigration_DeclaresTheSameKindsAsGo. A kind added in Go without the
// migration is a runtime INSERT failure on the first sale of that kind, which
// is the worst possible place to discover it.
func TestMigration_DeclaresTheSameKindsAsGo(t *testing.T) {
	sql := commerceMigration(t)
	got := checkList(t, sql, "internal_products", "kind")
	var want []string
	for _, k := range AllKinds() {
		want = append(want, string(k))
	}
	require.ElementsMatch(t, want, got)
}

// TestMigration_DeclaresTheSameStatusesAsGo.
func TestMigration_DeclaresTheSameStatusesAsGo(t *testing.T) {
	sql := commerceMigration(t)
	got := checkList(t, sql, "internal_products", "status")
	var want []string
	for _, s := range AllStatuses() {
		want = append(want, string(s))
	}
	require.ElementsMatch(t, want, got)
}

// TestMigration_DeclaresTheSameSellerStatusesAsGo.
func TestMigration_DeclaresTheSameSellerStatusesAsGo(t *testing.T) {
	got := checkList(t, commerceMigration(t), "internal_sellers", "status")
	require.ElementsMatch(t, []string{
		string(SellerActive), string(SellerSuspended), string(SellerClosed),
	}, got)
}

// TestMigration_AcceptsExactlyTheOriginsCommerceCanProduce. Too narrow and a
// legitimate sale is refused at COMMIT; too wide and the column stops being a
// statement about what commerce is allowed to mint.
func TestMigration_AcceptsExactlyTheOriginsCommerceCanProduce(t *testing.T) {
	sql := commerceMigration(t)
	got := checkList(t, sql, "internal_commerce_orders", "earning_origin")

	produced := map[string]bool{}
	for _, k := range AllKinds() {
		o, _ := EarningOrigin(k)
		produced[string(o)] = true
	}
	var want []string
	for o := range produced {
		want = append(want, o)
	}
	require.ElementsMatch(t, want, got,
		"the earning_origin CHECK must list exactly the origins the kind map can produce")
}

// TestMigration_NamesTheConstraintsAnOperatorWillSee. An unnamed CHECK reports
// as "internal_commerce_orders_check", and the two rules that matter here are
// precisely the ones somebody reading an incident needs to recognise
// immediately.
func TestMigration_NamesTheConstraintsAnOperatorWillSee(t *testing.T) {
	sql := commerceMigration(t)
	for _, name := range []string{
		"internal_commerce_orders_no_self_dealing",
		"internal_commerce_orders_no_self_earning",
		"internal_commerce_orders_price_is_split",
	} {
		require.Contains(t, sql, name)
	}
}

// ---------------------------------------------------------------------------
// The split
// ---------------------------------------------------------------------------

func TestSplit_RoundsTowardTheCreator(t *testing.T) {
	for _, tc := range []struct {
		price         int64
		bps           money.BPS
		fee, proceeds string
		what          string
	}{
		{1_000, 0, "0", "1000", "no fee at all"},
		{1_000, 1_000, "100", "900", "a clean 10%"},
		{999, 333, "33", "966", "33.2667 rounds down to 33"},
		{1, 3_000, "0", "1", "a fee below one unit is no fee"},
		{3, 3_000, "0", "3", "0.9 of a unit is still no fee"},
		{4, 3_000, "1", "3", "1.2 of a unit is one unit"},
		{1_000_000, 3_000, "300000", "700000", "the cap"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := Product{Price: q(tc.price), PlatformFeeBPS: tc.bps}
			fee, proceeds, err := p.Split()
			require.NoError(t, err)
			require.Equal(t, tc.fee, fee.String())
			require.Equal(t, tc.proceeds, proceeds.String())
		})
	}
}

// TestSplit_AlwaysAddsBackToThePrice over the whole plausible range. A split
// that loses or invents a unit would show up as an IC001 refusal at COMMIT,
// which is the right refusal and a terrible way to find out.
func TestSplit_AlwaysAddsBackToThePrice(t *testing.T) {
	for price := int64(1); price <= 400; price++ {
		for bps := money.BPS(0); bps <= MaxPlatformFeeBPS; bps += 137 {
			p := Product{Price: q(price), PlatformFeeBPS: bps}
			fee, proceeds, err := p.Split()
			require.NoError(t, err)
			require.False(t, fee.IsNegative())
			require.False(t, proceeds.IsNegative())
			require.Equal(t, q(price).String(), fee.Add(proceeds).String(),
				"price %d at %d bps: %s + %s", price, bps, fee, proceeds)
			require.LessOrEqual(t, fee.Cmp(proceeds.Add(fee)), 0)
		}
	}
}

func TestSplit_RefusesANonPositivePrice(t *testing.T) {
	_, _, err := Product{Price: q(0)}.Split()
	require.Error(t, err)
	_, _, err = Product{}.Split()
	require.Error(t, err)
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestValidate_RejectsWhatItShould(t *testing.T) {
	base := func() Product {
		return Product{
			SellerAccountID: someAccount(), Kind: KindData, Title: "Dataset",
			Price: q(100), Version: 1, Status: StatusDraft,
		}
	}
	require.NoError(t, base().Validate())

	for _, tc := range []struct {
		what   string
		mutate func(*Product)
	}{
		{"no seller", func(p *Product) { p.SellerAccountID = zeroAccount() }},
		{"unknown kind", func(p *Product) { p.Kind = "MYSTERY" }},
		{"no title", func(p *Product) { p.Title = "   " }},
		{"an over-long title", func(p *Product) { p.Title = strings.Repeat("x", 201) }},
		{"an over-long description", func(p *Product) { p.Description = strings.Repeat("x", 5_001) }},
		{"a zero price", func(p *Product) { p.Price = q(0) }},
		{"a fee above the cap", func(p *Product) { p.PlatformFeeBPS = MaxPlatformFeeBPS + 1 }},
		{"a negative fee", func(p *Product) { p.PlatformFeeBPS = -1 }},
		{"version zero", func(p *Product) { p.Version = 0 }},
		{"an unknown status", func(p *Product) { p.Status = "LIVE" }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := base()
			tc.mutate(&p)
			require.Error(t, p.Validate(), "a product with %s must not validate", tc.what)
		})
	}
}

func TestPurchaseRequest_Validate(t *testing.T) {
	ok := PurchaseRequest{
		ProductID: NewProductID(), BuyerAccountID: someAccount(),
		ExpectedPrice: q(10), IdempotencyKey: "k", EffectiveAt: someTime(),
	}
	require.NoError(t, ok.Validate())

	for _, tc := range []struct {
		what   string
		mutate func(*PurchaseRequest)
	}{
		{"no product", func(r *PurchaseRequest) { r.ProductID = ProductID{} }},
		{"no buyer", func(r *PurchaseRequest) { r.BuyerAccountID = zeroAccount() }},
		{"no agreed price", func(r *PurchaseRequest) { r.ExpectedPrice = q(0) }},
		{"no idempotency key", func(r *PurchaseRequest) { r.IdempotencyKey = "  " }},
		{"no effective_at", func(r *PurchaseRequest) { r.EffectiveAt = zeroTime() }},
	} {
		t.Run(tc.what, func(t *testing.T) {
			r := ok
			tc.mutate(&r)
			require.Error(t, r.Validate())
		})
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// TestCanTransition_WithdrawnIsTerminal. A product that has been taken down and
// can come back is PAUSED; the two mean different things to a buyer looking at
// their purchase history, and letting WITHDRAWN return would erase that.
func TestCanTransition_WithdrawnIsTerminal(t *testing.T) {
	for _, to := range AllStatuses() {
		require.False(t, CanTransition(StatusWithdrawn, to),
			"WITHDRAWN -> %s must be refused", to)
	}
}

func TestCanTransition_TheDeclaredGraph(t *testing.T) {
	legal := map[Status]map[Status]bool{
		StatusDraft:  {StatusActive: true, StatusWithdrawn: true},
		StatusActive: {StatusPaused: true, StatusWithdrawn: true},
		StatusPaused: {StatusActive: true, StatusWithdrawn: true},
	}
	for _, from := range AllStatuses() {
		for _, to := range AllStatuses() {
			require.Equal(t, legal[from][to], CanTransition(from, to), "%s -> %s", from, to)
		}
	}
	// No status may transition to itself: a no-op is handled before the check,
	// and declaring it legal would hide a genuine repeat.
	for _, s := range AllStatuses() {
		require.False(t, CanTransition(s, s))
	}
}

// TestStatus_OnlyActiveSells.
func TestStatus_OnlyActiveSells(t *testing.T) {
	for _, s := range AllStatuses() {
		require.Equal(t, s == StatusActive, s.Sellable(), "status %s", s)
	}
	require.False(t, Status("").Sellable())
	require.False(t, Status("ACTIVE ").Sellable())
}

func TestSellerStatus_OnlyActiveTakesOrders(t *testing.T) {
	require.True(t, SellerActive.CanSell())
	require.False(t, SellerSuspended.CanSell())
	require.False(t, SellerClosed.CanSell())
	require.False(t, SellerStatus("").CanSell())
	require.True(t, SellerActive.Valid())
	require.False(t, SellerStatus("BANNED").Valid())
}

// TestSeller_EarningAccountDefaultsToTheSeller. Attribution is recorded, never
// inferred, and the default must be the obvious one.
func TestSeller_EarningAccountDefaultsToTheSeller(t *testing.T) {
	a, b := someAccount(), someAccount()
	require.NotEqual(t, a, b)

	require.Equal(t, a, Seller{AccountID: a}.EarningAccount())
	require.Equal(t, b, Seller{AccountID: a, PayoutAccountID: &b}.EarningAccount())
}

// TestProduct_TermsFrozenTracksPublication.
func TestProduct_TermsFrozenTracksPublication(t *testing.T) {
	require.False(t, Product{}.TermsFrozen())
	now := someTime()
	require.True(t, Product{PublishedAt: &now}.TermsFrozen())
}

// TestMaxPlatformFeeBPS_MatchesTheMigration. Two places state the cap; a
// divergence means either Go refuses what the database would accept, or the
// database refuses what Go promised a seller.
func TestMaxPlatformFeeBPS_MatchesTheMigration(t *testing.T) {
	sql := commerceMigration(t)
	require.Contains(t, sql, "platform_fee_bps >= 0 AND platform_fee_bps <= 3000")
	require.Equal(t, money.BPS(3_000), MaxPlatformFeeBPS)
}

// ---------------------------------------------------------------------------

func someAccount() accounts.AccountID { return accounts.NewAccountID() }
func zeroAccount() accounts.AccountID { return accounts.AccountID{} }
func someTime() time.Time             { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) }
func zeroTime() time.Time             { return time.Time{} }
