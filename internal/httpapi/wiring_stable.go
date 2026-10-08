package httpapi

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// StableDeps holds what /advanced/stable-assets needs. The routes are wired
// only when every required field is present -- which in cmd/api means a
// sandbox chain is configured (internal/sandboxchain) -- and answer
// UNSUPPORTED otherwise, never a fabricated answer.
type StableDeps struct {
	// Service is the stable asset controller. It decides every rule.
	Service *stableasset.Service
	// Host is the simulated chain host the service runs on; the holdings
	// read the custodied wallets' chain balances through it.
	Host *devnethost.Host
	// HolderAddress derives an account's custodied sandbox wallet address
	// (stableasset.CustodialWalletSigner.HolderAddress). Only the public
	// address: this layer never holds a signer.
	HolderAddress func(accounts.AccountID) devnet.PublicKey
	// SnapshotKey is the snapshot signer's Ed25519 public key, the key every
	// published snapshot chain is verified against.
	SnapshotKey ed25519.PublicKey
	// Capital supplies the availability arithmetic the holdings report.
	Capital *capital.Service
	// Identity, Verification and Jurisdiction are the account facts the
	// settlement compiler and Risk Kernel V2 read, from the deployment's own
	// resolvers and never from a request. Identity is required (D-055); a
	// nil Verification means NONE and a nil Jurisdiction UNKNOWN, exactly as
	// for the derivatives routes.
	Identity     StableIdentityReader
	Verification VerificationResolver
	Jurisdiction JurisdictionResolver
}

func (d StableDeps) complete() bool {
	return d.Service != nil && d.Host != nil && d.HolderAddress != nil && len(d.SnapshotKey) == ed25519.PublicKeySize &&
		d.Capital != nil && d.Identity != nil
}

// wireStable wires the stable asset port when its dependencies are present.
func wireStable(p *Ports, d WireDeps) {
	sd := d.Advanced.Stable
	if !sd.complete() || d.DB == nil {
		return
	}
	clk := d.Clock
	if clk == nil {
		clk = clock.System()
	}
	repo := d.Assets
	if repo == nil {
		repo = assets.NewRepository()
	}
	p.Stable = stableAdapter{
		svc: sd.Service, host: sd.Host, holder: sd.HolderAddress, key: append(ed25519.PublicKey(nil), sd.SnapshotKey...),
		capital: sd.Capital, assets: repo, db: d.DB, clk: clk,
		facts: stableBridgeFacts{identity: sd.Identity, verification: sd.Verification, jurisdiction: sd.Jurisdiction},
	}
}

// stableAdapter serves StableAssetPort over the stable asset service.
//
// Issue and Redeem call the service's own workflows, which own their
// transactions. Every operator command runs one service operation in its own
// READ COMMITTED transaction, the isolation the service is written for. Every
// read runs in one read-only REPEATABLE READ snapshot, so a response never
// mixes two states of an asset; where the package exports no listing (assets,
// requests of an account, status history, alerts) the adapter reads its
// tables, strictly with SELECTs, and decides nothing.
type stableAdapter struct {
	svc     *stableasset.Service
	host    *devnethost.Host
	holder  func(accounts.AccountID) devnet.PublicKey
	key     ed25519.PublicKey
	capital *capital.Service
	facts   stableBridgeFacts
	assets  *assets.Repository
	db      *db.DB
	clk     clock.Clock
}

var _ StableAssetPort = stableAdapter{}

func (a stableAdapter) read(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, fn)
}

func (a stableAdapter) write(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return a.db.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted, MaxRetries: 3}, fn)
}

// The economic intent keys of the customer's commands: the client's
// Idempotency-Key, namespaced per operation, so the same client key used for
// an issuance and for a redemption names two different intents.
const (
	stableIssueKeyPrefix  = "http:stable:issue:"
	stableRedeemKeyPrefix = "http:stable:redeem:"
)

// stableAlertLimit bounds the alerts an asset's detail carries.
const stableAlertLimit = 20

const stableAssetIDsSQL = `SELECT id FROM stable_assets ORDER BY created_at, id LIMIT $1`

const stableAssetTransitionsSQL = `SELECT from_status, to_status, actor_type, actor_id, reason, occurred_at
	FROM stable_asset_transitions WHERE stable_asset_id = $1 ORDER BY occurred_at, id`

const stableAlertsSQL = `SELECT id, kind, severity, breaker_tripped, observed, observed_at
	FROM stable_peg_alerts WHERE stable_asset_id = $1 ORDER BY observed_at DESC, id DESC LIMIT $2`

const stableIssuanceIDsSQL = `SELECT id FROM stable_mint_requests
	WHERE account_id = $1 AND ($2::uuid IS NULL OR stable_asset_id = $2::uuid)
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $5`

const stableRedemptionIDsSQL = `SELECT id FROM stable_burn_requests
	WHERE account_id = $1 AND ($2::uuid IS NULL OR stable_asset_id = $2::uuid)
	  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::uuid))
	ORDER BY created_at DESC, id DESC LIMIT $5`

// stableEffectStateSQL reads an effect's status and, from its transition
// rows, when it last became UNKNOWN and when it last left UNKNOWN.
const stableEffectStateSQL = `SELECT e.status,
	(SELECT max(t.occurred_at) FROM economic_effect_transitions t WHERE t.effect_id = e.id AND t.to_status = 'UNKNOWN'),
	(SELECT max(t.occurred_at) FROM economic_effect_transitions t WHERE t.effect_id = e.id AND t.from_status = 'UNKNOWN')
	FROM economic_effects e WHERE e.id = $1`

// --- reference reads ----------------------------------------------------------

func (a stableAdapter) Assets(ctx context.Context, limit int) ([]StableAssetView, error) {
	var out []StableAssetView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		list, err := a.listAssets(ctx, tx, clampLimit(limit))
		if err != nil {
			return err
		}
		l := newStableLabels(a.assets)
		for _, sa := range list {
			v, err := l.view(ctx, tx, sa)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

func (a stableAdapter) Asset(ctx context.Context, sid stableasset.StableAssetID) (StableAssetDetailView, error) {
	var out StableAssetDetailView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.detail(ctx, tx, newStableLabels(a.assets), sid)
		return err
	})
	return out, err
}

func (a stableAdapter) Reserve(ctx context.Context, sid stableasset.StableAssetID) (StableReserveView, error) {
	var out StableReserveView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		sa, err := a.svc.Get(ctx, tx, sid)
		if err != nil {
			return err
		}
		rl, err := a.svc.ReserveLedger(ctx, tx, sid)
		if err != nil {
			return err
		}
		l := newStableLabels(a.assets)
		v, err := l.view(ctx, tx, sa)
		if err != nil {
			return err
		}
		labels, err := l.reserveLabels(ctx, tx, rl)
		if err != nil {
			return err
		}
		out = StableReserveView{Asset: v, Ledger: rl, Labels: labels}
		return nil
	})
	return out, err
}

func (a stableAdapter) Snapshots(ctx context.Context, sid stableasset.StableAssetID, limit int) (StableSnapshotChainView, error) {
	var out StableSnapshotChainView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := a.svc.Get(ctx, tx, sid); err != nil {
			return err
		}
		snaps, err := a.svc.Snapshots(ctx, tx, sid)
		if err != nil {
			return err
		}
		verification, err := stableVerify(snaps, a.key)
		if err != nil {
			return err
		}
		start := len(snaps) - clampLimit(limit)
		if start < 0 {
			start = 0
		}
		out = StableSnapshotChainView{StableAssetID: sid, Total: len(snaps), Offset: start, Items: snaps[start:], Verification: verification}
		return nil
	})
	return out, err
}

// stableVerify verifies a whole snapshot chain against the snapshot signer's
// key with the package's own verifier. A chain that does not verify is a
// finding, reported with the index and reason the verifier gave; anything
// else the verifier refuses (a key of the wrong length) is a fault.
func stableVerify(snaps []stableasset.SignedSnapshot, key ed25519.PublicKey) (StableSnapshotVerification, error) {
	v := StableSnapshotVerification{Checked: len(snaps), FailedIndex: -1, PublicKey: append(ed25519.PublicKey(nil), key...)}
	err := stableasset.VerifySnapshotChain(snaps, key)
	if err == nil {
		return v, nil
	}
	e, ok := errs.As(err)
	if !ok {
		return StableSnapshotVerification{}, err
	}
	index, okIndex := e.Fields["index"].(int)
	reason, okReason := e.Fields["reason"].(string)
	if !okIndex || !okReason || index < 0 {
		return StableSnapshotVerification{}, errs.Wrap(err, errs.CodeInternal, "the snapshot chain could not be verified")
	}
	v.FailedIndex, v.Reason = index, reason
	return v, nil
}

// --- account reads -------------------------------------------------------------

func (a stableAdapter) Holdings(ctx context.Context, account accounts.AccountID) (StableHoldingsView, error) {
	now := a.clk.Now().UTC()
	holder := a.holder(account)
	out := StableHoldingsView{AccountID: account, HolderAddress: holder.String(), AsOf: now}
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		out.Items, out.Payment = nil, nil
		list, err := a.listAssets(ctx, tx, maxPageLimit)
		if err != nil {
			return err
		}
		l := newStableLabels(a.assets)
		chainBalances := make([]money.Quantity, len(list))
		if err := a.host.ViewIn(ctx, tx, func(v devnethost.View) error {
			for i, sa := range list {
				bal, err := v.TokenBalance(sa.Chain, holder, sa.Mint)
				if err != nil {
					return err
				}
				chainBalances[i] = devnet.QuantityOf(bal)
			}
			return nil
		}); err != nil {
			return err
		}
		seenPayment := map[assets.AssetID]bool{}
		for i, sa := range list {
			label, err := l.label(ctx, tx, sa.AssetID)
			if err != nil {
				return err
			}
			avail, err := a.capital.Availability(ctx, tx, account.String(), sa.AssetID, now)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, StableHoldingView{Asset: sa, Label: label, Availability: avail, ChainBalance: chainBalances[i]})
			for _, r := range sa.Reserves {
				if !r.AcceptedForMint || seenPayment[r.AssetID] {
					continue
				}
				seenPayment[r.AssetID] = true
				rl, err := l.label(ctx, tx, r.AssetID)
				if err != nil {
					return err
				}
				ravail, err := a.capital.Availability(ctx, tx, account.String(), r.AssetID, now)
				if err != nil {
					return err
				}
				out.Payment = append(out.Payment, StablePaymentView{AssetID: r.AssetID, Label: rl, Availability: ravail})
			}
		}
		return nil
	})
	return out, err
}

func (a stableAdapter) Issuances(ctx context.Context, account accounts.AccountID, asset *stableasset.StableAssetID, cursor string, limit int) (StableIssuancePage, error) {
	at, after, err := stableBridgeCursor(cursor)
	if err != nil {
		return StableIssuancePage{}, err
	}
	limit = clampLimit(limit)
	var page StableIssuancePage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		page = StableIssuancePage{}
		ids, err := stableRequestIDs(ctx, tx, stableIssuanceIDsSQL, account, asset, at, after, limit+1)
		if err != nil {
			return err
		}
		l := newStableLabels(a.assets)
		for _, raw := range ids {
			var rid stableasset.MintRequestID
			if err := rid.UnmarshalText([]byte(raw)); err != nil {
				return errs.Wrap(err, errs.CodeInternal, "a stored mint request id does not parse")
			}
			rec, err := a.svc.GetMint(ctx, tx, rid)
			if err != nil {
				return err
			}
			v, err := a.issuanceView(ctx, tx, l, rec)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, v)
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			last := page.Items[limit-1].Record
			page.NextCursor = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
		}
		return nil
	})
	return page, err
}

func (a stableAdapter) Issuance(ctx context.Context, rid stableasset.MintRequestID) (StableIssuanceView, error) {
	var out StableIssuanceView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := a.svc.GetMint(ctx, tx, rid)
		if err != nil {
			return err
		}
		out, err = a.issuanceView(ctx, tx, newStableLabels(a.assets), rec)
		return err
	})
	return out, err
}

func (a stableAdapter) Redemptions(ctx context.Context, account accounts.AccountID, asset *stableasset.StableAssetID, cursor string, limit int) (StableRedemptionPage, error) {
	at, after, err := stableBridgeCursor(cursor)
	if err != nil {
		return StableRedemptionPage{}, err
	}
	limit = clampLimit(limit)
	var page StableRedemptionPage
	err = a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		page = StableRedemptionPage{}
		ids, err := stableRequestIDs(ctx, tx, stableRedemptionIDsSQL, account, asset, at, after, limit+1)
		if err != nil {
			return err
		}
		l := newStableLabels(a.assets)
		for _, raw := range ids {
			var rid stableasset.BurnRequestID
			if err := rid.UnmarshalText([]byte(raw)); err != nil {
				return errs.Wrap(err, errs.CodeInternal, "a stored burn request id does not parse")
			}
			rec, err := a.svc.GetBurn(ctx, tx, rid)
			if err != nil {
				return err
			}
			v, err := a.redemptionView(ctx, tx, l, rec)
			if err != nil {
				return err
			}
			page.Items = append(page.Items, v)
		}
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			last := page.Items[limit-1].Record
			page.NextCursor = encodeCursor(keysetCursor{At: last.CreatedAt, ID: last.ID.String()})
		}
		return nil
	})
	return page, err
}

func (a stableAdapter) Redemption(ctx context.Context, rid stableasset.BurnRequestID) (StableRedemptionView, error) {
	var out StableRedemptionView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rec, err := a.svc.GetBurn(ctx, tx, rid)
		if err != nil {
			return err
		}
		out, err = a.redemptionView(ctx, tx, newStableLabels(a.assets), rec)
		return err
	})
	return out, err
}

// --- the customer's commands -----------------------------------------------------

func (a stableAdapter) Issue(ctx context.Context, c StableIssueCommand) (StableIssuanceOutcome, error) {
	facts, err := a.facts.of(ctx, a.db, c.AccountID)
	if err != nil {
		return StableIssuanceOutcome{}, err
	}
	res, err := a.svc.Mint(ctx, stableasset.MintRequest{
		StableAssetID: c.StableAssetID, AccountID: c.AccountID, Quantity: c.Quantity, ReserveAssetID: c.ReserveAssetID,
		IdempotencyKey: stableIssueKeyPrefix + c.IdempotencyKey, Mode: c.Mode, Actor: c.Actor, CorrelationID: c.CorrelationID,
		Facts: stableasset.AccountFacts{
			Jurisdiction: facts.jurisdiction, Verification: facts.verification, IdentityVerified: facts.identityVerified,
		},
	})
	unknown := err != nil && errs.HasCode(err, errs.CodeSubmissionStateUnknown) && !res.Request.ID.IsZero()
	if err != nil && !unknown {
		return StableIssuanceOutcome{}, err
	}
	// The request exists: read it back as recorded. An unknown outcome is not
	// a failure and not a success; the request says SUBMISSION_UNKNOWN and is
	// settled only from the chain by transaction id.
	v, verr := a.Issuance(ctx, res.Request.ID)
	if verr != nil {
		return StableIssuanceOutcome{}, verr
	}
	return StableIssuanceOutcome{View: v, Replay: res.Replay, Settled: !unknown && v.Record.Status.Terminal()}, nil
}

func (a stableAdapter) Redeem(ctx context.Context, c StableRedeemCommand) (StableRedemptionOutcome, error) {
	facts, err := a.facts.of(ctx, a.db, c.AccountID)
	if err != nil {
		return StableRedemptionOutcome{}, err
	}
	res, err := a.svc.Burn(ctx, stableasset.BurnRequest{
		StableAssetID: c.StableAssetID, AccountID: c.AccountID, Quantity: c.Quantity,
		IdempotencyKey: stableRedeemKeyPrefix + c.IdempotencyKey, Mode: c.Mode, Actor: c.Actor, CorrelationID: c.CorrelationID,
		Facts: stableasset.AccountFacts{
			Jurisdiction: facts.jurisdiction, Verification: facts.verification, IdentityVerified: facts.identityVerified,
		},
	})
	unknown := err != nil && errs.HasCode(err, errs.CodeSubmissionStateUnknown) && !res.Request.ID.IsZero()
	if err != nil && !unknown {
		return StableRedemptionOutcome{}, err
	}
	v, verr := a.Redemption(ctx, res.Request.ID)
	if verr != nil {
		return StableRedemptionOutcome{}, verr
	}
	return StableRedemptionOutcome{View: v, Replay: res.Replay, Settled: !unknown && v.Record.Status.Terminal()}, nil
}

// --- the operator's commands -------------------------------------------------------

func (a stableAdapter) Register(ctx context.Context, def stableasset.Definition, by econ.Actor) (StableAssetDetailView, error) {
	sa, err := a.svc.Register(ctx, def, by)
	if err != nil {
		return StableAssetDetailView{}, err
	}
	return a.Asset(ctx, sa.ID)
}

func (a stableAdapter) SetStatus(ctx context.Context, sid stableasset.StableAssetID, to stableasset.Status, by econ.Actor, reason string) (StableAssetDetailView, error) {
	if err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a.svc.Transition(ctx, tx, sid, to, by, reason)
		return err
	}); err != nil {
		return StableAssetDetailView{}, err
	}
	return a.Asset(ctx, sid)
}

func (a stableAdapter) RunMonitor(ctx context.Context, sid stableasset.StableAssetID, by econ.Actor) (StableMonitorView, error) {
	var out StableMonitorView
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		report, err := a.svc.RunMonitor(ctx, tx, sid, by)
		if err != nil {
			return err
		}
		sa, err := a.svc.Get(ctx, tx, sid)
		if err != nil {
			return err
		}
		l := newStableLabels(a.assets)
		v, err := l.view(ctx, tx, sa)
		if err != nil {
			return err
		}
		labels, err := l.reserveLabels(ctx, tx, report.Reserve)
		if err != nil {
			return err
		}
		out = StableMonitorView{Report: report, Asset: v, Labels: labels}
		return nil
	})
	return out, err
}

func (a stableAdapter) ReconcileSupply(ctx context.Context, sid stableasset.StableAssetID, by econ.Actor) (stableasset.SupplyReconciliation, error) {
	var out stableasset.SupplyReconciliation
	err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		out, err = a.svc.ReconcileSupply(ctx, tx, sid, reconciliation.ModeFull, by)
		return err
	})
	return out, err
}

func (a stableAdapter) TakeSnapshot(ctx context.Context, sid stableasset.StableAssetID) (StableSnapshotTakenView, error) {
	var taken stableasset.SignedSnapshot
	if err := a.write(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		taken, err = a.svc.TakeSnapshot(ctx, tx, sid)
		return err
	}); err != nil {
		return StableSnapshotTakenView{}, err
	}
	var out StableSnapshotTakenView
	err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		snaps, err := a.svc.Snapshots(ctx, tx, sid)
		if err != nil {
			return err
		}
		verification, err := stableVerify(snaps, a.key)
		if err != nil {
			return err
		}
		for i, sn := range snaps {
			if sn.Hash == taken.Hash {
				out = StableSnapshotTakenView{Snapshot: sn, Index: i, Verification: verification}
				return nil
			}
		}
		return errs.New(errs.CodeInternal, "the snapshot just recorded is not in the asset's snapshot chain")
	})
	return out, err
}

func (a stableAdapter) ProcessRedemptions(ctx context.Context, sid stableasset.StableAssetID, limit int) (stableasset.SettleReport, error) {
	// An unknown asset is NOT_FOUND, not an empty queue.
	if err := a.read(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a.svc.Get(ctx, tx, sid)
		return err
	}); err != nil {
		return stableasset.SettleReport{}, err
	}
	return a.svc.ProcessRedemptions(ctx, sid, limit)
}

func (a stableAdapter) ResolveUnknown(ctx context.Context, limit int) (stableasset.SettleReport, error) {
	return a.svc.ReconcileUnknown(ctx, limit)
}

// --- views ---------------------------------------------------------------------

func (a stableAdapter) listAssets(ctx context.Context, q db.Querier, limit int) ([]stableasset.PlatformStableAsset, error) {
	rows, err := q.Query(ctx, stableAssetIDsSQL, limit)
	if err != nil {
		return nil, err
	}
	var ids []stableasset.StableAssetID
	for rows.Next() {
		var sid stableasset.StableAssetID
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, sid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]stableasset.PlatformStableAsset, 0, len(ids))
	for _, sid := range ids {
		sa, err := a.svc.Get(ctx, q, sid)
		if err != nil {
			return nil, err
		}
		out = append(out, sa)
	}
	return out, nil
}

func (a stableAdapter) detail(ctx context.Context, q db.Querier, l *stableLabels, sid stableasset.StableAssetID) (StableAssetDetailView, error) {
	sa, err := a.svc.Get(ctx, q, sid)
	if err != nil {
		return StableAssetDetailView{}, err
	}
	v, err := l.view(ctx, q, sa)
	if err != nil {
		return StableAssetDetailView{}, err
	}
	out := StableAssetDetailView{StableAssetView: v, Transitions: []StableAssetTransition{}, Alerts: []StableAlertView{}}
	rows, err := q.Query(ctx, stableAssetTransitionsSQL, sid)
	if err != nil {
		return StableAssetDetailView{}, err
	}
	for rows.Next() {
		var t StableAssetTransition
		var to string
		if err := rows.Scan(&t.From, &to, &t.Actor.Type, &t.Actor.ID, &t.Reason, &t.OccurredAt); err != nil {
			rows.Close()
			return StableAssetDetailView{}, err
		}
		t.To, t.OccurredAt = stableasset.Status(to), t.OccurredAt.UTC()
		out.Transitions = append(out.Transitions, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return StableAssetDetailView{}, err
	}
	rows, err = q.Query(ctx, stableAlertsSQL, sid, stableAlertLimit)
	if err != nil {
		return StableAssetDetailView{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var al StableAlertView
		var kind, severity string
		if err := rows.Scan(&al.ID, &kind, &severity, &al.BreakerTripped, &al.Observed, &al.ObservedAt); err != nil {
			return StableAssetDetailView{}, err
		}
		al.Kind, al.Severity, al.ObservedAt = stableasset.AlertKind(kind), stableasset.Severity(severity), al.ObservedAt.UTC()
		out.Alerts = append(out.Alerts, al)
	}
	return out, rows.Err()
}

func (a stableAdapter) issuanceView(ctx context.Context, q db.Querier, l *stableLabels, rec stableasset.MintRecord) (StableIssuanceView, error) {
	stable, err := l.stableLabel(ctx, q, a.svc, rec.StableAssetID)
	if err != nil {
		return StableIssuanceView{}, err
	}
	reserve, err := l.label(ctx, q, rec.ReserveAssetID)
	if err != nil {
		return StableIssuanceView{}, err
	}
	recon, err := stableRequestReconciliationOf(ctx, q, rec.EffectID)
	if err != nil {
		return StableIssuanceView{}, err
	}
	return StableIssuanceView{Record: rec, Stable: stable, Reserve: reserve, Reconciliation: recon}, nil
}

func (a stableAdapter) redemptionView(ctx context.Context, q db.Querier, l *stableLabels, rec stableasset.BurnRecord) (StableRedemptionView, error) {
	stable, err := l.stableLabel(ctx, q, a.svc, rec.StableAssetID)
	if err != nil {
		return StableRedemptionView{}, err
	}
	payout, err := l.label(ctx, q, rec.PayoutAssetID)
	if err != nil {
		return StableRedemptionView{}, err
	}
	recon, err := stableRequestReconciliationOf(ctx, q, rec.EffectID)
	if err != nil {
		return StableRedemptionView{}, err
	}
	return StableRedemptionView{Record: rec, Stable: stable, Payout: payout, Reconciliation: recon}, nil
}

// stableRequestReconciliationOf reads what a request's economic effect says
// about an unknown outcome.
func stableRequestReconciliationOf(ctx context.Context, q db.Querier, effect econ.EffectID) (StableRequestReconciliation, error) {
	var out StableRequestReconciliation
	var status string
	err := q.QueryRow(ctx, stableEffectStateSQL, effect).Scan(&status, &out.UnknownSince, &out.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StableRequestReconciliation{}, errs.New(errs.CodeInternal, "a stable asset request names an economic effect that does not exist")
	}
	if err != nil {
		return StableRequestReconciliation{}, err
	}
	out.EffectStatus = econ.EffectStatus(status)
	return out, nil
}

// stableRequestIDs pages the ids of an account's mint or burn requests,
// newest first.
func stableRequestIDs(ctx context.Context, q db.Querier, sql string, account accounts.AccountID, asset *stableasset.StableAssetID,
	at *time.Time, after *string, limit int) ([]string, error) {
	var assetArg *string
	if asset != nil {
		s := asset.String()
		assetArg = &s
	}
	rows, err := q.Query(ctx, sql, account, assetArg, at, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u uuid.UUID
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u.String())
	}
	return out, rows.Err()
}

// stableBridgeCursor decodes a page cursor into the bound parameters of a
// keyset query: (nil, nil) is the first page.
func stableBridgeCursor(cursor string) (*time.Time, *string, error) {
	c, ok, err := decodeCursor(cursor)
	if err != nil || !ok {
		return nil, nil, err
	}
	u, err := uuid.Parse(c.ID)
	if err != nil {
		return nil, nil, validationError("cursor", "cursor is not a valid pagination cursor")
	}
	at, after := c.At, u.String()
	return &at, &after, nil
}

// stableLabels resolves the assets a response names, once per call.
type stableLabels struct {
	repo   *assets.Repository
	seen   map[assets.AssetID]assets.Asset
	stable map[stableasset.StableAssetID]StableAssetLabel
}

func newStableLabels(repo *assets.Repository) *stableLabels {
	return &stableLabels{repo: repo, seen: map[assets.AssetID]assets.Asset{}, stable: map[stableasset.StableAssetID]StableAssetLabel{}}
}

func (l *stableLabels) asset(ctx context.Context, q db.Querier, aid assets.AssetID) (assets.Asset, error) {
	if a, ok := l.seen[aid]; ok {
		return a, nil
	}
	a, err := l.repo.Get(ctx, q, aid)
	if err != nil {
		return assets.Asset{}, err
	}
	l.seen[aid] = a
	return a, nil
}

func (l *stableLabels) label(ctx context.Context, q db.Querier, aid assets.AssetID) (StableAssetLabel, error) {
	a, err := l.asset(ctx, q, aid)
	if err != nil {
		return StableAssetLabel{}, err
	}
	return StableAssetLabel{Symbol: a.Symbol, Decimals: a.Decimals}, nil
}

// stableLabel is the label of a stable asset, by its registry id.
func (l *stableLabels) stableLabel(ctx context.Context, q db.Querier, svc *stableasset.Service, sid stableasset.StableAssetID) (StableAssetLabel, error) {
	if lb, ok := l.stable[sid]; ok {
		return lb, nil
	}
	sa, err := svc.Get(ctx, q, sid)
	if err != nil {
		return StableAssetLabel{}, err
	}
	lb, err := l.label(ctx, q, sa.AssetID)
	if err != nil {
		return StableAssetLabel{}, err
	}
	l.stable[sid] = lb
	return lb, nil
}

func (l *stableLabels) view(ctx context.Context, q db.Querier, sa stableasset.PlatformStableAsset) (StableAssetView, error) {
	self, err := l.asset(ctx, q, sa.AssetID)
	if err != nil {
		return StableAssetView{}, err
	}
	v := StableAssetView{Asset: sa, Label: StableAssetLabel{Symbol: self.Symbol, Decimals: self.Decimals}, PegCurrency: self.PegCurrency,
		Reserves: map[assets.AssetID]StableAssetLabel{}}
	for _, r := range sa.Reserves {
		lb, err := l.label(ctx, q, r.AssetID)
		if err != nil {
			return StableAssetView{}, err
		}
		v.Reserves[r.AssetID] = lb
	}
	return v, nil
}

// reserveLabels labels every asset a reserve ledger names.
func (l *stableLabels) reserveLabels(ctx context.Context, q db.Querier, rl stableasset.ReserveLedger) (map[assets.AssetID]StableAssetLabel, error) {
	out := map[assets.AssetID]StableAssetLabel{}
	for _, line := range [][]stableasset.ReserveHolding{rl.CashEquivalentReserve, rl.TokenizedReserve, rl.Receivable, rl.PendingRedemption} {
		for _, h := range line {
			if _, ok := out[h.AssetID]; ok {
				continue
			}
			lb, err := l.label(ctx, q, h.AssetID)
			if err != nil {
				return nil, err
			}
			out[h.AssetID] = lb
		}
	}
	return out, nil
}

// stableBridgeFacts resolves the account facts the stable asset and bridge
// services route and risk-check on, from the deployment's own resolvers. A
// request never states them.
type stableBridgeFacts struct {
	identity     StableIdentityReader
	verification VerificationResolver
	jurisdiction JurisdictionResolver
}

type stableBridgeAccountFacts struct {
	jurisdiction     string
	verification     valuedomain.VerificationLevel
	identityVerified bool
}

func (f stableBridgeFacts) of(ctx context.Context, q db.Querier, account accounts.AccountID) (stableBridgeAccountFacts, error) {
	out := stableBridgeAccountFacts{verification: valuedomain.VerificationNone}
	if f.verification != nil {
		lvl, err := f.verification.Level(ctx, account)
		if err != nil {
			return stableBridgeAccountFacts{}, err
		}
		out.verification = lvl
	}
	if f.jurisdiction != nil {
		j, err := f.jurisdiction.Jurisdiction(ctx, account)
		if err != nil {
			return stableBridgeAccountFacts{}, err
		}
		out.jurisdiction = j
	}
	verified, err := f.identity.IdentityVerified(ctx, q, account)
	if err != nil {
		return stableBridgeAccountFacts{}, err
	}
	out.identityVerified = verified
	return out, nil
}
