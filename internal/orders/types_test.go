package orders

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nodal/controlplane/internal/matching"
)

func unique[T comparable](t *testing.T, name string, xs []T) {
	t.Helper()
	seen := map[T]bool{}
	for _, x := range xs {
		assert.False(t, seen[x], "%s lists %v twice", name, x)
		seen[x] = true
	}
	assert.NotEmpty(t, xs, name)
}

func TestEnumerations_AreUniqueAndValid(t *testing.T) {
	unique(t, "owner types", AllOwnerTypes())
	unique(t, "statuses", AllStatuses())
	unique(t, "book statuses", AllBookStatuses())
	unique(t, "sides", AllSides())
	unique(t, "order types", AllOrderTypes())
	unique(t, "times in force", AllTimesInForce())
	unique(t, "stp modes", AllSTPModes())
	unique(t, "liquidity intents", AllLiquidityIntents())
	unique(t, "command kinds", AllCommandKinds())
	unique(t, "crossing command kinds", AllCrossingCommandKinds())
	unique(t, "event types", AllEventTypes())
	unique(t, "terms sources", AllTermsSources())
	unique(t, "book modes", AllBookModes())
	unique(t, "book instrument kinds", AllBookInstrumentKinds())
	unique(t, "house reservation statuses", AllHouseReservationStatuses())
	for _, s := range AllStatuses() {
		assert.True(t, s.Valid())
	}
	assert.Equal(t, []string{"ACCEPTED", "REJECTED", "RESTING", "TRADE", "PARTIAL_FILL", "FULL_FILL", "CANCELLED", "REPLACED", "BOOK_DELTA"},
		AllEventTypes())
	for _, s := range AllSTPModes() {
		assert.True(t, s.Valid())
	}
}

func TestOrderTransitions_NeverLeaveATerminalStatus(t *testing.T) {
	for _, from := range AllStatuses() {
		assert.False(t, CanTransition(from, from), "%s -> %s", from, from)
		if from.Terminal() {
			for _, to := range AllStatuses() {
				assert.False(t, CanTransition(from, to), "terminal %s -> %s", from, to)
			}
		}
	}
	for _, to := range []Status{StatusOpen, StatusPartiallyFilled, StatusFilled, StatusCancelled, StatusRejected} {
		assert.True(t, CanTransition(StatusPending, to), "a pending order can reach %s", to)
	}
	assert.False(t, CanTransition(StatusOpen, StatusRejected), "an order on the book is never rejected")
	assert.False(t, CanTransition(StatusPartiallyFilled, StatusOpen), "fills never shrink")
	for _, from := range AllBookStatuses() {
		assert.False(t, CanTransitionBook(from, from))
	}
	assert.False(t, CanTransitionBook(BookClosed, BookOpen), "a closed book stays closed")
}

func TestEngineMappings(t *testing.T) {
	assert.Equal(t, StatusOpen, statusOf(matching.StatusOpen))
	assert.Equal(t, StatusPartiallyFilled, statusOf(matching.StatusPartiallyFilled))
	assert.Equal(t, StatusFilled, statusOf(matching.StatusFilled))
	assert.Equal(t, StatusCancelled, statusOf(matching.StatusCancelled))
	assert.Equal(t, StatusRejected, statusOf(matching.StatusRejected))
	assert.Equal(t, TakeLiquidity, liquidityIntentOf(TypeMarket, IOC))
	assert.Equal(t, TakeLiquidity, liquidityIntentOf(TypeLimit, FOK))
	assert.Equal(t, ProvideLiquidity, liquidityIntentOf(TypeLimit, GTC))
	for _, s := range AllSides() {
		assert.Equal(t, s, sideOf(s.engine()))
	}
	assert.Equal(t, CommandReplace, commandKindOf(matching.CommandReplace))
}

func TestOrderShape_RefusesEveryMalformedOrderWithAStableReason(t *testing.T) {
	ok := orderShape{Side: SideBuy, Type: TypeLimit, TIF: GTC}
	assert.NoError(t, ok.validate())
	cases := map[string]struct {
		mutate func(*orderShape)
		reason string
	}{
		"side":              {func(o *orderShape) { o.Side = "HOLD" }, ReasonInvalidSide},
		"type":              {func(o *orderShape) { o.Type = "STOP" }, ReasonInvalidType},
		"tif":               {func(o *orderShape) { o.TIF = "DAY" }, ReasonInvalidTIF},
		"stp":               {func(o *orderShape) { o.STP = "CANCEL_NOBODY" }, ReasonInvalidSTP},
		"market GTC":        {func(o *orderShape) { o.Type = TypeMarket }, ReasonMarketGTC},
		"post-only IOC":     {func(o *orderShape) { o.PostOnly, o.TIF = true, IOC }, ReasonPostOnlyShape},
		"reduce-only buy":   {func(o *orderShape) { o.ReduceOnly = true }, ReasonReduceOnlyBuy},
		"client id spaces":  {func(o *orderShape) { o.ClientOrderID = " x" }, ReasonClientOrderID},
		"client id control": {func(o *orderShape) { o.ClientOrderID = "a\x00b" }, ReasonClientOrderID},
		"client id too long": {func(o *orderShape) {
			o.ClientOrderID = string(make([]byte, 129))
		}, ReasonClientOrderID},
	}
	for name, c := range cases {
		o := ok
		c.mutate(&o)
		assert.Equal(t, c.reason, ReasonOf(o.validate()), name)
	}
}
