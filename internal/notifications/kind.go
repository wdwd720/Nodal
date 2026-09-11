package notifications

// Kind is the notification category. The list is mirrored by the CHECK on
// notifications.kind and compared against it by test/integration/enums.
type Kind string

// The product vocabulary. These are the kinds this package's Producer emits;
// every one of them corresponds to a state change that already exists in the
// schema at the commit this package was written against.
const (
	// KindCreditPurchaseCaptured — a card payment for Credits reached CAPTURED.
	KindCreditPurchaseCaptured Kind = "CREDIT_PURCHASE_CAPTURED"
	// KindCreditPurchaseReversed — a captured purchase was reversed, refunded,
	// disputed or failed. Money the person believed they had is not there.
	KindCreditPurchaseReversed Kind = "CREDIT_PURCHASE_REVERSED"
	// KindNativeTradeFilled — a buy or sell executed against a native market.
	KindNativeTradeFilled Kind = "NATIVE_TRADE_FILLED"
	// KindNativeMarketPaused — a market a person has traded stopped accepting
	// one or both sides.
	KindNativeMarketPaused Kind = "NATIVE_MARKET_PAUSED"
	// KindPayoutAccepted — a payout request reached the provider.
	KindPayoutAccepted Kind = "PAYOUT_ACCEPTED"
	// KindPayoutSettled — a payout settled.
	KindPayoutSettled Kind = "PAYOUT_SETTLED"
	// KindPayoutFailed — a payout failed, was rejected or was reversed.
	KindPayoutFailed Kind = "PAYOUT_FAILED"
	// KindPayoutNeedsReview — a payout stopped and needs something from the
	// person or from an operator before it can continue.
	KindPayoutNeedsReview Kind = "PAYOUT_NEEDS_REVIEW"
	// KindVerificationUpdated — the verification level Nodal holds for this
	// person changed.
	KindVerificationUpdated Kind = "VERIFICATION_UPDATED"
	// KindAccountRestricted — an account left ACTIVE.
	KindAccountRestricted Kind = "ACCOUNT_RESTRICTED"
	// KindSecurityNewSession — a session was created for this person.
	KindSecurityNewSession Kind = "SECURITY_NEW_SESSION"
	// KindAgentPaused — an agent stopped acting.
	KindAgentPaused Kind = "AGENT_PAUSED"
	// KindSystem — an announcement addressed to one person that no other kind
	// describes. It is deliberately last and deliberately not suppressible.
	KindSystem Kind = "SYSTEM"
)

// The nine names migration 00640 declared and internal/notification still
// declares. Nothing on the product path writes them: they are here because the
// CHECK still admits them, and a value the database accepts that no Go list
// names is precisely what test/integration/enums exists to refuse. Producer
// refuses them (see Notification.Validate), so the only writer that can put one
// in the table is internal/notification itself.
//
// They are expected to disappear together with that package, in one change that
// narrows the CHECK and deletes the code -- not in this one, because deleting a
// package deletes its tests.
const (
	legacyKindFundingAvailable     Kind = "FUNDING_AVAILABLE"
	legacyKindFundingFailed        Kind = "FUNDING_FAILED"
	legacyKindFundingReversed      Kind = "FUNDING_REVERSED"
	legacyKindTradeFilled          Kind = "TRADE_FILLED"
	legacyKindTradeFailed          Kind = "TRADE_FAILED"
	legacyKindRiskLimitHit         Kind = "RISK_LIMIT_HIT"
	legacyKindSecuritySessionEvent Kind = "SECURITY_SESSION_EVENT"
	legacyKindReconciliationHold   Kind = "RECONCILIATION_HOLD"
)

// productKinds is the vocabulary Producer may write.
var productKinds = []Kind{
	KindCreditPurchaseCaptured, KindCreditPurchaseReversed,
	KindNativeTradeFilled, KindNativeMarketPaused,
	KindPayoutAccepted, KindPayoutSettled, KindPayoutFailed, KindPayoutNeedsReview,
	KindVerificationUpdated, KindAccountRestricted, KindSecurityNewSession, KindAgentPaused,
	KindSystem,
}

var legacyKinds = []Kind{
	legacyKindFundingAvailable, legacyKindFundingFailed, legacyKindFundingReversed,
	legacyKindTradeFilled, legacyKindTradeFailed, legacyKindRiskLimitHit,
	legacyKindSecuritySessionEvent, legacyKindReconciliationHold,
}

// ProductKinds returns the kinds the product surface emits and accepts as a
// filter, in declaration order.
func ProductKinds() []Kind { return append([]Kind(nil), productKinds...) }

// AllKinds returns every value notifications.kind admits: the product
// vocabulary plus the names 00640 declared and nothing produces. It is the
// list test/integration/enums compares against the CHECK.
func AllKinds() []Kind {
	out := make([]Kind, 0, len(productKinds)+len(legacyKinds))
	out = append(out, productKinds...)
	out = append(out, legacyKinds...)
	return out
}

// IsProduct reports whether k is a kind this package may write.
func (k Kind) IsProduct() bool {
	for _, p := range productKinds {
		if p == k {
			return true
		}
	}
	return false
}

// Valid reports whether k is a value the column admits.
func (k Kind) Valid() bool {
	if k.IsProduct() {
		return true
	}
	for _, l := range legacyKinds {
		if l == k {
			return true
		}
	}
	return false
}

// Suppressible reports whether a person may switch this kind off.
//
// The five that are not suppressible are the ones where "I turned that off" is
// not an answer anybody would accept afterwards: somebody signed in as you, your
// account stopped being able to act, money you were told you had was taken back,
// a payout did not arrive, or the platform is addressing you directly. Everything
// else -- a fill, a settled payout, a market pause, a verification step forward --
// is news, and news can be declined.
func (k Kind) Suppressible() bool {
	switch k {
	case KindSecurityNewSession, KindAccountRestricted, KindCreditPurchaseReversed,
		KindPayoutFailed, KindSystem:
		return false
	default:
		return true
	}
}

// Severity of a notification. Mirrors the CHECK on notifications.severity.
type Severity string

// Severities.
const (
	SeverityInfo     Severity = "INFO"
	SeverityWarn     Severity = "WARN"
	SeverityCritical Severity = "CRITICAL"
)

// AllSeverities returns every value notifications.severity admits.
func AllSeverities() []Severity {
	return []Severity{SeverityInfo, SeverityWarn, SeverityCritical}
}

// Valid reports whether s is a value the column admits.
func (s Severity) Valid() bool {
	return s == SeverityInfo || s == SeverityWarn || s == SeverityCritical
}

// defaultSeverity is the severity a kind carries unless the caller states one.
// It is a property of the kind rather than of the moment: two fills are not
// more or less serious than each other.
func (k Kind) defaultSeverity() Severity {
	switch k {
	case KindCreditPurchaseReversed, KindPayoutFailed, KindAccountRestricted, KindSecurityNewSession:
		return SeverityCritical
	case KindPayoutNeedsReview, KindNativeMarketPaused, KindAgentPaused:
		return SeverityWarn
	default:
		return SeverityInfo
	}
}
