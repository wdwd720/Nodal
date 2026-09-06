package agent

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// Invocation is one tool_invocations row: the immutable provenance of a
// single broker call, successful or refused (PART 66).
type Invocation struct {
	ID                  InvocationID
	ToolID              ToolID
	ToolCode            string
	ToolVersion         int
	Effect              ir.Effect
	AgentID             AgentID
	RunID               RunID
	StrategyVersionID   string
	CompileAttemptID    string
	DependencyName      string
	Mode                Mode
	RequestHash         []byte
	RequestRef          string
	OutputHash          []byte
	OutputRef           string
	Source              string
	SourceEventAt       *time.Time
	ProviderPublishedAt *time.Time
	ReceivedAt          time.Time
	DecisionAvailableAt *time.Time
	CostUSD             money.USD
	Latency             time.Duration
	Success             bool
	ErrorCode           string
	RateLimited         bool
	BudgetRefused       bool
	CorrelationID       string
	CreatedAt           time.Time
}

// Refused reports whether the call never reached a provider.
func (i Invocation) Refused() bool { return !i.Success && (i.RateLimited || i.BudgetRefused) }

// Validate mirrors the tool_invocations CHECKs.
func (i Invocation) Validate() error {
	fields := map[string]any{}
	fail := func(k, msg string) {
		if _, dup := fields[k]; !dup {
			fields[k] = msg
		}
	}
	if i.ID.IsZero() {
		fail("id", "required")
	}
	if i.ToolID.IsZero() {
		fail("tool_id", "required")
	}
	if i.RunID.IsZero() && i.CompileAttemptID == "" {
		fail("agent_run_id", "a run or a compile attempt must own the invocation")
	}
	if !i.Mode.Valid() {
		fail("mode", "must be one of the six modes")
	}
	if len(i.RequestHash) == 0 {
		fail("request_hash", "required")
	}
	if i.Success && len(i.OutputHash) == 0 {
		fail("output_hash", "required for a successful call")
	}
	if i.Source == "" {
		fail("source", "required")
	}
	if i.ReceivedAt.IsZero() {
		fail("received_at", "required")
	}
	if i.Latency < 0 {
		fail("latency_ms", "must be >= 0")
	}
	if i.CostUSD.IsNegative() {
		fail("cost_usd_minor", "must be >= 0")
	}
	if len(fields) == 0 {
		return nil
	}
	return errs.New(errs.CodeValidationFailed, "agent: invalid tool invocation").WithFields(fields)
}

const insertInvocationSQL = `
INSERT INTO tool_invocations (
    id, tool_id, tool_code, tool_version, effect, agent_id, agent_run_id, strategy_version_id,
    compile_attempt_id, dependency_name, mode, request_hash, request_ref, output_hash, output_ref,
    source, source_event_at, provider_published_at, received_at, decision_available_at,
    cost_usd_minor, latency_ms, success, error_code, rate_limited, budget_refused, correlation_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13, $14, $15,
    $16, $17, $18, $19, $20,
    $21, $22, $23, $24, $25, $26, $27
)`

// PGInvocationRecorder writes tool_invocations rows. The table is append-only
// (forbid_mutation trigger), so a recorded call can never be edited away.
type PGInvocationRecorder struct{}

var _ InvocationRecorder = PGInvocationRecorder{}

// NewInvocationRecorder returns the PostgreSQL recorder.
func NewInvocationRecorder() PGInvocationRecorder { return PGInvocationRecorder{} }

// Record inserts the provenance row inside the caller's transaction.
func (PGInvocationRecorder) Record(ctx context.Context, tx pgx.Tx, inv Invocation) error {
	if tx == nil {
		return errs.New(errs.CodeInternal, "agent: recording an invocation requires a transaction")
	}
	if err := inv.Validate(); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, insertInvocationSQL,
		inv.ID, inv.ToolID, inv.ToolCode, inv.ToolVersion, string(inv.Effect),
		nullUUID(inv.AgentID.String()), nullUUID(inv.RunID.String()), nullText(inv.StrategyVersionID),
		nullText(inv.CompileAttemptID), nullText(inv.DependencyName), string(inv.Mode),
		inv.RequestHash, nullText(inv.RequestRef), nilBytes(inv.OutputHash), nullText(inv.OutputRef),
		inv.Source, inv.SourceEventAt, inv.ProviderPublishedAt, inv.ReceivedAt, inv.DecisionAvailableAt,
		inv.CostUSD.Minor(), inv.Latency.Milliseconds(), inv.Success, nullText(inv.ErrorCode),
		inv.RateLimited, inv.BudgetRefused, nullText(inv.CorrelationID),
	)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "agent: record tool invocation")
	}
	return nil
}

// nullText maps "" to NULL so optional text columns stay NULL rather than
// holding an empty string.
func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullUUID maps a zero id's string form to NULL.
func nullUUID(s string) *string {
	if s == "" || s == "00000000-0000-0000-0000-000000000000" {
		return nil
	}
	return &s
}

// nilBytes maps an empty slice to NULL.
func nilBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// sortedKeys returns the sorted keys of a string map.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
