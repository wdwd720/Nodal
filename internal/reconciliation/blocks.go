package reconciliation

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// BlockReader answers the two questions the risk kernel and the intent
// boundary ask about reconciliation state.
type BlockReader interface {
	// BlocksNewRisk reports whether an account currently has an unresolved
	// record that blocks new risk, and returns those records so the caller
	// can explain the refusal.
	BlocksNewRisk(ctx context.Context, q db.Querier, accountID accounts.AccountID) (bool, []Record, error)
	// CountBlocking is the number the risk kernel takes as
	// AccountSnapshot.UnresolvedMaterialMismatches.
	CountBlocking(ctx context.Context, q db.Querier, accountID accounts.AccountID) (int, error)
	// OldestUnresolvedMaterial returns when the oldest unresolved material
	// mismatch was opened, across all accounts, or nil when there is none.
	OldestUnresolvedMaterial(ctx context.Context, q db.Querier) (*time.Time, error)
}

// Blocks is the read-only view of reconciliation state. It is a separate type
// from Engine so that packages which must not be able to write records (the
// risk kernel, the intent boundary, the API read path) can depend on the
// reader alone.
type Blocks struct {
	records *Repository
	policy  Policy
}

// NewBlocks returns the reader.
func NewBlocks(records *Repository, policy Policy) *Blocks {
	return &Blocks{records: records, policy: policy}
}

var _ BlockReader = (*Blocks)(nil)

// BlocksNewRisk implements BlockReader.
func (b *Blocks) BlocksNewRisk(ctx context.Context, q db.Querier, accountID accounts.AccountID) (bool, []Record, error) {
	if accountID.IsZero() {
		return false, nil, errs.New(errs.CodeValidationFailed, "reconciliation: account id is required")
	}
	recs, err := b.records.list(ctx, q, `SELECT `+recordColumns+` FROM reconciliation_records
		WHERE account_id = $1 AND blocks_new_risk
		  AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')
		ORDER BY opened_at, id`, accountID)
	if err != nil {
		return false, nil, err
	}
	return len(recs) > 0, recs, nil
}

// CountBlocking implements BlockReader.
func (b *Blocks) CountBlocking(ctx context.Context, q db.Querier, accountID accounts.AccountID) (int, error) {
	if accountID.IsZero() {
		return 0, errs.New(errs.CodeValidationFailed, "reconciliation: account id is required")
	}
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM reconciliation_records
		WHERE account_id = $1 AND blocks_new_risk
		  AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')`, accountID).Scan(&n); err != nil {
		return 0, dbErr("count blocking records", err)
	}
	return n, nil
}

// OldestUnresolvedMaterial implements BlockReader.
func (b *Blocks) OldestUnresolvedMaterial(ctx context.Context, q db.Querier) (*time.Time, error) {
	var at *time.Time
	if err := q.QueryRow(ctx, `SELECT min(opened_at) FROM reconciliation_records
		WHERE material AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED')`).Scan(&at); err != nil {
		return nil, dbErr("oldest unresolved material mismatch", err)
	}
	return at, nil
}

// GlobalHaltDue reports whether the oldest unresolved material mismatch has
// outlived Policy.MaxUnresolvedAge, which is the PART 158 condition for
// halting all new trading. A zero MaxUnresolvedAge disables the global rule;
// per-account blocking is unconditional and is not affected.
func (b *Blocks) GlobalHaltDue(ctx context.Context, q db.Querier, now time.Time) (bool, *time.Time, error) {
	if b.policy.MaxUnresolvedAge <= 0 {
		return false, nil, nil
	}
	at, err := b.OldestUnresolvedMaterial(ctx, q)
	if err != nil || at == nil {
		return false, at, err
	}
	return now.Sub(*at) > b.policy.MaxUnresolvedAge, at, nil
}

// RequireNoBlock returns RECONCILIATION_REQUIRED when an account has an
// unresolved record blocking new risk. It is the guard a new-risk boundary
// calls, and it is the only place in this package that refuses anything on
// behalf of another package.
//
// Note what it does *not* do: it never blocks reconciliation, settlement,
// observation, ledger posting or cancellation. Those classes stay open even
// while an account is blocked, exactly as PART 52 requires of kill switches.
func (b *Blocks) RequireNoBlock(ctx context.Context, q db.Querier, accountID accounts.AccountID) error {
	blocked, recs, err := b.BlocksNewRisk(ctx, q, accountID)
	if err != nil {
		return err
	}
	if !blocked {
		return nil
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID.String())
	}
	return errs.New(errs.CodeReconciliationRequired,
		"new risk is blocked while a reconciliation mismatch is unresolved").
		WithField("account_id", accountID.String()).
		WithField("reconciliation_record_ids", ids)
}

// Blocks returns the engine's reader.
func (e *Engine) Blocks() *Blocks { return NewBlocks(e.records, e.policy) }

// SweepEscalations escalates every unresolved material mismatch older than
// the policy budget and publishes the age of the oldest one. It changes at
// most one record per transaction, which is what the transition binding
// allows.
func (e *Engine) SweepEscalations(ctx context.Context, limit int) ([]Record, error) {
	recs, err := e.records.ListUnresolved(ctx, e.db, limit)
	if err != nil {
		return nil, err
	}
	now := e.clk.Now()
	blocks := e.Blocks()
	if at, err := blocks.OldestUnresolvedMaterial(ctx, e.db); err == nil && at != nil {
		e.metrics.oldestUnresolved(ctx, int64(now.Sub(*at)/time.Second))
	}
	out := []Record{}
	if e.policy.MaxUnresolvedAge <= 0 {
		return out, nil
	}
	for _, rec := range recs {
		if !rec.Material || rec.Status != StatusMismatch {
			continue
		}
		if now.Sub(rec.OpenedAt) <= e.policy.MaxUnresolvedAge {
			continue
		}
		var updated Record
		err := e.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			updated, err = e.records.Transition(ctx, tx, rec.ID, StatusEscalated, TransitionEvidence{
				Actor:  SystemActor(),
				Reason: "unresolved beyond the policy budget of " + e.policy.MaxUnresolvedAge.String(),
			})
			return err
		})
		if err != nil {
			return out, err
		}
		e.metrics.Raise(ctx, Alert{
			Name: "reconciliation_escalated", Severity: SEV1, RecordID: rec.ID.String(),
			Detail: "material mismatch unresolved beyond the policy budget",
		})
		out = append(out, updated)
	}
	return out, nil
}

// recordDifferenceQuantity reads the exact quantity out of a record's
// difference document. Quantities are stored as decimal strings, never as
// JSON numbers, so no float ever touches this path.
func recordDifferenceQuantity(rec Record) (money.Quantity, error) {
	var doc struct {
		Quantity string `json:"quantity"`
	}
	if len(rec.Difference) > 0 {
		if err := json.Unmarshal(rec.Difference, &doc); err != nil {
			return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "reconciliation: decode difference document")
		}
	}
	if doc.Quantity == "" {
		return money.Quantity{}, errs.New(errs.CodeValidationFailed,
			"reconciliation: the record has no quantified difference").
			WithField("record_id", rec.ID.String())
	}
	q, err := money.ParseQuantity(doc.Quantity)
	if err != nil {
		return money.Quantity{}, errs.Wrap(err, errs.CodeInternal, "reconciliation: decode difference quantity")
	}
	return q, nil
}
