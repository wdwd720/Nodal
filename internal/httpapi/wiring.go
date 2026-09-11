package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/idempotency"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/positions"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/withdrawal"
)

// WireDeps are the real domain services Wire adapts to the handler ports. It
// is the "wiring helper" half of this package: every adapter method below is
// one domain call plus the shape change the port declares. A service the
// deployment does not have stays nil and its operations answer UNSUPPORTED.
type WireDeps struct {
	DB    *db.DB
	Clock clock.Clock
	Env   config.Environment

	Identity     IdentityPort
	Sessions     *auth.Manager
	Accounts     *accounts.Repository
	Assets       *assets.Repository
	Instruments  *instruments.Repository
	Ledger       *ledger.Service
	Positions    *positions.Engine
	BuyingPower  *buyingpower.Engine
	Intents      *intent.Service
	IntentRepo   *intent.Repository
	Orders       *execution.Repository
	Attempts     *execution.AttemptRepository
	Funding      *funding.Service
	Withdrawals  *withdrawal.Service
	Gates        *gates.Admin
	GateChecker  *gates.Checker
	KillSwitches *killswitch.Controller
	AdminActions *admin.Service
	Idempotency  *idempotency.Store
	Quotes       QuotePort
	Reconcile    ReconciliationPort
	Providers    *provider.Registry

	// FundingSettlement describes the asset a deposit must deliver. It comes
	// from the asset registry, never from the client.
	FundingSettlement FundingSettlement
	// ProviderCatalog names each configured provider slot for the admin
	// status view.
	ProviderCatalog []ProviderDescriptor
	// AdminExecutors maps an admin action kind to its executor. A kind with
	// no executor cannot be executed over HTTP; the action stays APPROVED
	// until an operator runs its own tool.
	AdminExecutors map[admin.Kind]admin.ExecFunc

	// NativeEconomy holds the internal-economy services. Every field is
	// optional: a deployment that has not provisioned the Nodal-native economy
	// leaves them nil and those routes answer UNSUPPORTED.
	NativeEconomy NativeEconomyDeps

	// Withdrawal holds the verification, eligibility and conversion-request
	// services of goal PARTS 19-25. Every field is optional for the same
	// reason: a deployment with no identity vendor has no verification routes,
	// which is the honest state of one with no contract.
	Withdrawal WithdrawalDeps

	IdempotencyTTL time.Duration
}

// FundingSettlement is the deposit destination the platform accepts.
type FundingSettlement struct {
	AssetID  assets.AssetID
	Network  string // provider network name, e.g. "solana"
	Currency string // provider currency name, e.g. "usdc"
}

// ProviderDescriptor is one configured provider slot.
type ProviderDescriptor struct {
	Name         string
	Role         string
	Mode         config.ProviderMode
	Verification provider.VerificationLabel
}

// Wire builds the port set. Everything it returns is safe to share across
// requests: no adapter holds per-request state.
func Wire(d WireDeps) (Ports, error) {
	if d.DB == nil {
		return Ports{}, errors.New("httpapi: a database is required")
	}
	if d.Clock == nil {
		d.Clock = clock.System()
	}
	if d.IdempotencyTTL <= 0 {
		d.IdempotencyTTL = DefaultIdempotencyTTL
	}
	rm := NewReadModel(d.DB)

	p := Ports{
		Identity:       d.Identity,
		Reconciliation: d.Reconcile,
		Quotes:         d.Quotes,
		Health:         healthAdapter{db: d.DB},
	}
	if d.Sessions != nil {
		p.Sessions = sessionsAdapter{mgr: d.Sessions, q: d.DB}
	}
	if d.Accounts != nil {
		p.Accounts = accountsAdapter{repo: d.Accounts, db: d.DB, rm: rm, clk: d.Clock}
	}
	if d.BuyingPower != nil {
		p.BuyingPower = buyingPowerAdapter{engine: d.BuyingPower, q: d.DB}
	}
	if d.Positions != nil && d.BuyingPower != nil {
		p.Holdings = holdingsAdapter{
			positions: d.Positions, buyingPower: d.BuyingPower, rm: rm, q: d.DB, clk: d.Clock,
		}
	}
	if d.Ledger != nil {
		p.Ledger = ledgerAdapter{svc: d.Ledger, q: d.DB}
		p.Export = exportAdapter{ledger: d.Ledger, q: d.DB, clk: d.Clock}
	}
	p.Activity = activityAdapter{rm: rm}
	if d.Assets != nil {
		p.Assets = assetsAdapter{repo: d.Assets, q: d.DB}
	}
	if d.Instruments != nil {
		p.Instruments = instrumentsAdapter{repo: d.Instruments, assets: d.Assets, db: d.DB, q: d.DB}
	}
	if d.Intents != nil && d.IntentRepo != nil {
		p.Intents = intentsAdapter{
			svc: d.Intents, repo: d.IntentRepo, orders: d.Orders, db: d.DB, rm: rm, clk: d.Clock,
		}
	}
	if d.Orders != nil {
		p.Orders = ordersAdapter{repo: d.Orders, attempts: d.Attempts, rm: rm, q: d.DB}
	}
	if d.Funding != nil {
		p.Funding = fundingAdapter{svc: d.Funding, rm: rm, settlement: d.FundingSettlement}
	}
	if d.Withdrawals != nil {
		p.Withdrawals = withdrawalsAdapter{svc: d.Withdrawals, rm: rm}
	}
	if d.Gates != nil && d.GateChecker != nil {
		p.Gates = gatesAdapter{adm: d.Gates, checker: d.GateChecker, db: d.DB, q: d.DB, env: d.GateChecker.Environment()}
	}
	if d.KillSwitches != nil {
		p.KillSwitches = killSwitchesAdapter{ctl: d.KillSwitches, rm: rm, db: d.DB}
	}
	if d.AdminActions != nil {
		p.AdminActions = adminActionsAdapter{
			svc: d.AdminActions, rm: rm, db: d.DB, q: d.DB, executors: d.AdminExecutors,
		}
	}
	if d.Providers != nil || len(d.ProviderCatalog) > 0 {
		p.Providers = providersAdapter{registry: d.Providers, catalog: d.ProviderCatalog, clk: d.Clock}
	}
	if d.Idempotency != nil {
		p.Idempotency = idempotencyAdapter{store: d.Idempotency, db: d.DB}
	}
	wireNativeEconomy(&p, d)
	wireWithdrawal(&p, d)
	return p, nil
}

// --- health -----------------------------------------------------------------

type healthAdapter struct{ db *db.DB }

func (h healthAdapter) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return h.db.Ping(ctx)
}

// --- sessions ---------------------------------------------------------------

type sessionsAdapter struct {
	mgr *auth.Manager
	q   auth.Querier
}

func (s sessionsAdapter) ListForSubject(ctx context.Context, subjectID string) ([]auth.Summary, error) {
	return s.mgr.ListForSubject(ctx, s.q, subjectID)
}

func (s sessionsAdapter) Revoke(ctx context.Context, sessionID string) error {
	return s.mgr.Revoke(ctx, s.q, sessionID)
}

// --- accounts ---------------------------------------------------------------

type accountsAdapter struct {
	repo *accounts.Repository
	db   *db.DB
	rm   *ReadModel
	clk  clock.Clock
}

func (a accountsAdapter) Get(ctx context.Context, accountID accounts.AccountID) (accounts.Account, error) {
	return a.repo.Get(ctx, a.db, accountID)
}

func (a accountsAdapter) ListByOwner(ctx context.Context, owner accounts.UserID) ([]accounts.Account, error) {
	return a.repo.ListByOwner(ctx, a.db, owner)
}

func (a accountsAdapter) Search(ctx context.Context, query, cursor string, limit int) (AccountPage, error) {
	return a.rm.SearchAccounts(ctx, query, cursor, limit)
}

func (a accountsAdapter) Transition(ctx context.Context, accountID accounts.AccountID, ch accounts.StatusChange) (accounts.Account, error) {
	var out accounts.Account
	err := a.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var terr error
		out, terr = a.repo.Transition(ctx, tx, accountID, ch, a.clk.Now())
		return terr
	})
	return out, err
}

// --- buying power -----------------------------------------------------------

type buyingPowerAdapter struct {
	engine *buyingpower.Engine
	q      db.Querier
}

func (b buyingPowerAdapter) Compute(ctx context.Context, accountID accounts.AccountID, purpose buyingpower.Purpose) (buyingpower.BuyingPower, error) {
	return b.engine.ComputeFor(ctx, b.q, accountID, purpose)
}

// --- holdings ---------------------------------------------------------------

type holdingsAdapter struct {
	positions   *positions.Engine
	buyingPower *buyingpower.Engine
	rm          *ReadModel
	q           db.Querier
	clk         clock.Clock
}

// Holdings joins two authoritative answers: the lot basis from
// internal/positions and the USD mark internal/capital/buyingpower already
// computed from internal/valuation. The only arithmetic here is the exact
// subtraction mark − basis; no price, factor or status is recomputed.
func (h holdingsAdapter) Holdings(ctx context.Context, accountID accounts.AccountID) (HoldingsView, error) {
	bp, err := h.buyingPower.ComputeFor(ctx, h.q, accountID, buyingpower.PurposeDisplay)
	if err != nil {
		return HoldingsView{}, err
	}
	lots, err := h.positions.Holdings(ctx, h.q, accountID)
	if err != nil {
		return HoldingsView{}, err
	}
	basis := make(map[assets.AssetID]positions.Holding, len(lots))
	for _, l := range lots {
		basis[l.AssetID] = l
	}
	realized, err := h.positions.RealizedPnL(ctx, h.q, accountID, time.Time{}, h.clk.Now())
	if err != nil {
		return HoldingsView{}, err
	}
	realizedByAsset := make(map[assets.AssetID]money.USD, len(realized.ByAsset))
	for _, r := range realized.ByAsset {
		realizedByAsset[r.AssetID] = r.PnL
	}

	ids := make([]assets.AssetID, 0, len(bp.UnderlyingBalances))
	for _, u := range bp.UnderlyingBalances {
		ids = append(ids, u.AssetID)
	}
	refs, err := h.rm.AssetRefs(ctx, ids)
	if err != nil {
		return HoldingsView{}, err
	}
	custody, err := h.rm.CustodyAddress(ctx, accountID)
	if err != nil {
		return HoldingsView{}, err
	}

	out := HoldingsView{AsOf: bp.AsOf}
	for _, u := range bp.UnderlyingBalances {
		if u.Quantity.IsZero() {
			continue
		}
		hv := HoldingView{
			AssetID:        u.AssetID,
			Symbol:         u.Symbol,
			Decimals:       u.Decimals,
			Quantity:       u.Quantity,
			USDMark:        u.USDValue,
			PriceRef:       u.PriceRef,
			CustodyAddress: custody,
		}
		if r, ok := refs[u.AssetID]; ok {
			hv.Chain = r.Chain
			hv.MintAddress = r.Mint
		}
		if l, ok := basis[u.AssetID]; ok {
			hv.CostBasisUSD = l.CostBasis
		}
		unrealized, serr := u.USDValue.Sub(hv.CostBasisUSD)
		if serr != nil {
			return HoldingsView{}, errs.Wrap(serr, errs.CodeOverflow, "unrealized profit and loss overflowed")
		}
		hv.UnrealizedUSD = unrealized
		if r, ok := realizedByAsset[u.AssetID]; ok {
			v := r
			hv.RealizedUSD = &v
		}
		out.Holdings = append(out.Holdings, hv)
	}
	return out, nil
}

// --- ledger -----------------------------------------------------------------

type ledgerAdapter struct {
	svc *ledger.Service
	q   db.Querier
}

func (l ledgerAdapter) ListTransactions(ctx context.Context, f ledger.TransactionFilter, cursor string, limit int) ([]ledger.Transaction, string, error) {
	return l.svc.ListTransactions(ctx, l.q, f, cursor, limit)
}

// --- activity ---------------------------------------------------------------

type activityAdapter struct{ rm *ReadModel }

func (a activityAdapter) Activity(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) (ActivityPage, error) {
	return a.rm.Activity(ctx, accountID, cursor, limit)
}

// --- export -----------------------------------------------------------------

type exportAdapter struct {
	ledger *ledger.Service
	q      db.Querier
	clk    clock.Clock
}

// Export renders the account's journal history for the window. It reports what
// the ledger holds and computes nothing.
func (e exportAdapter) Export(ctx context.Context, accountID accounts.AccountID, from, to time.Time) (ExportDocument, error) {
	filter := ledger.TransactionFilter{
		OwnerType: ledger.OwnerCustomer,
		OwnerID:   accountID.String(),
		Since:     from,
		Until:     to,
	}
	const exportPageLimit = ledger.MaxListLimit
	var (
		all    []ledger.Transaction
		cursor string
	)
	for {
		batch, next, err := e.ledger.ListTransactions(ctx, e.q, filter, cursor, exportPageLimit)
		if err != nil {
			return ExportDocument{}, err
		}
		all = append(all, batch...)
		if next == "" || len(all) >= 100_000 {
			break
		}
		cursor = next
	}

	doc := ExportDocument{JSON: map[string]any{
		"account_id":   accountID.String(),
		"generated_at": e.clk.Now().UTC().Format(time.RFC3339Nano),
		"from":         formatOptionalTime(from),
		"to":           formatOptionalTime(to),
	}}
	txs := make([]map[string]any, 0, len(all))
	csv := &csvBuilder{}
	csv.row("transaction_id", "kind", "posted_at", "effective_at", "reason_code",
		"seq", "account_code", "asset_id", "side", "quantity", "usd_value")
	for _, t := range all {
		entries := make([]map[string]any, 0, len(t.Entries))
		for _, en := range t.Entries {
			usd := ""
			if en.USDValueMinor != nil {
				usd = money.USDFromMinor(*en.USDValueMinor).String()
			}
			entries = append(entries, map[string]any{
				"seq":          en.Seq,
				"account_code": string(en.Account.Code),
				"asset_id":     en.Account.AssetID.String(),
				"side":         string(en.Side),
				"quantity":     en.Quantity.String(),
				"usd_value":    usd,
			})
			csv.row(t.ID.String(), string(t.Kind),
				t.PostedAt.UTC().Format(time.RFC3339Nano),
				t.EffectiveAt.UTC().Format(time.RFC3339Nano),
				t.ReasonCode, itoa(int(en.Seq)), string(en.Account.Code),
				en.Account.AssetID.String(), string(en.Side), en.Quantity.String(), usd)
		}
		txs = append(txs, map[string]any{
			"id":           t.ID.String(),
			"kind":         string(t.Kind),
			"posted_at":    t.PostedAt.UTC().Format(time.RFC3339Nano),
			"effective_at": t.EffectiveAt.UTC().Format(time.RFC3339Nano),
			"reason_code":  t.ReasonCode,
			"content_hash": hexString(t.ContentHash),
			"entries":      entries,
		})
	}
	doc.JSON["journal_transactions"] = txs
	doc.CSV = csv.bytes()
	return doc, nil
}

func formatOptionalTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --- markets ----------------------------------------------------------------

type assetsAdapter struct {
	repo *assets.Repository
	q    db.Querier
}

func (a assetsAdapter) List(ctx context.Context, limit int) ([]assets.Asset, error) {
	return a.repo.List(ctx, a.q, limit)
}

type instrumentsAdapter struct {
	repo   *instruments.Repository
	assets *assets.Repository
	db     *db.DB
	q      db.Querier
}

func (i instrumentsAdapter) List(ctx context.Context, limit int) ([]instruments.Instrument, error) {
	return i.repo.List(ctx, i.q, limit)
}

func (i instrumentsAdapter) Detail(ctx context.Context, instrumentID instruments.InstrumentID) (InstrumentDetail, error) {
	inst, err := i.repo.Get(ctx, i.q, instrumentID)
	if err != nil {
		return InstrumentDetail{}, err
	}
	listings, err := i.repo.ListingsForInstrument(ctx, i.q, instrumentID)
	if err != nil {
		return InstrumentDetail{}, err
	}
	out := InstrumentDetail{Instrument: inst, Listings: listings}
	if i.assets != nil {
		if inst.BaseAssetID != nil {
			if a, aerr := i.assets.Get(ctx, i.q, *inst.BaseAssetID); aerr == nil {
				out.Base = a
			}
		}
		if inst.QuoteAssetID != nil {
			if a, aerr := i.assets.Get(ctx, i.q, *inst.QuoteAssetID); aerr == nil {
				out.Quote = a
			}
		}
	}
	return out, nil
}

func (i instrumentsAdapter) Transition(ctx context.Context, instrumentID instruments.InstrumentID, ch instruments.StatusChange) (instruments.Instrument, error) {
	var out instruments.Instrument
	err := i.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var terr error
		out, terr = i.repo.TransitionStatus(ctx, tx, instrumentID, ch)
		return terr
	})
	return out, err
}

// --- intents ----------------------------------------------------------------

type intentsAdapter struct {
	svc    *intent.Service
	repo   *intent.Repository
	orders *execution.Repository
	db     *db.DB
	rm     *ReadModel
	clk    clock.Clock
}

func (i intentsAdapter) Submit(ctx context.Context, p security.Principal, req intent.SubmitRequest) (intent.TradeIntent, error) {
	return i.svc.Submit(ctx, i.db, p, req)
}

func (i intentsAdapter) Get(ctx context.Context, intentID intent.IntentID) (intent.TradeIntent, error) {
	return i.repo.Get(ctx, i.db, intentID)
}

func (i intentsAdapter) ListForAccount(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) (intent.Page, error) {
	return i.repo.ListForAccount(ctx, i.db, accountID.String(), cursor, limit)
}

func (i intentsAdapter) Detail(ctx context.Context, intentID intent.IntentID) (IntentDetail, error) {
	t, err := i.repo.Get(ctx, i.db, intentID)
	if err != nil {
		return IntentDetail{}, err
	}
	out := IntentDetail{Intent: t}
	if trs, terr := i.rm.IntentTransitions(ctx, intentID.String()); terr == nil {
		out.Transitions = trs
	}
	if i.orders != nil && t.Links.OrderID != "" {
		if o, oerr := i.orders.GetByIntent(ctx, i.db, intentID.String()); oerr == nil {
			out.Order = &o
		}
	}
	return out, nil
}

// RequestCancel routes to the package that owns cancellation semantics. When
// an order exists, internal/execution records CANCEL_REQUESTED and only
// external confirmation can ever move it to CANCELLED (PART 227). Before an
// order exists, the intent's own transition table decides whether CANCELLED is
// legal; an illegal transition is refused there, not here.
func (i intentsAdapter) RequestCancel(ctx context.Context, p security.Principal, intentID intent.IntentID, idempotencyKey string) (intent.TradeIntent, error) {
	t, err := i.repo.Get(ctx, i.db, intentID)
	if err != nil {
		return intent.TradeIntent{}, err
	}
	evidence := intent.TransitionEvidence{
		ActorType: p.ActorType,
		ActorID:   p.SubjectID,
		Reason:    "cancellation requested by " + string(p.ActorType),
	}
	var out intent.TradeIntent
	err = i.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		if t.Links.OrderID != "" && i.orders != nil {
			orderID, perr := execution.ParseOrderID(t.Links.OrderID)
			if perr != nil {
				return errs.Wrap(perr, errs.CodeInternal, "internal error")
			}
			if _, rerr := i.orders.RequestCancel(ctx, tx, orderID, execution.TransitionEvidence{
				ActorType: string(p.ActorType),
				ActorID:   p.SubjectID,
				Reason:    evidence.Reason,
				RequestID: observability.RequestID(ctx),
			}); rerr != nil {
				return rerr
			}
			out = t
			return nil
		}
		updated, terr := i.repo.Transition(ctx, tx, intentID, intent.StatusCancelled, evidence)
		if terr != nil {
			return terr
		}
		out = updated
		return nil
	})
	return out, err
}

// --- orders -----------------------------------------------------------------

type ordersAdapter struct {
	repo     *execution.Repository
	attempts *execution.AttemptRepository
	rm       *ReadModel
	q        db.Querier
}

func (o ordersAdapter) ListForAccount(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) (OrderPage, error) {
	return o.rm.ListOrders(ctx, accountID, cursor, limit)
}

func (o ordersAdapter) Detail(ctx context.Context, orderID execution.OrderID) (OrderDetail, error) {
	ord, err := o.repo.Get(ctx, o.q, orderID)
	if err != nil {
		return OrderDetail{}, err
	}
	out := OrderDetail{Order: ord}
	if fills, ferr := o.repo.ListFills(ctx, o.q, orderID); ferr == nil {
		out.Fills = fills
	}
	if o.attempts != nil {
		if att, aerr := o.attempts.ListForOrder(ctx, o.q, orderID); aerr == nil {
			out.Attempts = att
		}
	}
	return out, nil
}

// --- funding ----------------------------------------------------------------

type fundingAdapter struct {
	svc        *funding.Service
	rm         *ReadModel
	settlement FundingSettlement
}

func (f fundingAdapter) Start(ctx context.Context, p security.Principal, req StartDeposit) (funding.StartResult, error) {
	if f.settlement.AssetID.IsZero() || f.settlement.Network == "" || f.settlement.Currency == "" {
		return funding.StartResult{}, errNotWired("funding (no settlement asset configured)")
	}
	walletID, address, err := f.rm.Wallet(ctx, req.AccountID)
	if err != nil {
		return funding.StartResult{}, err
	}
	if address == "" {
		return funding.StartResult{}, errs.New(errs.CodeValidationFailed,
			"the account has no active wallet to receive the deposit")
	}
	return f.svc.Start(ctx, p, funding.StartRequest{
		AccountID:           req.AccountID,
		AssetID:             f.settlement.AssetID,
		DestinationNetwork:  f.settlement.Network,
		DestinationCurrency: f.settlement.Currency,
		DestinationAddress:  address,
		DestinationWalletID: walletID,
		FiatCurrency:        req.FiatCurrency,
		FiatAmountMinor:     req.FiatAmountMinor,
		CustomerIPAddress:   req.CustomerIP,
		IdempotencyKey:      req.IdempotencyKey,
		CorrelationID:       req.CorrelationID,
		RequestID:           req.RequestID,
	})
}

func (f fundingAdapter) Get(ctx context.Context, depositID funding.DepositID) (funding.Deposit, error) {
	return f.svc.GetDeposit(ctx, depositID)
}

func (f fundingAdapter) List(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) ([]funding.Deposit, string, error) {
	return f.svc.ListDeposits(ctx, accountID, cursor, limit)
}

func (f fundingAdapter) Detail(ctx context.Context, depositID funding.DepositID) (DepositDetail, error) {
	d, err := f.svc.GetDeposit(ctx, depositID)
	if err != nil {
		return DepositDetail{}, err
	}
	out := DepositDetail{Deposit: d}
	if trs, terr := f.rm.DepositTransitions(ctx, depositID.String()); terr == nil {
		out.Transitions = trs
	}
	return out, nil
}

// --- withdrawals ------------------------------------------------------------

type withdrawalsAdapter struct {
	svc *withdrawal.Service
	rm  *ReadModel
}

func (w withdrawalsAdapter) Request(ctx context.Context, p security.Principal, req WithdrawalRequest) (withdrawal.Withdrawal, error) {
	actor, err := withdrawal.HumanFrom(p)
	if err != nil {
		return withdrawal.Withdrawal{}, err
	}
	_, source, err := w.rm.Wallet(ctx, req.AccountID)
	if err != nil {
		return withdrawal.Withdrawal{}, err
	}
	return w.svc.Request(ctx, actor, withdrawal.Request{
		AccountID:           req.AccountID,
		AssetID:             req.AssetID,
		Quantity:            req.Quantity,
		DestinationType:     withdrawal.DestinationExternalAddress,
		DestinationAddress:  req.DestinationAddress,
		SourceWalletAddress: source,
		IdempotencyKey:      req.IdempotencyKey,
		CorrelationID:       req.CorrelationID,
		RequestID:           req.RequestID,
	})
}

// --- gates ------------------------------------------------------------------

type gatesAdapter struct {
	adm     *gates.Admin
	checker *gates.Checker
	db      *db.DB
	q       db.Querier
	env     string
}

func (g gatesAdapter) List(ctx context.Context) ([]GateView, error) {
	rows, err := gates.List(ctx, g.q, g.env)
	if err != nil {
		return nil, err
	}
	out := make([]GateView, 0, len(rows))
	for _, row := range rows {
		v, verr := g.checker.IsActive(ctx, g.q, row.Capability)
		if verr != nil {
			return nil, verr
		}
		out = append(out, GateView{Gate: row, Verdict: v})
	}
	return out, nil
}

func (g gatesAdapter) Act(ctx context.Context, capability gates.Capability, action GateAction, req gates.Proposal, note string) (GateView, error) {
	var gate gates.Gate
	err := g.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		switch action {
		case GateActionPropose:
			gate, aerr = g.adm.Propose(ctx, tx, capability, req)
		case GateActionApprove:
			gate, aerr = g.adm.Approve(ctx, tx, capability, note)
		case GateActionActivate:
			gate, aerr = g.adm.Activate(ctx, tx, capability, note)
		case GateActionSuspend:
			gate, aerr = g.adm.Suspend(ctx, tx, capability, note)
		case GateActionResume:
			gate, aerr = g.adm.Resume(ctx, tx, capability, note)
		case GateActionRevoke:
			gate, aerr = g.adm.Revoke(ctx, tx, capability, note)
		case GateActionSandbox:
			gate, aerr = g.adm.Sandbox(ctx, tx, capability, note)
		case GateActionUnsandbox:
			gate, aerr = g.adm.Unsandbox(ctx, tx, capability, note)
		default:
			aerr = validationError("action", "unknown gate action")
		}
		return aerr
	})
	if err != nil {
		return GateView{}, err
	}
	v, err := g.checker.IsActive(ctx, g.q, capability)
	if err != nil {
		return GateView{}, err
	}
	return GateView{Gate: gate, Verdict: v}, nil
}

// --- kill switches ----------------------------------------------------------

type killSwitchesAdapter struct {
	ctl *killswitch.Controller
	rm  *ReadModel
	db  *db.DB
}

func (k killSwitchesAdapter) List(ctx context.Context) ([]killswitch.Switch, error) {
	return k.rm.ListKillSwitches(ctx)
}

func (k killSwitchesAdapter) Activate(ctx context.Context, kind killswitch.Kind, scope, reason string) (killswitch.Switch, error) {
	var out killswitch.Switch
	err := k.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		out, aerr = k.ctl.Activate(ctx, tx, kind, scope, reason)
		return aerr
	})
	return out, err
}

func (k killSwitchesAdapter) Release(ctx context.Context, kind killswitch.Kind, scope, reason string, approvalID *string) (killswitch.Switch, error) {
	var out killswitch.Switch
	err := k.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		out, rerr = k.ctl.Release(ctx, tx, kind, scope, reason, approvalID)
		return rerr
	})
	return out, err
}

// --- admin actions ----------------------------------------------------------

type adminActionsAdapter struct {
	svc       *admin.Service
	rm        *ReadModel
	db        *db.DB
	q         db.Querier
	executors map[admin.Kind]admin.ExecFunc
}

func (a adminActionsAdapter) List(ctx context.Context, status, cursor string, limit int) (AdminActionPage, error) {
	return a.rm.ListAdminActions(ctx, status, cursor, limit)
}

func (a adminActionsAdapter) Propose(ctx context.Context, p admin.Proposal) (admin.Action, error) {
	var out admin.Action
	err := a.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var perr error
		out, perr = a.svc.Propose(ctx, tx, p)
		return perr
	})
	return out, err
}

func (a adminActionsAdapter) Approve(ctx context.Context, actionID, note string) (admin.Action, error) {
	var out admin.Action
	err := a.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var aerr error
		out, aerr = a.svc.Approve(ctx, tx, actionID, note)
		return aerr
	})
	return out, err
}

func (a adminActionsAdapter) Reject(ctx context.Context, actionID, reason string) (admin.Action, error) {
	var out admin.Action
	err := a.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		out, rerr = a.svc.Reject(ctx, tx, actionID, reason)
		return rerr
	})
	return out, err
}

// Execute runs the executor registered for the action's kind. A kind with no
// registered executor is refused: the HTTP surface never invents an effect for
// an approved action it does not know how to perform.
func (a adminActionsAdapter) Execute(ctx context.Context, actionID string) (admin.Action, error) {
	current, err := a.svc.Get(ctx, a.q, actionID)
	if err != nil {
		return admin.Action{}, err
	}
	exec, ok := a.executors[current.Kind]
	if !ok || exec == nil {
		return admin.Action{}, errs.Newf(errs.CodeUnsupported,
			"this deployment cannot execute %s actions over the API", current.Kind)
	}
	var out admin.Action
	err = a.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var eerr error
		out, eerr = a.svc.Execute(ctx, tx, actionID, exec)
		return eerr
	})
	return out, err
}

// --- providers --------------------------------------------------------------

type providersAdapter struct {
	registry *provider.Registry
	catalog  []ProviderDescriptor
	clk      clock.Clock
}

func (p providersAdapter) List(_ context.Context) ([]ProviderView, error) {
	now := p.clk.Now()
	out := make([]ProviderView, 0, len(p.catalog))
	for _, d := range p.catalog {
		v := ProviderView{
			Name:         d.Name,
			Role:         d.Role,
			Mode:         string(d.Mode),
			Verification: string(d.Verification),
			Health:       string(provider.Disabled),
		}
		if p.registry != nil {
			if t, ok := p.registry.Get(d.Name); ok {
				snap := t.Snapshot(now)
				v.Health = string(snap.Health)
				v.ErrorRateBPS = snap.ErrorRateBPS
				v.P95Millis = snap.P95.Milliseconds()
				v.LastSuccessAt = snap.LastSuccessAt
				v.DisableReason = snap.DisableReason
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// --- idempotency ------------------------------------------------------------

type idempotencyAdapter struct {
	store *idempotency.Store
	db    *db.DB
}

// Run implements the PART 36 contract on top of internal/idempotency: claim
// the key in its own transaction, run the command in its own, then record the
// outcome. The command is never nested inside the claiming transaction, so a
// long command cannot hold the key row's lock.
func (i idempotencyAdapter) Run(ctx context.Context, cmd IdempotentCommand, fn func(context.Context) (CommandResult, error)) (CommandResult, error) {
	var begun idempotency.Begun
	if err := i.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var berr error
		begun, berr = i.store.Begin(ctx, tx, cmd.ActorID, cmd.Endpoint, cmd.Key, cmd.RequestHash, cmd.TTL)
		return berr
	}); err != nil {
		return CommandResult{}, err
	}

	switch b := begun.(type) {
	case idempotency.Replay:
		return CommandResult{
			Status:       b.ResponseStatus,
			ResourceType: b.ResourceType,
			ResourceID:   b.ResourceID,
			Body:         b.ResponseBody,
			Replayed:     true,
		}, nil
	case idempotency.InProgress:
		return CommandResult{}, errs.New(errs.CodeIdempotencyInProgress,
			"a request with this Idempotency-Key is still being processed").
			WithRetryAfter(time.Second)
	case idempotency.Acquired:
		// fall through and run the command
	default:
		return CommandResult{}, errs.New(errs.CodeInternal, "internal error")
	}

	res, runErr := fn(ctx)
	if runErr != nil {
		code := errs.CodeOf(runErr)
		if idempotencyOutcomeIsConclusion(res.Status, code) {
			// A definite rejection: record it so a retry with the same key
			// replays the same answer instead of running the command again.
			i.record(ctx, cmd, res)
		} else {
			i.fail(ctx, cmd, res)
		}
		return CommandResult{}, runErr
	}
	if err := i.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return i.store.Complete(ctx, tx, cmd.ActorID, cmd.Endpoint, cmd.Key,
			res.Status, res.ResourceType, res.ResourceID, res.Body)
	}); err != nil {
		return CommandResult{}, err
	}
	return res, nil
}

func (i idempotencyAdapter) record(ctx context.Context, cmd IdempotentCommand, res CommandResult) {
	_ = i.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return i.store.Complete(ctx, tx, cmd.ActorID, cmd.Endpoint, cmd.Key, res.Status, "", "", res.Body)
	})
}

func (i idempotencyAdapter) fail(ctx context.Context, cmd IdempotentCommand, res CommandResult) {
	status := res.Status
	if status == 0 {
		status = 500
	}
	_ = i.db.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		return i.store.Fail(ctx, tx, cmd.ActorID, cmd.Endpoint, cmd.Key, status, res.Body)
	})
}

// csvBuilder renders a minimal RFC 4180 document.
type csvBuilder struct{ out []byte }

func (c *csvBuilder) row(cells ...string) {
	for i, cell := range cells {
		if i > 0 {
			c.out = append(c.out, ',')
		}
		c.out = append(c.out, '"')
		for j := 0; j < len(cell); j++ {
			if cell[j] == '"' {
				c.out = append(c.out, '"')
			}
			c.out = append(c.out, cell[j])
		}
		c.out = append(c.out, '"')
	}
	c.out = append(c.out, '\r', '\n')
}

func (c *csvBuilder) bytes() []byte { return c.out }

// jsonObject is a convenience for the export document.
var _ = json.Marshal
