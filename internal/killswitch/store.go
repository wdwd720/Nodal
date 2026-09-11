package killswitch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/security"
)

const switchColumns = `id, kind, scope_id, active, severity, reason,
	coalesce(activated_by_actor_id,''), activated_at, coalesce(released_by_actor_id,''), released_at,
	coalesce(release_approval_id::text,''), coalesce(release_reason,''), version, updated_at`

func scanSwitch(row pgx.Row) (Switch, error) {
	var s Switch
	err := row.Scan(&s.ID, &s.Kind, &s.ScopeID, &s.Active, &s.Severity, &s.Reason,
		&s.ActivatedBy, &s.ActivatedAt, &s.ReleasedBy, &s.ReleasedAt,
		&s.ReleaseApprovalID, &s.ReleaseReason, &s.Version, &s.UpdatedAt)
	return s, err
}

// loadSwitch returns the row for (kind, scope) or nil when none exists.
func loadSwitch(ctx context.Context, q db.Querier, kind Kind, scope string, forUpdate bool) (*Switch, error) {
	sql := `SELECT ` + switchColumns + ` FROM kill_switches WHERE kind = $1 AND scope_id = $2`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	s, err := scanSwitch(q.QueryRow(ctx, sql, string(kind), scope))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("killswitch: load %s/%s: %w", kind, scope, err)
	}
	return &s, nil
}

// listActive returns every active switch sorted by (kind, scope).
func listActive(ctx context.Context, q db.Querier) ([]Switch, error) {
	rows, err := q.Query(ctx, `SELECT `+switchColumns+` FROM kill_switches WHERE active ORDER BY kind, scope_id`)
	if err != nil {
		return nil, fmt.Errorf("killswitch: list active: %w", err)
	}
	defer rows.Close()
	out := []Switch{}
	for rows.Next() {
		s, err := scanSwitch(rows)
		if err != nil {
			return nil, fmt.Errorf("killswitch: scan active: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("killswitch: list active: %w", err)
	}
	return out, nil
}

func insertActive(ctx context.Context, tx pgx.Tx, s *Switch) error {
	err := tx.QueryRow(ctx, `INSERT INTO kill_switches
		(id, kind, scope_id, active, severity, reason, activated_by_actor_id, activated_at, version, updated_at)
		VALUES ($1, $2, $3, true, $4, $5, $6, $7, 1, $7)
		RETURNING version, updated_at`,
		s.ID, string(s.Kind), s.ScopeID, string(s.Severity), s.Reason, s.ActivatedBy, s.ActivatedAt).
		Scan(&s.Version, &s.UpdatedAt)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return errs.New(errs.CodeConflict, "kill switch was modified concurrently").WithField("switch", string(s.Kind))
		}
		return fmt.Errorf("killswitch: insert %s/%s: %w", s.Kind, s.ScopeID, err)
	}
	return nil
}

// reloadSwitch refreshes s from the row the transition trigger wrote.
//
// The version and the timestamps are produced by the database now, so the
// in-memory Switch a caller returns has to come back from it rather than from
// what the caller intended -- which is the same reason 00747 stopped assigning
// the stamps from a local clock.
func reloadSwitch(ctx context.Context, tx pgx.Tx, s *Switch) error {
	got, err := scanSwitch(tx.QueryRow(ctx, `SELECT `+switchColumns+` FROM kill_switches WHERE id = $1`, s.ID))
	if err != nil {
		return fmt.Errorf("killswitch: reload %s/%s: %w", s.Kind, s.ScopeID, err)
	}
	*s = got
	return nil
}

// The statement 00753 replaced, kept here because the shape of what moved is
// worth being able to read:
//
//	UPDATE kill_switches SET active = $2, severity = $3, reason = $4,
//	    activated_by_actor_id = ..., released_by_actor_id = ...,
//	    version = version + 1
//	  WHERE id = $1 AND version = $11
//	  RETURNING version, updated_at
//
// Every part of it is now in cp_kill_switch_apply_transition, driven by the
// transition row: the column writes, the optimistic-concurrency check and the
// version bump. Inserting the transition IS the change, and the check applies
// to every writer rather than to this one statement.

// Transition is a kill_switch_transitions row.
type Transition struct {
	ID         TransitionID
	SwitchID   SwitchID
	Kind       Kind
	ScopeID    string
	ToActive   bool
	ActorType  security.ActorType
	ActorID    string
	Reason     string
	ApprovalID string
	OccurredAt time.Time

	// What this transition makes true of the switch, and the version it
	// expected to find. Since 00753 the row IS the change: the trigger performs
	// the compare-and-swap and the version bump that saveSwitch used to carry.
	//
	// FromVersion nil means "do not apply" -- the birth row written straight
	// after insertActive, where the switch is already in its destination state.
	ToSeverity    Severity
	FromVersion   *int64
	ReleaseReason string
}

// nilIfEmpty maps "" to a SQL NULL, so a transition that does not restate a
// value leaves the switch's alone rather than blanking it.
func nilIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func insertTransition(ctx context.Context, tx pgx.Tx, t Transition) error {
	var approvalID *string
	if t.ApprovalID != "" {
		v := t.ApprovalID
		approvalID = &v
	}
	_, err := tx.Exec(ctx, `INSERT INTO kill_switch_transitions
		(id, switch_id, kind, scope_id, to_active, actor_type, actor_id, reason, approval_id, occurred_at,
		 to_severity, from_version, release_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid, $10, $11, $12, $13)`,
		t.ID, t.SwitchID, string(t.Kind), t.ScopeID, t.ToActive, string(t.ActorType), t.ActorID, t.Reason, approvalID, t.OccurredAt,
		nilIfEmpty(string(t.ToSeverity)), t.FromVersion, nilIfEmpty(t.ReleaseReason))
	if err != nil {
		return fmt.Errorf("killswitch: insert transition: %w", err)
	}
	return nil
}

// Transitions lists the recorded transitions of a switch, oldest first.
func Transitions(ctx context.Context, q db.Querier, switchID SwitchID) ([]Transition, error) {
	rows, err := q.Query(ctx, `SELECT id, switch_id, kind, scope_id, to_active, actor_type, actor_id, reason, coalesce(approval_id::text,''), occurred_at
		FROM kill_switch_transitions WHERE switch_id = $1 ORDER BY occurred_at, id`, switchID)
	if err != nil {
		return nil, fmt.Errorf("killswitch: list transitions: %w", err)
	}
	defer rows.Close()
	var out []Transition
	for rows.Next() {
		var t Transition
		if err := rows.Scan(&t.ID, &t.SwitchID, &t.Kind, &t.ScopeID, &t.ToActive, &t.ActorType, &t.ActorID, &t.Reason, &t.ApprovalID, &t.OccurredAt); err != nil {
			return nil, fmt.Errorf("killswitch: scan transition: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("killswitch: list transitions: %w", err)
	}
	return out, nil
}

// Get returns the switch row for (kind, scope), NOT_FOUND when none exists.
// It needs no principal: switch state is operator-visible information.
func Get(ctx context.Context, q db.Querier, kind Kind, scope string) (Switch, error) {
	scope, err := kind.NormalizeScope(scope)
	if err != nil {
		return Switch{}, err
	}
	s, err := loadSwitch(ctx, q, kind, scope, false)
	if err != nil {
		return Switch{}, err
	}
	if s == nil {
		return Switch{}, errs.New(errs.CodeNotFound, "kill switch not found").WithField("switch", string(kind)).WithField("scope", scope)
	}
	return *s, nil
}
