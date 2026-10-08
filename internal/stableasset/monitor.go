package stableasset

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// MonitorReport is one run of the peg monitor: the market price against the
// peg, the reserve coverage, and the supply on the books against the chain.
type MonitorReport struct {
	StableAssetID StableAssetID `json:"stable_asset_id"`
	ObservedAt    time.Time     `json:"observed_at"`
	// MarketPrice is the stable asset's own oracle price; nil when stale.
	MarketPrice  *PriceDoc            `json:"market_price,omitempty"`
	PriceStale   bool                 `json:"price_stale"`
	DeviationBPS money.BPS            `json:"deviation_bps"`
	Reserve      ReserveLedger        `json:"reserve"`
	Supply       SupplyReconciliation `json:"supply"`
	Alerts       []AlertKind          `json:"alerts"`
	// BreakerTripped reports that this run paused the asset.
	BreakerTripped bool   `json:"breaker_tripped"`
	Status         Status `json:"status"`
}

// RunMonitor runs the peg monitor for one stable asset in the caller's
// transaction, under the asset's row lock:
//
//   - market price monitor: the stable asset's price from its oracle source;
//     a deviation from the peg above the threshold is a CRITICAL
//     PRICE_DEVIATION alert and trips the breaker; no fresh price is a
//     PRICE_STALE warning (the monitor cannot see the peg, so it says so).
//   - reserve coverage monitor: coverage below 100% is RESERVE_SHORTFALL and
//     trips the breaker; a reserve asset with a balance and no fresh price is
//     a RESERVE_PRICE_STALE warning (and counts zero, so it can cause the
//     shortfall).
//   - supply monitor: the books against the chain and the wallets
//     (ReconcileSupply); a difference is SUPPLY_MISMATCH, a STABLE_RESERVE
//     reconciliation record that blocks new risk, and trips the breaker.
//
// The circuit breaker pauses the asset with a bound transition naming the
// reason. It never resumes it.
func (s *Service) RunMonitor(ctx context.Context, tx pgx.Tx, sid StableAssetID, actor econ.Actor) (MonitorReport, error) {
	if err := requireActor(actor); err != nil {
		return MonitorReport{}, err
	}
	a, err := s.get(ctx, tx, sid, true)
	if err != nil {
		return MonitorReport{}, err
	}
	now := s.clk.Now().UTC()
	report := MonitorReport{StableAssetID: a.ID, ObservedAt: now, Alerts: []AlertKind{}}
	wasActive := a.Status == StatusActive

	price, fresh, err := s.marketPrice(ctx, tx, a, now)
	if err != nil {
		return MonitorReport{}, err
	}
	if !fresh {
		report.PriceStale = true
		report.Alerts = append(report.Alerts, AlertPriceStale)
		if err := s.alert(ctx, tx, a, AlertPriceStale, SeverityWarning, map[string]string{
			"reason": "no fresh market price from the oracle source", "oracle_source": a.OracleSource,
		}, false); err != nil {
			return MonitorReport{}, err
		}
	} else {
		doc := priceDoc(price)
		report.MarketPrice = &doc
		report.DeviationBPS = deviationBPS(price, a.Peg)
		if report.DeviationBPS > a.PegDeviationThreshold {
			report.Alerts = append(report.Alerts, AlertPriceDeviation)
			if err := s.trip(ctx, tx, &a, AlertPriceDeviation, SeverityCritical, map[string]any{
				"market_price": doc, "peg": priceDoc(a.Peg), "deviation_bps": report.DeviationBPS,
				"threshold_bps": a.PegDeviationThreshold,
			}); err != nil {
				return MonitorReport{}, err
			}
		}
	}

	if report.Reserve, err = s.reserveLedger(ctx, tx, a, now); err != nil {
		return MonitorReport{}, err
	}
	if len(report.Reserve.Unverified) > 0 {
		report.Alerts = append(report.Alerts, AlertReservePriceStale)
		if err := s.alert(ctx, tx, a, AlertReservePriceStale, SeverityWarning, map[string]any{
			"unverified_reserve_assets": report.Reserve.Unverified,
		}, false); err != nil {
			return MonitorReport{}, err
		}
	}
	if report.Supply, err = s.reconcileLocked(ctx, tx, &a, reconciliation.ModePeriodic, actor); err != nil {
		return MonitorReport{}, err
	}
	if !report.Supply.SupplyMatches || !report.Supply.WalletsMatch {
		report.Alerts = append(report.Alerts, AlertSupplyMismatch)
	}
	if !report.Supply.Covered {
		report.Alerts = append(report.Alerts, AlertReserveShortfall)
	}
	report.Status = a.Status
	report.BreakerTripped = wasActive && a.Status == StatusPaused
	return report, nil
}
