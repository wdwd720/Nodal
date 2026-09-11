package activity

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

func testAccount(t *testing.T) accounts.AccountID {
	t.Helper()
	a, err := accounts.ParseAccountID(id.New[id.Any]().String())
	require.NoError(t, err)
	return a
}

// TestEveryKindHasASource is half of the extension point's guarantee: a Kind
// declared without a Source would be a filter a client can select and a feed
// that then returns nothing, with no error to say why.
func TestEveryKindHasASource(t *testing.T) {
	t.Parallel()
	byKind := map[Kind]int{}
	for _, s := range Sources() {
		byKind[s.Kind]++
	}
	for _, k := range AllKinds() {
		assert.Equal(t, 1, byKind[k], "kind %s must have exactly one source", k)
	}
	assert.Len(t, byKind, len(AllKinds()), "a source names a kind that is not declared")
}

// TestEveryKindHasASummaryTemplate is the other half: a Kind with no template
// would render as its own enum name on somebody's activity page.
func TestEveryKindHasASummaryTemplate(t *testing.T) {
	t.Parallel()
	for _, k := range AllKinds() {
		got := summaryFor(k, "SETTLED", "BUY", "DEMOORB")
		require.NotEmpty(t, got)
		assert.NotEqual(t, string(k), got, "kind %s falls through to the default branch", k)
	}
	// The default branch exists and is legible rather than empty.
	assert.Equal(t, "SOMETHING_NEW", summaryFor(Kind("SOMETHING_NEW"), "", "", ""))
}

// TestEverySourceIsInTheCompiledQuery: the union is a constant, so a Source
// added to the registry and forgotten in the concatenation would be a source
// that exists and never runs.
func TestEverySourceIsInTheCompiledQuery(t *testing.T) {
	t.Parallel()
	q := Query()
	for _, s := range Sources() {
		assert.Contains(t, q, strings.TrimSpace(s.SQL),
			"source %s is registered but is not part of feedQuery", s.Kind)
		// And it carries the predicate that lets PostgreSQL skip it when the
		// caller filtered it out.
		assert.Contains(t, s.SQL, "'"+string(s.Kind)+"' = ANY($2)",
			"source %s cannot be excluded by the kind filter", s.Kind)
	}
	assert.Equal(t, len(Sources())-1, strings.Count(q, unionAll),
		"the union must join every source and no more")
}

// TestEverySourceSatisfiesTheColumnContract: a UNION ALL takes the type of its
// first branch, so a branch that is missing a column or names it differently is
// a runtime scan error rather than a compile one.
func TestEverySourceSatisfiesTheColumnContract(t *testing.T) {
	t.Parallel()
	names := columnNames()
	require.Len(t, names, 14)
	for _, s := range Sources() {
		for _, col := range names {
			// The kind column is produced as a literal; the rest are aliased
			// or selected under their own name.
			assert.True(t,
				strings.Contains(s.SQL, "AS "+col) || strings.Contains(s.SQL, "."+col+",") ||
					strings.Contains(s.SQL, "."+col+"\n") || strings.Contains(s.SQL, "."+col+" "),
				"source %s does not produce %s", s.Kind, col)
		}
	}
	// Every branch binds the account parameter and nothing else beyond the
	// kind filter: the cursor and the limit belong to the statement around it.
	for _, s := range Sources() {
		assert.Contains(t, s.SQL, "$1", "source %s does not scope to an account", s.Kind)
		assert.NotContains(t, s.SQL, "$3", "source %s reaches for a parameter that is not its own", s.Kind)
	}
}

// TestRequest_UnknownKindIsAnError: a client that misspells a filter is told,
// not shown an empty page it will read as "nothing happened".
func TestRequest_UnknownKindIsAnError(t *testing.T) {
	t.Parallel()
	acct := testAccount(t)
	require.NoError(t, Request{AccountID: acct}.Validate())
	require.NoError(t, Request{AccountID: acct, Kinds: AllKinds()}.Validate())

	err := Request{AccountID: acct, Kinds: []Kind{KindNativeTrade, "TRADE"}}.Validate()
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))

	err = Request{}.Validate()
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestCursor_IsOpaqueAndRefusesTampering.
func TestCursor_IsOpaqueAndRefusesTampering(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)
	encoded := encodeCursor(cursor{At: at, ID: "01a08db8-90f7-776e-93f1-1b1eeb69f9f4"})
	got, ok, err := decodeCursor(encoded)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, at, got.At.UTC())

	_, ok, err = decodeCursor("   ")
	require.NoError(t, err)
	assert.False(t, ok)

	for _, bad := range []string{"!!!", "bm90LWpzb24", encodeCursor(cursor{At: at}), encodeCursor(cursor{ID: "x"})} {
		_, _, err := decodeCursor(bad)
		require.Error(t, err, "cursor %q must be refused", bad)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	}
}

// TestTemperature_SimulatedWinsOverEverything: on a sandbox tier no value is
// real by construction, so nothing in the feed may claim to be.
func TestTemperature_SimulatedWinsOverEverything(t *testing.T) {
	t.Parallel()
	live := NewFeed(false)
	amounts := live.amountsOf(false, "500", 1999, "USD", "PURCHASED", "", "0")
	require.Len(t, amounts, 2)
	assert.Equal(t, UnitCredits, amounts[0].Unit)
	assert.Equal(t, TemperatureEconomy, amounts[0].Temperature)
	assert.Equal(t, "PURCHASED", amounts[0].Origin)
	assert.Equal(t, UnitMoneyMinor, amounts[1].Unit)
	assert.Equal(t, TemperatureReal, amounts[1].Temperature)
	assert.Equal(t, "USD", amounts[1].Currency)

	sandbox := NewFeed(true)
	for _, a := range sandbox.amountsOf(false, "500", 1999, "USD", "PURCHASED", "", "0") {
		assert.Equal(t, TemperatureSimulated, a.Temperature,
			"a sandbox tier moves nothing, so no amount on it is REAL or ECONOMY")
	}
	// A demo object is SIMULATED even where the deployment is not stamped.
	for _, a := range live.amountsOf(true, "500", 0, "", "", "DEMOORB", "12") {
		assert.Equal(t, TemperatureSimulated, a.Temperature)
	}

	// A zero amount is omitted: "no money on this item" and "none left" are
	// different facts.
	assert.Empty(t, live.amountsOf(false, "0", 0, "", "", "", "0"))
	assert.True(t, isZeroDigits(" 0 "))
	assert.False(t, isZeroDigits("10"))
}

// TestSummary_NeverCarriesACharacterSomebodyChose: the sentence is data in a
// JSON field and stays that way, whatever a creator called their asset.
func TestSummary_NeverCarriesACharacterSomebodyChose(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "script", sanitizeSymbol("<script>"), "the markup is gone; the letters are just letters")
	assert.Equal(t, "ABC", sanitizeSymbol("A<B>C"))
	assert.Equal(t, "AB", sanitizeSymbol("A"+string(rune(0x202E))+"B"),
		"a bidirectional override is not a ticker character, and a source file that contained one "+
			"would be a file a reviewer approves something other than what they read")
	assert.LessOrEqual(t, len(sanitizeSymbol(strings.Repeat("A", 200))), 32)

	got := summaryFor(KindNativeTrade, "", "BUY", "<img src=x>")
	assert.NotContains(t, got, "<")
	assert.Contains(t, got, "imgsrcx")

	assert.Equal(t, "Bought an asset on the internal market", summaryFor(KindNativeTrade, "", "BUY", ""))
	assert.Equal(t, "Sold ORB on the internal market", summaryFor(KindNativeTrade, "", "SELL", "ORB"))
	assert.Equal(t, "A Credit purchase was refunded", summaryFor(KindCreditReversal, "REFUNDED", "", ""))
	assert.Equal(t, "A Credit purchase is disputed", summaryFor(KindCreditReversal, "DISPUTED", "", ""))
	assert.Equal(t, "Payout provider pending", summaryFor(KindPayoutStateChanged, "PROVIDER_PENDING", "", ""))
}
