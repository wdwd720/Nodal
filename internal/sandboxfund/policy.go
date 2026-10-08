package sandboxfund

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Policy is the versioned faucet policy. The cap is stated in whole units of
// the granted asset, so one policy fits every simulated asset: it is a limit
// on play money handed out, not a valuation.
type Policy struct {
	Version string `json:"version"`
	// WindowMS is the rolling window the cap applies to.
	WindowMS int64 `json:"window_ms"`
	// CapWholeUnits is the most one account may be granted of one asset
	// inside one window, in whole units of that asset.
	CapWholeUnits int64 `json:"cap_whole_units"`
}

// DefaultPolicy is 100,000 whole units per account, per asset, per rolling
// 24 hours.
func DefaultPolicy() Policy {
	return Policy{Version: "sandbox-faucet-v1", WindowMS: (24 * time.Hour).Milliseconds(), CapWholeUnits: 100_000}
}

// Validate refuses a policy that could not be enforced as written.
func (p Policy) Validate() error {
	var problems []string
	if v := strings.TrimSpace(p.Version); v == "" || v != p.Version || len(v) > 100 {
		problems = append(problems, "version must be 1..100 bytes without surrounding whitespace")
	}
	if p.WindowMS <= 0 {
		problems = append(problems, "window_ms must be positive")
	}
	if p.CapWholeUnits <= 0 {
		problems = append(problems, "cap_whole_units must be positive")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid faucet policy").WithField("problems", problems)
	}
	return nil
}

// Hash is the hex sha256 of the policy's canonical JSON.
func (p Policy) Hash() (string, error) {
	b, err := audit.CanonicalJSON(p)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "sandboxfund: policy is not canonically encodable")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Window is the rolling cap window.
func (p Policy) Window() time.Duration { return time.Duration(p.WindowMS) * time.Millisecond }

// Cap is the window cap in base units of an asset with the given decimals:
// CapWholeUnits × 10^decimals, exactly.
func (p Policy) Cap(decimals uint8) money.Quantity {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	return money.QuantityFromBigInt(new(big.Int).Mul(big.NewInt(p.CapWholeUnits), scale))
}

// Admit decides whether a grant of requested base units fits beside used
// base units already granted in the window. It is pure: used + requested may
// not exceed the cap, and a non-positive request never fits.
func (p Policy) Admit(used, requested money.Quantity, decimals uint8) error {
	if !requested.IsPositive() {
		return errs.New(errs.CodeValidationFailed, "a grant is a positive quantity")
	}
	limit := p.Cap(decimals)
	if used.IsNegative() || used.Add(requested).Cmp(limit) > 0 {
		return errs.New(errs.CodeRateLimited, "the sandbox faucet cap for this account and asset is reached in the current window").
			WithField("cap", limit.String()).
			WithField("used", used.String()).
			WithField("requested", requested.String()).
			WithField("window_ms", p.WindowMS).
			WithField("policy_version", p.Version)
	}
	return nil
}

// Allowed decides whether the faucet may run in an environment:
//
//   - LOCAL, TEST and DEV: always. A fresh deployment must be able to fund
//     its own demonstration accounts.
//   - STAGING and PROD: only when deployment configuration enables it.
//     Handing out play money to the users of a deployed system is a product
//     decision, and it is still only ever SIMULATED value (Fund refuses any
//     other asset, and the ledger and the database refuse it again).
//   - anything else: never.
//
// The faucet is deliberately not a capability gate: it moves no value
// between parties and has no live counterpart to activate.
func Allowed(env config.Environment, enabled bool) bool {
	switch env {
	case config.EnvLocal, config.EnvTest, config.EnvDev:
		return true
	case config.EnvStaging, config.EnvProd:
		return enabled
	}
	return false
}

// AllEnvironments is the Go twin of the CHECK sandbox_fundings_environment_check.
func AllEnvironments() []config.Environment { return config.Environments() }
