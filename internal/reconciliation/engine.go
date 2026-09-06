package reconciliation

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/chain"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/money"
)

// Config wires an Engine. Records, DB, Clock and Policy are mandatory;
// everything else narrows what the engine can do and is checked at the point
// of use with an explicit error rather than a nil dereference.
type Config struct {
	DB      *db.DB
	Clock   clock.Clock
	Records *Repository
	Policy  Policy

	Orders       Orders
	Attempts     Attempts
	Ledger       Ledger
	Positions    Positions
	Reservations Reservations
	Approvals    Approvals
	Assets       AssetRegistry
	Wallets      Wallets
	Valuer       USDValuer

	Observers Observers
	Adapters  Adapters

	// NativeAsset is the chain's native asset (SOL). It is what a network fee
	// is denominated in; without it an observed fee cannot be attributed to a
	// ledger account and is recorded as evidence only.
	NativeAsset assets.AssetID

	Metrics *Metrics
	Logger  *slog.Logger
}

// Engine compares internal accounting truth with external truth in the three
// PART 50 modes and records every difference.
//
// Nothing in this type consults a kill switch. Reconciliation, observation,
// settlement and ledger posting are the classes PART 52 declares un-killable,
// and the engine is where that promise is kept.
type Engine struct {
	db      *db.DB
	clk     clock.Clock
	records *Repository
	policy  Policy

	orders       Orders
	attempts     Attempts
	ledger       Ledger
	positions    Positions
	reservations Reservations
	approvals    Approvals
	assets       AssetRegistry
	wallets      Wallets
	valuer       USDValuer

	observers   Observers
	adapters    Adapters
	nativeAsset assets.AssetID

	metrics *Metrics
	log     *slog.Logger
}

// NewEngine validates the configuration and returns an Engine.
func NewEngine(cfg Config) (*Engine, error) {
	if cfg.DB == nil {
		return nil, errs.New(errs.CodeInternal, "reconciliation: a database is required")
	}
	if cfg.Records == nil {
		return nil, errs.New(errs.CodeInternal, "reconciliation: a record repository is required")
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	if cfg.Observers.Primary != nil {
		if err := cfg.Observers.Policy.Validate(); err != nil {
			return nil, err
		}
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	wallets := cfg.Wallets
	if wallets == nil {
		wallets = NewSQLWallets()
	}
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = NoopMetrics()
	}
	return &Engine{
		db: cfg.DB, clk: clk, records: cfg.Records, policy: cfg.Policy,
		orders: cfg.Orders, attempts: cfg.Attempts, ledger: cfg.Ledger, positions: cfg.Positions,
		reservations: cfg.Reservations, approvals: cfg.Approvals, assets: cfg.Assets, wallets: wallets,
		valuer: cfg.Valuer, observers: cfg.Observers, adapters: cfg.Adapters, nativeAsset: cfg.NativeAsset,
		metrics: metrics, log: log,
	}, nil
}

// Policy returns the engine's policy.
func (e *Engine) Policy() Policy { return e.policy }

// Records returns the record repository, so a worker or admin handler can read
// and transition records through the same audited path.
func (e *Engine) Records() *Repository { return e.records }

// inTx runs fn in a read-committed transaction with the standard retry policy.
func (e *Engine) inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return e.db.InTx(ctx, db.TxOptions{}, fn)
}

// upsert records one comparison outcome, converging instead of accumulating.
//
//	no record for the scope                → open one and classify it
//	an unresolved record                   → refresh its evidence in place; an
//	                                         OPEN one is classified
//	a terminal record and nothing is wrong  → return it untouched (a resolved
//	                                         record is history and is never
//	                                         rewritten)
//	a terminal record and a new mismatch    → open a new record: this is a new
//	                                         divergence, not the old one
//
// So replaying the same external observation any number of times produces one
// record in one status, and no second posting.
func (e *Engine) upsert(ctx context.Context, tx pgx.Tx, req OpenRequest) (Record, error) {
	existing, found, err := e.records.FindLatestByScopeForUpdate(ctx, tx, req.Kind, req.ScopeType, req.ScopeID)
	if err != nil {
		return Record{}, err
	}
	if found && !existing.Status.Unresolved() {
		if req.Status != StatusMismatch {
			return existing, nil
		}
		found = false // a fresh divergence after a resolution needs its own record
	}
	if !found {
		rec, err := e.records.Open(ctx, tx, req)
		if err != nil {
			return Record{}, err
		}
		if rec.Status == StatusMismatch {
			e.metrics.mismatchOpened(ctx, rec)
		}
		return rec, nil
	}
	updated, err := e.records.UpdateObservation(ctx, tx, existing.ID, req.Expected, req.Observed, req.Difference,
		req.Material, req.BlocksNewRisk)
	if err != nil {
		return Record{}, err
	}
	// An OPEN record may still be classified; anything past OPEN keeps its
	// status until a resolver moves it.
	if updated.Status == StatusOpen && (req.Status == StatusMatched || req.Status == StatusMismatch) {
		classified, err := e.records.Transition(ctx, tx, updated.ID, req.Status, TransitionEvidence{
			Actor: req.Actor, Reason: req.Reason, EvidenceRef: req.EvidenceRef,
		})
		if err != nil {
			return Record{}, err
		}
		if classified.Status == StatusMismatch {
			e.metrics.mismatchOpened(ctx, classified)
		}
		return classified, nil
	}
	return updated, nil
}

// ReconcileAttempt is the EVENT_DRIVEN execution path for one attempt
// (PARTS 48, 49). It never submits anything and never releases a reservation
// that an order still needs.
func (e *Engine) ReconcileAttempt(ctx context.Context, attemptID execution.AttemptID) (Record, error) {
	out, err := e.RecoverAttempt(ctx, attemptID)
	return out.Record, err
}

// ReconcileOrder reconciles every recoverable attempt of an order and returns
// the records produced, oldest attempt first.
func (e *Engine) ReconcileOrder(ctx context.Context, orderID execution.OrderID) ([]Record, error) {
	if e.attempts == nil {
		return nil, errs.New(errs.CodeInternal, "reconciliation: no attempt repository is configured")
	}
	list, err := e.attempts.ListForOrder(ctx, e.db, orderID)
	if err != nil {
		return nil, err
	}
	out := []Record{}
	for _, a := range list {
		if !a.Status.Recoverable() {
			continue
		}
		res, err := e.RecoverAttempt(ctx, a.ID)
		if err != nil {
			return out, err
		}
		out = append(out, res.Record)
	}
	return out, nil
}

// RunPeriodic sweeps the attempts whose fate is still unknown, oldest first,
// and reconciles each one. It is bounded by limit so a backlog cannot starve
// the worker loop, and it is safe to run concurrently with itself: each
// attempt is reconciled in its own transaction under the order's row lock.
func (e *Engine) RunPeriodic(ctx context.Context, limit int) ([]Record, error) {
	if e.attempts == nil {
		return nil, errs.New(errs.CodeInternal, "reconciliation: no attempt repository is configured")
	}
	if limit <= 0 {
		limit = 100
	}
	pending, err := e.attempts.ListRecoverable(ctx, e.db, limit)
	if err != nil {
		return nil, err
	}
	out := []Record{}
	for _, a := range pending {
		res, err := e.RecoverAttempt(ctx, a.ID)
		if err != nil {
			e.log.ErrorContext(ctx, "reconciliation: attempt sweep failed",
				slog.String("attempt_id", a.ID.String()), slog.String("error", err.Error()))
			continue
		}
		out = append(out, res.Record)
	}
	return out, nil
}

// RunFull compares every wallet balance of an account with the chain, and Σ
// open lots with the WALLET ledger balance (PART 50 "full balance
// reconciliation"). It returns one record per (wallet, asset) compared plus
// the position records.
func (e *Engine) RunFull(ctx context.Context, accountID accounts.AccountID) ([]Record, error) {
	if accountID.IsZero() {
		return nil, errs.New(errs.CodeValidationFailed, "reconciliation: account id is required")
	}
	out := []Record{}
	balances, err := e.reconcileWalletBalances(ctx, accountID)
	if err != nil {
		return out, err
	}
	out = append(out, balances...)
	pos, err := e.verifyPositions(ctx, accountID)
	if err != nil {
		return out, err
	}
	return append(out, pos...), nil
}

// reconcileWalletBalances compares, for every wallet of the account and every
// asset the account holds, the WALLET ledger balance with the balance both
// chain observers agree on (RECONCILIATION.md §4).
func (e *Engine) reconcileWalletBalances(ctx context.Context, accountID accounts.AccountID) ([]Record, error) {
	if e.ledger == nil {
		return nil, errs.New(errs.CodeInternal, "reconciliation: no ledger is configured")
	}
	if e.observers.Primary == nil {
		return nil, errs.New(errs.CodeInternal, "reconciliation: no chain observer is configured")
	}
	wallets, err := e.wallets.ListForAccount(ctx, e.db, accountID)
	if err != nil {
		return nil, err
	}
	if len(wallets) == 0 {
		return []Record{}, nil
	}
	held, err := e.ledger.BalancesForOwner(ctx, e.db, ledgerOwnerCustomer, accountID.String())
	if err != nil {
		return nil, err
	}
	wanted := map[assets.AssetID]money.Quantity{}
	for _, b := range held {
		if b.Account.Code == ledgerCodeWallet {
			wanted[b.Account.AssetID] = b.Balance
		}
	}
	out := []Record{}
	for _, w := range wallets {
		for asset, expected := range wanted {
			rec, err := e.reconcileWalletAsset(ctx, w, asset, expected)
			if err != nil {
				return out, err
			}
			out = append(out, rec)
		}
	}
	return out, nil
}

// reconcileWalletAsset compares one (wallet, asset) pair.
func (e *Engine) reconcileWalletAsset(ctx context.Context, w Wallet, asset assets.AssetID, expected money.Quantity) (Record, error) {
	a, err := e.asset(ctx, asset)
	if err != nil {
		return Record{}, err
	}
	mint := a.MintAddress
	primary, perr := e.observers.Primary.GetBalances(ctx, w.Address, []string{mint})
	var resolution chain.BalanceResolution
	switch {
	case perr != nil && e.observers.Secondary == nil:
		return Record{}, errs.Wrap(perr, errs.CodeProviderUnavailable, "reconciliation: balance observation failed")
	case e.observers.Secondary == nil:
		resolution = e.observers.Policy.ResolveBalancesSingle(primary, chain.SidePrimary, "secondary observer not configured")
	default:
		secondary, serr := e.observers.Secondary.GetBalances(ctx, w.Address, []string{mint})
		switch {
		case perr != nil && serr != nil:
			return Record{}, errs.Wrap(perr, errs.CodeProviderUnavailable, "reconciliation: no observer answered")
		case perr != nil:
			resolution = e.observers.Policy.ResolveBalancesSingle(secondary, chain.SideSecondary, "primary unavailable: "+perr.Error())
		case serr != nil:
			resolution = e.observers.Policy.ResolveBalancesSingle(primary, chain.SidePrimary, "secondary unavailable: "+serr.Error())
		default:
			resolution = e.observers.Policy.ResolveBalances(primary, secondary)
		}
	}

	observed := money.QuantityFromInt64(0)
	for _, b := range resolution.Balances {
		if b.Mint == mint {
			observed = observed.Add(b.Amount)
		}
	}
	now := e.clk.Now()
	for _, b := range resolution.Balances {
		// A chain balance is never negative. An observer that reports one is
		// broken: the reading is kept as record evidence but not stored as an
		// observation (the column forbids it), and the comparison continues so
		// one bad observer cannot stop the sweep.
		if b.Mint != mint || b.Amount.IsNegative() {
			continue
		}
		if _, err := e.records.RecordBalanceObservation(ctx, e.db, BalanceObservation{
			WalletID: w.ID, AssetID: asset, Quantity: b.Amount, Source: b.Source,
			Slot: slotPtr(b.Slot), ObservedAt: b.ObservedAt, RawRef: b.RawRef,
		}); err != nil {
			return Record{}, err
		}
	}
	diff := observed.Sub(expected)
	usd := e.valueUSD(ctx, a, diff, now)

	req := OpenRequest{
		Kind: KindWalletBalance, Mode: ModeFull, ScopeType: ScopeWallet, ScopeID: w.ID + ":" + asset.String(),
		AccountID: w.AccountID, AssetID: asset,
		Expected: map[string]any{
			"source": "ledger.WALLET", "quantity": expected.String(),
			"decimal": expected.ToDecimalString(a.Decimals), "asset": a.Symbol,
		},
		Observed: map[string]any{
			"source": "chain", "state": string(resolution.State), "degraded": resolution.Degraded,
			"quantity": observed.String(), "decimal": observed.ToDecimalString(a.Decimals),
			"slot_skew": resolution.SlotSkew, "detail": resolution.Detail, "wallet_address": w.Address,
		},
		Difference: differenceDoc(diff, a.Decimals, usd, resolution.Differences),
		Actor:      SystemActor(), Reason: "full balance reconciliation",
	}
	switch {
	case resolution.State == chain.Disagreed:
		// PART 196: never pick the optimistic answer; block dependent activity.
		req.Status, req.Material, req.BlocksNewRisk = StatusMismatch, true, true
	case diff.IsZero():
		req.Status = StatusMatched
	default:
		req.Status = StatusMismatch
		req.Material = e.policy.Material(KindWalletBalance, asset, diff, usd)
		req.BlocksNewRisk = req.Material
	}
	var rec Record
	err = e.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = e.upsert(ctx, tx, req)
		return err
	})
	return rec, err
}

func slotPtr(slot uint64) *int64 {
	if slot == 0 || slot > math.MaxInt64 {
		return nil
	}
	s := int64(slot)
	return &s
}

// differenceDoc renders the operator-facing difference document.
func differenceDoc(diff money.Quantity, decimals uint8, usd *money.USD, fields []string) map[string]any {
	doc := map[string]any{
		"quantity": diff.String(),
		"decimal":  diff.ToDecimalString(decimals),
		"sign":     diff.Sign(),
	}
	if usd != nil {
		doc["usd_minor"] = usd.Minor()
		doc["usd"] = usd.String()
	}
	if len(fields) > 0 {
		doc["fields"] = fields
	}
	return doc
}

// asset reads an asset from the registry, or returns a minimal record when no
// registry is configured (evidence rendering only; no arithmetic depends on
// it).
func (e *Engine) asset(ctx context.Context, assetID assets.AssetID) (assets.Asset, error) {
	if e.assets == nil {
		return assets.Asset{ID: assetID}, nil
	}
	return e.assets.Get(ctx, e.db, assetID)
}

// valueUSD converts a quantity into USD for the materiality test. A
// USD-pegged stablecoin converts exactly with no price feed; anything else
// needs the injected valuer. A nil result means "not valued", and
// Policy.Material then falls back to the dust thresholds.
func (e *Engine) valueUSD(ctx context.Context, a assets.Asset, qty money.Quantity, at time.Time) *money.USD {
	if qty.IsZero() {
		z := money.USDFromMinor(0)
		return &z
	}
	if a.IsStablecoin && a.PegCurrency == "USD" {
		if v, err := money.QuoteQuantityToUSD(qty, a.Decimals, money.RoundHalfEven); err == nil {
			return &v
		}
	}
	if e.valuer == nil || a.ID.IsZero() {
		return nil
	}
	v, err := e.valuer.ValueUSD(ctx, e.db, a.ID, qty, at)
	if err != nil {
		return nil
	}
	return &v
}

// observability wiring for the SEV1 counters lives in metrics.go; these two
// constants keep the ledger import surface of engine.go to the types it needs.
const (
	ledgerOwnerCustomer = "CUSTOMER"
	ledgerCodeWallet    = "WALLET"
)
