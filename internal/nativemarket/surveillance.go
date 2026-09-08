package nativemarket

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Market integrity (gola.md PART XVI).
//
// An automated market maker has no order book, so several of the controls PART
// XVI lists have no direct analogue and saying so is more useful than
// pretending otherwise:
//
//   - Self-trade prevention has nothing to prevent. Every trade's counterparty
//     is the pool; there is no resting order from another account to cross
//     with, so an account cannot trade with itself.
//   - Front-running protection likewise: there is no queue to jump. Ordering is
//     the transaction order the database imposes, and a trade is priced against
//     the state it commits against, which is the strongest form of "no
//     ordering advantage" available.
//
// What DOES happen in an AMM, and what is detected here:
//
//   - Rapid round tripping: buy then sell within seconds, which manufactures
//     volume and moves a price chart without taking real risk. This is the
//     honest analogue of wash trading in this venue.
//   - Wash trading ACROSS ACCOUNTS, where the accounts are provably under one
//     controlled actor: two accounts owned by the same user taking opposite
//     sides of one market inside the window. PART LXXII asks for this "where
//     detectable", and that qualifier is doing real work — shared ownership is
//     a fact in the accounts table, while coordination between two people who
//     merely know each other is not visible here at all. The detector claims
//     only what it can see, and its detail says so.
//   - Creator self-dealing: the creator trading their own asset. It is not
//     forbidden — a creator may legitimately want a position — but it is the
//     single most important thing for a buyer to be able to see.
//   - Concentration: one account holding enough of the supply that their exit
//     is the market's price.
//   - Anomalous volume: a trade that is large relative to the pool.
//
// Every detector RAISES AN ALERT and none of them blocks. A heuristic that
// halts a market is a denial-of-service vector against creators, and the
// heuristics here are exactly the kind that produce false positives. Blocking
// is reserved for states an operator or the asset's lifecycle chose — HALTED,
// FROZEN, CLOSE_ONLY — which the database enforces on every fill.

// AlertKind names a surveillance finding.
type AlertKind string

// Alert kinds. They match the database's CHECK constraint.
const (
	AlertSelfTrade               AlertKind = "SELF_TRADE"
	AlertWashTrade               AlertKind = "WASH_TRADE"
	AlertRapidRoundTrip          AlertKind = "RAPID_ROUND_TRIP"
	AlertConcentration           AlertKind = "CONCENTRATION"
	AlertCreatorSelfDealing      AlertKind = "CREATOR_SELF_DEALING"
	AlertAnomalousVolume         AlertKind = "ANOMALOUS_VOLUME"
	AlertQuoteSpam               AlertKind = "QUOTE_SPAM"
	AlertCoordinatedAccumulation AlertKind = "COORDINATED_ACCUMULATION"
)

// AlertSeverity is how much attention a finding deserves.
type AlertSeverity string

// Alert severities.
const (
	SeverityInfo     AlertSeverity = "INFO"
	SeverityWarn     AlertSeverity = "WARN"
	SeverityCritical AlertSeverity = "CRITICAL"
)

// Alert is one surveillance finding.
type Alert struct {
	ID        AlertID
	MarketID  MarketID
	AccountID *accounts.AccountID
	Kind      AlertKind
	Severity  AlertSeverity
	Detail    map[string]any
	FillID    *FillID
	CreatedAt time.Time
}

// Thresholds for the detectors. They are constants rather than configuration
// because a tunable threshold on a non-blocking detector is a knob nobody
// turns, and a constant is at least visible in review.
const (
	// RoundTripWindow is how close together a buy and a sell must be to look
	// like manufactured volume rather than a change of mind.
	RoundTripWindow = 60 * time.Second
	// ConcentrationWarnBPS is the share of circulating supply above which one
	// holder is flagged.
	ConcentrationWarnBPS money.BPS = 2_500 // 25%
	// ConcentrationCriticalBPS is the share above which one holder effectively
	// is the market.
	ConcentrationCriticalBPS money.BPS = 5_000 // 50%
	// VolumeWarnBPS is the fraction of the pool's Credit reserve a single
	// trade may move before it is flagged as anomalous.
	VolumeWarnBPS money.BPS = 3_000 // 30%
)

// surveil runs every detector against a just-executed fill and records what it
// finds. It never returns an error that would undo the trade unless writing the
// alert itself failed, because losing an alert silently is worse than failing
// loudly.
func (s *Service) surveil(ctx context.Context, tx pgx.Tx, m Market, r ExecuteRequest, fill Fill, fillID FillID, creatorID accounts.AccountID) ([]Alert, error) {
	var found []Alert

	if r.AccountID == creatorID {
		found = append(found, Alert{
			Kind: AlertCreatorSelfDealing, Severity: SeverityWarn,
			Detail: map[string]any{
				"side":   string(r.Side),
				"reason": "the asset's creator traded their own market",
			},
		})
	}

	roundTrip, err := s.recentOppositeFill(ctx, tx, m.ID, r.AccountID, r.Side, s.clk.Now().Add(-RoundTripWindow))
	if err != nil {
		return nil, err
	}
	if roundTrip {
		found = append(found, Alert{
			Kind: AlertRapidRoundTrip, Severity: SeverityWarn,
			Detail: map[string]any{
				"window_seconds": int(RoundTripWindow / time.Second),
				"reason":         "the same account traded the opposite side of this market within the window, which manufactures volume without taking risk",
			},
		})
	}

	// Wash trading between accounts under one controlled actor. Shared
	// ownership is the only form of "same actor" this system can prove, so it
	// is the only one alerted on; the alert names the owner so a reviewer can
	// see what the finding actually rests on.
	washOwner, washAccount, err := s.recentOppositeFillBySameOwner(ctx, tx, m.ID, r.AccountID, r.Side, s.clk.Now().Add(-RoundTripWindow))
	if err != nil {
		return nil, err
	}
	if washOwner != "" {
		found = append(found, Alert{
			Kind: AlertWashTrade, Severity: SeverityCritical,
			Detail: map[string]any{
				"window_seconds":       int(RoundTripWindow / time.Second),
				"other_account_id":     washAccount,
				"shared_owner_user_id": washOwner,
				"reason":               "another account owned by the same user traded the opposite side of this market inside the window; one actor took both sides, which manufactures volume and a price without transferring risk to anybody",
				"detection_basis":      "shared account ownership, which is a recorded fact",
				"what_this_cannot_see": "coordination between accounts owned by different users",
			},
		})
	}

	// Anomalous volume, measured against the pool's real reserve before the
	// trade. A trade larger than a third of the reserve moves the price enough
	// that it is worth a human knowing about.
	moved := fill.CreditsToPool
	before := fill.StateBefore.RealCreditReserve
	if before.IsPositive() {
		threshold := before.MulBPS(VolumeWarnBPS, money.RoundDown)
		if moved.Cmp(threshold) > 0 {
			found = append(found, Alert{
				Kind: AlertAnomalousVolume, Severity: SeverityWarn,
				Detail: map[string]any{
					"credits_moved":  moved.String(),
					"reserve_before": before.String(),
					"threshold_bps":  int(VolumeWarnBPS),
					"reason":         "a single trade moved a large fraction of the pool",
				},
			})
		}
	}

	// Concentration, computed from the ledger after the trade.
	circulating := m.Curve.InitialAssetReserve.Sub(fill.StateAfter.AssetReserve)
	if circulating.IsPositive() {
		holders, herr := s.Holders(ctx, tx, m.AssetID, 1)
		if herr != nil {
			return nil, herr
		}
		if len(holders) == 1 {
			top := holders[0]
			warn := circulating.MulBPS(ConcentrationWarnBPS, money.RoundDown)
			crit := circulating.MulBPS(ConcentrationCriticalBPS, money.RoundDown)
			if top.Quantity.Cmp(crit) > 0 || top.Quantity.Cmp(warn) > 0 {
				sev := SeverityWarn
				if top.Quantity.Cmp(crit) > 0 {
					sev = SeverityCritical
				}
				holder := top.AccountID
				found = append(found, Alert{
					AccountID: &holder,
					Kind:      AlertConcentration, Severity: sev,
					Detail: map[string]any{
						"holding":     top.Quantity.String(),
						"circulating": circulating.String(),
						"reason":      "one account holds a large share of the circulating supply, so their exit would be the market's price",
					},
				})
			}
		}
	}

	// Attribute every alert to this fill and trader unless a detector already
	// named a different account.
	out := make([]Alert, 0, len(found))
	for _, a := range found {
		a.ID = NewAlertID()
		a.MarketID = m.ID
		f := fillID
		a.FillID = &f
		if a.AccountID == nil {
			acct := r.AccountID
			a.AccountID = &acct
		}
		if err := s.writeAlert(ctx, tx, a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// recentOppositeFill reports whether the account traded the other side of this
// market since the given time.
func (s *Service) recentOppositeFill(ctx context.Context, q db.Querier, marketID MarketID, account accounts.AccountID, side Side, since time.Time) (bool, error) {
	opposite := Sell
	if side == Sell {
		opposite = Buy
	}
	var exists bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM native_market_fills
		    WHERE market_id = $1 AND account_id = $2 AND side = $3 AND created_at >= $4)`,
		marketID, account, string(opposite), since).Scan(&exists)
	if err != nil {
		return false, mapError(err)
	}
	return exists, nil
}

// recentOppositeFillBySameOwner reports whether a DIFFERENT account owned by
// the same user traded the other side of this market since the given time, and
// names both.
//
// It joins through accounts.owner_user_id rather than guessing at behavioural
// similarity. A heuristic that inferred common control from timing would
// produce false accusations against strangers who happened to trade together,
// and this alert is CRITICAL — it must not be something a coincidence can
// trigger.
func (s *Service) recentOppositeFillBySameOwner(
	ctx context.Context, q db.Querier, marketID MarketID, account accounts.AccountID, side Side, since time.Time,
) (ownerID string, otherAccount string, err error) {
	opposite := Sell
	if side == Sell {
		opposite = Buy
	}
	err = q.QueryRow(ctx, washTradeQuery, marketID, account, string(opposite), since).
		Scan(&ownerID, &otherAccount)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", mapError(err)
	}
	return ownerID, otherAccount, nil
}

// washTradeQuery finds the most recent opposite-side fill on this market by a
// different account with the same owner as $2.
const washTradeQuery = `
	SELECT mine.owner_user_id::text, f.account_id::text
	  FROM native_market_fills f
	  JOIN accounts theirs ON theirs.id = f.account_id
	  JOIN accounts mine   ON mine.id = $2
	 WHERE f.market_id = $1
	   AND f.side = $3
	   AND f.created_at >= $4
	   AND f.account_id <> $2
	   AND theirs.owner_user_id = mine.owner_user_id
	 ORDER BY f.created_at DESC
	 LIMIT 1`

func (s *Service) writeAlert(ctx context.Context, tx pgx.Tx, a Alert) error {
	detail, err := json.Marshal(a.Detail)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "nativemarket: alert detail is not JSON-encodable")
	}
	var account, fill any
	if a.AccountID != nil {
		account = *a.AccountID
	}
	if a.FillID != nil {
		fill = *a.FillID
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO native_market_alerts (id, market_id, account_id, kind, severity, detail, fill_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		a.ID, a.MarketID, account, string(a.Kind), string(a.Severity), detail, fill)
	return mapError(err)
}

// Alerts returns a market's surveillance findings, newest first.
func (s *Service) Alerts(ctx context.Context, q db.Querier, marketID MarketID, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := q.Query(ctx,
		`SELECT id, market_id, account_id, kind, severity, detail, fill_id, created_at
		   FROM native_market_alerts WHERE market_id = $1
		  ORDER BY created_at DESC LIMIT $2`, marketID, limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var (
			a         Alert
			kind, sev string
			detail    []byte
			accountID *accounts.AccountID
			fillID    *FillID
		)
		if err := rows.Scan(&a.ID, &a.MarketID, &accountID, &kind, &sev, &detail, &fillID, &a.CreatedAt); err != nil {
			return nil, mapError(err)
		}
		a.Kind, a.Severity = AlertKind(kind), AlertSeverity(sev)
		a.AccountID, a.FillID = accountID, fillID
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &a.Detail)
		}
		a.CreatedAt = a.CreatedAt.UTC()
		out = append(out, a)
	}
	return out, mapError(rows.Err())
}
