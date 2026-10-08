package httpapi

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxfund"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/transfers"
)

// TransfersDeps holds the services behind /advanced/transfers. Each is
// optional and stands alone: a nil service leaves only its own routes
// UNSUPPORTED.
type TransfersDeps struct {
	// Service sends, reads, reverses and reconciles transfers.
	Service *transfers.Service
	// Faucet grants the sandbox dollar.
	Faucet *sandboxfund.Faucet
	// FaucetEnabled must be the Enabled the faucet was built with
	// (sandboxfund.Deps.Enabled), so the allowance read answers in this
	// environment exactly what a grant would.
	FaucetEnabled bool
}

// wireTransfers wires the transfer and faucet ports whose services are present.
func wireTransfers(p *Ports, d WireDeps) {
	t := d.Advanced.Transfers
	if t.Service != nil {
		p.Transfers = transfersAdapter{svc: t.Service, db: d.DB, assets: assets.NewRepository()}
	}
	if t.Faucet != nil {
		p.SandboxFaucet = sandboxFaucetAdapter{faucet: t.Faucet, db: d.DB, clk: d.Clock, env: d.Env, enabled: t.FaucetEnabled}
	}
}

// transfersCaller is the principal the authenticator attached. The services take it
// explicitly and judge it themselves; the adapter only carries it.
func transfersCaller(ctx context.Context) (security.Principal, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return security.Principal{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return p, nil
}

// transfersSnapshot runs a multi-statement read against one consistent snapshot,
// which is also what Reconcile asks for.
func transfersSnapshot(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

// transfersWriteTx runs a mutation the way the domain packages' own tests do: READ
// COMMITTED, serialized by the services' own advisory and row locks.
func transfersWriteTx(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

// --- transfers --------------------------------------------------------------

type transfersAdapter struct {
	svc    *transfers.Service
	db     *db.DB
	assets *assets.Repository
}

func (a transfersAdapter) Send(ctx context.Context, r TransferSend) (TransferSent, error) {
	p, err := transfersCaller(ctx)
	if err != nil {
		return TransferSent{}, err
	}
	assetID := r.AssetID
	if assetID.IsZero() {
		assetID = econ.SandboxDollarID()
	}
	var (
		out     TransferSent
		refusal error
	)
	err = transfersWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		out, refusal = TransferSent{}, nil // nothing survives from an earlier attempt
		qty, qerr := a.quantity(ctx, tx, assetID, r.Amount, r.Quantity)
		if qerr != nil {
			return qerr
		}
		res, serr := a.svc.Send(ctx, tx, p, transfers.SendRequest{
			SenderAccountID: r.SenderAccountID, RecipientAccountID: r.RecipientAccountID, RecipientHandle: r.RecipientHandle,
			AssetID: assetID, Quantity: qty, Mode: r.Mode, Memo: r.Memo,
			IdempotencyKey: r.IdempotencyKey, CorrelationID: r.CorrelationID,
		})
		if serr != nil {
			if res.Transfer.Status == transfers.StatusRejected {
				// A recorded refusal. The service leaves the transaction usable
				// and asks for it to be committed: the REJECTED transfer, its
				// intent, its risk decision and its evidence are the record.
				refusal = serr
				return nil
			}
			return serr
		}
		v, verr := a.view(ctx, tx, res.Transfer, nil)
		if verr != nil {
			return verr
		}
		out = TransferSent{View: v, Replayed: res.Replayed}
		return nil
	})
	if err != nil {
		return TransferSent{}, withoutRecipientAccount(err, r)
	}
	if refusal != nil {
		return TransferSent{}, withoutRecipientAccount(refusal, r)
	}
	return out, nil
}

// quantity is the client's figure in base units: the quantity as given, or the
// decimal amount parsed against the asset's own decimals by
// transfers.ParseAmount, which never rounds.
func (a transfersAdapter) quantity(ctx context.Context, q db.Querier, assetID assets.AssetID, amount string, qty *money.Quantity) (money.Quantity, error) {
	if qty != nil {
		return *qty, nil
	}
	asset, err := a.assets.Get(ctx, q, assetID)
	if err != nil {
		return money.Quantity{}, err
	}
	return transfers.ParseAmount(amount, asset.Decimals)
}

// withoutRecipientAccount keeps the account id behind a handle on the server.
// The service names the recipient's account id in some refusals (an account
// that is frozen or not a customer account); when the sender named the
// recipient by handle, that field would resolve the handle for them, which no
// route of this API does.
func withoutRecipientAccount(err error, r TransferSend) error {
	if r.RecipientHandle == "" {
		return err
	}
	e, ok := errs.As(err)
	if !ok {
		return err
	}
	if _, named := e.Fields["recipient_account_id"]; !named {
		return err
	}
	out := errs.Wrap(err, e.Code, e.Detail)
	for k, v := range e.Fields {
		if k != "recipient_account_id" {
			out = out.WithField(k, v)
		}
	}
	if e.RetryAfter != nil {
		out = out.WithRetryAfter(*e.RetryAfter)
	}
	return out
}

func (a transfersAdapter) List(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]TransferView, string, error) {
	p, err := transfersCaller(ctx)
	if err != nil {
		return nil, "", err
	}
	var (
		out  []TransferView
		next string
	)
	err = transfersSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		items, nxt, lerr := a.svc.ListForAccount(ctx, tx, p, account, cursor, limit)
		if lerr != nil {
			return lerr
		}
		seen := map[assets.AssetID]assets.Asset{}
		out = make([]TransferView, 0, len(items))
		for _, t := range items {
			v, verr := a.view(ctx, tx, t, seen)
			if verr != nil {
				return verr
			}
			out = append(out, v)
		}
		next = nxt
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

func (a transfersAdapter) Get(ctx context.Context, id transfers.TransferID) (TransferView, error) {
	p, err := transfersCaller(ctx)
	if err != nil {
		return TransferView{}, err
	}
	var out TransferView
	err = transfersSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		t, gerr := a.svc.Get(ctx, tx, p, id)
		if gerr != nil {
			return gerr
		}
		out, gerr = a.view(ctx, tx, t, nil)
		return gerr
	})
	return out, err
}

func (a transfersAdapter) Reverse(ctx context.Context, r TransferReversal) (TransferView, error) {
	p, err := transfersCaller(ctx)
	if err != nil {
		return TransferView{}, err
	}
	var out TransferView
	err = transfersWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		t, rerr := a.svc.Reverse(ctx, tx, p, transfers.ReverseRequest{
			TransferID: r.TransferID, ReasonCode: r.ReasonCode, Reason: r.Reason, CorrelationID: r.CorrelationID,
		})
		if rerr != nil {
			return rerr
		}
		out, rerr = a.view(ctx, tx, t, nil)
		return rerr
	})
	return out, err
}

func (a transfersAdapter) Reconcile(ctx context.Context, asset *assets.AssetID) (transfers.ReconcileReport, error) {
	var scope []assets.AssetID
	if asset != nil {
		scope = append(scope, *asset)
	}
	var out transfers.ReconcileReport
	err := transfersSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		out, rerr = a.svc.Reconcile(ctx, tx, scope...)
		return rerr
	})
	return out, err
}

func (a transfersAdapter) RegisterHandle(ctx context.Context, account accounts.AccountID, handle string) (transfers.Handle, error) {
	p, err := transfersCaller(ctx)
	if err != nil {
		return transfers.Handle{}, err
	}
	var out transfers.Handle
	err = transfersWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		var herr error
		out, herr = a.svc.RegisterHandle(ctx, tx, p, account, handle)
		return herr
	})
	return out, err
}

func (a transfersAdapter) SetRecipientRule(ctx context.Context, r TransferRecipientRule) error {
	p, err := transfersCaller(ctx)
	if err != nil {
		return err
	}
	return transfersWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		recipient := r.RecipientAccountID
		if r.RecipientHandle != "" {
			// Resolved here, inside the transaction that records the rule, and
			// never returned: the rule is echoed back the way it was named.
			var rerr error
			if recipient, rerr = a.svc.ResolveHandle(ctx, tx, r.RecipientHandle); rerr != nil {
				return rerr
			}
		}
		return a.svc.SetRecipientRule(ctx, tx, p, r.SenderAccountID, recipient, r.Rule, r.Reason)
	})
}

func (a transfersAdapter) SetSenderMode(ctx context.Context, account accounts.AccountID, mode transfers.SenderMode) error {
	p, err := transfersCaller(ctx)
	if err != nil {
		return err
	}
	return transfersWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		return a.svc.SetSenderMode(ctx, tx, p, account, mode)
	})
}

// view adds the asset's symbol and decimals. seen, when not nil, saves a read
// per repeated asset within one page.
func (a transfersAdapter) view(ctx context.Context, q db.Querier, t transfers.Transfer, seen map[assets.AssetID]assets.Asset) (TransferView, error) {
	asset, ok := seen[t.AssetID]
	if !ok {
		var err error
		if asset, err = a.assets.Get(ctx, q, t.AssetID); err != nil {
			if errs.HasCode(err, errs.CodeNotFound) {
				// The register's foreign key makes this unreachable; if it ever
				// happens the transfer exists and saying NOT_FOUND would be false.
				return TransferView{}, errs.Wrap(err, errs.CodeInternal, "a transfer names an asset the registry does not hold")
			}
			return TransferView{}, err
		}
		if seen != nil {
			seen[t.AssetID] = asset
		}
	}
	return TransferView{Transfer: t, AssetSymbol: asset.Symbol, AssetDecimals: asset.Decimals}, nil
}

// --- the sandbox faucet -----------------------------------------------------

type sandboxFaucetAdapter struct {
	faucet  *sandboxfund.Faucet
	db      *db.DB
	clk     clock.Clock
	env     config.Environment
	enabled bool
}

func (a sandboxFaucetAdapter) Grant(ctx context.Context, r FaucetRequest) (FaucetGrantView, error) {
	p, err := transfersCaller(ctx)
	if err != nil {
		return FaucetGrantView{}, err
	}
	var out FaucetGrantView
	err = transfersWriteTx(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		// The sandbox dollar is provisioned on first use, inside the grant's
		// own transaction, so a refused grant leaves nothing behind.
		asset, aerr := sandboxfund.EnsureSandboxDollar(ctx, tx)
		if aerr != nil {
			return aerr
		}
		var qty money.Quantity
		if r.Quantity != nil {
			qty = *r.Quantity
		} else if qty, aerr = transfers.ParseAmount(r.Amount, asset.Decimals); aerr != nil {
			return aerr
		}
		g, ferr := a.faucet.Fund(ctx, tx, p, r.AccountID, asset.ID, qty, r.IdempotencyKey)
		if ferr != nil {
			return ferr
		}
		out = FaucetGrantView{Grant: g, AssetSymbol: asset.Symbol, AssetDecimals: asset.Decimals}
		return nil
	})
	return out, err
}

func (a sandboxFaucetAdapter) Allowance(ctx context.Context, account accounts.AccountID) (FaucetAllowance, error) {
	// The same environment rule a grant applies, with the same answer: a cap
	// shown where nothing may be granted would be a figure about nothing.
	if !sandboxfund.Allowed(a.env, a.enabled) {
		return FaucetAllowance{}, errs.Newf(errs.CodeCapabilityNotApproved, "the sandbox faucet is not enabled in %s", a.env).
			WithField("environment", string(a.env))
	}
	var out FaucetAllowance
	err := transfersSnapshot(ctx, a.db, func(ctx context.Context, tx pgx.Tx) error {
		asset, aerr := econ.SandboxDollar(ctx, tx)
		switch {
		case aerr == nil:
		case errs.HasCode(aerr, errs.CodeNotFound):
			// Not provisioned yet. The first grant provisions it with exactly
			// these figures (econ refuses any other row under its id), and
			// nothing can have been granted of an asset that does not exist.
			asset = assets.Asset{ID: econ.SandboxDollarID(), Symbol: econ.SandboxDollarSymbol, Decimals: econ.SandboxDollarDecimals}
		default:
			return aerr
		}
		policy, hash := a.faucet.Policy()
		since := a.clk.Now().UTC().Add(-policy.Window())
		used, uerr := sandboxfund.UsedInWindow(ctx, tx, account, asset.ID, since)
		if uerr != nil {
			return uerr
		}
		capQty := policy.Cap(asset.Decimals)
		remaining := capQty.Sub(used)
		if remaining.IsNegative() {
			// A policy lowered below what was already granted leaves nothing,
			// never a negative allowance.
			remaining = money.QuantityFromInt64(0)
		}
		out = FaucetAllowance{
			AccountID: account, AssetID: asset.ID, AssetSymbol: asset.Symbol, AssetDecimals: asset.Decimals,
			Cap: capQty, Used: used, Remaining: remaining, Policy: policy, PolicyHash: hash, Environment: string(a.env),
		}
		return nil
	})
	return out, err
}
