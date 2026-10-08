package equities

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

var updateConversation = flag.Bool("update-conversation", false, "re-record testdata/paper_broker_conversation.json from the paper venue")

const conversationFixture = "paper_broker_conversation.json"

// runContractScenario drives an adapter through a fixed conversation and
// returns every answer in canonical form: a response's canonical JSON, or
// "error <code>: <detail>".
func runContractScenario(t *testing.T, a EquitiesExecutionAdapter) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	answer := func(v any, err error) {
		if err != nil {
			ee, ok := errs.As(err)
			require.True(t, ok, "adapters answer with typed errors: %v", err)
			out = append(out, "error "+string(ee.Code)+": "+ee.Detail)
			return
		}
		b, err := audit.CanonicalJSON(v)
		require.NoError(t, err)
		out = append(out, string(b))
	}
	ten := ny(t, 2026, 10, 7, 10, 0, 0)
	eleven := ny(t, 2026, 10, 7, 11, 0, 0)
	noon := ny(t, 2026, 10, 7, 12, 0, 0)
	one := ny(t, 2026, 10, 7, 13, 0, 0)
	closeAt := ny(t, 2026, 10, 7, 16, 0, 0)
	pre := ny(t, 2026, 10, 7, 8, 0, 0)

	// 1. An indicative quote, then a market buy inside its protection.
	answer(a.Quote(ctx, QuoteRequest{Symbol: "SBXA", ExchangeMIC: "XNAS", Side: SideBuy, Quantity: q(2_500_000), QuantityDecimals: 6,
		PriceScale: 2, TickSize: q(1), Mark: mark(15_000, 2, ten), At: ten}))
	buy := marketOrder(SideBuy, 15_165, closeAt)
	buy.ClientOrderID = "01920000-0000-7000-8000-00000000000a"
	answer(a.PlaceOrder(ctx, PlaceOrderRequest{Order: buy, Mark: mark(15_000, 2, ten), At: ten}))
	// 2. A limit sell rests, is polled while the mark is below it, then fills.
	sell := limitOrder(SideSell, 15_100, false, closeAt)
	sell.ClientOrderID = "01920000-0000-7000-8000-00000000000b"
	answer(a.PlaceOrder(ctx, PlaceOrderRequest{Order: sell, Mark: mark(15_000, 2, ten), At: ten}))
	answer(a.OrderStatus(ctx, OrderStatusRequest{BrokerOrderID: "paper-" + sell.ClientOrderID, Order: sell, Mark: mark(15_050, 2, eleven), At: eleven}))
	answer(a.OrderStatus(ctx, OrderStatusRequest{BrokerOrderID: "paper-" + sell.ClientOrderID, Order: sell, Mark: mark(15_120, 2, noon), At: noon}))
	// 3. A limit buy rests and is cancelled.
	rest := limitOrder(SideBuy, 14_900, false, closeAt)
	rest.ClientOrderID = "01920000-0000-7000-8000-00000000000c"
	answer(a.PlaceOrder(ctx, PlaceOrderRequest{Order: rest, Mark: mark(15_000, 2, one), At: one}))
	answer(a.Cancel(ctx, CancelRequest{BrokerOrderID: "paper-" + rest.ClientOrderID, Order: rest, Mark: mark(15_000, 2, one), At: one}))
	// 4. A market order before the open is refused; a quote with no mark is an error.
	early := marketOrder(SideBuy, 15_165, closeAt)
	early.ClientOrderID = "01920000-0000-7000-8000-00000000000d"
	answer(a.PlaceOrder(ctx, PlaceOrderRequest{Order: early, Mark: mark(15_000, 2, pre), At: pre}))
	answer(a.Quote(ctx, QuoteRequest{Symbol: "SBXA", ExchangeMIC: "XNAS", Side: SideSell, Quantity: q(1), QuantityDecimals: 6,
		PriceScale: 2, TickSize: q(1), At: ten}))
	// 5. A resting order expires at the close.
	late := limitOrder(SideBuy, 14_000, false, closeAt)
	late.ClientOrderID = "01920000-0000-7000-8000-00000000000e"
	answer(a.OrderStatus(ctx, OrderStatusRequest{Order: late, Mark: mark(15_000, 2, closeAt), At: closeAt}))
	return out
}

// TestContract_PaperAndReplayAdaptersAgree: the recorded paper broker
// conversation in testdata is exactly what the paper venue answers today,
// and the replay adapter, serving that recording, gives a client the same
// answers in the same order and nothing more.
func TestContract_PaperAndReplayAdaptersAgree(t *testing.T) {
	rec, err := NewRecordingAdapter(testVenue(t, 10), "a recorded paper broker conversation: quote, market fill, "+
		"resting limit that fills on a crossing mark, cancel, pre-market refusal, quote without a mark, expiry")
	require.NoError(t, err)
	paperAnswers := runContractScenario(t, rec)
	encoded, err := rec.Conversation().Encode()
	require.NoError(t, err)

	path := filepath.Join("testdata", conversationFixture)
	if *updateConversation {
		require.NoError(t, os.WriteFile(path, encoded, 0o600))
	}
	fixture, err := os.ReadFile(path) // #nosec G304 -- a fixed test fixture path
	require.NoError(t, err)
	require.Equal(t, string(encoded), string(fixture),
		"the paper venue no longer produces the recorded conversation; if that is intended, re-record with -update-conversation")

	conv, err := DecodeConversation(fixture)
	require.NoError(t, err)
	replay, err := NewReplayAdapter(conv)
	require.NoError(t, err)
	assert.Equal(t, "nodal-paper-equities", replay.Name())
	assert.Equal(t, config.ProviderModeFake, replay.Mode())
	replayAnswers := runContractScenario(t, replay)
	assert.Equal(t, paperAnswers, replayAnswers)
	assert.Zero(t, replay.Remaining(), "the client used the whole recorded conversation")
}

// TestReplay_RefusesAClientThatDriftsFromTheRecording: a different request,
// a different call or one call too many is a CONFLICT, never somebody else's
// answer.
func TestReplay_RefusesAClientThatDriftsFromTheRecording(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", conversationFixture))
	require.NoError(t, err)
	conv, err := DecodeConversation(fixture)
	require.NoError(t, err)
	ctx := context.Background()
	ten := ny(t, 2026, 10, 7, 10, 0, 0)

	r, err := NewReplayAdapter(conv)
	require.NoError(t, err)
	_, err = r.Quote(ctx, QuoteRequest{Symbol: "SBXA", ExchangeMIC: "XNAS", Side: SideBuy, Quantity: q(2_500_001), QuantityDecimals: 6,
		PriceScale: 2, TickSize: q(1), Mark: mark(15_000, 2, ten), At: ten})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))

	r, err = NewReplayAdapter(conv)
	require.NoError(t, err)
	_, err = r.PlaceOrder(ctx, PlaceOrderRequest{Order: marketOrder(SideBuy, 15_165, ten), At: ten})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "the first recorded call is a QUOTE")

	empty, err := NewReplayAdapter(Conversation{Kind: ConversationKind, Broker: "b", ProviderMode: config.ProviderModeFake})
	require.NoError(t, err)
	_, err = empty.Quote(ctx, QuoteRequest{})
	require.Error(t, err)
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
}

func TestReplay_DecodeConversationIsStrict(t *testing.T) {
	good := `{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"fake","description":"","exchanges":[` +
		`{"call":"QUOTE","request":{},"response":{}}]}`
	_, err := DecodeConversation([]byte(good))
	require.NoError(t, err)
	for name, bad := range map[string]string{
		"unknown field":    `{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"fake","x":1,"exchanges":[]}`,
		"live":             `{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"live","exchanges":[]}`,
		"wrong kind":       `{"kind":"other","broker":"b","provider_mode":"fake","exchanges":[]}`,
		"no answer":        `{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"fake","exchanges":[{"call":"QUOTE","request":{}}]}`,
		"two answers":      `{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"fake","exchanges":[{"call":"QUOTE","request":{},"response":{},"error":{"code":"X","detail":"y"}}]}`,
		"unknown call":     `{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"fake","exchanges":[{"call":"WIRE","request":{},"response":{}}]}`,
		"request not obj":  `{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"fake","exchanges":[{"call":"QUOTE","request":[],"response":{}}]}`,
		"trailing garbage": good + `{}`,
		"not json":         `{`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeConversation([]byte(bad))
			require.Error(t, err)
		})
	}
	live := Conversation{Kind: ConversationKind, Broker: "b", ProviderMode: config.ProviderModeLive}
	_, err = NewReplayAdapter(live)
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
}

func FuzzDecodeConversation(f *testing.F) {
	if b, err := os.ReadFile(filepath.Join("testdata", conversationFixture)); err == nil {
		f.Add(b)
	}
	f.Add([]byte(`{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"fake","exchanges":[]}`))
	f.Add([]byte(`{"kind":"equities.broker_conversation.v1","broker":"b","provider_mode":"sandbox","exchanges":[{"call":"CANCEL","request":{"a":1},"error":{"code":"X","detail":"y"}}]}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := DecodeConversation(data)
		if err != nil {
			return
		}
		enc, err := c.Encode()
		if err != nil {
			t.Fatalf("a decoded conversation must encode: %v", err)
		}
		again, err := DecodeConversation(enc)
		if err != nil {
			t.Fatalf("an encoded conversation must decode: %v", err)
		}
		a, _ := json.Marshal(c.Exchanges)
		b, _ := json.Marshal(again.Exchanges)
		ca, errA := audit.CanonicalJSON(json.RawMessage(a))
		cb, errB := audit.CanonicalJSON(json.RawMessage(b))
		if errA != nil || errB != nil || string(ca) != string(cb) {
			t.Fatalf("re-encoding changed the exchanges")
		}
	})
}
