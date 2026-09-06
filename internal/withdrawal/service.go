package withdrawal

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/security"
)

// StepUpMaxAge is how recent a strong authentication must be to request a
// withdrawal.
const StepUpMaxAge = 15 * time.Minute

// TxRunner opens the authorizing transaction. *db.DB satisfies it.
type TxRunner interface {
	InTx(ctx context.Context, opts db.TxOptions, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// GateChecker is the capability-gate guard. *gates.Checker satisfies it.
type GateChecker interface {
	RequireActive(ctx context.Context, q db.Querier, cap gates.Capability) error
}

// KillSwitchChecker is the kill-switch guard. *killswitch.Checker
// satisfies it.
type KillSwitchChecker interface {
	Check(ctx context.Context, q db.Querier, a killswitch.Action) error
}

// Accounts reads account status. *accounts.Repository satisfies it.
type Accounts interface {
	Get(ctx context.Context, q db.Querier, accountID accounts.AccountID) (accounts.Account, error)
}

// Deps are the Service collaborators; every field is required.
type Deps struct {
	DB           TxRunner
	Clock        clock.Clock
	Store        Store
	Accounts     Accounts
	Gates        GateChecker
	KillSwitches KillSwitchChecker
	Velocity     VelocityPolicy
}

// Service implements the withdrawal boundary.
type Service struct {
	d Deps
}

// NewService validates the dependencies.
func NewService(d Deps) (*Service, error) {
	if d.DB == nil || d.Clock == nil || d.Store == nil || d.Accounts == nil || d.Gates == nil || d.KillSwitches == nil {
		return nil, errs.New(errs.CodeValidationFailed, "withdrawal: every dependency is required")
	}
	if err := d.Velocity.Validate(); err != nil {
		return nil, err
	}
	return &Service{d: d}, nil
}

func authError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, security.ErrUnauthenticated):
		return errs.Wrap(err, errs.CodeUnauthenticated, "authentication required")
	case errors.Is(err, security.ErrStepUpRequired):
		return errs.Wrap(err, errs.CodeStepUpRequired, "step-up authentication required")
	default:
		return errs.Wrap(err, errs.CodeForbidden, "forbidden")
	}
}

// Request records a withdrawal request (PART 94). Checks run in this
// order and the first failure wins: the actor is human (AGENT is refused
// before anything else, even before authentication), withdrawal:create,
// account ownership, step-up within StepUpMaxAge, request validation
// (destination type and address), then inside the authorizing
// transaction: the WITHDRAWALS gate (CAPABILITY_NOT_APPROVED while
// DISABLED), kill switches (WITHDRAW class), account status, the velocity
// policy, and finally the REQUESTED row with its audit event.
func (s *Service) Request(ctx context.Context, actor HumanActor, req Request) (Withdrawal, error) {
	if !actor.Valid() || actor.principal.ActorType == security.ActorAgent {
		if actor.principal.ActorType == security.ActorAgent {
			return Withdrawal{}, errs.New(errs.CodeForbidden, "withdrawal: agents can never request withdrawals")
		}
		return Withdrawal{}, errs.New(errs.CodeUnauthenticated, "withdrawal: a human actor is required")
	}
	p := actor.Principal()
	ctx = security.WithPrincipal(ctx, p)
	if err := security.RequireAt(ctx, security.PermWithdrawalCreate, s.d.Clock.Now); err != nil {
		return Withdrawal{}, authError(err)
	}
	if err := security.RequireAccount(ctx, req.AccountID.String()); err != nil {
		return Withdrawal{}, authError(err)
	}
	if err := security.RequireStepUp(ctx, StepUpMaxAge, s.d.Clock.Now); err != nil {
		return Withdrawal{}, authError(err)
	}
	if err := req.Validate(); err != nil {
		return Withdrawal{}, err
	}
	userID, err := accounts.ParseUserID(p.SubjectID)
	if err != nil {
		return Withdrawal{}, errs.New(errs.CodeForbidden, "withdrawal: requester must be a user").WithField("subject", p.SubjectID)
	}
	ev := TransitionEvidence{
		ActorType: p.ActorType, ActorID: p.SubjectID, Reason: "customer requested withdrawal",
		CorrelationID: req.CorrelationID, RequestID: req.RequestID,
	}
	var out Withdrawal
	err = s.d.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.d.Gates.RequireActive(ctx, tx, gates.Withdrawals); err != nil {
			return err
		}
		if err := s.d.KillSwitches.Check(ctx, tx, killswitch.Action{Class: killswitch.Withdraw, AccountID: req.AccountID.String()}); err != nil {
			return err
		}
		acct, err := s.d.Accounts.Get(ctx, tx, req.AccountID)
		if err != nil {
			return err
		}
		if acct.Status != accounts.StatusActive {
			code := errs.CodeForbidden
			if acct.Status == accounts.StatusFrozen {
				code = errs.CodeAccountFrozen
			}
			return errs.Newf(code, "account status %s does not allow withdrawals", acct.Status).WithField("account_status", string(acct.Status))
		}
		now := s.d.Clock.Now().UTC()
		recent, err := s.d.Store.ListRecent(ctx, tx, req.AccountID, req.AssetID, now.Add(-s.d.Velocity.Window))
		if err != nil {
			return err
		}
		if err := s.d.Velocity.Check(req.Quantity, recent, now); err != nil {
			return err
		}
		w, _, err := s.d.Store.Create(ctx, tx, Withdrawal{
			AccountID: req.AccountID, AssetID: req.AssetID, Quantity: req.Quantity,
			DestinationAddress: req.DestinationAddress, DestinationValidated: true, Status: StatusRequested,
			RequestedByUserID: userID, CapabilityCheckRef: "gate:" + string(gates.Withdrawals),
			IdempotencyKey: req.IdempotencyKey, CorrelationID: req.CorrelationID,
		}, ev)
		if err != nil {
			return err
		}
		out = w
		return nil
	})
	if err != nil {
		return Withdrawal{}, err
	}
	return out, nil
}
