package intent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Canonical returns the deterministic JSON encoding of the semantic content
// of t: the fields that make it a particular financial intent. It is what
// ContentHash digests and what the idempotency replay compares.
//
// Included: account_id, actor_type, actor_id, agent_id, strategy_version_id,
// prediction_id, action, instrument_id, notional_usd, target_exposure_usd,
// quantity, constraints, deadline, idempotency_key, mode.
//
// Excluded on purpose: ID (server-assigned), RequestedAt (a retry of the same
// command may be stamped later), CorrelationID (transport metadata, not
// intent), and every lifecycle field. Two submissions that differ only in
// those are the same financial intent.
//
// Encoding: one JSON object, keys sorted bytewise at every level, no
// whitespace, no HTML escaping; money as decimal strings; bps and
// milliseconds as integers; times as RFC3339Nano in UTC; identifiers
// lower-cased; absent optional fields omitted; AllowedVenues sorted (it is a
// set). The output is valid JSON and never depends on struct field order,
// map iteration order or the caller's time zone. It cannot fail.
func Canonical(t TradeIntent) []byte {
	obj := newObject()
	obj.str("account_id", strings.ToLower(t.AccountID))
	obj.str("actor_type", string(t.ActorType))
	obj.str("actor_id", t.ActorID)
	obj.optStr("agent_id", lowerPtr(t.AgentID))
	obj.optStr("strategy_version_id", lowerPtr(t.StrategyVersionID))
	obj.optStr("prediction_id", lowerPtr(t.PredictionID))
	obj.str("action", string(t.Action))
	obj.str("instrument_id", strings.ToLower(t.InstrumentID.String()))
	if t.NotionalUSD != nil {
		obj.str("notional_usd", t.NotionalUSD.String())
	}
	if t.TargetExposureUSD != nil {
		obj.str("target_exposure_usd", t.TargetExposureUSD.String())
	}
	if t.Quantity != nil {
		obj.str("quantity", t.Quantity.String())
	}
	obj.raw("constraints", canonicalConstraints(t.Constraints))
	if !t.Deadline.IsZero() {
		obj.str("deadline", formatTime(t.Deadline))
	}
	obj.str("idempotency_key", t.IdempotencyKey)
	obj.str("mode", string(t.Mode))
	return obj.bytes()
}

// ContentHash returns sha256(Canonical(t)); it is persisted as
// trade_intents.content_hash.
func ContentHash(t TradeIntent) []byte {
	sum := sha256.Sum256(Canonical(t))
	return sum[:]
}

func canonicalConstraints(c Constraints) []byte {
	obj := newObject()
	obj.num("max_slippage_bps", int64(c.MaxSlippageBPS))
	obj.num("max_fee_bps", int64(c.MaxFeeBPS))
	obj.num("max_price_impact_bps", int64(c.MaxPriceImpactBPS))
	if c.MaxPrice != nil {
		p := newObject()
		p.str("mantissa", c.MaxPrice.Mantissa.String())
		p.num("scale", int64(c.MaxPrice.Scale))
		p.str("quote_asset", c.MaxPrice.QuoteAsset)
		p.str("source", c.MaxPrice.Source)
		if !c.MaxPrice.At.IsZero() {
			p.str("at", formatTime(c.MaxPrice.At))
		}
		obj.raw("max_price", p.bytes())
	}
	if c.MinReceive != nil {
		obj.str("min_receive", c.MinReceive.String())
	}
	if len(c.AllowedVenues) > 0 {
		venues := append([]string(nil), c.AllowedVenues...)
		sort.Strings(venues)
		obj.strs("allowed_venues", venues)
	}
	obj.num("quote_freshness_ms", int64(c.QuoteFreshness/time.Millisecond))
	if !c.ExecutionDeadline.IsZero() {
		obj.str("execution_deadline", formatTime(c.ExecutionDeadline))
	}
	return obj.bytes()
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func lowerPtr(s *string) *string {
	if s == nil {
		return nil
	}
	l := strings.ToLower(*s)
	return &l
}

// object accumulates already-encoded members and emits them sorted by key.
type object struct {
	members map[string][]byte
}

func newObject() *object { return &object{members: map[string][]byte{}} }

func (o *object) raw(k string, encoded []byte) { o.members[k] = encoded }

func (o *object) str(k, v string) { o.members[k] = jsonString(v) }

func (o *object) optStr(k string, v *string) {
	if v != nil {
		o.str(k, *v)
	}
}

func (o *object) num(k string, v int64) { o.members[k] = []byte(strconv.FormatInt(v, 10)) }

func (o *object) strs(k string, vs []string) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, v := range vs {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonString(v))
	}
	buf.WriteByte(']')
	o.members[k] = buf.Bytes()
}

func (o *object) bytes() []byte {
	keys := make([]string, 0, len(o.members))
	for k := range o.members {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonString(k))
		buf.WriteByte(':')
		buf.Write(o.members[k])
	}
	buf.WriteByte('}')
	return buf.Bytes()
}

// jsonString encodes s as a JSON string without HTML escaping. Encoding a
// string cannot fail; invalid UTF-8 is replaced by U+FFFD by encoding/json,
// which keeps the output deterministic for any input.
func jsonString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
