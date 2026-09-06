package funding

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/webhook"
)

// Lifecycle advances one deposit by one step. It is the workflow-neutral
// driver contract: cmd/workflow-worker wraps Advance in a durable workflow
// activity later; this package never imports a workflow engine.
type Lifecycle interface {
	Advance(ctx context.Context, depositID DepositID) error
}

// Driver is the default Lifecycle: it polls the provider for
// provider-owned states, asks the chain observer for the receipt, then
// reconciles, applies the availability policy and finally promotes
// withdrawal eligibility. Each step runs in its own transaction and is
// idempotent, so a crashed or repeated Advance is safe.
type Driver struct {
	svc      *Service
	observer ChainReceiptObserver
	// ObservationSlack is how far before provider_confirmed_at the chain
	// observer is asked to look (provider clocks and finality lag).
	ObservationSlack time.Duration
}

var _ Lifecycle = (*Driver)(nil)

// NewDriver builds a Driver.
func NewDriver(svc *Service, observer ChainReceiptObserver) (*Driver, error) {
	if svc == nil || observer == nil {
		return nil, errs.New(errs.CodeValidationFailed, "funding: service and chain observer are required")
	}
	return &Driver{svc: svc, observer: observer, ObservationSlack: 15 * time.Minute}, nil
}

// Advance performs the next step for the deposit and returns. It is a
// no-op for terminal, REVIEW_REQUIRED and CREATED deposits (CREATED is
// owned by Start).
func (d *Driver) Advance(ctx context.Context, depositID DepositID) error {
	dep, err := d.svc.d.Repo.Get(ctx, d.svc.d.DB, depositID)
	if err != nil {
		return err
	}
	switch dep.Status {
	case StatusSessionCreated, StatusCustomerActionRequired, StatusProviderProcessing:
		return d.poll(ctx, dep)
	case StatusProviderConfirmed:
		return d.observe(ctx, dep)
	case StatusSettlementObserved:
		return d.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := d.svc.Reconcile(ctx, tx, dep.ID)
			return err
		})
	case StatusReconciled:
		return d.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return d.svc.MarkAvailable(ctx, tx, dep.ID, d.svc.cfg.Availability)
		})
	case StatusAvailable:
		if dep.WithdrawalEligible || dep.ReversibleUntil == nil || d.svc.d.Clock.Now().Before(*dep.ReversibleUntil) {
			return nil
		}
		return d.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := d.svc.d.Repo.SetWithdrawalEligible(ctx, tx, dep.ID, d.svc.d.Clock.Now(),
				SystemEvidence("reversible window elapsed with clean fraud state", "", dep.CorrelationID))
			return err
		})
	}
	return nil
}

func (d *Driver) inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.svc.d.DB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, fn)
}

// poll re-reads the provider session (SAFE_RETRY) and applies it exactly
// as a webhook would, then expires abandoned sessions.
func (d *Driver) poll(ctx context.Context, dep Deposit) error {
	now := d.svc.d.Clock.Now().UTC()
	session, err := d.svc.d.Provider.GetSession(ctx, dep.ProviderSessionID)
	if err != nil {
		return err
	}
	ev := WebhookEvent{
		Identity: webhook.Identity{
			Provider: d.svc.d.Provider.Name(), EventID: "poll:" + dep.ProviderSessionID + ":" + strconv.FormatInt(now.UnixNano(), 10),
			EventType: "poll", PublishedAt: now, SignedAt: now, Livemode: session.Livemode,
		},
		Session: session, SessionKnown: true,
	}
	if err := d.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		res, err := d.svc.ApplyProviderEvent(ctx, tx, ev)
		if err != nil {
			return err
		}
		observability.LoggerFrom(ctx).DebugContext(ctx, "funding: polled provider session",
			"deposit_id", dep.ID.String(), "outcome", string(res.Outcome), "reason", res.Reason)
		return nil
	}); err != nil {
		return err
	}
	if dep.Status == StatusProviderProcessing || now.Sub(dep.CreatedAt) < d.svc.cfg.SessionTTL {
		return nil
	}
	return d.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := d.svc.d.Repo.GetForUpdate(ctx, tx, dep.ID)
		if err != nil {
			return err
		}
		if cur.Status != StatusSessionCreated && cur.Status != StatusCustomerActionRequired {
			return nil
		}
		ev := SystemEvidence("session ttl elapsed without customer completion", "", cur.CorrelationID)
		_, err = d.svc.d.Repo.Transition(ctx, tx, dep.ID, StatusExpired, ev)
		return err
	})
}

// observe asks the chain observer for the credit; a receipt records
// settlement, a missing receipt past SettlementTimeout escalates.
func (d *Driver) observe(ctx context.Context, dep Deposit) error {
	now := d.svc.d.Clock.Now().UTC()
	since := dep.CreatedAt
	if dep.ProviderConfirmedAt != nil {
		since = dep.ProviderConfirmedAt.Add(-d.ObservationSlack)
	}
	receipt, found, err := d.observer.ObserveCredit(ctx, ChainReceiptQuery{Address: dep.DestinationAddress, AssetID: dep.ExpectedAssetID, Since: since})
	if err != nil {
		return err
	}
	if found {
		return d.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return d.svc.RecordSettlement(ctx, tx, dep.ID, receipt.Quantity, receipt.Signature, receipt.Slot)
		})
	}
	confirmedAt := dep.CreatedAt
	if dep.ProviderConfirmedAt != nil {
		confirmedAt = *dep.ProviderConfirmedAt
	}
	if now.Sub(confirmedAt) < d.svc.cfg.SettlementTimeout {
		return nil
	}
	return d.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		cur, err := d.svc.d.Repo.GetForUpdate(ctx, tx, dep.ID)
		if err != nil {
			return err
		}
		if cur.Status != StatusProviderConfirmed {
			return nil
		}
		ev := SystemEvidence("provider confirmed but no chain receipt within settlement timeout", "", cur.CorrelationID)
		ev.Detail = map[string]any{"settlement_timeout": d.svc.cfg.SettlementTimeout.String(), "mismatch": "FUNDING"}
		_, err = d.svc.d.Repo.Transition(ctx, tx, dep.ID, StatusReviewRequired, ev)
		return err
	})
}
