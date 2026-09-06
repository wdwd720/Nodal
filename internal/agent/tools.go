package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// ToolStatus mirrors the tools.status CHECK.
type ToolStatus string

// Tool statuses. Only ACTIVE tools may be invoked: DEGRADED and DISABLED are
// refused by the broker as a first-class outcome, never skipped silently.
const (
	ToolActive   ToolStatus = "ACTIVE"
	ToolDegraded ToolStatus = "DEGRADED"
	ToolDisabled ToolStatus = "DISABLED"
)

var allToolStatuses = []ToolStatus{ToolActive, ToolDegraded, ToolDisabled}

// ToolStatuses returns every declared tool status.
func ToolStatuses() []ToolStatus { return append([]ToolStatus(nil), allToolStatuses...) }

// Valid reports whether s is declared.
func (s ToolStatus) Valid() bool {
	for _, v := range allToolStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// String renders the status.
func (s ToolStatus) String() string { return string(s) }

// Invocable reports whether a tool in this status may be dialed.
func (s ToolStatus) Invocable() bool { return s == ToolActive }

// toolEffects is the closed set of effects a tool may declare. It mirrors the
// tools.effect CHECK exactly. Every one of them is a read or a model call:
// there is deliberately no write effect and adding one is a security
// decision, not an implementation detail (PART 9, PART 66).
var toolEffects = []ir.Effect{
	ir.EffectReadMarketData,
	ir.EffectReadOnchainData,
	ir.EffectReadApprovedSocialData,
	ir.EffectReadWalletIntelligence,
	ir.EffectCallModel,
}

// ToolEffects returns the closed set of effects a tool may declare.
func ToolEffects() []ir.Effect { return append([]ir.Effect(nil), toolEffects...) }

// IsToolEffect reports whether e may appear on a tools row. It is false for
// COMMIT_PREDICTION and CREATE_TRADE_INTENT (those are runtime actions, not
// tools) and for every forbidden effect.
func IsToolEffect(e ir.Effect) bool {
	for _, v := range toolEffects {
		if v == e {
			return true
		}
	}
	return false
}

// Tool is one tools row: a registered, versioned adapter with a declared
// effect, an egress allowlist, per-call limits and a status. Agents never see
// a credential or a URL; the adapter holds those and the broker holds the
// adapter.
type Tool struct {
	ID                  ToolID
	Code                string
	Version             int
	Effect              ir.Effect
	Provider            string
	DataSourceID        string
	Description         string
	EgressHosts         []string
	InputSchemaVersion  int
	OutputSchemaVersion int
	OutputSchema        json.RawMessage
	CostPerCall         money.USD
	MaxCallsPerMinute   int
	MaxCallsPerRun      int
	Timeout             time.Duration
	PipelineLatency     time.Duration
	Status              ToolStatus
	Environments        []string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Key is the (code, version) identity strategy dependencies reference.
func (t Tool) Key() string { return ToolKey(t.Code, t.Version) }

// ToolKey renders the (code, version) identity of a tool.
func ToolKey(code string, version int) string {
	return code + "@" + itoa(version)
}

// itoa avoids pulling strconv into the hot path signature; version numbers
// are small non-negative integers.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// Validate reports the structural reasons t is not a usable tool row.
func (t Tool) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if t.ID.IsZero() {
		fail("id", "required")
	}
	if t.Code == "" {
		fail("code", "required")
	}
	if t.Version < 1 {
		fail("version", "must be >= 1")
	}
	if !IsToolEffect(t.Effect) {
		fail("effect", "must be one of the five declared read/model effects")
	}
	if t.Provider == "" {
		fail("provider", "required")
	}
	if !t.Status.Valid() {
		fail("status", "must be ACTIVE, DEGRADED or DISABLED")
	}
	if t.MaxCallsPerMinute < 1 {
		fail("max_calls_per_minute", "must be > 0")
	}
	if t.MaxCallsPerRun < 1 {
		fail("max_calls_per_run", "must be > 0")
	}
	if t.Timeout <= 0 {
		fail("timeout_ms", "must be > 0")
	}
	if t.PipelineLatency < 0 {
		fail("pipeline_latency_ms", "must be >= 0")
	}
	if t.CostPerCall.IsNegative() {
		fail("cost_per_call_usd_minor", "must be >= 0")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "tool: invalid").WithFields(fields)
}

// AllowsHost reports whether host is on the tool's egress allowlist. An empty
// allowlist permits nothing: a tool that has not declared where it may dial
// cannot dial (fail closed, PART 66 §4).
func (t Tool) AllowsHost(host string) bool {
	if host == "" {
		return false
	}
	for _, h := range t.EgressHosts {
		if h == host {
			return true
		}
	}
	return false
}

// AllowsEnvironment reports whether the tool is registered for env. An empty
// list means "every environment", matching the tools.environments default.
func (t Tool) AllowsEnvironment(env string) bool {
	if len(t.Environments) == 0 {
		return true
	}
	for _, e := range t.Environments {
		if e == env {
			return true
		}
	}
	return false
}

// ToolRegistry resolves (code, version) to the registered tool. It is a read
// interface: nothing in this package registers or mutates a tool, because a
// tool registration is an operator action.
type ToolRegistry interface {
	Lookup(ctx context.Context, q db.Querier, code string, version int) (Tool, error)
}

const selectToolSQL = `
SELECT id, code, version, effect, provider, coalesce(data_source_id::text, ''), description,
       egress_hosts, input_schema_version, output_schema_version, output_schema,
       cost_per_call_usd_minor, max_calls_per_minute, max_calls_per_run, timeout_ms,
       pipeline_latency_ms, status, environments, created_at, updated_at
  FROM tools WHERE code = $1 AND version = $2`

// PGToolRegistry reads the tools table. It holds no connection.
type PGToolRegistry struct{}

var _ ToolRegistry = PGToolRegistry{}

// NewToolRegistry returns the PostgreSQL tool registry.
func NewToolRegistry() PGToolRegistry { return PGToolRegistry{} }

// Lookup returns the registered tool, or NOT_FOUND.
func (PGToolRegistry) Lookup(ctx context.Context, q db.Querier, code string, version int) (Tool, error) {
	var (
		t                                 Tool
		costMinor                         int64
		timeoutMS, latencyMS              int
		effect, status                    string
		egress, environments              []string
		schema                            []byte
		inSchemaVersion, outSchemaVersion int
	)
	err := q.QueryRow(ctx, selectToolSQL, code, version).Scan(
		&t.ID, &t.Code, &t.Version, &effect, &t.Provider, &t.DataSourceID, &t.Description,
		&egress, &inSchemaVersion, &outSchemaVersion, &schema,
		&costMinor, &t.MaxCallsPerMinute, &t.MaxCallsPerRun, &timeoutMS,
		&latencyMS, &status, &environments, &t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		if isNoRows(err) {
			return Tool{}, errs.Newf(errs.CodeNotFound, "agent: tool %s is not registered", ToolKey(code, version)).
				WithField("tool", ToolKey(code, version))
		}
		return Tool{}, errs.Wrap(err, errs.CodeInternal, "agent: load tool")
	}
	t.Effect = ir.Effect(effect)
	t.Status = ToolStatus(status)
	t.EgressHosts = egress
	t.Environments = environments
	t.OutputSchema = schema
	t.InputSchemaVersion = inSchemaVersion
	t.OutputSchemaVersion = outSchemaVersion
	t.CostPerCall = money.USDFromMinor(costMinor)
	t.Timeout = time.Duration(timeoutMS) * time.Millisecond
	t.PipelineLatency = time.Duration(latencyMS) * time.Millisecond
	return t, nil
}
