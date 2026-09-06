package modeltest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/model"
	"github.com/nodal/controlplane/internal/money"
)

// Turn is one scripted response. Exactly one of Body or Err is used: Err
// takes precedence, so a turn can inject a provider failure at a chosen
// point in a retry sequence.
type Turn struct {
	// Body is the raw structured output. It is deliberately a string rather
	// than a typed value so a test can script malformed JSON, a
	// schema-valid-but-forbidden document, or a truncated body.
	Body string
	// Err is returned instead of a response.
	Err error
	// StopReason overrides the reported stop reason (default "end_turn").
	// Setting "max_tokens" scripts a truncated answer.
	StopReason string
	// Delay advances the fake clock before answering, so a caller's deadline
	// handling can be exercised without real time passing.
	Delay time.Duration
	// Usage overrides the reported token counts.
	Usage *model.Usage
	// ModelID overrides the reported model identifier.
	ModelID string
}

// Fake is a scripted model.Provider. It is safe for concurrent use, and it
// records every request it was given so a test can assert on what the
// caller actually sent — which is how the prompt-injection tests prove that
// untrusted content never reached the instruction channel.
type Fake struct {
	mu       sync.Mutex
	turns    []Turn
	next     int
	requests []model.Request
	clk      *clock.Fake

	// ModelName is reported as the model identifier when a Turn does not
	// override it.
	ModelName string
	// Prices converts scripted usage into cost.
	Prices model.PriceTable
}

// New returns a Fake, refusing any environment where a scripted model could
// reach real money. The check is the constructor's first act.
func New(env config.Environment, turns ...Turn) (*Fake, error) {
	switch env {
	case config.EnvLocal, config.EnvTest, config.EnvDev:
	default:
		return nil, fmt.Errorf("%w: environment %s", model.ErrFakeNotAllowed, env)
	}
	return &Fake{
		turns:     turns,
		clk:       clock.NewFake(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)),
		ModelName: model.DefaultModel,
		Prices:    model.DefaultPriceTable(),
	}, nil
}

// MustNew is New for tests that have already decided the environment.
func MustNew(env config.Environment, turns ...Turn) *Fake {
	f, err := New(env, turns...)
	if err != nil {
		panic("modeltest: " + err.Error())
	}
	return f
}

// Script replaces the remaining turns and rewinds the cursor.
func (f *Fake) Script(turns ...Turn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = turns
	f.next = 0
}

// Name identifies the fake in provenance rows. It is deliberately not
// "anthropic": a persisted row must never claim a real provider answered.
func (f *Fake) Name() string { return "fake" }

// Calls is how many times Complete was invoked.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// Requests returns a copy of every request received, in order.
func (f *Fake) Requests() []model.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.Request(nil), f.requests...)
}

// LastRequest returns the most recent request.
func (f *Fake) LastRequest() (model.Request, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return model.Request{}, false
	}
	return f.requests[len(f.requests)-1], true
}

// Complete returns the next scripted turn. Running past the end of the
// script is an error rather than a repeat of the last turn: a test that
// makes more calls than it scripted has found a real bug in the caller's
// retry bounds, and silently serving another answer would hide it.
func (f *Fake) Complete(ctx context.Context, req model.Request) (model.Response, error) {
	if err := req.Validate(); err != nil {
		return model.Response{}, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	if f.next >= len(f.turns) {
		f.mu.Unlock()
		return model.Response{}, fmt.Errorf("modeltest: no scripted turn for call %d (script has %d)", len(f.requests), len(f.turns))
	}
	turn := f.turns[f.next]
	f.next++
	modelID := turn.ModelID
	if modelID == "" {
		modelID = f.ModelName
	}
	prices := f.Prices
	f.mu.Unlock()

	requestedAt := f.clk.Now().UTC()
	if turn.Delay > 0 {
		f.clk.Advance(turn.Delay)
	}
	if err := ctx.Err(); err != nil {
		return model.Response{}, model.Unavailable("context canceled", err)
	}
	if turn.Err != nil {
		return model.Response{}, turn.Err
	}
	respondedAt := f.clk.Now().UTC()

	inputHash, err := req.InputHash()
	if err != nil {
		return model.Response{}, err
	}
	usage := model.Usage{InputTokens: 1000, OutputTokens: 500}
	if turn.Usage != nil {
		usage = *turn.Usage
	}
	if cost, err := prices.Cost(modelID, usage); err == nil {
		usage.Cost = cost
	} else {
		usage.Cost = money.USDFromMinor(0)
	}
	stop := turn.StopReason
	if stop == "" {
		stop = "end_turn"
	}
	outputHash := sha256.Sum256([]byte(turn.Body))

	return model.Response{
		Provider:    f.Name(),
		ModelID:     modelID,
		Structured:  json.RawMessage(turn.Body),
		StopReason:  stop,
		Usage:       usage,
		RequestedAt: requestedAt,
		RespondedAt: respondedAt,
		InputHash:   inputHash,
		OutputHash:  outputHash[:],
		RequestID:   fmt.Sprintf("fake-req-%d", f.Calls()),
		Latency:     respondedAt.Sub(requestedAt),
	}, nil
}

// Clock exposes the fake clock so a test can assert on scripted delays.
func (f *Fake) Clock() *clock.Fake { return f.clk }

// Unavailable is a convenience turn for provider outage tests (PART 177).
func Unavailable(detail string) Turn {
	return Turn{Err: model.Unavailable(detail, model.ErrUnavailable)}
}

// Truncated is a convenience turn for a response cut short by the output
// cap. The body is deliberately valid-looking but incomplete.
func Truncated(body string) Turn {
	return Turn{Body: body, StopReason: "max_tokens"}
}
