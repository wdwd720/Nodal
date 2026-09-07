package valuedomain

import (
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/errs"
)

// Domain is the kind of value a balance represents (PART IX).
//
// It is carried by assets, inherited by ledger accounts, and reported with
// every balance the API returns. Two balances in different domains are never
// summed without an explicitly defined, legally correct aggregation.
type Domain string

// Value domains.
const (
	// InternalCredit is Nodal Credits: a closed-loop internal unit of account.
	// Not money, not a claim on money, not redeemable by default.
	InternalCredit Domain = "INTERNAL_CREDIT"

	// InternalNativeAsset is a Nodal-native asset created inside the platform.
	// It is not a blockchain token and has no external existence.
	InternalNativeAsset Domain = "INTERNAL_NATIVE_ASSET"

	// Simulated is backtest, paper and shadow capital. It has no economic
	// substance and may never touch any other domain.
	Simulated Domain = "SIMULATED"

	// HostedFiat is real fiat held by a licensed financial partner on the
	// customer's behalf. The partner is authoritative; Nodal mirrors.
	HostedFiat Domain = "HOSTED_FIAT"

	// HostedCrypto is real crypto held by a licensed partner or qualified
	// custodian. The partner is authoritative; Nodal mirrors.
	HostedCrypto Domain = "HOSTED_CRYPTO"

	// SelfCustodialCrypto is real crypto in a wallet the customer controls.
	// The chain is authoritative; Nodal observes and reconciles.
	SelfCustodialCrypto Domain = "SELF_CUSTODIAL_CRYPTO"

	// PayoutPending is value that has left an internal domain for a payout
	// and has not yet settled externally. It is a staging domain: value here
	// is spoken for and is not spendable, and it exists so that a payout in
	// flight is never double-counted as either internal or external.
	PayoutPending Domain = "PAYOUT_PENDING"

	// ExternalSettled is value that has irrevocably left the system through an
	// approved provider. It is terminal and exists for reconciliation only.
	ExternalSettled Domain = "EXTERNAL_SETTLED"
)

type domainInfo struct {
	rail CapitalRail
	// economic reports whether the domain carries real economic substance.
	// Simulated is the only domain that does not.
	economic bool
	// internalUnit reports whether Nodal itself is the authoritative record.
	internalUnit bool
	// spendable reports whether a positive balance can fund new activity in
	// the ordinary course. PayoutPending and ExternalSettled cannot.
	spendable bool
}

var domainRegistry = map[Domain]domainInfo{
	InternalCredit:      {rail: RailNativeInternal, economic: true, internalUnit: true, spendable: true},
	InternalNativeAsset: {rail: RailNativeInternal, economic: true, internalUnit: true, spendable: true},
	Simulated:           {rail: RailSimulated, economic: false, internalUnit: true, spendable: true},
	HostedFiat:          {rail: RailHostedPartner, economic: true, internalUnit: false, spendable: true},
	HostedCrypto:        {rail: RailHostedPartner, economic: true, internalUnit: false, spendable: true},
	SelfCustodialCrypto: {rail: RailSelfCustodialOnchain, economic: true, internalUnit: false, spendable: true},
	PayoutPending:       {rail: RailNativeInternal, economic: true, internalUnit: true, spendable: false},
	ExternalSettled:     {rail: RailNativeInternal, economic: true, internalUnit: false, spendable: false},
}

var allDomains = []Domain{
	InternalCredit, InternalNativeAsset, Simulated,
	HostedFiat, HostedCrypto, SelfCustodialCrypto,
	PayoutPending, ExternalSettled,
}

// AllDomains returns every declared domain in declaration order (a copy).
func AllDomains() []Domain { return append([]Domain(nil), allDomains...) }

// Valid reports whether d is a declared domain.
func (d Domain) Valid() bool {
	_, ok := domainRegistry[d]
	return ok
}

func (d Domain) String() string { return string(d) }

// Rail returns the capital rail the domain lives on. An unknown domain
// returns the empty rail, which no policy treats as permissive.
func (d Domain) Rail() CapitalRail { return domainRegistry[d].rail }

// IsEconomic reports whether the domain carries real economic substance.
// Simulated is the only declared domain that does not.
func (d Domain) IsEconomic() bool { return domainRegistry[d].economic }

// IsInternalUnit reports whether Nodal's own ledger is the authoritative
// record for the domain. Where this is false some external party — a partner,
// or a blockchain — is authoritative and Nodal holds a mirror that must
// reconcile (PART XXIII).
func (d Domain) IsInternalUnit() bool { return domainRegistry[d].internalUnit }

// IsSpendable reports whether a positive balance in the domain can fund new
// activity. Value in PayoutPending is committed to a payout and value in
// ExternalSettled has left the system.
func (d Domain) IsSpendable() bool { return domainRegistry[d].spendable }

// ParseDomain parses the canonical uppercase string form.
func ParseDomain(s string) (Domain, error) {
	d := Domain(strings.ToUpper(strings.TrimSpace(s)))
	if !d.Valid() {
		return "", errs.Newf(errs.CodeValidationFailed, "unknown value domain %q", s)
	}
	return d, nil
}

// SortDomains orders domains canonically (declaration order) so that any
// serialised set of domains hashes identically regardless of how it was built.
func SortDomains(ds []Domain) {
	rank := make(map[Domain]int, len(allDomains))
	for i, d := range allDomains {
		rank[d] = i
	}
	sort.SliceStable(ds, func(i, j int) bool {
		ri, oki := rank[ds[i]]
		rj, okj := rank[ds[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki:
			return true
		case okj:
			return false
		default:
			return ds[i] < ds[j]
		}
	})
}
