package nativemarket

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Internal market safety (product goal §47).
//
// # What was already here, and what was not
//
// The risk kernel refuses a BUY that would leave one account holding too large
// a share of an asset's supply, or too much of its Credits committed to one
// creator (risk.go). surveillance.go raises alerts for wash trading, rapid
// round tripping, creator self-dealing, concentration and anomalous volume, and
// deliberately does not block on them. An operator can halt a market, and the
// database enforces the halt.
//
// Nothing bounded price impact, nothing set a floor under the liquidity a
// market may open with, nothing stopped a market that was moving violently
// without somebody watching it, and nothing stopped a creator buying their own
// asset. Those four are here.
//
// # Why these limits are not in risk.Policy
//
// A risk policy composes GLOBAL ∧ ACCOUNT ∧ AGENT and answers "how much risk
// may this ACCOUNT take". Three of these four are properties of a MARKET: the
// breaker belongs to the venue, the opening floor belongs to the market being
// opened, and the creator rule is about a relationship between an account and
// an asset. Letting an account-scoped row compose with a venue rule would let
// the wrong party loosen it. So this is its own document, with the same
// discipline: versioned, hashed, immutable, chosen by effective_at, recorded
// with an actor and a reason (migration 00773).
//
// # Why impact and slippage bind a BUY only
//
// The same reason the concentration limits do. Both numbers describe the cost
// of the caller's own size; refusing somebody's EXIT because their exit is
// large traps them in the position, and a control that can trap a holder is
// worse than the manipulation it prevents. A seller's protection against their
// own size is MinOutput, which they set and the engine honours.

// SafetyPolicy is the versioned market-safety document.
//
// Every field is a pointer because "unset" and "zero" are different facts: an
// unset limit is a policy that did not decide, which Validate refuses, and a
// zero limit is a decision to permit nothing.
type SafetyPolicy struct {
	// Version is the row version. It is not part of the hashed rules.
	Version string `json:"-"`

	// MaxPriceImpactBPS caps how far one BUY may move the market's marginal
	// price, in basis points of the pre-trade spot. It is the "price impact"
	// and "low liquidity" limb of §47 at once: on a constant-product curve
	// those are the same measurement seen from two sides.
	MaxPriceImpactBPS *money.BPS `json:"max_price_impact_bps"`
	// MaxSlippageBPS caps how far one BUY's achieved price may sit from the
	// pre-trade spot. Impact is where the market ends up; slippage is what the
	// caller actually paid on the way. A curve can have small impact and large
	// slippage on a fee-heavy market, so both are stated.
	MaxSlippageBPS *money.BPS `json:"max_slippage_bps"`

	// CircuitBreakerMoveBPS is how far a market's marginal price may travel,
	// up or down, within CircuitBreakerWindowSeconds before trading is paused.
	// Zero disables the breaker, which is a decision a policy may record.
	CircuitBreakerMoveBPS *money.BPS `json:"circuit_breaker_move_bps"`
	// CircuitBreakerWindowSeconds is the window the move is measured over.
	CircuitBreakerWindowSeconds *int `json:"circuit_breaker_window_seconds"`

	// MinOpeningLiquidityCredits is the smallest virtual Credit reserve a
	// market may be created with, in Credit base units. The virtual reserve is
	// the pool's depth: a market opened with a tiny one can be moved to any
	// price by a trivial order, which is the "spam assets" and "fake volume"
	// limb of §47 at its cheapest.
	MinOpeningLiquidityCredits *money.Quantity `json:"min_opening_liquidity_credits"`

	// CreatorMayBuyOwnAsset says whether the account that created an asset may
	// BUY it on its own market. Selling is always permitted: the creator's
	// allocation is theirs and trapping it would be the same wrong as trapping
	// any other holder.
	CreatorMayBuyOwnAsset *bool `json:"creator_may_buy_own_asset"`
}

// ConservativeSafetyVersion names the compiled-in policy.
const ConservativeSafetyVersion = "market-safety-conservative-1"

// ConservativeSafetyPolicy is the policy a deployment runs under until an
// operator records another one.
//
// It is a real, complete policy rather than an absence, for the reason the
// payout policy and the legal policy are: a deployment that has recorded
// nothing has not decided that everything is permitted. Every number is chosen
// to bind the abusive case and not the ordinary one, and each is explained
// where it is set, because a limit nobody can justify is a limit somebody will
// eventually raise without argument.
func ConservativeSafetyPolicy() SafetyPolicy {
	bps := func(v money.BPS) *money.BPS { return &v }
	sec := func(v int) *int { return &v }
	permitted := true
	// 1,000 Credits at the Credit asset's six decimal places. A market opened
	// with less than this can be moved several percent by a single Credit.
	minLiquidity := money.QuantityFromInt64(1_000_000_000)
	return SafetyPolicy{
		Version: ConservativeSafetyVersion,
		// 90%, which on this curve means one order may be at most about 38% of
		// the pool's effective reserve: price impact is (x'/x)^2 - 1, so 9,000
		// basis points is x'/x = 1.378. Past that the order is not discovering
		// a price, it is larger than the market it is trading against. Below
		// it the caller's own MinOutput is the protection, and it is the number
		// they actually agreed to.
		MaxPriceImpactBPS: bps(9_000),
		MaxSlippageBPS:    bps(9_000),
		// The circuit breaker is BUILT, ENFORCED, TESTED -- and DISARMED in the
		// compiled-in policy. That is a decision, not an omission.
		//
		// A trip moves the market to CLOSE_ONLY, and only a person can move it
		// back: that is deliberate (resuming a market after an economic
		// incident should be somebody's decision) and it is exactly why an
		// automatic threshold is dangerous on a deployment with nobody
		// watching. A freshly launched constant-product market on a virtual
		// reserve legitimately moves several hundred percent in minutes -- the
		// first few buyers ARE the price discovery -- so any threshold low
		// enough to catch manipulation catches every launch, and the result is
		// a product whose markets pause on their first good day and stay
		// paused.
		//
		// So the mechanism is here and a deployment that has an operator to
		// watch its markets arms it by recording a policy with a threshold and
		// a window it has chosen. Zero is off, and Validate accepts zero for
		// exactly this reason.
		CircuitBreakerMoveBPS:       bps(0),
		CircuitBreakerWindowSeconds: sec(300),
		MinOpeningLiquidityCredits:  &minLiquidity,
		// PERMITTED by default, and this is the one limit here whose default
		// is the permissive one. It is deliberate and it is not this file's
		// decision to reverse.
		//
		// A creator buying their own asset is the cheapest way to manufacture
		// volume and a price on a market whose fee they also collect, so goal
		// SS47 names self-dealing -- and it says "prevent OR EXPOSE". This
		// package already decided which, in doc.go: an automated market maker
		// has no order book, so the counterparty is always the pool and there
		// is no matched self-trade to prevent; what exists is a pattern, and
		// "a detector that halts a market on a heuristic is a denial-of-service
		// vector against creators". surveillance.go raises
		// CREATOR_SELF_DEALING on exactly this and does not block, and
		// TestIntegration_SurveillanceRaisesAlertsWithoutBlocking holds that
		// line.
		//
		// A deployment that would rather prevent than expose records a policy
		// with this false, and every creator BUY is then refused with
		// ASSET_RESTRICTED. That is the point of it being a policy value: the
		// choice is a deployment's, and it is written down either way.
		CreatorMayBuyOwnAsset: &permitted,
	}
}

// ParseSafetyPolicy decodes a rules document, rejecting unknown fields.
func ParseSafetyPolicy(raw json.RawMessage) (SafetyPolicy, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return SafetyPolicy{}, errs.New(errs.CodeValidationFailed,
			"nativemarket: safety policy rules must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var p SafetyPolicy
	if err := dec.Decode(&p); err != nil {
		return SafetyPolicy{}, errs.Wrap(err, errs.CodeValidationFailed,
			"nativemarket: safety policy rules do not parse: "+err.Error())
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return SafetyPolicy{}, errs.New(errs.CodeValidationFailed,
			"nativemarket: safety policy rules contain trailing data")
	}
	if err := p.Validate(); err != nil {
		return SafetyPolicy{}, err
	}
	return p, nil
}

// Missing lists the JSON keys this policy leaves unset.
func (p SafetyPolicy) Missing() []string {
	var missing []string
	add := func(name string, isNil bool) {
		if isNil {
			missing = append(missing, name)
		}
	}
	add("max_price_impact_bps", p.MaxPriceImpactBPS == nil)
	add("max_slippage_bps", p.MaxSlippageBPS == nil)
	add("circuit_breaker_move_bps", p.CircuitBreakerMoveBPS == nil)
	add("circuit_breaker_window_seconds", p.CircuitBreakerWindowSeconds == nil)
	add("min_opening_liquidity_credits", p.MinOpeningLiquidityCredits == nil)
	add("creator_may_buy_own_asset", p.CreatorMayBuyOwnAsset == nil)
	sort.Strings(missing)
	return missing
}

// Validate checks completeness and ranges. A market-safety policy is complete
// or it is not a policy: an absent limit is not a permissive one.
func (p SafetyPolicy) Validate() error {
	if missing := p.Missing(); len(missing) > 0 {
		return errs.New(errs.CodeValidationFailed,
			"nativemarket: a market safety policy must set every limit").
			WithField("missing", missing)
	}
	var problems []string
	boundedBPS := func(name string, v, max money.BPS) {
		if v < 0 || v > max {
			problems = append(problems, fmt.Sprintf("%s: must be between 0 and %d basis points", name, max))
		}
	}
	boundedBPS("max_price_impact_bps", *p.MaxPriceImpactBPS, money.OneHundredPercent)
	boundedBPS("max_slippage_bps", *p.MaxSlippageBPS, money.OneHundredPercent)
	// A breaker threshold above 100% is meaningful -- a price can more than
	// double -- so it is bounded by a much larger number, not by 10,000.
	boundedBPS("circuit_breaker_move_bps", *p.CircuitBreakerMoveBPS, 1_000*money.OneHundredPercent)
	if *p.CircuitBreakerWindowSeconds <= 0 {
		problems = append(problems, "circuit_breaker_window_seconds: must be positive")
	}
	if *p.CircuitBreakerWindowSeconds > 24*60*60 {
		problems = append(problems, "circuit_breaker_window_seconds: must not exceed one day")
	}
	if p.MinOpeningLiquidityCredits.IsNegative() {
		problems = append(problems, "min_opening_liquidity_credits: must not be negative")
	}
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errs.New(errs.CodeValidationFailed, "nativemarket: invalid market safety rules").
		WithField("problems", problems)
}

// CanonicalJSON renders the rules exactly as they are hashed and stored.
//
// The document is a flat struct with no maps, so encoding/json already emits
// its fields in declaration order and the rendering is deterministic without a
// canonicaliser. Adding a field changes the hash, which is correct: it is a
// different policy.
func (p SafetyPolicy) CanonicalJSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Hash is the hex SHA-256 of CanonicalJSON, stored in rules_hash.
func (p SafetyPolicy) Hash() string {
	canon, err := p.CanonicalJSON()
	if err != nil {
		// Unreachable: every field renders. Hashing the error rather than
		// panicking keeps a decision recordable and obviously wrong.
		sum := sha256.Sum256([]byte("unhashable:" + err.Error()))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// BreakerEnabled reports whether the policy asks for a circuit breaker at all.
func (p SafetyPolicy) BreakerEnabled() bool {
	return p.CircuitBreakerMoveBPS != nil && *p.CircuitBreakerMoveBPS > 0 &&
		p.CircuitBreakerWindowSeconds != nil && *p.CircuitBreakerWindowSeconds > 0
}

// Safety is the part of the safety-policy store this package uses.
//
// It is a port for the same reason Risk is: which limits apply is deployment
// state that an operator recorded, not a constant this package holds. It takes
// the caller's own transaction so reading the policy cannot take a second pool
// connection while the market row is locked (F-27).
type Safety interface {
	SafetyPolicy(ctx context.Context, q db.Querier, at time.Time) (SafetyPolicy, error)
}

// SetSafety installs the deployment's safety-policy source.
//
// A Service with none uses ConservativeSafetyPolicy, which is why this is a
// setter rather than a constructor argument: the default is a real policy, not
// an absence, so a deployment that has recorded nothing is still protected and
// no caller has to remember to pass one.
func (s *Service) SetSafety(sf Safety) { s.safety = sf }

// safetyPolicy returns the policy in force.
func (s *Service) safetyPolicy(ctx context.Context, q db.Querier) (SafetyPolicy, error) {
	if s.safety == nil {
		return ConservativeSafetyPolicy(), nil
	}
	return s.safety.SafetyPolicy(ctx, q, s.clk.Now())
}

// checkSafety refuses a trade the market-safety policy does not permit.
//
// It runs beside checkRisk, before anything is posted, and for the same reason:
// both limits describe what the trade would DO, and a check that happens after
// the posting is a check that has to undo something.
func (s *Service) checkSafety(p SafetyPolicy, m Market, r ExecuteRequest, fill Fill, creator accounts.AccountID) error {
	if r.Side != Buy {
		return nil
	}
	if p.CreatorMayBuyOwnAsset != nil && !*p.CreatorMayBuyOwnAsset && r.AccountID == creator {
		return errs.New(errs.CodeAssetRestricted,
			"the creator of an asset may not buy it on its own market; the creator fee on every trade "+
				"makes a creator's own buying indistinguishable from manufacturing volume").
			WithField("market_id", m.ID.String()).
			WithField("asset_id", m.AssetID.String()).
			WithField("limit", "creator_may_buy_own_asset").
			WithField("policy_version", p.Version)
	}
	impact := PriceImpactBPS(fill)
	if p.MaxPriceImpactBPS != nil && impact > *p.MaxPriceImpactBPS {
		return errs.Newf(errs.CodeVenueLiquidityInsufficient,
			"this order would move the market's price by %d basis points, past the %d this market permits; "+
				"a smaller order moves it less", int(impact), int(*p.MaxPriceImpactBPS)).
			WithField("market_id", m.ID.String()).
			WithField("limit", "max_price_impact_bps").
			WithField("price_impact_bps", int(impact)).
			WithField("max_price_impact_bps", int(*p.MaxPriceImpactBPS)).
			WithField("policy_version", p.Version)
	}
	slip := fill.SlippageBPS()
	if p.MaxSlippageBPS != nil && slip > *p.MaxSlippageBPS {
		return errs.Newf(errs.CodeVenueLiquidityInsufficient,
			"this order would fill %d basis points away from the price on screen, past the %d this market "+
				"permits; a smaller order fills closer", int(slip), int(*p.MaxSlippageBPS)).
			WithField("market_id", m.ID.String()).
			WithField("limit", "max_slippage_bps").
			WithField("slippage_bps", int(slip)).
			WithField("max_slippage_bps", int(*p.MaxSlippageBPS)).
			WithField("policy_version", p.Version)
	}
	return nil
}

// PriceImpactBPS is how far a fill moves the market's marginal price, in basis
// points of the pre-trade spot, as a magnitude.
//
// It is the market's move, not the caller's cost: SlippageBPS is the caller's
// cost. A buy raises the spot and a sell lowers it, and the limit is on the
// size of the move either way, so this is an absolute value.
func PriceImpactBPS(f Fill) money.BPS {
	if f.SpotBefore.Sign() == 0 {
		return 0
	}
	diff := f.SpotAfter.Sub(f.SpotBefore).Abs()
	scaled := diff.Mul(money.QuantityFromInt64(int64(money.OneHundredPercent)))
	out, err := scaled.Div(f.SpotBefore, money.RoundDown)
	if err != nil {
		return money.OneHundredPercent
	}
	v, err := out.Int64()
	if err != nil {
		return 1_000 * money.OneHundredPercent
	}
	return money.BPS(v)
}

// checkOpeningLiquidity refuses a market whose pool is too shallow to price.
func checkOpeningLiquidity(p SafetyPolicy, r CreateRequest) error {
	if p.MinOpeningLiquidityCredits == nil {
		return nil
	}
	if r.VirtualCreditReserve.Cmp(*p.MinOpeningLiquidityCredits) >= 0 {
		return nil
	}
	return errs.Newf(errs.CodeValidationFailed,
		"a market must open with at least %s Credit base units of virtual reserve; this one opens with %s. "+
			"A pool this shallow can be moved to any price by a trivial order",
		p.MinOpeningLiquidityCredits.String(), r.VirtualCreditReserve.String()).
		WithField("min_opening_liquidity_credits", p.MinOpeningLiquidityCredits.String()).
		WithField("virtual_credit_reserve", r.VirtualCreditReserve.String()).
		WithField("policy_version", p.Version)
}

// --- the circuit breaker ----------------------------------------------------

// BreakerTrip is a recorded pause of a market.
type BreakerTrip struct {
	ID             string
	MarketID       MarketID
	FillID         FillID
	PolicyVersion  string
	WindowSeconds  int
	LimitBPS       money.BPS
	MoveBPS        money.BPS
	ReferencePrice money.Quantity
	ObservedPrice  money.Quantity
	ReferenceAt    time.Time
	TrippedAt      time.Time
}

// BreakerPauseStatus is where a tripped market goes.
//
// CLOSE_ONLY and not HALTED. The runaway a breaker exists to stop is NEW
// exposure, and CLOSE_ONLY stops exactly that while leaving every holder able
// to sell. HALTED would trap them, and 00712's own comment says a halt "traps
// holders, so it is for investigations and is meant to be short" -- which is a
// decision for a person, not for a threshold. Resuming is CLOSE_ONLY -> ACTIVE,
// a recorded operator transition like any other.
const BreakerPauseStatus = StatusCloseOnly

// applyBreaker pauses a market whose price has moved too far, too fast.
//
// It runs AFTER the fill has been written, and the fill stands. That is
// deliberate: the trade was legal when it was priced and refusing it after the
// fact would mean the caller's own confirmation was a lie. What the breaker
// stops is the NEXT one -- which is what a circuit breaker is.
func (s *Service) applyBreaker(ctx context.Context, tx pgx.Tx, p SafetyPolicy, m Market, fill Fill, fillID FillID, at time.Time) (*BreakerTrip, error) {
	if !p.BreakerEnabled() || m.Status != StatusActive {
		return nil, nil
	}
	window := time.Duration(*p.CircuitBreakerWindowSeconds) * time.Second
	var (
		reference   money.Quantity
		referenceAt time.Time
		refText     string
	)
	err := tx.QueryRow(ctx,
		`SELECT spot_price_before::text, printed_at
		   FROM native_market_prints
		  WHERE market_id = $1 AND printed_at >= $2
		  ORDER BY printed_at, seq
		  LIMIT 1`, m.ID, at.Add(-window)).Scan(&refText, &referenceAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No print inside the window, not even this trade's own. Nothing to
		// measure against, so nothing to conclude.
		return nil, nil
	case err != nil:
		return nil, mapError(err)
	}
	reference, perr := money.ParseQuantity(refText)
	if perr != nil {
		return nil, errs.Wrap(perr, errs.CodeInternal, "nativemarket: a recorded price is not an integer")
	}
	if reference.Sign() == 0 {
		return nil, nil
	}
	move := moveBPS(reference, fill.SpotAfter)
	if move < *p.CircuitBreakerMoveBPS {
		return nil, nil
	}

	trip := BreakerTrip{
		ID: id.New[id.Any]().String(), MarketID: m.ID, FillID: fillID,
		PolicyVersion: p.Version, WindowSeconds: *p.CircuitBreakerWindowSeconds,
		LimitBPS: *p.CircuitBreakerMoveBPS, MoveBPS: move,
		ReferencePrice: reference, ObservedPrice: fill.SpotAfter,
		ReferenceAt: referenceAt.UTC(), TrippedAt: at.UTC(),
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO native_market_breaker_events
		   (id, market_id, fill_id, policy_version, window_seconds, limit_bps, move_bps,
		    reference_price, observed_price, reference_at, tripped_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9::numeric,$10,$11)`,
		trip.ID, trip.MarketID, trip.FillID, trip.PolicyVersion, trip.WindowSeconds,
		int(trip.LimitBPS), int(trip.MoveBPS), trip.ReferencePrice.String(), trip.ObservedPrice.String(),
		trip.ReferenceAt, trip.TrippedAt); err != nil {
		return nil, mapError(err)
	}
	reason := fmt.Sprintf(
		"circuit breaker: the price moved %d basis points within %ds, past the %d basis points policy %s permits",
		int(trip.MoveBPS), trip.WindowSeconds, int(trip.LimitBPS), trip.PolicyVersion,
	)
	// The pause is a status change like every other one: a transition row, the
	// trigger that writes status from it, and the instrument registry kept in
	// step. The actor is the system, named for the control that acted.
	sysCtx := security.WithPrincipal(ctx, security.Principal{
		SubjectID: "market:circuit-breaker", ActorType: security.ActorSystem,
	})
	if _, err := s.SetStatus(sysCtx, tx, m.ID, BreakerPauseStatus, reason); err != nil {
		return nil, err
	}
	return &trip, nil
}

// moveBPS is |b - a| / a in basis points, saturating rather than overflowing.
func moveBPS(a, b money.Quantity) money.BPS {
	if a.Sign() == 0 {
		return 0
	}
	diff := b.Sub(a).Abs()
	scaled := diff.Mul(money.QuantityFromInt64(int64(money.OneHundredPercent)))
	out, err := scaled.Div(a, money.RoundDown)
	if err != nil {
		return 1_000 * money.OneHundredPercent
	}
	v, err := out.Int64()
	if err != nil {
		return 1_000 * money.OneHundredPercent
	}
	return money.BPS(v)
}

// --- the deployment's implementation ----------------------------------------

// safetyStore reads the effective policy from native_market_safety_policies.
type safetyStore struct{}

// NewSafetyStore returns the Safety a deployment uses: the newest policy whose
// effective_at has passed, or the compiled-in conservative one if none has been
// recorded.
func NewSafetyStore() Safety { return safetyStore{} }

func (safetyStore) SafetyPolicy(ctx context.Context, q db.Querier, at time.Time) (SafetyPolicy, error) {
	var (
		version string
		rules   []byte
	)
	err := q.QueryRow(ctx,
		`SELECT version, rules FROM native_market_safety_policies
		  WHERE effective_at <= $1
		  ORDER BY effective_at DESC, created_at DESC
		  LIMIT 1`, at.UTC()).Scan(&version, &rules)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ConservativeSafetyPolicy(), nil
	case err != nil:
		return SafetyPolicy{}, mapError(err)
	}
	p, perr := ParseSafetyPolicy(rules)
	if perr != nil {
		// A stored policy that no longer parses is a deployment fault, not a
		// reason to trade unbounded. Fail closed by refusing, loudly.
		return SafetyPolicy{}, errs.Wrapf(perr, errs.CodeInternal,
			"nativemarket: the recorded market safety policy %s does not parse", version)
	}
	p.Version = version
	return p, nil
}

// SafetyPolicyRecord is an operator recording a new safety policy.
type SafetyPolicyRecord struct {
	Version     string
	Rules       json.RawMessage
	EffectiveAt time.Time
	ActorType   security.ActorType
	ActorID     string
	Reason      string
}

// RecordSafetyPolicy stores a new version of the market-safety policy.
//
// Append-only: versions are unique and a row is immutable, so a policy that was
// in force stays readable forever and a decision made under it stays
// recomputable. An AGENT may never record one; the database refuses it too.
func RecordSafetyPolicy(ctx context.Context, tx pgx.Tx, rec SafetyPolicyRecord) (SafetyPolicy, error) {
	switch rec.ActorType {
	case security.ActorOperator, security.ActorSystem:
	default:
		return SafetyPolicy{}, errs.Newf(errs.CodeForbidden,
			"nativemarket: a market safety policy may only be recorded by an OPERATOR or SYSTEM actor, not %q",
			rec.ActorType)
	}
	switch {
	case strings.TrimSpace(rec.Version) == "":
		return SafetyPolicy{}, errs.New(errs.CodeValidationFailed, "nativemarket: safety policy version required")
	case strings.TrimSpace(rec.ActorID) == "":
		return SafetyPolicy{}, errs.New(errs.CodeValidationFailed, "nativemarket: actor id required")
	case strings.TrimSpace(rec.Reason) == "":
		return SafetyPolicy{}, errs.New(errs.CodeValidationFailed, "nativemarket: reason required")
	case rec.EffectiveAt.IsZero():
		return SafetyPolicy{}, errs.New(errs.CodeValidationFailed, "nativemarket: effective_at required")
	}
	p, err := ParseSafetyPolicy(rec.Rules)
	if err != nil {
		return SafetyPolicy{}, err
	}
	canon, err := p.CanonicalJSON()
	if err != nil {
		return SafetyPolicy{}, errs.Wrap(err, errs.CodeInternal, "nativemarket: canonical safety rules")
	}
	p.Version = rec.Version
	if _, err := tx.Exec(ctx,
		`INSERT INTO native_market_safety_policies
		   (id, version, rules, rules_hash, effective_at, created_by_actor_type, created_by_actor_id, reason)
		 VALUES ($1,$2,$3::jsonb,$4,$5,$6,$7,$8)`,
		id.New[id.Any]().String(), rec.Version, string(canon), p.Hash(), rec.EffectiveAt.UTC(),
		string(rec.ActorType), rec.ActorID, rec.Reason); err != nil {
		if db.IsUniqueViolation(err) {
			return SafetyPolicy{}, errs.Newf(errs.CodeConflict,
				"nativemarket: market safety policy %s is already recorded", rec.Version)
		}
		return SafetyPolicy{}, mapError(err)
	}
	return p, nil
}

// EffectiveSafetyPolicy returns the policy in force, for callers outside a
// trade -- the asset detail response's "limits in force", and operator tools.
func (s *Service) EffectiveSafetyPolicy(ctx context.Context, q db.Querier) (SafetyPolicy, error) {
	return s.safetyPolicy(ctx, q)
}
