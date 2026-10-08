package equities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

// ConversationKind is the format tag of a recorded broker conversation.
const ConversationKind = "equities.broker_conversation.v1"

// Call names one adapter method in a recorded conversation.
type Call string

// Calls.
const (
	CallQuote       Call = "QUOTE"
	CallPlaceOrder  Call = "PLACE_ORDER"
	CallOrderStatus Call = "ORDER_STATUS"
	CallCancel      Call = "CANCEL"
)

// AllCalls returns every call.
func AllCalls() []Call { return []Call{CallQuote, CallPlaceOrder, CallOrderStatus, CallCancel} }

func (c Call) valid() bool {
	for _, x := range AllCalls() {
		if x == c {
			return true
		}
	}
	return false
}

// RecordedError is an error answer in a conversation.
type RecordedError struct {
	Code   errs.Code `json:"code"`
	Detail string    `json:"detail"`
}

// Exchange is one request and its recorded answer: exactly one of Response
// and Error.
type Exchange struct {
	Call     Call            `json:"call"`
	Request  json.RawMessage `json:"request"`
	Response json.RawMessage `json:"response,omitempty"`
	Error    *RecordedError  `json:"error,omitempty"`
}

// Conversation is a recorded broker conversation: the requests a client
// made, in order, and what the broker answered.
type Conversation struct {
	Kind         string              `json:"kind"`
	Broker       string              `json:"broker"`
	ProviderMode config.ProviderMode `json:"provider_mode"`
	Description  string              `json:"description"`
	Exchanges    []Exchange          `json:"exchanges"`
}

// maxConversationBytes bounds what DecodeConversation reads.
const maxConversationBytes = 4 << 20

// DecodeConversation parses a recorded conversation strictly: unknown fields,
// trailing data, an unknown kind or call, a live provider mode, and an
// exchange that is not exactly one request with exactly one answer are all
// refused.
func DecodeConversation(b []byte) (Conversation, error) {
	if len(b) > maxConversationBytes {
		return Conversation{}, errs.New(errs.CodeValidationFailed, "equities: conversation too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Conversation
	if err := dec.Decode(&c); err != nil {
		return Conversation{}, errs.Wrap(err, errs.CodeValidationFailed, "equities: conversation is not valid JSON of the recorded format")
	}
	if dec.More() {
		return Conversation{}, errs.New(errs.CodeValidationFailed, "equities: trailing data after the conversation")
	}
	if err := c.validate(); err != nil {
		return Conversation{}, err
	}
	if c.Exchanges == nil {
		c.Exchanges = []Exchange{}
	}
	return c, nil
}

func (c Conversation) validate() error {
	if c.Kind != ConversationKind {
		return errs.Newf(errs.CodeValidationFailed, "equities: conversation kind must be %s", ConversationKind)
	}
	if strings.TrimSpace(c.Broker) == "" {
		return errs.New(errs.CodeValidationFailed, "equities: a conversation names its broker")
	}
	if c.ProviderMode == config.ProviderModeLive {
		return ErrLiveExecutionUnsupported()
	}
	if c.ProviderMode != config.ProviderModeFake && c.ProviderMode != config.ProviderModeSandbox {
		return errs.New(errs.CodeValidationFailed, "equities: a conversation is recorded in provider mode fake or sandbox")
	}
	for i, ex := range c.Exchanges {
		if !ex.Call.valid() {
			return errs.Newf(errs.CodeValidationFailed, "equities: exchange %d has an unknown call", i)
		}
		if !json.Valid(ex.Request) || len(bytes.TrimSpace(ex.Request)) == 0 || bytes.TrimSpace(ex.Request)[0] != '{' {
			return errs.Newf(errs.CodeValidationFailed, "equities: exchange %d request must be a JSON object", i)
		}
		hasResponse := len(bytes.TrimSpace(ex.Response)) > 0
		if hasResponse == (ex.Error != nil) {
			return errs.Newf(errs.CodeValidationFailed, "equities: exchange %d needs exactly one of response and error", i)
		}
		if hasResponse && (!json.Valid(ex.Response) || bytes.TrimSpace(ex.Response)[0] != '{') {
			return errs.Newf(errs.CodeValidationFailed, "equities: exchange %d response must be a JSON object", i)
		}
		if ex.Error != nil && (strings.TrimSpace(string(ex.Error.Code)) == "" || strings.TrimSpace(ex.Error.Detail) == "") {
			return errs.Newf(errs.CodeValidationFailed, "equities: exchange %d error needs a code and a detail", i)
		}
		// Requests are compared in canonical form, so a recording that cannot
		// be canonicalised (a fractional number, an unsafe integer) could never
		// be matched: refuse it here rather than at the first call.
		if _, err := audit.CanonicalJSON(ex.Request); err != nil {
			return errs.Newf(errs.CodeValidationFailed, "equities: exchange %d request is not canonical JSON", i)
		}
		if hasResponse {
			if _, err := audit.CanonicalJSON(ex.Response); err != nil {
				return errs.Newf(errs.CodeValidationFailed, "equities: exchange %d response is not canonical JSON", i)
			}
		}
	}
	return nil
}

// Encode renders the conversation as indented JSON with every request and
// response in canonical form, so a re-recording of the same conversation is
// byte-identical.
func (c Conversation) Encode() ([]byte, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	out := c
	out.Exchanges = make([]Exchange, len(c.Exchanges))
	for i, ex := range c.Exchanges {
		req, err := audit.CanonicalJSON(ex.Request)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "equities: canonicalise recorded request")
		}
		ex.Request = req
		if len(ex.Response) > 0 {
			resp, err := audit.CanonicalJSON(ex.Response)
			if err != nil {
				return nil, errs.Wrap(err, errs.CodeInternal, "equities: canonicalise recorded response")
			}
			ex.Response = resp
		}
		out.Exchanges[i] = ex
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "equities: encode conversation")
	}
	return buf.Bytes(), nil
}

// ReplayAdapter answers from a recorded conversation. Each call must be the
// next recorded call with a canonically identical request; anything else --
// a different call, a different request, a call past the end -- is refused
// with CONFLICT, so a client that drifts from the recorded contract fails
// loudly instead of being answered with somebody else's response.
type ReplayAdapter struct {
	mu   sync.Mutex
	conv Conversation
	next int
}

var _ EquitiesExecutionAdapter = (*ReplayAdapter)(nil)

// NewReplayAdapter replays conv.
func NewReplayAdapter(conv Conversation) (*ReplayAdapter, error) {
	if err := conv.validate(); err != nil {
		return nil, err
	}
	return &ReplayAdapter{conv: conv}, nil
}

// Name implements EquitiesExecutionAdapter.
func (r *ReplayAdapter) Name() string { return r.conv.Broker }

// Mode implements EquitiesExecutionAdapter.
func (r *ReplayAdapter) Mode() config.ProviderMode { return r.conv.ProviderMode }

// Remaining returns how many recorded exchanges have not been replayed.
func (r *ReplayAdapter) Remaining() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conv.Exchanges) - r.next
}

// Quote implements EquitiesExecutionAdapter.
func (r *ReplayAdapter) Quote(ctx context.Context, req QuoteRequest) (QuoteResponse, error) {
	var out QuoteResponse
	err := r.replay(ctx, CallQuote, req, &out)
	return out, err
}

// PlaceOrder implements EquitiesExecutionAdapter.
func (r *ReplayAdapter) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (OrderReport, error) {
	var out OrderReport
	err := r.replay(ctx, CallPlaceOrder, req, &out)
	return out, err
}

// OrderStatus implements EquitiesExecutionAdapter.
func (r *ReplayAdapter) OrderStatus(ctx context.Context, req OrderStatusRequest) (OrderReport, error) {
	var out OrderReport
	err := r.replay(ctx, CallOrderStatus, req, &out)
	return out, err
}

// Cancel implements EquitiesExecutionAdapter.
func (r *ReplayAdapter) Cancel(ctx context.Context, req CancelRequest) (OrderReport, error) {
	var out OrderReport
	err := r.replay(ctx, CallCancel, req, &out)
	return out, err
}

func (r *ReplayAdapter) replay(ctx context.Context, call Call, req, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	got, err := audit.CanonicalJSON(req)
	if err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, "equities: canonicalise request")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.next >= len(r.conv.Exchanges) {
		return errs.Newf(errs.CodeConflict, "equities: the recorded conversation has no exchange left for %s", call).
			WithField("call", string(call))
	}
	ex := r.conv.Exchanges[r.next]
	if ex.Call != call {
		return errs.Newf(errs.CodeConflict, "equities: recorded exchange %d is %s, not %s", r.next, ex.Call, call).
			WithField("exchange", r.next)
	}
	want, err := audit.CanonicalJSON(ex.Request)
	if err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, "equities: canonicalise recorded request")
	}
	if !bytes.Equal(got, want) {
		return errs.Newf(errs.CodeConflict, "equities: %s request differs from recorded exchange %d", call, r.next).
			WithField("exchange", r.next)
	}
	r.next++
	if ex.Error != nil {
		return errs.New(ex.Error.Code, ex.Error.Detail).WithField("recorded_exchange", r.next-1)
	}
	dec := json.NewDecoder(bytes.NewReader(ex.Response))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return errs.Wrap(err, errs.CodeValidationFailed, fmt.Sprintf("equities: recorded %s response does not decode", call))
	}
	return nil
}

// RecordingAdapter wraps an adapter and records every exchange, producing a
// Conversation a ReplayAdapter can serve.
type RecordingAdapter struct {
	inner       EquitiesExecutionAdapter
	description string
	mu          sync.Mutex
	exchanges   []Exchange
}

var _ EquitiesExecutionAdapter = (*RecordingAdapter)(nil)

// NewRecordingAdapter records inner.
func NewRecordingAdapter(inner EquitiesExecutionAdapter, description string) (*RecordingAdapter, error) {
	if inner == nil {
		return nil, errs.New(errs.CodeValidationFailed, "equities: nothing to record")
	}
	if inner.Mode() == config.ProviderModeLive {
		return nil, ErrLiveExecutionUnsupported()
	}
	return &RecordingAdapter{inner: inner, description: description}, nil
}

// Name implements EquitiesExecutionAdapter.
func (r *RecordingAdapter) Name() string { return r.inner.Name() }

// Mode implements EquitiesExecutionAdapter.
func (r *RecordingAdapter) Mode() config.ProviderMode { return r.inner.Mode() }

// Quote implements EquitiesExecutionAdapter.
func (r *RecordingAdapter) Quote(ctx context.Context, req QuoteRequest) (QuoteResponse, error) {
	out, err := r.inner.Quote(ctx, req)
	return out, r.record(CallQuote, req, out, err)
}

// PlaceOrder implements EquitiesExecutionAdapter.
func (r *RecordingAdapter) PlaceOrder(ctx context.Context, req PlaceOrderRequest) (OrderReport, error) {
	out, err := r.inner.PlaceOrder(ctx, req)
	return out, r.record(CallPlaceOrder, req, out, err)
}

// OrderStatus implements EquitiesExecutionAdapter.
func (r *RecordingAdapter) OrderStatus(ctx context.Context, req OrderStatusRequest) (OrderReport, error) {
	out, err := r.inner.OrderStatus(ctx, req)
	return out, r.record(CallOrderStatus, req, out, err)
}

// Cancel implements EquitiesExecutionAdapter.
func (r *RecordingAdapter) Cancel(ctx context.Context, req CancelRequest) (OrderReport, error) {
	out, err := r.inner.Cancel(ctx, req)
	return out, r.record(CallCancel, req, out, err)
}

func (r *RecordingAdapter) record(call Call, req, resp any, callErr error) error {
	reqJSON, err := audit.CanonicalJSON(req)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "equities: record request")
	}
	ex := Exchange{Call: call, Request: reqJSON}
	if callErr != nil {
		ee, ok := errs.As(callErr)
		if !ok {
			return callErr
		}
		ex.Error = &RecordedError{Code: ee.Code, Detail: ee.Detail}
	} else {
		respJSON, err := audit.CanonicalJSON(resp)
		if err != nil {
			return errs.Wrap(err, errs.CodeInternal, "equities: record response")
		}
		ex.Response = respJSON
	}
	r.mu.Lock()
	r.exchanges = append(r.exchanges, ex)
	r.mu.Unlock()
	return callErr
}

// Conversation returns what was recorded so far.
func (r *RecordingAdapter) Conversation() Conversation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Conversation{
		Kind: ConversationKind, Broker: r.inner.Name(), ProviderMode: r.inner.Mode(),
		Description: r.description, Exchanges: append([]Exchange(nil), r.exchanges...),
	}
}
