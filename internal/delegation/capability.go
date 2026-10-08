package delegation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/valuation"
)

// MaxCapabilityLifetime bounds how long a grant can last; the database
// refuses anything longer (delegated_capabilities_lifetime).
const MaxCapabilityLifetime = 90 * 24 * time.Hour

// MaxDestinations bounds a capability's destination allowlist.
const MaxDestinations = 64

const capabilityHashDomain = "nodal-sandbox/delegated-capability/v1\x00"

// CapabilityTerms is everything a grant states. Normalize sorts and
// de-duplicates every list, so two grants that say the same thing hash the
// same.
type CapabilityTerms struct {
	AccountID             accounts.AccountID
	AgentID               string
	StrategyID            string
	Environment           config.Environment
	Mode                  gates.Mode
	InstrumentClasses     []InstrumentClass
	ActionClasses         []ActionClass
	MaxNotional           money.USD
	MaxDailyNotional      money.USD
	MaxSingleTrade        money.USD
	DestinationAllowlist  []devnet.PublicKey
	ChainAllowlist        []devnet.ChainID
	ExpiresAt             time.Time
	RequiresHumanApproval bool
}

// Normalize returns the terms with every list sorted and de-duplicated, ids
// lower-cased and the expiry in UTC at the database's microsecond precision.
func (t CapabilityTerms) Normalize() CapabilityTerms {
	t.AgentID = strings.ToLower(strings.TrimSpace(t.AgentID))
	t.StrategyID = strings.ToLower(strings.TrimSpace(t.StrategyID))
	t.InstrumentClasses = sortedUnique(t.InstrumentClasses)
	t.ActionClasses = sortedUnique(t.ActionClasses)
	t.ChainAllowlist = sortedUnique(t.ChainAllowlist)
	keys := make([]string, 0, len(t.DestinationAllowlist))
	for _, k := range t.DestinationAllowlist {
		keys = append(keys, k.String())
	}
	keys = sortedUnique(keys)
	t.DestinationAllowlist = t.DestinationAllowlist[:0:0]
	for _, k := range keys {
		pk, err := devnet.ParsePublicKey(k)
		if err == nil {
			t.DestinationAllowlist = append(t.DestinationAllowlist, pk)
		}
	}
	t.ExpiresAt = dbTime(t.ExpiresAt)
	return t
}

// Validate checks the terms against now: everything the database will also
// refuse, plus the shape of the identifiers.
func (t CapabilityTerms) Validate(now time.Time) error {
	var problems []string
	if t.AccountID.IsZero() {
		problems = append(problems, "account_id required")
	}
	if _, err := id.ParseAny(t.AgentID); err != nil {
		problems = append(problems, "agent_id must be a canonical uuid")
	}
	if _, err := id.ParseAny(t.StrategyID); err != nil {
		problems = append(problems, "strategy_id must be a canonical uuid")
	}
	if !t.Environment.IsValid() {
		problems = append(problems, "environment must be LOCAL, TEST, DEV, STAGING or PROD")
	}
	if !contains(SupportedModes(), t.Mode) {
		problems = append(problems, "mode must be SANDBOX or DEVNET")
	}
	if len(t.InstrumentClasses) == 0 {
		problems = append(problems, "at least one instrument class")
	}
	for _, c := range t.InstrumentClasses {
		if !c.Valid() {
			problems = append(problems, fmt.Sprintf("unknown instrument class %q", c))
		}
	}
	if len(t.ActionClasses) == 0 {
		problems = append(problems, "at least one action class")
	}
	for _, a := range t.ActionClasses {
		if !a.Valid() {
			problems = append(problems, fmt.Sprintf("unknown action class %q", a))
		}
	}
	if !t.MaxSingleTrade.IsPositive() || t.MaxSingleTrade.Cmp(t.MaxDailyNotional) > 0 || t.MaxDailyNotional.Cmp(t.MaxNotional) > 0 {
		problems = append(problems, "limits must satisfy 0 < max single trade <= max daily notional <= max notional")
	}
	if len(t.DestinationAllowlist) == 0 || len(t.DestinationAllowlist) > MaxDestinations {
		problems = append(problems, fmt.Sprintf("destination allowlist must name 1..%d addresses", MaxDestinations))
	}
	for _, d := range t.DestinationAllowlist {
		if d.IsZero() {
			problems = append(problems, "destination allowlist names the zero key")
		}
	}
	if len(t.ChainAllowlist) == 0 {
		problems = append(problems, "at least one chain")
	}
	for _, c := range t.ChainAllowlist {
		if !contains(Chains(), c) {
			problems = append(problems, "unknown chain "+string(c))
		}
	}
	granted := dbTime(now)
	if !t.ExpiresAt.After(granted) || t.ExpiresAt.Sub(granted) > MaxCapabilityLifetime {
		problems = append(problems, "expiry must lie after now and at most 90 days ahead")
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "delegation: invalid capability terms").WithField("problems", problems)
	}
	return nil
}

// Grants reports whether the terms grant action a.
func (t CapabilityTerms) Grants(a ActionClass) bool { return contains(t.ActionClasses, a) }

// AllowsClass reports whether the terms allow instrument class c.
func (t CapabilityTerms) AllowsClass(c InstrumentClass) bool { return contains(t.InstrumentClasses, c) }

// AllowsChain reports whether the terms allow chain c.
func (t CapabilityTerms) AllowsChain(c devnet.ChainID) bool { return contains(t.ChainAllowlist, c) }

// AllowsDestination reports whether the terms allow destination d.
func (t CapabilityTerms) AllowsDestination(d devnet.PublicKey) bool {
	return !d.IsZero() && contains(t.DestinationAllowlist, d)
}

// Capability is a delegated_capabilities row.
type Capability struct {
	ID               CapabilityID
	Terms            CapabilityTerms
	GrantedAt        time.Time
	Nonce            string
	ContentHash      [32]byte
	IdempotencyKey   string
	Status           CapabilityStatus
	GrantedBy        Actor
	RevokedBy        *Actor
	RevocationReason string
	RevokedAt        *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Actor is who did something (econ.Actor).
type Actor = econ.Actor

// capabilityDocument is what the content hash covers: the identity, the
// terms, the grant and its nonce. Never the status: revocation does not
// change what was granted.
type capabilityDocument struct {
	Version               string    `json:"version"`
	ID                    string    `json:"id"`
	AccountID             string    `json:"account_id"`
	AgentID               string    `json:"agent_id"`
	StrategyID            string    `json:"strategy_id"`
	Environment           string    `json:"environment"`
	Mode                  string    `json:"mode"`
	InstrumentClasses     []string  `json:"instrument_classes"`
	ActionClasses         []string  `json:"action_classes"`
	MaxNotionalMinor      string    `json:"max_notional_minor"`
	MaxDailyNotionalMinor string    `json:"max_daily_notional_minor"`
	MaxSingleTradeMinor   string    `json:"max_single_trade_minor"`
	DestinationAllowlist  []string  `json:"destination_allowlist"`
	ChainAllowlist        []string  `json:"chain_allowlist"`
	GrantedAt             time.Time `json:"granted_at"`
	ExpiresAt             time.Time `json:"expires_at"`
	Nonce                 string    `json:"nonce"`
	RequiresHumanApproval bool      `json:"requires_human_approval"`
	GrantedByActorType    string    `json:"granted_by_actor_type"`
	GrantedByActorID      string    `json:"granted_by_actor_id"`
}

// ComputeContentHash returns the hash the capability must carry.
func (c Capability) ComputeContentHash() ([32]byte, error) {
	t := c.Terms.Normalize()
	doc := capabilityDocument{
		Version: "nodal-sandbox/delegated-capability/v1", ID: c.ID.String(), AccountID: t.AccountID.String(),
		AgentID: t.AgentID, StrategyID: t.StrategyID, Environment: string(t.Environment), Mode: string(t.Mode),
		InstrumentClasses: strs(t.InstrumentClasses), ActionClasses: strs(t.ActionClasses),
		MaxNotionalMinor:      strconv.FormatInt(t.MaxNotional.Minor(), 10),
		MaxDailyNotionalMinor: strconv.FormatInt(t.MaxDailyNotional.Minor(), 10),
		MaxSingleTradeMinor:   strconv.FormatInt(t.MaxSingleTrade.Minor(), 10),
		ChainAllowlist:        strs(t.ChainAllowlist), GrantedAt: dbTime(c.GrantedAt), ExpiresAt: t.ExpiresAt,
		Nonce: c.Nonce, RequiresHumanApproval: t.RequiresHumanApproval,
		GrantedByActorType: string(c.GrantedBy.Type), GrantedByActorID: c.GrantedBy.ID,
	}
	for _, d := range t.DestinationAllowlist {
		doc.DestinationAllowlist = append(doc.DestinationAllowlist, d.String())
	}
	if doc.DestinationAllowlist == nil {
		doc.DestinationAllowlist = []string{}
	}
	b, err := audit.CanonicalJSON(doc)
	if err != nil {
		return [32]byte{}, errs.Wrap(err, errs.CodeInternal, "delegation: capability is not canonically encodable")
	}
	return sha256.Sum256(append([]byte(capabilityHashDomain), b...)), nil
}

// HashHex is the content hash in hex.
func (c Capability) HashHex() string { return hex.EncodeToString(c.ContentHash[:]) }

// termsKey is what makes two grant requests "the same grant" for
// idempotency: the normalized terms, without anything the server assigns.
func termsKey(t CapabilityTerms) (string, error) {
	t = t.Normalize()
	doc := capabilityDocument{
		AccountID: t.AccountID.String(), AgentID: t.AgentID, StrategyID: t.StrategyID, Environment: string(t.Environment),
		Mode: string(t.Mode), InstrumentClasses: strs(t.InstrumentClasses), ActionClasses: strs(t.ActionClasses),
		MaxNotionalMinor:      strconv.FormatInt(t.MaxNotional.Minor(), 10),
		MaxDailyNotionalMinor: strconv.FormatInt(t.MaxDailyNotional.Minor(), 10),
		MaxSingleTradeMinor:   strconv.FormatInt(t.MaxSingleTrade.Minor(), 10),
		ChainAllowlist:        strs(t.ChainAllowlist), ExpiresAt: t.ExpiresAt, RequiresHumanApproval: t.RequiresHumanApproval,
	}
	for _, d := range t.DestinationAllowlist {
		doc.DestinationAllowlist = append(doc.DestinationAllowlist, d.String())
	}
	b, err := audit.CanonicalJSON(doc)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeInternal, "delegation: terms are not canonically encodable")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Authority reason codes: why a capability does not cover a use. Stable and
// machine-readable; they appear in signing decisions and refusals.
const (
	ReasonCapabilityRevoked       = "DELEGATION_CAPABILITY_REVOKED"
	ReasonCapabilityExpired       = "DELEGATION_CAPABILITY_EXPIRED"
	ReasonCapabilityNotYetValid   = "DELEGATION_CAPABILITY_NOT_YET_VALID"
	ReasonCapabilityTampered      = "DELEGATION_CAPABILITY_TAMPERED"
	ReasonAccountMismatch         = "DELEGATION_ACCOUNT_MISMATCH"
	ReasonAgentMismatch           = "DELEGATION_AGENT_MISMATCH"
	ReasonStrategyMismatch        = "DELEGATION_STRATEGY_MISMATCH"
	ReasonEnvironmentMismatch     = "DELEGATION_ENVIRONMENT_MISMATCH"
	ReasonModeMismatch            = "DELEGATION_MODE_MISMATCH"
	ReasonActionNotGranted        = "DELEGATION_ACTION_NOT_GRANTED"
	ReasonInstrumentNotGranted    = "DELEGATION_INSTRUMENT_CLASS_NOT_GRANTED"
	ReasonChainNotGranted         = "DELEGATION_CHAIN_NOT_GRANTED"
	ReasonDestinationNotGranted   = "DELEGATION_DESTINATION_NOT_GRANTED"
	ReasonSingleLimitExceeded     = "DELEGATION_SINGLE_LIMIT"
	ReasonDailyLimitExceeded      = "DELEGATION_DAILY_LIMIT"
	ReasonLifetimeLimitExceeded   = "DELEGATION_LIFETIME_LIMIT"
	ReasonApprovalRequired        = "DELEGATION_APPROVAL_REQUIRED"
	ReasonApprovalPlanMismatch    = "DELEGATION_APPROVAL_PLAN_MISMATCH"
	ReasonQuoteExpired            = "DELEGATION_QUOTE_EXPIRED"
	ReasonCoolingPeriod           = "DELEGATION_COOLING_PERIOD"
	ReasonDestinationRemoved      = "DELEGATION_DESTINATION_REMOVED"
	ReasonChainFeeChanged         = "DELEGATION_CHAIN_FEE_CHANGED"
	ReasonFeeAbovePolicy          = "DELEGATION_FEE_ABOVE_POLICY"
	ReasonSlippageAbovePolicy     = "DELEGATION_SLIPPAGE_ABOVE_POLICY"
	ReasonSubjectNotSignable      = "DELEGATION_SUBJECT_NOT_SIGNABLE"
	ReasonMessageNotPersisted     = "DELEGATION_MESSAGE_NOT_THE_COMPILED_ONE"
	ReasonSignerKeyMismatch       = "DELEGATION_SIGNER_KEY_MISMATCH"
	ReasonPlanTampered            = "DELEGATION_PLAN_TAMPERED"
	ReasonVenueSignerUnavailable  = "DELEGATION_VENUE_SIGNER_UNAVAILABLE"
	ReasonAccountNotActive        = "DELEGATION_ACCOUNT_NOT_ACTIVE"
	ReasonAgentNotActive          = "DELEGATION_AGENT_NOT_ACTIVE"
	ReasonBlockedBeforeSubmission = "BLOCKED_BEFORE_SUBMISSION"
	// ReasonActionMismatch: the swap's action label is not what the swap
	// does (SwapAction).
	ReasonActionMismatch = "DELEGATION_ACTION_MISMATCH"
)

// Use is one use of a capability the caller asks to make, with the usage
// already recorded under it.
type Use struct {
	AccountID   accounts.AccountID
	AgentID     string
	StrategyID  string
	Action      ActionClass
	Classes     []InstrumentClass
	Chain       devnet.ChainID
	Destination devnet.PublicKey
	Notional    money.USD
	DayUsed     money.USD
	TotalUsed   money.USD
	Environment config.Environment
	Mode        gates.Mode
	Now         time.Time
}

// AuthorityCheck is a capability measured against one use.
type AuthorityCheck struct {
	Present      bool
	Expired      bool
	Revoked      bool
	WithinLimits bool
	InScope      bool
	Codes        []string
}

// OK reports whether the capability covers the use.
func (a AuthorityCheck) OK() bool { return len(a.Codes) == 0 }

// Snapshot is the check as Risk Kernel V2's authority section.
func (a AuthorityCheck) Snapshot() *risk.AuthoritySnapshot {
	return &risk.AuthoritySnapshot{Present: a.Present, Expired: a.Expired, Revoked: a.Revoked,
		WithinLimits: a.WithinLimits, InScope: a.InScope}
}

// Check measures the capability against a use. It recomputes the content
// hash, so a row edited behind the trigger's back reads as absent. It is pure.
func (c Capability) Check(u Use) AuthorityCheck {
	var codes []string
	add := func(s string) { codes = append(codes, s) }
	t := c.Terms.Normalize()
	present := true
	if h, err := c.ComputeContentHash(); err != nil || h != c.ContentHash {
		present = false
		add(ReasonCapabilityTampered)
	}
	if t.AccountID != u.AccountID {
		present = false
		add(ReasonAccountMismatch)
	}
	if !strings.EqualFold(t.AgentID, u.AgentID) {
		present = false
		add(ReasonAgentMismatch)
	}
	revoked := c.Status != CapabilityActive
	if revoked {
		add(ReasonCapabilityRevoked)
	}
	now := u.Now.UTC()
	expired := false
	switch {
	case !now.Before(t.ExpiresAt):
		expired = true
		add(ReasonCapabilityExpired)
	case now.Before(c.GrantedAt):
		expired = true
		add(ReasonCapabilityNotYetValid)
	}
	inScope := true
	scope := func(ok bool, code string) {
		if !ok {
			inScope = false
			add(code)
		}
	}
	scope(strings.EqualFold(t.StrategyID, u.StrategyID), ReasonStrategyMismatch)
	scope(t.Environment == u.Environment, ReasonEnvironmentMismatch)
	scope(t.Mode == u.Mode, ReasonModeMismatch)
	scope(t.Grants(u.Action), ReasonActionNotGranted)
	classesOK := len(u.Classes) > 0
	for _, cl := range u.Classes {
		if !t.AllowsClass(cl) {
			classesOK = false
		}
	}
	scope(classesOK, ReasonInstrumentNotGranted)
	scope(t.AllowsChain(u.Chain), ReasonChainNotGranted)
	scope(t.AllowsDestination(u.Destination), ReasonDestinationNotGranted)
	within := true
	limit := func(ok bool, code string) {
		if !ok {
			within = false
			add(code)
		}
	}
	limit(u.Notional.IsPositive() && u.Notional.Cmp(t.MaxSingleTrade) <= 0, ReasonSingleLimitExceeded)
	day, err := u.DayUsed.Add(u.Notional)
	limit(err == nil && day.Cmp(t.MaxDailyNotional) <= 0, ReasonDailyLimitExceeded)
	total, err := u.TotalUsed.Add(u.Notional)
	limit(err == nil && total.Cmp(t.MaxNotional) <= 0, ReasonLifetimeLimitExceeded)
	sort.Strings(codes)
	return AuthorityCheck{Present: present, Expired: expired, Revoked: revoked, WithinLimits: within, InScope: inScope, Codes: codes}
}

// UsableAt reports whether the capability may be used at all at now: ACTIVE,
// granted no later than now, not yet expired, and its content hash intact.
// It is the status-and-lifetime half of Check, for a reader deciding whether
// a capability is worth offering; it says nothing about any particular use,
// which only Check measures and every admission measures again.
func (c Capability) UsableAt(now time.Time) bool {
	if c.Status != CapabilityActive {
		return false
	}
	now = now.UTC()
	if now.Before(c.GrantedAt) || !now.Before(c.Terms.Normalize().ExpiresAt) {
		return false
	}
	h, err := c.ComputeContentHash()
	return err == nil && h == c.ContentHash
}

// ClassifyAsset returns the instrument class of a SIMULATED asset: a
// USD-pegged stable unit is STABLE_VALUE, a SPECULATIVE asset is
// SPECULATIVE_TOKEN, any other supported asset is SPOT_TOKEN. Anything that
// is not SIMULATED, or is UNSUPPORTED, has no class and is never delegated.
func ClassifyAsset(a assets.Asset) (InstrumentClass, bool) {
	if a.Kind != assets.KindSimulated {
		return "", false
	}
	switch {
	case a.IsStablecoin && a.PegCurrency == valuation.USDPeg:
		return ClassStableValue, true
	case a.RiskClass == assets.RiskSpeculative:
		return ClassSpeculativeToken, true
	case a.RiskClass == assets.RiskUnsupported:
		return "", false
	}
	return ClassSpotToken, true
}

// SwapAction is what a swap paying pay for receive does, decided by the
// direction of its value and never by the agent's label. A venue quotes
// every asset in the sandbox dollar, so a USD-pegged stable unit
// (STABLE_VALUE) is the quote side of any pair it is in: paying a token
// (SPOT_TOKEN or SPECULATIVE_TOKEN) for it sells that exposure, and only that
// is REDUCE_EXPOSURE. Everything else acquires exposure -- buying a token
// with the stable unit, one token for another, one stable unit for another --
// and so does a swap touching an asset with no instrument class: what cannot
// be classified is never a reduction. cp_delegation_swap_action (migration
// 00762) is the same rule in the database.
func SwapAction(pay, receive assets.Asset) ActionClass {
	payClass, payOK := ClassifyAsset(pay)
	receiveClass, receiveOK := ClassifyAsset(receive)
	return swapActionOf(payClass, payOK, receiveClass, receiveOK)
}

// swapActionOf is SwapAction over the two assets' instrument classes.
func swapActionOf(pay InstrumentClass, payOK bool, receive InstrumentClass, receiveOK bool) ActionClass {
	if payOK && receiveOK && receive == ClassStableValue && pay != ClassStableValue {
		return ActionReduceExposure
	}
	return ActionAcquireExposure
}

// dbTime is t in UTC at the database's microsecond precision, so a value
// hashed before it is stored hashes the same after it is read back.
func dbTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func sortedUnique[T ~string](in []T) []T {
	out := append([]T(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 0
	for i, v := range out {
		if i == 0 || v != out[n-1] {
			out[n] = v
			n++
		}
	}
	return out[:n]
}

func strs[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	return out
}
