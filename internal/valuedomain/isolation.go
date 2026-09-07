package valuedomain

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// This file answers one question: which value domains may appear together
// inside a single atomic economic event?
//
// The question matters because a journal transaction is the unit of value
// movement. If a transaction may debit INTERNAL_CREDIT and credit
// SELF_CUSTODIAL_CRYPTO, then Credits are convertible to SOL and every
// statement the product makes about closed-loop Credits is false — regardless
// of what any policy row or UI copy says. So the rule is enforced where value
// actually moves, in Go here and again in PostgreSQL (migration 00710).
//
// Two tiers:
//
//   - Structurally forbidden pairs. No capability, policy version, approval or
//     configuration enables them. They are compiled in because they are the
//     product's core promises, and a promise that a config flag can revoke is
//     not a promise.
//   - Gated conversions. Legitimate cross-domain movements that exist, are
//     named, and require an ACTIVE capability gate every single time.
//
// Anything not in either tier is forbidden by default.

// ConversionKey identifies an ordered pair of domains.
type ConversionKey struct {
	From Domain
	To   Domain
}

func (k ConversionKey) String() string { return string(k.From) + "->" + string(k.To) }

// Conversion is a declared, capability-gated movement of value between two
// domains.
type Conversion struct {
	From Domain
	To   Domain

	// RequiredCapability must be ACTIVE for the movement to be permitted. An
	// empty capability means the movement is always permitted, which is only
	// ever correct for a return path — see AlwaysPermitted.
	RequiredCapability CapabilityKey

	// Why is operator-facing text explaining what this conversion is for.
	Why string
}

// AlwaysPermitted reports whether the conversion needs no capability.
//
// Only unwind paths qualify. PART XXXII requires that stopping new risk must
// not stop required unwind workflows: if PAYOUT_SETTLE is revoked while a
// payout is in flight, the value sitting in PAYOUT_PENDING has to be able to
// get back to the user. A capability check on the return path would strand it.
func (c Conversion) AlwaysPermitted() bool { return c.RequiredCapability == "" }

// Capability keys used by the declared conversions. They are the names the
// gate rows carry; internal/gates converts them to its own Capability type.
const (
	CapNativeMarketTrading CapabilityKey = "NATIVE_MARKET_TRADING"
	CapPayoutReserve       CapabilityKey = "PAYOUT_RESERVE"
	CapPayoutSettle        CapabilityKey = "PAYOUT_SETTLE"
	CapHostedTrading       CapabilityKey = "HOSTED_TRADING"
	CapHostedFunding       CapabilityKey = "HOSTED_FUNDING"
)

// gatedConversions is the complete set of legal cross-domain movements.
// Adding an entry here is an architectural decision and belongs in
// DECISION_REGISTER.md.
var gatedConversions = map[ConversionKey]Conversion{
	{InternalCredit, InternalNativeAsset}: {
		From: InternalCredit, To: InternalNativeAsset,
		RequiredCapability: CapNativeMarketTrading,
		Why:                "buying a Nodal-native asset with Credits on the internal market",
	},
	{InternalNativeAsset, InternalCredit}: {
		From: InternalNativeAsset, To: InternalCredit,
		RequiredCapability: CapNativeMarketTrading,
		Why:                "selling a Nodal-native asset for Credits on the internal market",
	},
	{InternalCredit, PayoutPending}: {
		From: InternalCredit, To: PayoutPending,
		RequiredCapability: CapPayoutReserve,
		Why:                "reserving eligible Credits against a payout request",
	},
	{PayoutPending, ExternalSettled}: {
		From: PayoutPending, To: ExternalSettled,
		RequiredCapability: CapPayoutSettle,
		Why:                "an approved provider has irrevocably paid the user",
	},
	{PayoutPending, InternalCredit}: {
		From: PayoutPending, To: InternalCredit,
		// Deliberately ungated: see AlwaysPermitted.
		Why: "returning reserved Credits after a payout was rejected, failed or cancelled",
	},
	{HostedFiat, HostedCrypto}: {
		From: HostedFiat, To: HostedCrypto,
		RequiredCapability: CapHostedTrading,
		Why:                "buying crypto with fiat inside a hosted partner account",
	},
	{HostedCrypto, HostedFiat}: {
		From: HostedCrypto, To: HostedFiat,
		RequiredCapability: CapHostedTrading,
		Why:                "selling crypto for fiat inside a hosted partner account",
	},
	{ExternalSettled, HostedFiat}: {
		From: ExternalSettled, To: HostedFiat,
		RequiredCapability: CapHostedFunding,
		Why:                "an external deposit arriving into a hosted partner account",
	},
	{HostedFiat, ExternalSettled}: {
		From: HostedFiat, To: ExternalSettled,
		RequiredCapability: CapHostedFunding,
		Why:                "a withdrawal leaving a hosted partner account",
	},
}

// AllConversions returns every declared conversion, ordered canonically.
func AllConversions() []Conversion {
	out := make([]Conversion, 0, len(gatedConversions))
	for _, c := range gatedConversions {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

// LookupConversion returns the declared conversion for a domain pair.
func LookupConversion(from, to Domain) (Conversion, bool) {
	c, ok := gatedConversions[ConversionKey{from, to}]
	return c, ok
}

// StructurallyForbidden reports whether a pair of domains may never appear in
// one transaction, no matter what any policy, gate or operator says, and the
// reason.
//
// The three rules:
//
//  1. SIMULATED never mixes with anything. Simulated capital has no substance;
//     a transaction that moved value between a shadow portfolio and any real
//     or internal domain would be manufacturing value out of a backtest.
//  2. INTERNAL_CREDIT and INTERNAL_NATIVE_ASSET never appear alongside a
//     real-capital domain. This is the promise that Credits are closed-loop.
//     The only route from Credits to the outside world is the declared payout
//     path, INTERNAL_CREDIT → PAYOUT_PENDING → EXTERNAL_SETTLED, which is
//     gated at both steps and goes through an approved provider. PART IX
//     forbids a disguised Credits → crypto path, and this is the rule that
//     makes it impossible rather than merely discouraged.
//  3. SELF_CUSTODIAL_CRYPTO never mixes with anything. Nodal does not hold it,
//     cannot move it, and only mirrors what the chain already did; a
//     cross-domain transaction touching it would assert an atomic effect
//     across a boundary Nodal does not control.
func StructurallyForbidden(a, b Domain) (bool, string) {
	if a == b {
		return false, ""
	}
	if a == Simulated || b == Simulated {
		return true, "simulated capital has no economic substance and never moves to or from any other domain"
	}
	if a == SelfCustodialCrypto || b == SelfCustodialCrypto {
		return true, "self-custodial value is authoritative on-chain; Nodal mirrors it and never moves it atomically with another domain"
	}
	internal := func(d Domain) bool { return d == InternalCredit || d == InternalNativeAsset }
	realCapital := func(d Domain) bool { return d == HostedFiat || d == HostedCrypto || d == SelfCustodialCrypto }
	if (internal(a) && realCapital(b)) || (internal(b) && realCapital(a)) {
		return true, "internal Credits and native assets are closed-loop; the only route to external value is the gated payout path INTERNAL_CREDIT -> PAYOUT_PENDING -> EXTERNAL_SETTLED"
	}
	return false, ""
}

// CheckConversion reports whether value may move from one domain to another.
//
// Direction is not optional. An earlier draft of this function took only the
// set of domains a transaction touched and accepted the pair if a conversion
// was declared in *either* direction. That was wrong in a way that mattered:
// PAYOUT_PENDING → INTERNAL_CREDIT is deliberately ungated so an in-flight
// payout can always be unwound, and a direction-blind check therefore let that
// ungated return path authorise the gated INTERNAL_CREDIT → PAYOUT_PENDING
// reserve step, defeating PAYOUT_RESERVE entirely. Callers know which way value
// moved, so they state it.
//
// activeCaps is the set of capability keys currently ACTIVE. A nil map is
// treated as "nothing is active", which is the correct reading for a fresh
// deployment.
func CheckConversion(from, to Domain, activeCaps map[CapabilityKey]bool) error {
	if !from.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown source value domain %q", from)
	}
	if !to.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "unknown destination value domain %q", to)
	}
	if from == to {
		return nil
	}
	if forbidden, why := StructurallyForbidden(from, to); forbidden {
		return errs.Newf(errs.CodeForbidden,
			"value domains %s and %s may never move together: %s", from, to, why).
			WithField("from_domain", string(from)).
			WithField("to_domain", string(to)).
			WithField("structural", true)
	}
	conv, ok := gatedConversions[ConversionKey{from, to}]
	if !ok {
		return errs.Newf(errs.CodeForbidden,
			"no declared conversion moves value from %s to %s", from, to).
			WithField("from_domain", string(from)).
			WithField("to_domain", string(to))
	}
	if conv.AlwaysPermitted() {
		return nil
	}
	if activeCaps[conv.RequiredCapability] {
		return nil
	}
	return errs.Newf(errs.CodeCapabilityNotApproved,
		"moving value from %s to %s requires capability %s to be ACTIVE",
		from, to, conv.RequiredCapability).
		WithField("from_domain", string(from)).
		WithField("to_domain", string(to)).
		WithField("required_capability", string(conv.RequiredCapability))
}

// CheckPosting is what the ledger calls before committing a journal
// transaction. domains is every domain the transaction's entries touch, and
// declared is the conversion the caller says it is performing.
//
// A single-domain transaction must declare no conversion; single-domain safety
// is the ledger's own business (balance, sign, immutability). A transaction
// touching two domains must declare exactly which conversion it is, and the
// declaration must match the domains present — so a cross-domain movement is
// always a stated intent that appears in the audit trail, never an emergent
// property of which accounts happened to be involved.
//
// Three or more domains are always rejected: every declared conversion is a
// pair, and a wider movement would be an undeclared composite whose legality
// nobody has reasoned about.
func CheckPosting(domains []Domain, declared *ConversionKey, activeCaps map[CapabilityKey]bool) error {
	uniq := make(map[Domain]struct{}, len(domains))
	for _, d := range domains {
		if !d.Valid() {
			return errs.Newf(errs.CodeValidationFailed, "unknown value domain %q in transaction", d)
		}
		uniq[d] = struct{}{}
	}
	switch len(uniq) {
	case 0:
		return errs.New(errs.CodeValidationFailed, "transaction touches no value domain")
	case 1:
		if declared != nil && declared.From != declared.To {
			return errs.Newf(errs.CodeValidationFailed,
				"transaction declares conversion %s but touches only one value domain", declared)
		}
		return nil
	case 2: // checked below
	default:
		list := make([]string, 0, len(uniq))
		for d := range uniq {
			list = append(list, string(d))
		}
		sort.Strings(list)
		return errs.Newf(errs.CodeForbidden,
			"a transaction may touch at most two value domains; this one touches %d (%s). Every declared conversion is a pair; split the movement into declared steps",
			len(uniq), strings.Join(list, ", "))
	}

	pair := make([]Domain, 0, 2)
	for d := range uniq {
		pair = append(pair, d)
	}
	SortDomains(pair)
	a, b := pair[0], pair[1]

	// Report a structural violation before complaining about the missing
	// declaration: "these may never move together" is the useful answer, and
	// adding a declaration would not have helped.
	if forbidden, why := StructurallyForbidden(a, b); forbidden {
		return errs.Newf(errs.CodeForbidden,
			"value domains %s and %s may never move together: %s", a, b, why).
			WithField("domain_a", string(a)).
			WithField("domain_b", string(b)).
			WithField("structural", true)
	}
	if declared == nil {
		return errs.Newf(errs.CodeValidationFailed,
			"transaction touches value domains %s and %s but declares no conversion; a cross-domain movement must state which conversion it is",
			a, b).
			WithField("domain_a", string(a)).
			WithField("domain_b", string(b))
	}
	if !((declared.From == a && declared.To == b) || (declared.From == b && declared.To == a)) {
		return errs.Newf(errs.CodeValidationFailed,
			"transaction declares conversion %s but touches value domains %s and %s",
			declared, a, b)
	}
	return CheckConversion(declared.From, declared.To, activeCaps)
}

// DescribeIsolation renders the full isolation matrix as operator-facing text.
// It is used by documentation generation and by the admin plane so that what
// operators read is derived from the same table the ledger enforces, rather
// than a prose copy of it that can drift.
func DescribeIsolation() string {
	var b strings.Builder
	b.WriteString("VALUE DOMAIN ISOLATION MATRIX\n\n")
	b.WriteString("Structurally forbidden (no capability can enable these):\n")
	seen := map[string]bool{}
	for _, x := range allDomains {
		for _, y := range allDomains {
			if x >= y {
				continue
			}
			forbidden, why := StructurallyForbidden(x, y)
			if !forbidden {
				continue
			}
			key := why
			if seen[key] {
				continue
			}
			seen[key] = true
			b.WriteString("  - " + why + "\n")
		}
	}
	b.WriteString("\nDeclared conversions (each requires its capability to be ACTIVE):\n")
	for _, c := range AllConversions() {
		cap := string(c.RequiredCapability)
		if c.AlwaysPermitted() {
			cap = "(none - unwind path, must never be blocked)"
		}
		b.WriteString(fmt.Sprintf("  - %-24s -> %-24s %s\n      %s\n", c.From, c.To, cap, c.Why))
	}
	return b.String()
}
