package equities

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/security"
)

func TestTransitions_OrderAndCorporateActionTables(t *testing.T) {
	for _, s := range AllOrderStatuses() {
		assert.False(t, CanTransitionOrder(s, s), "%s self transition", s)
		if s.Terminal() {
			for _, to := range AllOrderStatuses() {
				assert.False(t, CanTransitionOrder(s, to), "terminal %s -> %s", s, to)
			}
		}
	}
	assert.False(t, CanTransitionOrder(OrderOpen, OrderRejected), "a venue does not reject an order it accepted")
	assert.False(t, CanTransitionOrder(OrderOpen, OrderAccepted))
	assert.True(t, CanTransitionOrder(OrderAccepted, OrderFilled), "a market order fills on its first answer")
	for _, s := range AllCorporateActionStatuses() {
		assert.False(t, CanTransitionAction(s, s))
	}
	assert.False(t, CanTransitionAction(ActionApplied, ActionCancelled), "an applied action is history")
	assert.False(t, CanTransitionAction(ActionApplying, ActionCancelled), "a delisting that began cannot be withdrawn")
}

func TestEnumLists_AreDistinctAndValid(t *testing.T) {
	distinct := func(name string, xs []string) {
		seen := map[string]bool{}
		for _, x := range xs {
			assert.False(t, seen[x], "%s lists %s twice", name, x)
			seen[x] = true
		}
		assert.NotEmpty(t, xs, name)
	}
	str := func(f func() []string) []string { return f() }
	distinct("sides", mapStrings(AllSides()))
	distinct("order types", mapStrings(AllOrderTypes()))
	distinct("order statuses", mapStrings(AllOrderStatuses()))
	distinct("sessions", mapStrings(AllSessions()))
	distinct("rejection codes", mapStrings(AllRejectionCodes()))
	distinct("action types", mapStrings(AllCorporateActionTypes()))
	distinct("action statuses", mapStrings(AllCorporateActionStatuses()))
	distinct("roundings", mapStrings(AllCorporateRoundings()))
	distinct("modes", mapStrings(AllOrderModes()))
	distinct("calendars", str(AllCalendarCodes))
	for _, m := range AllOrderModes() {
		assert.True(t, m.Simulated(), "an equity order mode is simulated: %s", m)
		assert.NotEqual(t, gates.ModeLive, m)
	}
	for _, a := range AllTransitionActorTypes() {
		assert.NotEqual(t, security.ActorAgent, a, "an agent never moves an order")
	}
	for _, c := range AllRejectionCodes() {
		assert.NotEmpty(t, c.errorCode())
	}
}

func mapStrings[T ~string](xs []T) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, string(x))
	}
	return out
}

func TestOrderRequest_ValidateRefusesEveryMalformedShape(t *testing.T) {
	valid := func() OrderRequest {
		return OrderRequest{AccountID: accounts.AccountID(NewOrderID()), InstrumentID: instruments.NewInstrumentID(), Side: SideBuy,
			Type: OrderLimit, Quantity: q(1_000_000), LimitPrice: &PricePoint{Mantissa: q(15_000), Scale: 2}, Mode: gates.ModePaper,
			IdempotencyKey: "k-1", ActorType: security.ActorUser, ActorID: "u"}
	}
	require.NoError(t, valid().Validate())
	for name, mutate := range map[string]func(*OrderRequest){
		"live mode":              func(r *OrderRequest) { r.Mode = gates.ModeLive },
		"devnet mode":            func(r *OrderRequest) { r.Mode = gates.ModeDevnet },
		"zero quantity":          func(r *OrderRequest) { r.Quantity = q(0) },
		"negative quantity":      func(r *OrderRequest) { r.Quantity = q(-1) },
		"limit without price":    func(r *OrderRequest) { r.LimitPrice = nil },
		"limit at zero":          func(r *OrderRequest) { r.LimitPrice = &PricePoint{Mantissa: q(0), Scale: 2} },
		"market with price":      func(r *OrderRequest) { r.Type = OrderMarket },
		"market extended":        func(r *OrderRequest) { r.Type, r.LimitPrice, r.ExtendedHours = OrderMarket, nil, true },
		"unknown side":           func(r *OrderRequest) { r.Side = "SHORT" },
		"no instrument":          func(r *OrderRequest) { r.InstrumentID = instruments.InstrumentID{} },
		"padded idempotency key": func(r *OrderRequest) { r.IdempotencyKey = " k " },
	} {
		t.Run(name, func(t *testing.T) {
			r := valid()
			mutate(&r)
			err := r.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// liveAdapter claims live; NewService must refuse it whatever else is wired.
type liveAdapter struct{ *PaperVenue }

func (liveAdapter) Mode() config.ProviderMode { return config.ProviderModeLive }

func TestNewService_RefusesALiveAdapterAndIncompleteWiring(t *testing.T) {
	_, err := NewService(Config{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	_, err = NewService(Config{Adapter: liveAdapter{testVenue(t, 0)}})
	require.Error(t, err)
	_, err = NewRecordingAdapter(liveAdapter{testVenue(t, 0)}, "x")
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
}

func TestOrderRefusal_RendersTheStableCode(t *testing.T) {
	o := Order{ID: NewOrderID(), Status: OrderRejected, RejectionCode: RejectRisk, RejectionReasons: []string{"RISK_KILL_SWITCH"}}
	err := o.Refusal()
	require.Error(t, err)
	assert.Equal(t, errs.CodeRiskRejected, errs.CodeOf(err))
	ee, _ := errs.As(err)
	assert.Equal(t, []string{"RISK_KILL_SWITCH"}, ee.Fields["reason_codes"])
	o.Status = OrderFilled
	assert.NoError(t, o.Refusal())
}
