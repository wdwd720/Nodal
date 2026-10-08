package httpapi

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/capital"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/valuation"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// PortfolioDeps holds what the portfolio reads beyond the ledger and the
// advanced core: the capital service (reservations, withdrawal holds and the
// availability arithmetic) and the reconciliation records. The portfolio only
// reads through them. Both are required, with WireDeps.Ledger and
// AdvancedDeps.Effects: leaving any out leaves /advanced/portfolio UNSUPPORTED
// rather than showing half a portfolio as if it were whole.
type PortfolioDeps struct {
	Capital        *capital.Service
	Reconciliation *reconciliation.Repository
}

func (d PortfolioDeps) complete() bool {
	return d.Capital != nil && d.Reconciliation != nil
}

// portfolioListBound bounds every list the portfolio reads itself, and the
// pages it asks the subsystems' reads for.
const portfolioListBound = 50

// portfolioElsewhereCodes are the ledger codes a portfolio balance reports as
// held outside the wallet, and portfolioOwedCodes those it reports as owed,
// each in the order a balance lists them. Buying power V2 values the same
// codes (capital/buyingpower/v2_load.go).
var (
	portfolioElsewhereCodes = []ledger.Code{
		ledger.CodeMarginCollateral, ledger.CodeDerivativesMargin, ledger.CodeUnsettledReceivable, ledger.CodeBridgeInFlight,
	}
	portfolioOwedCodes = []ledger.Code{ledger.CodeMarginLoan, ledger.CodeDeficit}
)

// portfolioPurposes is what a reservation is for, by the capability of the
// economic intent it serves. A capability it does not name is OTHER and is
// reported under its own name, so a capability added later is shown as
// itself rather than under a purpose it may not have.
var portfolioPurposes = map[gates.Capability]PortfolioPurpose{
	gates.InternalMatching:      PortfolioPurposeOpenOrders,
	gates.Equities:              PortfolioPurposeOpenOrders,
	gates.Derivatives:           PortfolioPurposeOpenOrders,
	gates.SpreadEngine:          PortfolioPurposeOpenOrders,
	gates.P2PTransfers:          PortfolioPurposePendingTransfers,
	gates.CrossChainBuyingPower: PortfolioPurposeBridgeLocks,
	gates.StrategyMarketplace:   PortfolioPurposeStrategy,
	gates.UserCodeSandbox:       PortfolioPurposeStrategy,
	gates.AgentWithdrawals:      PortfolioPurposeWithdrawals,
}

// portfolioPurposeOrder is the order a balance lists its reservations in.
var portfolioPurposeOrder = map[PortfolioPurpose]int{
	PortfolioPurposeOpenOrders: 0, PortfolioPurposePendingTransfers: 1, PortfolioPurposeBridgeLocks: 2,
	PortfolioPurposeStrategy: 3, PortfolioPurposeWithdrawals: 4, PortfolioPurposeOther: 5,
}

// wirePortfolio wires the portfolio's own reads when their dependencies are
// present.
func wirePortfolio(p *Ports, d WireDeps) {
	pd := d.Advanced.Portfolio
	if !pd.complete() || d.Ledger == nil || d.Advanced.Effects == nil {
		return
	}
	accts := d.Accounts
	if accts == nil {
		accts = accounts.NewRepository()
	}
	repo := d.Assets
	if repo == nil {
		repo = assets.NewRepository()
	}
	p.Portfolio = portfolioAdapter{
		db: d.DB, clk: d.Clock, ledger: d.Ledger, capital: pd.Capital, records: pd.Reconciliation,
		// BlocksNewRisk reads the records only; the policy is not consulted.
		blocks:  reconciliation.NewBlocks(pd.Reconciliation, reconciliation.DefaultPolicy()),
		effects: d.Advanced.Effects, prices: d.Advanced.Prices, maxAge: d.Advanced.MaxPriceAge,
		accounts: accts, assets: repo,
	}
}

// portfolioAdapter serves PortfolioPort. Every read runs in one read-only
// REPEATABLE READ snapshot, through the services that own the data: the
// ledger's balances, the capital service's availability and reservations,
// the price store's marks, the reconciliation repository's records and the
// econ effect store's effects. The SQL of its own selects only which rows to
// ask those services about.
type portfolioAdapter struct {
	db       *db.DB
	clk      clock.Clock
	ledger   *ledger.Service
	capital  *capital.Service
	records  *reconciliation.Repository
	blocks   *reconciliation.Blocks
	effects  *econ.EffectStore
	prices   valuation.PriceSource
	maxAge   time.Duration
	accounts *accounts.Repository
	assets   *assets.Repository
}

var _ PortfolioPort = portfolioAdapter{}

func (a portfolioAdapter) Snapshot(ctx context.Context, account accounts.AccountID) (PortfolioSnapshot, error) {
	var out PortfolioSnapshot
	err := a.db.InTx(ctx, db.TxOptions{Isolation: pgx.RepeatableRead, ReadOnly: true}, func(ctx context.Context, tx pgx.Tx) error {
		now := a.clk.Now().UTC()
		acct, err := a.accounts.Get(ctx, tx, account)
		if err != nil {
			return err
		}
		s := PortfolioSnapshot{AccountID: account, AccountStatus: acct.Status, AsOf: now, MaxMarkAge: a.maxAge}
		if s.Quote, s.QuoteProvisioned, err = portfolioQuote(ctx, tx); err != nil {
			return err
		}
		if s.Balances, err = a.balances(ctx, tx, account, s.Quote, s.QuoteProvisioned, now); err != nil {
			return err
		}
		if s.BlocksNewRisk, _, err = a.blocks.BlocksNewRisk(ctx, tx, account); err != nil {
			return err
		}
		if s.Reconciliation, s.ReconciliationTruncated, err = a.unresolvedRecords(ctx, tx, account); err != nil {
			return err
		}
		if s.UnknownEffects, s.UnknownEffectsTruncated, err = a.unknownEffects(ctx, tx, account); err != nil {
			return err
		}
		if s.PredictionMarkets, s.PredictionMarketsTruncated, err = portfolioPredictionMarkets(ctx, tx, account); err != nil {
			return err
		}
		out = s
		return nil
	})
	return out, err
}

// portfolioQuote is the sandbox dollar. A deployment that has not provisioned
// it holds no simulated value it could be stated in: the portfolio still
// answers, names the unit by its well-known identity and values nothing.
func portfolioQuote(ctx context.Context, q db.Querier) (assets.Asset, bool, error) {
	usd, err := econ.SandboxDollar(ctx, q)
	if errs.HasCode(err, errs.CodeNotFound) {
		return assets.Asset{
			ID: econ.SandboxDollarID(), Chain: assets.SandboxChain, Symbol: econ.SandboxDollarSymbol,
			Name: econ.SandboxDollarName, Decimals: econ.SandboxDollarDecimals,
		}, false, nil
	}
	if err != nil {
		return assets.Asset{}, false, err
	}
	return usd, true, nil
}

// portfolioReadsCode reports whether a balance under code is one a portfolio
// balance reports.
func portfolioReadsCode(code ledger.Code) bool {
	if code == ledger.CodeWallet {
		return true
	}
	for _, c := range portfolioElsewhereCodes {
		if c == code {
			return true
		}
	}
	for _, c := range portfolioOwedCodes {
		if c == code {
			return true
		}
	}
	return false
}

// balances reads every SIMULATED asset the account holds: the ledger's own
// balances of the account, by code, and for each asset the capital service's
// availability and ACTIVE reservations and its mark.
func (a portfolioAdapter) balances(ctx context.Context, tx pgx.Tx, account accounts.AccountID, quote assets.Asset, provisioned bool, now time.Time) ([]PortfolioBalance, error) {
	all, err := a.ledger.BalancesForOwner(ctx, tx, ledger.OwnerCustomer, account.String())
	if err != nil {
		return nil, err
	}
	byAsset := map[assets.AssetID]map[ledger.Code]money.Quantity{}
	var held []assets.AssetID
	for _, b := range all {
		if !portfolioReadsCode(b.Account.Code) || b.Balance.IsZero() {
			continue
		}
		codes, ok := byAsset[b.Account.AssetID]
		if !ok {
			codes = map[ledger.Code]money.Quantity{}
			byAsset[b.Account.AssetID] = codes
			held = append(held, b.Account.AssetID)
		}
		codes[b.Account.Code] = b.Balance
	}

	out := make([]PortfolioBalance, 0, len(held))
	for _, id := range held {
		asset, err := a.assets.Get(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		// Real value is the core application's to show; the advanced area
		// reports the SIMULATED domain only.
		if asset.Kind != assets.KindSimulated || asset.ValueDomain != valuedomain.Simulated {
			continue
		}
		codes := byAsset[id]
		b := PortfolioBalance{Asset: asset, Wallet: codes[ledger.CodeWallet], Total: codes[ledger.CodeWallet]}
		avail, err := a.capital.Availability(ctx, tx, account.String(), id, now)
		if err != nil {
			return nil, err
		}
		if avail.WalletBalance.Cmp(b.Wallet) != 0 {
			return nil, errs.New(errs.CodeInternal, "the capital service and the ledger disagree on a wallet balance").
				WithField("asset_id", id.String())
		}
		b.Reserved, b.WithdrawalHolds, b.Available = avail.Reserved, avail.Held, avail.Available
		if b.Reservations, err = a.reservations(ctx, tx, account, id, avail.Reserved); err != nil {
			return nil, err
		}
		var owed money.Quantity
		for _, c := range portfolioElsewhereCodes {
			if q, ok := codes[c]; ok {
				b.Elsewhere = append(b.Elsewhere, PortfolioLedgerBalance{Code: c, Quantity: q})
				b.Total = b.Total.Add(q)
			}
		}
		for _, c := range portfolioOwedCodes {
			if q, ok := codes[c]; ok {
				b.Owed = append(b.Owed, PortfolioLedgerBalance{Code: c, Quantity: q})
				owed = owed.Add(q)
			}
		}
		if b.Valuation, err = a.valuation(ctx, tx, asset, quote, provisioned, b.Total, owed, now); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i].Asset, out[j].Asset
		if x.Symbol != y.Symbol {
			return x.Symbol < y.Symbol
		}
		if x.Chain != y.Chain {
			return x.Chain < y.Chain
		}
		return x.ID.String() < y.ID.String()
	})
	return out, nil
}

// reservations breaks an asset's reserved quantity down by purpose: the
// capital service's ACTIVE reservations, each classed by the capability of
// the economic intent it serves. They must add up to what the capital service
// says is reserved; a breakdown that does not is refused, not shown.
func (a portfolioAdapter) reservations(ctx context.Context, tx pgx.Tx, account accounts.AccountID, asset assets.AssetID, reserved money.Quantity) ([]PortfolioReservation, error) {
	active, err := a.capital.ListActive(ctx, tx, account.String(), asset)
	if err != nil {
		return nil, err
	}
	capabilities, err := portfolioIntentCapabilities(ctx, tx, active)
	if err != nil {
		return nil, err
	}
	type group struct {
		purpose    PortfolioPurpose
		capability string
	}
	byGroup := map[group]*PortfolioReservation{}
	var out []PortfolioReservation
	var sum money.Quantity
	for _, r := range active {
		g := group{purpose: PortfolioPurposeOther}
		switch {
		case r.EconomicIntentID != "":
			capability, ok := capabilities[r.EconomicIntentID]
			if !ok {
				return nil, errs.New(errs.CodeInternal, "a reservation names an economic intent that cannot be read").
					WithField("reservation_id", r.ID.String())
			}
			g.capability = capability
			if purpose, named := portfolioPurposes[gates.Capability(capability)]; named {
				g.purpose = purpose
			}
		case r.IntentID != "":
			// A trade intent is an order to be executed.
			g.purpose = PortfolioPurposeOpenOrders
		}
		remaining := r.Remaining()
		sum = sum.Add(remaining)
		if existing, ok := byGroup[g]; ok {
			existing.Quantity = existing.Quantity.Add(remaining)
			existing.Count++
			continue
		}
		out = append(out, PortfolioReservation{Purpose: g.purpose, Capability: g.capability, Quantity: remaining, Count: 1})
		byGroup[g] = &out[len(out)-1]
	}
	if sum.Cmp(reserved) != 0 {
		return nil, errs.New(errs.CodeInternal, "the asset's reservations do not add up to what the capital service says is reserved").
			WithField("asset_id", asset.String())
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Purpose != out[j].Purpose {
			return portfolioPurposeOrder[out[i].Purpose] < portfolioPurposeOrder[out[j].Purpose]
		}
		return out[i].Capability < out[j].Capability
	})
	return out, nil
}

// portfolioIntentCapabilities reads the capability of every economic intent
// the reservations serve, in one statement.
func portfolioIntentCapabilities(ctx context.Context, q db.Querier, active []capital.Reservation) (map[string]string, error) {
	ids := make([]string, 0, len(active))
	for _, r := range active {
		if r.EconomicIntentID != "" {
			ids = append(ids, r.EconomicIntentID)
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id::text, capability FROM economic_intents WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, capability string
		if err := rows.Scan(&id, &capability); err != nil {
			return nil, err
		}
		out[id] = capability
	}
	return out, rows.Err()
}

// valuation values what the account holds of an asset, and what it owes in
// it, in base units of the sandbox dollar: the sandbox dollar at face, any
// other asset at its latest mark no older than the bound buying power V2
// uses, rounding as buying power V2 does (a holding down, a liability up).
// Without such a mark the asset is UNVALUED, never valued at zero.
func (a portfolioAdapter) valuation(ctx context.Context, q db.Querier, asset, quote assets.Asset, provisioned bool, total, owed money.Quantity, now time.Time) (PortfolioValuation, error) {
	if !provisioned {
		return PortfolioValuation{Status: PortfolioValuationUnvalued, Problem: errs.New(errs.CodeUnsupported,
			"the sandbox dollar is not provisioned in this deployment, so there is nothing to value the asset in")}, nil
	}
	if asset.ID == quote.ID {
		v := total
		out := PortfolioValuation{Status: PortfolioValuationPar, Value: &v}
		if !owed.IsZero() {
			o := owed
			out.OwedValue = &o
		}
		return out, nil
	}
	if a.prices == nil {
		return PortfolioValuation{Status: PortfolioValuationUnvalued, Problem: errs.New(errs.CodeUnsupported,
			"no price source is configured in this deployment, so no asset but the sandbox dollar is valued")}, nil
	}
	mark, err := a.prices.Latest(ctx, q, asset.ID, quote.ID, a.maxAge, now)
	if errs.HasCode(err, errs.CodeStaleMarketData) {
		return PortfolioValuation{Status: PortfolioValuationUnvalued, Problem: errs.Newf(errs.CodeStaleMarketData,
			"no mark of %s in the sandbox dollar within the maximum age (%s)", asset.Symbol, a.maxAge)}, nil
	}
	if err != nil {
		return PortfolioValuation{}, err
	}
	value, err := money.Notional(total, asset.Decimals, mark, quote.Decimals, money.RoundDown)
	if err != nil {
		return PortfolioValuation{}, errs.Wrap(err, errs.CodeInternal, "value a holding at its mark")
	}
	out := PortfolioValuation{Status: PortfolioValuationMarked, Mark: &mark, Value: &value}
	if !owed.IsZero() {
		owedValue, err := money.Notional(owed, asset.Decimals, mark, quote.Decimals, money.RoundUp)
		if err != nil {
			return PortfolioValuation{}, errs.Wrap(err, errs.CodeInternal, "value a liability at its mark")
		}
		out.OwedValue = &owedValue
	}
	return out, nil
}

// unresolvedRecords reads the unresolved reconciliation records on the
// account, oldest first, each through the reconciliation repository.
func (a portfolioAdapter) unresolvedRecords(ctx context.Context, tx pgx.Tx, account accounts.AccountID) ([]reconciliation.Record, bool, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM reconciliation_records
		WHERE account_id = $1 AND status = ANY($2::text[]) ORDER BY opened_at, id LIMIT $3`,
		account, opsUnresolvedStatuses(), portfolioListBound+1)
	if err != nil {
		return nil, false, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[reconciliation.RecordID])
	if err != nil {
		return nil, false, err
	}
	truncated := len(ids) > portfolioListBound
	if truncated {
		ids = ids[:portfolioListBound]
	}
	out := make([]reconciliation.Record, 0, len(ids))
	for _, id := range ids {
		rec, err := a.records.Get(ctx, tx, id)
		if err != nil {
			return nil, false, err
		}
		out = append(out, rec)
	}
	return out, truncated, nil
}

// unknownEffects reads the account's economic effects whose outcome is
// UNKNOWN, oldest first, each through the econ effect store.
func (a portfolioAdapter) unknownEffects(ctx context.Context, tx pgx.Tx, account accounts.AccountID) ([]econ.Effect, bool, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM economic_effects
		WHERE account_id = $1 AND status = $2 ORDER BY updated_at, id LIMIT $3`,
		account.String(), string(econ.EffectUnknown), portfolioListBound+1)
	if err != nil {
		return nil, false, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[econ.EffectID])
	if err != nil {
		return nil, false, err
	}
	truncated := len(ids) > portfolioListBound
	if truncated {
		ids = ids[:portfolioListBound]
	}
	out := make([]econ.Effect, 0, len(ids))
	for _, id := range ids {
		e, err := a.effects.Get(ctx, tx, id)
		if err != nil {
			return nil, false, err
		}
		out = append(out, e)
	}
	return out, truncated, nil
}

// portfolioPredictionMarkets lists the markets in which the account holds
// outcome shares (the prediction_positions projection of the ledger) or is
// owed a held payout it has not claimed.
func portfolioPredictionMarkets(ctx context.Context, q db.Querier, account accounts.AccountID) ([]predmarket.MarketID, bool, error) {
	rows, err := q.Query(ctx, `SELECT market_id FROM (
			SELECT market_id FROM prediction_positions WHERE account_id = $1
			UNION
			SELECT h.market_id FROM prediction_held_payouts h
			 WHERE h.account_id = $1
			   AND NOT EXISTS (SELECT 1 FROM prediction_held_payout_claims c WHERE c.held_payout_id = h.id)
		) m ORDER BY market_id LIMIT $2`, account, portfolioListBound+1)
	if err != nil {
		return nil, false, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[predmarket.MarketID])
	if err != nil {
		return nil, false, err
	}
	truncated := len(ids) > portfolioListBound
	if truncated {
		ids = ids[:portfolioListBound]
	}
	return ids, truncated, nil
}
