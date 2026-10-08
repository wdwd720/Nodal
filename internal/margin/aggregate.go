package margin

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/money"
)

// Aggregate is the margin book's standing across its open accounts, for
// operator metrics. It is read-only and recomputed from the engine's own
// evaluation of each account; nothing here is stored or decides anything.
//
// Figures are USD at par of each account's numeraire, rounded against the
// book: exposure up, equity and buffer down. An account whose evaluation
// fails, or whose evaluation rests on a stale, missing or future mark
// (PricesFresh false), is counted in Unvalued and left out of the sums rather
// than valued at a price the engine itself would not act on.
type Aggregate struct {
	Accounts          int
	Unvalued          int
	GrossExposure     money.USD
	Equity            money.USD
	LiquidationBuffer money.USD
}

// LeverageBPS is aggregate gross exposure over aggregate equity in basis
// points, rounded up; ok is false when equity is not positive.
func (a Aggregate) LeverageBPS() (money.BPS, bool) {
	if !a.Equity.IsPositive() {
		return 0, false
	}
	num := a.GrossExposure.Minor() * int64(money.OneHundredPercent)
	den := a.Equity.Minor()
	return money.BPS((num + den - 1) / den), true
}

// Aggregate evaluates up to limit open margin accounts of this service's
// policy and sums them.
func (s *Service) Aggregate(ctx context.Context, q db.Querier, limit int) (Aggregate, error) {
	if limit <= 0 || limit > s.cfg.MaxQueueScan {
		limit = s.cfg.MaxQueueScan
	}
	rows, err := q.Query(ctx, `SELECT account_id FROM margin_accounts WHERE status IN ('NORMAL','WARNING','MARGIN_CALL','LIQUIDATING')
		AND policy_version = $1 ORDER BY id LIMIT $2`, s.policy.Version, limit)
	if err != nil {
		return Aggregate{}, dbErr("list accounts to aggregate", err)
	}
	var ids []accounts.AccountID
	for rows.Next() {
		var id accounts.AccountID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return Aggregate{}, dbErr("scan account to aggregate", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Aggregate{}, dbErr("list accounts to aggregate", err)
	}
	var out Aggregate
	now := s.now()
	for _, id := range ids {
		out.Accounts++
		a, err := s.st.accountByCustomer(ctx, q, id, false)
		if err != nil {
			out.Unvalued++
			continue
		}
		st, _, err := s.loadState(ctx, q, a, now)
		if err != nil {
			out.Unvalued++
			continue
		}
		e, err := Evaluate(s.policy, st)
		if err != nil || !e.PricesFresh {
			out.Unvalued++
			continue
		}
		exposure, err1 := money.QuoteQuantityToUSD(e.GrossExposure, e.NumeraireDecimals, money.RoundCeil)
		equity, err2 := money.QuoteQuantityToUSD(e.Equity, e.NumeraireDecimals, money.RoundFloor)
		buffer, err3 := money.QuoteQuantityToUSD(e.LiquidationBuffer, e.NumeraireDecimals, money.RoundFloor)
		if err1 != nil || err2 != nil || err3 != nil {
			out.Unvalued++
			continue
		}
		sum := func(acc *money.USD, v money.USD) error {
			n, err := acc.Add(v)
			if err != nil {
				return err
			}
			*acc = n
			return nil
		}
		if err := sum(&out.GrossExposure, exposure); err != nil {
			return Aggregate{}, err
		}
		if err := sum(&out.Equity, equity); err != nil {
			return Aggregate{}, err
		}
		if err := sum(&out.LiquidationBuffer, buffer); err != nil {
			return Aggregate{}, err
		}
	}
	return out, nil
}
