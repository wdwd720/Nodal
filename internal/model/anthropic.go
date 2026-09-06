package model

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

// ProviderAnthropic is the value persisted in model_calls.provider.
const ProviderAnthropic = "anthropic"

// DefaultModel is the compiler's model. The provider documentation
// recommends Opus 5 for this workload and notes that Fable 5.1 rejects a
// forced tool_choice, which is why constrained output here goes through
// output_config.format rather than a forced tool call.
const DefaultModel = "claude-opus-5"

// AnthropicConfig configures the adapter.
type AnthropicConfig struct {
	// APIKey is resolved by the caller from a config.SecretRef. This package
	// never reads an environment variable or a secret store itself.
	APIKey string
	// BaseURL overrides the endpoint (used by the contract tests against a
	// recorded transport). Empty means the SDK default.
	BaseURL string
	// Model is the default model identifier when a Request does not name one.
	Model string
	// MaxRetries bounds SDK-level retries. Every retry that reaches the
	// model is billed, so this is deliberately small and explicit.
	MaxRetries int
	// RequestTimeout bounds a single call.
	RequestTimeout time.Duration
	// Prices converts usage into money. Required: an unpriced call cannot be
	// budgeted or billed.
	Prices PriceTable
	// HTTPClient overrides the transport, so a test can serve recorded
	// responses without a network.
	HTTPClient option.HTTPClient
	Clock      clock.Clock
}

// Anthropic is a Provider backed by the pinned Anthropic SDK.
type Anthropic struct {
	client anthropic.Client
	cfg    AnthropicConfig
	clk    clock.Clock
}

// NewAnthropic builds the adapter. It fails closed on missing credentials
// or an unusable price table rather than deferring the failure to the first
// call.
func NewAnthropic(cfg AnthropicConfig) (*Anthropic, error) {
	if cfg.APIKey == "" && cfg.HTTPClient == nil {
		return nil, errs.New(errs.CodeValidationFailed, "model: anthropic adapter requires an API key")
	}
	if err := cfg.Prices.Validate(); err != nil {
		return nil, err
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 2 * time.Minute
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System()
	}
	opts := []option.RequestOption{
		option.WithMaxRetries(cfg.MaxRetries),
		option.WithRequestTimeout(cfg.RequestTimeout),
	}
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	return &Anthropic{client: anthropic.NewClient(opts...), cfg: cfg, clk: cfg.Clock}, nil
}

// Name identifies the provider in provenance rows.
func (a *Anthropic) Name() string { return ProviderAnthropic }

// Complete performs one schema-constrained inference.
//
// The three prompt sections are mapped onto the API deliberately: the
// system policy becomes the `system` parameter (the instruction channel),
// and the tool-result and untrusted segments become a single user message
// whose text is the rendered, delimited data block. No tools are declared,
// so there is no path by which content in the data block can invoke one.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	if err := req.Validate(); err != nil {
		return Response{}, err
	}
	rendered, err := req.Render()
	if err != nil {
		return Response{}, err
	}
	var schema map[string]any
	if err := json.Unmarshal(req.OutputSchema, &schema); err != nil {
		return Response{}, errs.Wrap(err, errs.CodeValidationFailed, "model: output schema is not a JSON object")
	}

	modelID := req.Model
	if modelID == "" {
		modelID = a.cfg.Model
	}
	inputSum := sha256.Sum256([]byte(rendered))

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(modelID),
		MaxTokens: req.MaxOutputTokens,
		// The system parameter is the only instruction channel. It is marked
		// cacheable because it is byte-stable across compiles, which also
		// keeps the schema grammar cache warm.
		System: []anthropic.TextBlockParam{{
			Text:         req.SystemPolicy,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(dataMessage(req))),
		},
		OutputConfig: anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
	}
	if req.TenantHash != "" {
		params.Metadata = anthropic.MetadataParam{UserID: param.NewOpt(req.TenantHash)}
	}
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}

	requestedAt := a.clk.Now().UTC()
	var httpResp *http.Response
	msg, err := a.client.Messages.New(ctx, params, option.WithResponseInto(&httpResp))
	respondedAt := a.clk.Now().UTC()
	if err != nil {
		return Response{}, classifyAnthropicError(err)
	}

	// A body is only usable when the model finished normally. Truncation is
	// never repaired: a JSON document cut short is not a smaller valid
	// document, and guessing the remainder is how a compiler starts
	// inventing strategies.
	if msg.StopReason != anthropic.StopReasonEndTurn {
		return Response{}, BadResponse("stop reason " + string(msg.StopReason) + " is not a completed response")
	}
	body, err := structuredBody(msg)
	if err != nil {
		return Response{}, err
	}
	usage := Usage{
		InputTokens:              msg.Usage.InputTokens,
		OutputTokens:             msg.Usage.OutputTokens,
		CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
	}
	cost, err := a.cfg.Prices.Cost(string(msg.Model), usage)
	if err != nil {
		return Response{}, err
	}
	usage.Cost = cost
	outputSum := sha256.Sum256(body)

	return Response{
		Provider:    ProviderAnthropic,
		ModelID:     string(msg.Model),
		Structured:  body,
		StopReason:  string(msg.StopReason),
		Usage:       usage,
		RequestedAt: requestedAt,
		RespondedAt: respondedAt,
		InputHash:   inputSum[:],
		OutputHash:  outputSum[:],
		RequestID:   requestIDOf(httpResp),
		Latency:     respondedAt.Sub(requestedAt),
	}, nil
}

// dataMessage renders the two data sections as the user turn. The system
// policy is not repeated here: it travels in the system parameter, so the
// data block never sits alongside instructions in the same channel.
func dataMessage(req Request) string {
	data := Request{
		TemplateVersion: req.TemplateVersion,
		SystemPolicy:    "(supplied in the system parameter)",
		ToolResults:     req.ToolResults,
		Untrusted:       req.Untrusted,
		OutputSchema:    req.OutputSchema,
		MaxOutputTokens: req.MaxOutputTokens,
		Purpose:         req.Purpose,
	}
	rendered, err := data.Render()
	if err != nil {
		// Render only fails validation that Complete already ran; fall back
		// to the sections alone rather than dropping the data silently.
		var b []byte
		for _, s := range append(append([]Segment{}, req.ToolResults...), req.Untrusted...) {
			b = append(b, []byte(string(s.Kind)+" "+s.Label+"\n"+s.Content+"\n")...)
		}
		return string(b)
	}
	return rendered
}

// structuredBody extracts the single JSON text block of a constrained
// response.
func structuredBody(msg *anthropic.Message) (json.RawMessage, error) {
	for _, block := range msg.Content {
		if block.Type == "text" && block.Text != "" {
			if !json.Valid([]byte(block.Text)) {
				return nil, BadResponse("schema-constrained response is not valid JSON")
			}
			return json.RawMessage(block.Text), nil
		}
	}
	return nil, BadResponse("response carried no text block")
}

func requestIDOf(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	return resp.Header.Get("request-id")
}

// classifyAnthropicError maps a provider failure onto our typed errors.
// Everything unknown fails closed as unavailable: an unrecognized error is
// never treated as a usable answer.
func classifyAnthropicError(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Unavailable("request deadline exceeded", err)
	}
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return Unavailable("transport failure", err)
	}
	switch apiErr.StatusCode {
	case http.StatusTooManyRequests:
		// A spend-cap refusal carries no retry-after and will not succeed on
		// retry; a rate-limit refusal does. The distinction decides whether
		// the caller may try again at all.
		return RateLimited("provider refused: "+apiErr.Error(), retryAfterOf(apiErr))
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return errs.Wrap(err, errs.CodeValidationFailed, "model: provider rejected the request")
	case http.StatusUnauthorized, http.StatusForbidden:
		return errs.Wrap(err, errs.CodeInternal, "model: provider credentials rejected")
	case http.StatusRequestEntityTooLarge:
		return errs.Wrap(err, errs.CodeValidationFailed, "model: request too large")
	default:
		return Unavailable("provider error "+strconv.Itoa(apiErr.StatusCode), err)
	}
}

func retryAfterOf(apiErr *anthropic.Error) *time.Duration {
	if apiErr == nil || apiErr.Response == nil {
		return nil
	}
	raw := apiErr.Response.Header.Get("retry-after")
	if raw == "" {
		return nil
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 0 {
		return nil
	}
	d := time.Duration(secs) * time.Second
	return &d
}
