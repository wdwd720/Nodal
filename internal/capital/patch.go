package capital

import (
	"slices"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// applyAuthorityPatch returns e with p applied and the audit map of what
// actually changed ({column: {from, to}}); unchanged fields produce no
// entry. An allocation change moves the delta through available so that
// available + reserved + deployed == allocation keeps holding; a decrease
// that would need more than the currently available budget is rejected
// (capital that is reserved or deployed cannot be taken back here).
func applyAuthorityPatch(e Envelope, p EnvelopeAuthorityPatch) (Envelope, map[string]FieldChange, error) {
	if err := p.Validate(); err != nil {
		return Envelope{}, nil, err
	}
	changes := map[string]FieldChange{}

	if p.Allocation != nil && !p.Allocation.Equal(e.Allocation) {
		delta, err := p.Allocation.Sub(e.Allocation)
		if err != nil {
			return Envelope{}, nil, moneyErr(err)
		}
		newAvailable, err := e.Available.Add(delta)
		if err != nil {
			return Envelope{}, nil, moneyErr(err)
		}
		if newAvailable.IsNegative() {
			committed, err := e.Reserved.Add(e.Deployed)
			if err != nil {
				return Envelope{}, nil, moneyErr(err)
			}
			return Envelope{}, nil, errs.New(errs.CodeValidationFailed, "allocation cannot drop below reserved + deployed capital").
				WithField("allocation_usd_minor", p.Allocation.Minor()).
				WithField("reserved_usd_minor", e.Reserved.Minor()).
				WithField("deployed_usd_minor", e.Deployed.Minor()).
				WithField("minimum_allocation_usd_minor", committed.Minor())
		}
		changes["allocation_usd_minor"] = FieldChange{From: e.Allocation.Minor(), To: p.Allocation.Minor()}
		changes["available_usd_minor"] = FieldChange{From: e.Available.Minor(), To: newAvailable.Minor()}
		e.Allocation = *p.Allocation
		e.Available = newAvailable
	}

	patchUSD(changes, "max_daily_loss_usd_minor", &e.MaxDailyLoss, p.MaxDailyLoss)
	patchUSD(changes, "max_drawdown_usd_minor", &e.MaxDrawdown, p.MaxDrawdown)
	patchUSD(changes, "max_single_trade_usd_minor", &e.MaxSingleTrade, p.MaxSingleTrade)
	patchUSD(changes, "max_position_usd_minor", &e.MaxPosition, p.MaxPosition)
	patchUSD(changes, "max_model_spend_usd_minor", &e.MaxModelSpend, p.MaxModelSpend)
	patchUSD(changes, "max_data_spend_usd_minor", &e.MaxDataSpend, p.MaxDataSpend)
	patchList(changes, "allowed_instruments", &e.AllowedInstruments, p.AllowedInstruments)
	patchList(changes, "allowed_asset_classes", &e.AllowedAssetClasses, p.AllowedAssetClasses)
	patchList(changes, "allowed_venues", &e.AllowedVenues, p.AllowedVenues)

	if p.MaxOrderRatePerHour != nil && *p.MaxOrderRatePerHour != e.MaxOrderRatePerHour {
		changes["max_order_rate_per_hour"] = FieldChange{From: e.MaxOrderRatePerHour, To: *p.MaxOrderRatePerHour}
		e.MaxOrderRatePerHour = *p.MaxOrderRatePerHour
	}
	if p.PolicyVersion != nil && *p.PolicyVersion != e.PolicyVersion {
		changes["policy_version"] = FieldChange{From: e.PolicyVersion, To: *p.PolicyVersion}
		e.PolicyVersion = *p.PolicyVersion
	}
	if p.EffectiveAt != nil && !p.EffectiveAt.Equal(e.EffectiveAt) {
		changes["effective_at"] = FieldChange{From: timeValue(&e.EffectiveAt), To: timeValue(p.EffectiveAt)}
		e.EffectiveAt = p.EffectiveAt.UTC()
	}
	switch {
	case p.ClearExpiresAt && e.ExpiresAt != nil:
		changes["expires_at"] = FieldChange{From: timeValue(e.ExpiresAt), To: nil}
		e.ExpiresAt = nil
	case p.ExpiresAt != nil && (e.ExpiresAt == nil || !p.ExpiresAt.Equal(*e.ExpiresAt)):
		changes["expires_at"] = FieldChange{From: timeValue(e.ExpiresAt), To: timeValue(p.ExpiresAt)}
		t := p.ExpiresAt.UTC()
		e.ExpiresAt = &t
	}
	if e.ExpiresAt != nil && !e.ExpiresAt.After(e.EffectiveAt) {
		return Envelope{}, nil, errs.New(errs.CodeValidationFailed, "expires_at must be after effective_at").
			WithField("effective_at", timeValue(&e.EffectiveAt)).
			WithField("expires_at", timeValue(e.ExpiresAt))
	}
	return e, changes, nil
}

func patchUSD(changes map[string]FieldChange, column string, cur, want *money.USD) {
	if want == nil || want.Equal(*cur) {
		return
	}
	changes[column] = FieldChange{From: cur.Minor(), To: want.Minor()}
	*cur = *want
}

func patchList(changes map[string]FieldChange, column string, cur, want *[]string) {
	if want == nil {
		return
	}
	next := normalizeList(*want)
	if slices.Equal(normalizeList(*cur), next) {
		return
	}
	changes[column] = FieldChange{From: normalizeList(*cur), To: next}
	*cur = next
}

// normalizeList trims entries, drops blanks and duplicates, and always
// returns a non-nil slice (array columns are NOT NULL).
func normalizeList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// timeValue renders an optional instant for an audit row: RFC 3339 with
// nanoseconds in UTC, or nil.
func timeValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
