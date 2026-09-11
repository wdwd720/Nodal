package agents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/strategy"
)

// AcceptAction is the audit action recorded on the owner's account stream when
// they accept a compiled strategy version.
const AcceptAction = "strategy.version.accepted"

// AcceptRequest is POST /v1/strategies/{strategyId}/versions/{version}/accept.
//
// Three identifiers, and each answers a different question.
//
//   - StrategyID and Version name the document. They are a complete key —
//     strategy_versions is UNIQUE (strategy_id, version) — and they are the
//     two things a person reading the review screen can see, so a request
//     built from what is on the screen names what is on the screen.
//   - IRHashHex is the digest of the document the caller READ. It is required,
//     it is compared, and a mismatch is refused. Without it "accept version 2"
//     would mean "accept whatever version 2 is when this arrives", and the one
//     race that matters here is the one where a second compile lands between
//     the review and the press: the caller would be approving a document
//     nobody looked at. Versions are immutable, so the hash cannot drift under
//     a caller who read THIS version; what it catches is a caller who read a
//     different one.
type AcceptRequest struct {
	StrategyID string
	Version    int
	// IRHashHex is the lowercase hex semantic hash shown on the review screen.
	IRHashHex     string
	CorrelationID string
	RequestID     string
}

// Accept records that a person read a compiled strategy version and approved it.
//
// # Why this exists at all
//
// agents.Service.Create refuses an agent whose strategy version is not ACCEPTED
// with accepted_by_user_id and accepted_at set (F-187, D-105), migration 00500
// pairs the status and the columns in a CHECK, and the compiler is forbidden
// from returning an accepted version because "a backend that could return it
// would be approving on the user's behalf, which is exactly what goal SS18's
// review step exists to stop". Every one of those was in place. Nothing could
// write the row: there was no route, no service method and no SQL anywhere that
// set the status to ACCEPTED, so on every deployment of this build an agent was
// unreachable even with a working compiler (F-255).
//
// # What acceptance is, and is not
//
// It is a record of a person's act. It is not a state a compiler can reach, not
// a side effect of compiling, and not something an operator can do on somebody's
// behalf: RequireAccountOwner is the check, and account:read_any does not
// substitute for it, because approving a strategy is not a read.
//
// It grants nothing by itself. An accepted version is a version an agent may be
// created FROM; creating the agent is the next separate act, and enabling it is
// the one after that.
//
// # Idempotence
//
// Accepting a version that is already ACCEPTED, with the same hash, is a replay
// and answers the version. It is not a conflict: the caller asked for a state
// the row is already in, by the same person, and refusing would make a lost
// response look like an error. Accepting a version somebody else accepted
// cannot happen — only the owner reaches this — and accepting one in any status
// other than COMPILED or ACCEPTED is INVALID_STATE_TRANSITION.
func (s *StrategyService) Accept(ctx context.Context, req AcceptRequest) (StrategyVersion, error) {
	st, err := s.Get(ctx, req.StrategyID)
	if err != nil {
		return StrategyVersion{}, err
	}
	p, err := s.ownerActor(ctx, st.AccountID)
	if err != nil {
		return StrategyVersion{}, err
	}
	if req.Version < 1 {
		return StrategyVersion{}, errs.New(errs.CodeValidationFailed, "agents: version must be >= 1").
			WithField("version", "must be >= 1")
	}
	wanted := strings.ToLower(strings.TrimSpace(req.IRHashHex))
	if wanted == "" {
		return StrategyVersion{}, errs.New(errs.CodeValidationFailed,
			"agents: accepting a strategy means naming the document you read; send its ir_hash").
			WithField("ir_hash", "required: the semantic hash of the version you reviewed")
	}

	now := s.clk.Now().UTC()
	var out StrategyVersion
	err = s.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		v, ferr := loadVersionForUpdate(ctx, tx, req.StrategyID, req.Version)
		if ferr != nil {
			return ferr
		}
		if v.IRHashHex != wanted {
			// Deliberately CONFLICT and not VALIDATION_FAILED: the request is
			// well formed and the caller did nothing wrong. What they read is
			// not what is here.
			return errs.New(errs.CodeConflict,
				"agents: the strategy you reviewed is not the one at this version; read the current compiled strategy and accept that").
				WithFields(map[string]any{
					"ir_hash":            "does not match this version",
					"version":            req.Version,
					"expected_ir_prefix": prefix(v.IRHashHex),
				})
		}
		switch v.Status {
		case strategy.StatusAccepted:
			// A replay. Nothing is written, and the row already carries the
			// person and the instant, so the answer is the same as the first
			// time.
			out = v
			return nil
		case strategy.StatusCompiled:
		default:
			return errs.Newf(errs.CodeInvalidStateTransition,
				"agents: this strategy version is %s and cannot be accepted", strings.ToLower(v.Status)).
				WithField("status", v.Status)
		}

		tag, uerr := tx.Exec(ctx, `
			UPDATE strategy_versions
			   SET status = $2, accepted_by_user_id = $3, accepted_at = $4
			 WHERE id = $1::uuid AND status = $5`,
			v.ID, strategy.StatusAccepted, p.SubjectID, now, strategy.StatusCompiled)
		if uerr != nil {
			return errs.Wrap(uerr, errs.CodeInternal, "agents: accept the strategy version")
		}
		if tag.RowsAffected() != 1 {
			// Somebody moved it between the read and the write. The row is
			// locked FOR UPDATE above, so this is defence rather than a
			// reachable path, and it fails closed.
			return errs.New(errs.CodeConflict, "agents: this strategy version changed while it was being accepted")
		}
		v.Status = strategy.StatusAccepted
		v.AcceptedByUserID = p.SubjectID
		accepted := now
		v.AcceptedAt = &accepted
		out = v

		if s.audit == nil {
			return errs.New(errs.CodeInternal,
				"agents: this deployment has no audit writer, and an acceptance that leaves no trail is not an acceptance")
		}
		payload, merr := json.Marshal(map[string]any{
			"strategy_id":         req.StrategyID,
			"version":             v.Version,
			"strategy_version_id": v.ID,
			"ir_hash":             v.IRHashHex,
			"effect_set":          v.EffectSet,
			"sandbox":             v.Sandbox,
		})
		if merr != nil {
			return errs.Wrap(merr, errs.CodeInternal, "agents: encode the acceptance audit payload")
		}
		_, aerr := s.audit.Append(ctx, tx, audit.Event{
			Stream: audit.AccountStream(st.AccountID), ActorType: string(p.ActorType), ActorID: p.SubjectID,
			Action: AcceptAction, ResourceType: "strategy_version", ResourceID: v.ID,
			RequestID: req.RequestID, CorrelationID: req.CorrelationID,
			Reason:     "the owner read the compiled strategy and accepted it",
			Payload:    payload,
			OccurredAt: now,
		})
		if aerr != nil {
			return aerr
		}
		return nil
	})
	if err != nil {
		return StrategyVersion{}, err
	}
	return out, nil
}

// loadVersionForUpdate reads one version of one strategy and locks it.
//
// A version that does not exist, and a version number that belongs to another
// strategy, answer identically: the caller has already been shown to own the
// strategy, so this is a plain not-found rather than the cross-tenant refusal
// Get makes.
func loadVersionForUpdate(ctx context.Context, tx pgx.Tx, strategyID string, version int) (StrategyVersion, error) {
	var (
		v           StrategyVersion
		effects     []string
		irDoc       []byte
		acceptedBy  *string
		acceptedAt  *time.Time
		builtAt     time.Time
		createdAt   time.Time
		environment string
	)
	err := tx.QueryRow(ctx, `
		SELECT id::text, version, status, encode(ir_hash, 'hex'), effect_set, human_readable, ir,
		       sandbox, environment, accepted_by_user_id::text, accepted_at, built_at, created_at
		  FROM strategy_versions
		 WHERE strategy_id = $1::uuid AND version = $2
		 FOR UPDATE`, strategyID, version).
		Scan(&v.ID, &v.Version, &v.Status, &v.IRHashHex, &effects, &v.HumanReadable, &irDoc,
			&v.Sandbox, &environment, &acceptedBy, &acceptedAt, &builtAt, &createdAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return StrategyVersion{}, errs.New(errs.CodeNotFound, "agents: this strategy has no such version").
			WithField("version", version)
	case err != nil:
		return StrategyVersion{}, errs.Wrap(err, errs.CodeInternal, "agents: read the strategy version")
	}
	v.EffectSet = effects
	v.IR = json.RawMessage(irDoc)
	v.Environment = environment
	v.AcceptedAt = acceptedAt
	if acceptedBy != nil {
		v.AcceptedByUserID = *acceptedBy
	}
	v.BuiltAt = builtAt
	v.CreatedAt = createdAt
	return v, nil
}

// sandboxVersions reports whether the versions this deployment compiles are
// rehearsals. It is a property of the compiler that is wired, not of the
// request, which is why it is read here rather than taken from a backend's
// answer.
func (s *StrategyService) sandboxVersions() bool {
	return s.structured != nil && s.structured.SandboxCompiler()
}

// environmentOfNewVersions is stamped on every version this deployment writes,
// so migration 00812's CHECK can refuse a sandbox row in PROD. It is empty on a
// deployment that did not say which environment it is, which the CHECK reads as
// "not a sandbox row" and refuses to pair with sandbox = true.
func (s *StrategyService) environmentOfNewVersions() string { return s.env }

func stringsOrEmpty(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func prefix(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
