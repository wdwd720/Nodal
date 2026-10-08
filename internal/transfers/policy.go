package transfers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
)

// Policy is the versioned transfer policy. Every limit is an exact USD amount
// compared with the transfer's notional rounded UP, so rounding can only make
// a limit stricter, never looser. Durations are whole milliseconds so the
// canonical form (and therefore the hash) has no floating point and no
// locale.
type Policy struct {
	Version string `json:"version"`
	// MaxPerTransfer bounds one transfer.
	MaxPerTransfer money.USD `json:"max_per_transfer"`
	// MaxDaily bounds what one sender sends in a rolling DailyWindowMS.
	MaxDaily      money.USD `json:"max_daily"`
	DailyWindowMS int64     `json:"daily_window_ms"`
	// Velocity: at most VelocityMaxCount transfers and VelocityMaxNotional of
	// value per sender in a rolling VelocityWindowMS.
	VelocityWindowMS    int64     `json:"velocity_window_ms"`
	VelocityMaxCount    int       `json:"velocity_max_count"`
	VelocityMaxNotional money.USD `json:"velocity_max_notional"`
	// StepUpAbove: a transfer whose notional exceeds this needs a strong
	// (multi-factor) authentication no older than StepUpMaxAgeMS.
	StepUpAbove    money.USD `json:"step_up_above"`
	StepUpMaxAgeMS int64     `json:"step_up_max_age_ms"`
	// AllowlistOnly makes every sender allowlist-only, whatever their own
	// sender mode says.
	AllowlistOnly bool `json:"allowlist_only"`
	// MemoMaxRunes bounds the memo (at most 280, the column's bound).
	MemoMaxRunes int `json:"memo_max_runes"`
}

// MaxMemoRunes is the hard bound on a memo; a policy may only lower it.
const MaxMemoRunes = 280

// DefaultPolicy is the sandbox transfer policy: small, fully specified, and
// never a statement that real value may move.
func DefaultPolicy() Policy {
	return Policy{
		Version:             "transfers-sandbox-v1",
		MaxPerTransfer:      money.USDFromMinor(10_000_00),
		MaxDaily:            money.USDFromMinor(25_000_00),
		DailyWindowMS:       (24 * time.Hour).Milliseconds(),
		VelocityWindowMS:    time.Hour.Milliseconds(),
		VelocityMaxCount:    20,
		VelocityMaxNotional: money.USDFromMinor(20_000_00),
		StepUpAbove:         money.USDFromMinor(1_000_00),
		StepUpMaxAgeMS:      (5 * time.Minute).Milliseconds(),
		AllowlistOnly:       false,
		MemoMaxRunes:        140,
	}
}

// Validate refuses a policy that could not be enforced as written. A missing
// or non-positive limit is not "unlimited": it is an unusable policy.
func (p Policy) Validate() error {
	var problems []string
	if v := strings.TrimSpace(p.Version); v == "" || v != p.Version || len(v) > 100 {
		problems = append(problems, "version must be 1..100 bytes without surrounding whitespace")
	}
	for _, lim := range []struct {
		name string
		usd  money.USD
	}{
		{"max_per_transfer", p.MaxPerTransfer}, {"max_daily", p.MaxDaily}, {"velocity_max_notional", p.VelocityMaxNotional},
	} {
		if !lim.usd.IsPositive() {
			problems = append(problems, lim.name+" must be positive")
		}
	}
	if p.StepUpAbove.IsNegative() {
		problems = append(problems, "step_up_above must not be negative")
	}
	if p.DailyWindowMS <= 0 || p.VelocityWindowMS <= 0 || p.StepUpMaxAgeMS <= 0 {
		problems = append(problems, "windows and the step-up age must be positive")
	}
	if p.VelocityMaxCount <= 0 {
		problems = append(problems, "velocity_max_count must be positive")
	}
	if p.MemoMaxRunes <= 0 || p.MemoMaxRunes > MaxMemoRunes {
		problems = append(problems, fmt.Sprintf("memo_max_runes must be within 1..%d", MaxMemoRunes))
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "invalid transfer policy").WithField("problems", problems)
	}
	return nil
}

// Hash is the hex sha256 of the policy's canonical JSON: what a decision
// records so an auditor can tell exactly which limits applied.
func (p Policy) Hash() (string, error) {
	b, err := audit.CanonicalJSON(p)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "transfers: policy is not canonically encodable")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// DailyWindow is the rolling window of the daily limit.
func (p Policy) DailyWindow() time.Duration { return time.Duration(p.DailyWindowMS) * time.Millisecond }

// VelocityWindow is the rolling window of the velocity limits.
func (p Policy) VelocityWindow() time.Duration {
	return time.Duration(p.VelocityWindowMS) * time.Millisecond
}

// StepUpMaxAge is how recent a strong authentication must be above the
// step-up threshold.
func (p Policy) StepUpMaxAge() time.Duration {
	return time.Duration(p.StepUpMaxAgeMS) * time.Millisecond
}

// StepUpRequired reports whether a transfer of this notional needs a recent
// strong authentication.
func (p Policy) StepUpRequired(notional money.USD) bool { return notional.Cmp(p.StepUpAbove) > 0 }

// Usage is what a sender already sent in a window: transfers that moved
// value (COMPLETED, and REVERSED ones, which did move it). Refused transfers
// moved nothing and do not count.
type Usage struct {
	Count    int       `json:"count"`
	Notional money.USD `json:"notional"`
}

// PolicyInput is everything Evaluate reads.
type PolicyInput struct {
	// Notional is this transfer's value, rounded up.
	Notional money.USD
	Daily    Usage
	Velocity Usage
	// Rule is the rule in force for (sender, recipient); empty when none.
	Rule       RuleKind
	SenderMode SenderMode
}

// PolicyDecision is Evaluate's answer and the record of what was applied.
type PolicyDecision struct {
	// Codes are every violated rule, in AllRejectionCodes precedence order.
	Codes []string `json:"codes"`
	// Allowlisted is true when the recipient passes the allow/block rules.
	// It is what Risk Kernel V2 receives as Destination.Allowlisted.
	Allowlisted    bool      `json:"allowlisted"`
	AllowlistOnly  bool      `json:"allowlist_only"`
	Rule           string    `json:"rule"`
	StepUpRequired bool      `json:"step_up_required"`
	Notional       money.USD `json:"notional"`
	DailyBefore    Usage     `json:"daily_before"`
	VelocityBefore Usage     `json:"velocity_before"`
	// DailyAfter and VelocityNotionalAfter saturate at the int64 bound, which
	// exceeds every limit: an overflow can only refuse.
	DailyAfter            money.USD `json:"daily_after"`
	VelocityCountAfter    int       `json:"velocity_count_after"`
	VelocityNotionalAfter money.USD `json:"velocity_notional_after"`
}

// Evaluate applies the policy. It is pure and deterministic: the same policy
// and input always give the same decision, byte for byte.
func (p Policy) Evaluate(in PolicyInput) PolicyDecision {
	mode := in.SenderMode
	if mode == "" {
		mode = SenderOpen
	}
	d := PolicyDecision{
		AllowlistOnly:  p.AllowlistOnly || mode == SenderAllowlistOnly,
		Rule:           string(in.Rule),
		StepUpRequired: p.StepUpRequired(in.Notional),
		Notional:       in.Notional,
		DailyBefore:    in.Daily,
		VelocityBefore: in.Velocity,
	}
	violated := map[string]bool{}
	switch {
	case in.Rule == RuleBlock:
		violated[RejectRecipientBlocked] = true
	case d.AllowlistOnly && in.Rule != RuleAllow:
		violated[RejectRecipientNotAllowed] = true
	}
	// A rule this package does not know, or a mode it does not know, is not
	// a pass.
	if in.Rule != "" && !in.Rule.Valid() {
		violated[RejectRecipientBlocked] = true
	}
	if !mode.Valid() {
		violated[RejectRecipientNotAllowed] = true
	}
	d.Allowlisted = !violated[RejectRecipientBlocked] && !violated[RejectRecipientNotAllowed]

	if !in.Notional.IsPositive() || in.Notional.Cmp(p.MaxPerTransfer) > 0 {
		violated[RejectLimitPerTransfer] = true
	}
	d.DailyAfter = saturatingAdd(in.Daily.Notional, in.Notional)
	if in.Daily.Notional.IsNegative() || d.DailyAfter.Cmp(p.MaxDaily) > 0 {
		violated[RejectLimitDaily] = true
	}
	d.VelocityCountAfter = in.Velocity.Count + 1
	if in.Velocity.Count < 0 || d.VelocityCountAfter > p.VelocityMaxCount {
		violated[RejectVelocityCount] = true
	}
	d.VelocityNotionalAfter = saturatingAdd(in.Velocity.Notional, in.Notional)
	if in.Velocity.Notional.IsNegative() || d.VelocityNotionalAfter.Cmp(p.VelocityMaxNotional) > 0 {
		violated[RejectVelocityNotional] = true
	}
	d.Codes = []string{}
	for _, c := range AllRejectionCodes() {
		if violated[c] {
			d.Codes = append(d.Codes, c)
		}
	}
	return d
}

// saturatingAdd adds two USD amounts, saturating at the int64 bounds instead
// of failing: a saturated sum exceeds every limit, so overflow refuses.
func saturatingAdd(a, b money.USD) money.USD {
	s, err := a.Add(b)
	if err != nil {
		if b.IsNegative() {
			return money.MinUSD()
		}
		return money.MaxUSD()
	}
	return s
}
