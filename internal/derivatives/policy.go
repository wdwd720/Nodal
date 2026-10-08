package derivatives

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// ClassRates are the linear margin rates for underlyings of one risk class.
type ClassRates struct {
	RiskClass      assets.RiskClass `json:"risk_class"`
	InitialBPS     money.BPS        `json:"initial_bps"`
	MaintenanceBPS money.BPS        `json:"maintenance_bps"`
}

// Policy is the versioned margin, funding and paper-fill policy of the
// derivatives subsystem. Every number is an exact integer and every decision
// that depends on it records Version and Hash.
type Policy struct {
	Version string `json:"version"`
	// Linear holds the initial and maintenance rates of futures and
	// perpetuals by the risk class of the underlying asset. A class that is
	// not listed cannot be traded: there is no default rate.
	Linear []ClassRates `json:"linear"`
	// FundingBufferIntervals is how many funding intervals at the maximum
	// rate a perpetual's initial margin carries on top of its linear rate.
	FundingBufferIntervals int64 `json:"funding_buffer_intervals"`
	// MaxFundingRateE8 clamps the funding rate to ±MaxFundingRateE8 / 10^8 per
	// interval.
	MaxFundingRateE8 int64 `json:"max_funding_rate_e8"`
	// OptionShocksBPS is the deterministic grid of underlying moves over which
	// a written option's worst loss is computed.
	OptionShocksBPS []money.BPS `json:"option_shocks_bps"`
	// OptionMinMarginBPS is the floor of a written option's margin, as a share
	// of the underlying notional.
	OptionMinMarginBPS money.BPS `json:"option_min_margin_bps"`
	// OptionMaintenanceRatioBPS is maintenance as a share of initial margin
	// for written options.
	OptionMaintenanceRatioBPS money.BPS `json:"option_maintenance_ratio_bps"`
	// ExpiryGapWindowMS is how long before expiry the expiry-jump scenario
	// applies.
	ExpiryGapWindowMS int64 `json:"expiry_gap_window_ms"`
	// ExpiryGapShockBPS is the size of the gap a contract must survive in its
	// last window before expiry.
	ExpiryGapShockBPS money.BPS `json:"expiry_gap_shock_bps"`
	// PaperSlippageBPS is the disclosed slippage of a paper fill against the
	// mark: a buy fills above, a sell below.
	PaperSlippageBPS money.BPS `json:"paper_slippage_bps"`
	// VariationMarginIntervalMS is the length of a variation-margin interval.
	VariationMarginIntervalMS int64 `json:"variation_margin_interval_ms"`
}

// DefaultPolicy is the sandbox policy. It describes simulated contracts only
// and is never a statement that any live activity would be acceptable.
func DefaultPolicy() Policy {
	return Policy{
		Version: "derivatives-sandbox-v1",
		Linear: []ClassRates{
			{RiskClass: assets.RiskSettlement, InitialBPS: 500, MaintenanceBPS: 250},
			{RiskClass: assets.RiskMajor, InitialBPS: 1_000, MaintenanceBPS: 500},
			{RiskClass: assets.RiskStandard, InitialBPS: 2_000, MaintenanceBPS: 1_000},
			{RiskClass: assets.RiskSpeculative, InitialBPS: 5_000, MaintenanceBPS: 2_500},
		},
		FundingBufferIntervals:    3,
		MaxFundingRateE8:          750_000, // 0.75% per interval
		OptionShocksBPS:           []money.BPS{-3_000, -2_000, -1_000, -500, 0, 500, 1_000, 2_000, 3_000},
		OptionMinMarginBPS:        1_000,
		OptionMaintenanceRatioBPS: 8_000,
		ExpiryGapWindowMS:         24 * 3600 * 1000,
		ExpiryGapShockBPS:         5_000,
		PaperSlippageBPS:          10,
		VariationMarginIntervalMS: 3600 * 1000,
	}
}

// Validate refuses a policy that cannot be evaluated safely.
func (p Policy) Validate() error {
	var problems []string
	if p.Version == "" {
		problems = append(problems, "version required")
	}
	if len(p.Linear) == 0 {
		problems = append(problems, "at least one linear risk class is required")
	}
	seen := map[assets.RiskClass]bool{}
	for _, c := range p.Linear {
		if seen[c.RiskClass] {
			problems = append(problems, fmt.Sprintf("risk class %s listed twice", c.RiskClass))
		}
		seen[c.RiskClass] = true
		if c.InitialBPS <= 0 || c.InitialBPS > money.OneHundredPercent || c.MaintenanceBPS <= 0 || c.MaintenanceBPS > c.InitialBPS {
			problems = append(problems, fmt.Sprintf("risk class %s needs 0 < maintenance <= initial <= 100%%", c.RiskClass))
		}
	}
	if p.FundingBufferIntervals < 0 {
		problems = append(problems, "funding buffer intervals must not be negative")
	}
	if p.MaxFundingRateE8 <= 0 || p.MaxFundingRateE8 > FundingRateScale {
		problems = append(problems, "max funding rate must be in (0, 100%]")
	}
	if len(p.OptionShocksBPS) == 0 {
		problems = append(problems, "the option scenario grid is empty")
	}
	for _, s := range p.OptionShocksBPS {
		if s < -money.OneHundredPercent {
			problems = append(problems, "an option shock cannot move a price below zero")
		}
	}
	if p.OptionMinMarginBPS <= 0 || p.OptionMinMarginBPS > money.OneHundredPercent {
		problems = append(problems, "option minimum margin must be in (0, 100%]")
	}
	if p.OptionMaintenanceRatioBPS <= 0 || p.OptionMaintenanceRatioBPS > money.OneHundredPercent {
		problems = append(problems, "option maintenance ratio must be in (0, 100%]")
	}
	if p.ExpiryGapWindowMS < 0 || p.ExpiryGapShockBPS < 0 || p.ExpiryGapShockBPS > money.OneHundredPercent {
		problems = append(problems, "expiry gap window and shock must be non-negative and the shock at most 100%")
	}
	if p.PaperSlippageBPS < 0 || p.PaperSlippageBPS >= money.OneHundredPercent {
		problems = append(problems, "paper slippage must be in [0, 100%)")
	}
	if p.VariationMarginIntervalMS <= 0 {
		problems = append(problems, "variation margin interval must be positive")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "derivatives: invalid policy").WithField("problems", problems)
	}
	return nil
}

// Hash is the sha256 of the policy's canonical JSON.
func (p Policy) Hash() string {
	b, err := audit.CanonicalJSON(p)
	if err != nil {
		// Unreachable: the policy holds only strings, integers and slices of
		// them.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// linearRates returns the rates for a risk class, failing closed when the
// class is not covered by the policy.
func (p Policy) linearRates(c assets.RiskClass) (ClassRates, error) {
	for _, r := range p.Linear {
		if r.RiskClass == c {
			return r, nil
		}
	}
	return ClassRates{}, errs.Newf(errs.CodeUnsupported, "derivatives: no margin rate for risk class %q in policy %s", c, p.Version).
		WithField("risk_class", string(c))
}

// maxInitialBPS is the highest linear initial rate in the policy.
func (p Policy) maxInitialBPS() money.BPS {
	var m money.BPS
	for _, r := range p.Linear {
		if r.InitialBPS > m {
			m = r.InitialBPS
		}
	}
	return m
}
