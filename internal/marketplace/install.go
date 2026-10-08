package marketplace

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
)

// InstallRequest installs a version for an account.
type InstallRequest struct {
	VersionID VersionID
	AccountID accounts.AccountID
	// Mode must be PAPER. It is a field so that a request for anything else
	// is refused explicitly -- here, and by the database's CHECK if this
	// code were ever bypassed -- rather than silently rewritten.
	Mode  gates.Mode
	Actor econ.Actor
}

// InstallVersion installs an APPROVED, CLEARED version for an account in
// PAPER mode. Installing an already-installed version returns the existing
// install.
func (s *Service) InstallVersion(ctx context.Context, tx pgx.Tx, req InstallRequest) (Install, bool, error) {
	if err := requireActor(req.Actor, CreatorActorTypes(), "install a strategy"); err != nil {
		return Install{}, false, err
	}
	if req.Mode != gates.ModePaper {
		return Install{}, false, errs.Newf(errs.CodeForbidden,
			"marketplace: installs are PAPER only; %q would allocate value the marketplace may never allocate", req.Mode).
			WithField("mode", string(req.Mode))
	}
	if req.AccountID.IsZero() {
		return Install{}, false, errs.New(errs.CodeValidationFailed, "marketplace: an account is required")
	}
	if err := s.authorize(ctx, tx, gates.ModePaper); err != nil {
		return Install{}, false, err
	}
	v, err := s.Version(ctx, tx, req.VersionID)
	if err != nil {
		return Install{}, false, err
	}
	if !v.Followable() {
		return Install{}, false, errs.Newf(errs.CodeForbidden, "marketplace: a %s/%s version cannot be installed", v.Status, v.Moderation).
			WithField("version_id", v.ID.String())
	}
	existing, err := scanInstall(tx.QueryRow(ctx, `SELECT `+installColumns+` FROM marketplace_installs
		WHERE account_id = $1 AND version_id = $2 AND status = 'ACTIVE'`, req.AccountID, req.VersionID))
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Install{}, false, dbErr("find install", err)
	}
	in, err := scanInstall(tx.QueryRow(ctx, `INSERT INTO marketplace_installs (id, account_id, version_id, mode, status,
		installed_by_actor_type, installed_by_actor_id) VALUES ($1,$2,$3,$4,'ACTIVE',$5,$6) RETURNING `+installColumns,
		id.New[installKind](), req.AccountID, req.VersionID, req.Mode, req.Actor.Type, req.Actor.ID))
	if err != nil {
		return Install{}, false, dbErr("create install", err)
	}
	if err := s.auditEvent(ctx, tx, audit.AccountStream(req.AccountID.String()), req.Actor, "marketplace.install.created",
		"marketplace_install", in.ID.String(), "installed in PAPER mode",
		map[string]string{"version_id": v.ID.String(), "strategy_ref": v.StrategyRef(), "mode": string(in.Mode)}); err != nil {
		return Install{}, false, err
	}
	return in, false, nil
}

// RemoveInstall removes an ACTIVE install.
func (s *Service) RemoveInstall(ctx context.Context, tx pgx.Tx, iid InstallID, by econ.Actor, reason string) (Install, error) {
	if err := requireActor(by, TransitionActorTypes(), "remove an install"); err != nil {
		return Install{}, err
	}
	in, err := s.loadInstall(ctx, tx, iid, true)
	if err != nil {
		return Install{}, err
	}
	if in.Status != InstallActive {
		return Install{}, errs.Newf(errs.CodeInvalidStateTransition, "marketplace: install is %s", in.Status)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO marketplace_install_transitions (id, install_id, from_status, to_status, actor_type, actor_id, reason, occurred_at)
		VALUES ($1,$2,'ACTIVE','REMOVED',$3,$4,$5,$6)`, id.New[id.Any](), iid, by.Type, by.ID, reasonOr(reason, "removed"), s.now()); err != nil {
		return Install{}, dbErr("record install transition", err)
	}
	out, err := scanInstall(tx.QueryRow(ctx, `UPDATE marketplace_installs SET status = 'REMOVED' WHERE id = $1 RETURNING `+installColumns, iid))
	if err != nil {
		return Install{}, dbErr("remove install", err)
	}
	if err := s.auditEvent(ctx, tx, audit.AccountStream(in.AccountID.String()), by, "marketplace.install.removed", "marketplace_install",
		iid.String(), reasonOr(reason, "removed"), map[string]string{"version_id": in.VersionID.String()}); err != nil {
		return Install{}, err
	}
	return out, nil
}
