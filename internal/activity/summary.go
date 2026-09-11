package activity

import "strings"

// The human sentence, per kind.
//
// # Why no amount appears in the sentence
//
// Every amount on an item is an exact integer in a unit the item names --
// Credit base units, minor units of a currency, base units of a native asset --
// and turning one into "12.50" needs that asset's decimals. This package would
// have to carry a decimals column for every source to do it, and a summary that
// got it wrong would be a wrong number in a sentence beside a right number in a
// field.
//
// So the sentence says WHAT happened and the fields say HOW MUCH, each with its
// unit and its temperature. §46 requires the units to be visible anyway: a
// client that renders "500" for both Credits and dollars is the exact confusion
// that section exists to forbid, and a client that must render the unit can
// render the number too.
//
// # Why the templates interpolate almost nothing
//
// A summary is displayed. The only values interpolated below are a native
// asset's SYMBOL and a domain STATUS. Status values come from columns with
// CHECK constraints, so they are a closed set. A symbol is creator-supplied, so
// nativeasset.Screen has already refused control characters, bidi overrides and
// confusables in it, and sanitizeSymbol below is the second line: the sentence
// is data in a JSON field, never markup, and it stays that way.

func summaryFor(k Kind, status, side, symbol string) string {
	symbol = sanitizeSymbol(symbol)
	switch k {
	case KindCreditPurchase:
		return "Bought Credits with a card payment" + statusSuffix(status)
	case KindCreditReversal:
		switch status {
		case "REFUNDED":
			return "A Credit purchase was refunded"
		case "DISPUTED":
			return "A Credit purchase is disputed"
		default:
			return "A Credit purchase was reversed"
		}
	case KindNativeTrade:
		if side == "SELL" {
			return "Sold " + orSomething(symbol) + " on the internal market"
		}
		return "Bought " + orSomething(symbol) + " on the internal market"
	case KindNativeAssetCreated:
		return "Created the native asset " + orSomething(symbol) + statusSuffix(status)
	case KindPayoutRequested:
		return "Requested a payout" + statusSuffix(status)
	case KindPayoutStateChanged:
		return "Payout " + strings.ToLower(strings.ReplaceAll(status, "_", " "))
	case KindAdminAdjustment:
		return "An operator adjusted your Credit balance"
	case KindVerificationUpdated:
		return verificationSummary(status)
	case KindPayoutDestinationAdded:
		return "Added a payout destination" + statusSuffix(status)
	case KindPayoutDestinationDisabled:
		if status == "REJECTED" {
			return "A payout destination was rejected by the provider"
		}
		return "A payout destination was disabled"
	case KindTermsAccepted:
		return "Accepted the " + documentName(status)
	case KindAccountClosureRequested:
		return "Requested to close this account"
	case KindAccountClosureDecided:
		return closureSummary(status)
	case KindAgentCreated:
		return "Created an agent"
	case KindAgentPaused:
		if status == "OWNER_REQUEST" {
			return "Paused an agent"
		}
		return "An agent was paused" + statusSuffix(status)
	case KindAgentResumed:
		return "An agent was resumed"
	case KindAgentDisabled:
		return "Disabled an agent; its authority is revoked"
	case KindNativeMarketPaused:
		return marketPauseSummary(status, orSomething(symbol))
	}
	// Unreachable: TestEveryKindHasASummaryTemplate proves the switch is total.
	// Returning the kind rather than an empty string keeps an unknown item
	// legible if one ever arrives from a newer writer.
	return string(k)
}

// verificationSummary renders one move of the financial verification state.
//
// Two of the ten states get a sentence rather than a state name, because a
// state name is what the product must NOT show for them: EXPIRED is not a
// rejection and must never read as one (D-057, D-061), and REQUIRED is a next
// step rather than a refusal.
func verificationSummary(status string) string {
	switch status {
	case "VERIFIED":
		return "Your identity verification completed"
	case "EXPIRED":
		return "Your identity verification expired and can be renewed"
	case "REQUIRED":
		return "Identity verification was requested"
	case "REJECTED":
		return "Your identity verification was not approved"
	case "NEEDS_INFORMATION":
		return "Your identity verification needs more information"
	case "RESTRICTED":
		return "Your identity verification carries a restriction"
	case "SUSPENDED":
		return "Your identity verification is suspended pending review"
	default:
		return "Your identity verification moved to" + strings.ToLower(" "+strings.ReplaceAll(status, "_", " "))
	}
}

// closureSummary renders what happened to a closure request. "Cancelled" says
// who cancelled it only where the schema knows: the transition's actor is not
// on the item, so the sentence stays about the request.
func closureSummary(status string) string {
	switch status {
	case "CANCELLED":
		return "The request to close this account was cancelled"
	case "REFUSED":
		return "The request to close this account was refused"
	case "EFFECTED":
		return "This account was closed"
	default:
		return "The request to close this account moved to" + strings.ToLower(" "+strings.ReplaceAll(status, "_", " "))
	}
}

// marketPauseSummary is the same four statuses internal/notifications names,
// said once here for the timeline. A holding is never described as lost: a
// paused market stops trading and changes nobody's balance.
func marketPauseSummary(status, symbol string) string {
	switch status {
	case "CLOSE_ONLY":
		return symbol + " stopped accepting buys; you can still sell what you hold"
	case "DELISTED":
		return symbol + " was delisted; your holding is unchanged"
	case "FROZEN":
		return symbol + " is frozen; your holding is unchanged"
	default: // HALTED
		return symbol + " is halted; your holding is unchanged"
	}
}

// documentName turns a terms document id into the words on the document.
//
// The map is here rather than read from internal/terms deliberately: this
// package would otherwise carry a dependency on the legal registry to render a
// noun, and the ids are a database CHECK's closed set. An id with no entry
// falls back to its own lowercased words, which stays legible if the registry
// gains a document before this switch does.
func documentName(documentID string) string {
	switch documentID {
	case "TERMS_OF_SERVICE":
		return "Terms of Service"
	case "PRIVACY_POLICY":
		return "Privacy Notice"
	case "RISK_DISCLOSURE":
		return "Risk Disclosure"
	case "CREDITS_TERMS":
		return "Credits Terms"
	case "WITHDRAWAL_DISCLOSURE":
		return "Withdrawal and Verification Disclosure"
	default:
		return strings.ToLower(strings.ReplaceAll(documentID, "_", " "))
	}
}

func statusSuffix(status string) string {
	if status == "" {
		return ""
	}
	return " (" + strings.ToLower(strings.ReplaceAll(status, "_", " ")) + ")"
}

func orSomething(symbol string) string {
	if symbol == "" {
		return "an asset"
	}
	return symbol
}

// sanitizeSymbol keeps a ticker to the characters a ticker can have.
//
// nativeasset.Screen already refuses everything else at creation, so this can
// only ever be a no-op -- which is the point: it is the assertion that a
// sentence this package builds cannot carry a character somebody chose.
func sanitizeSymbol(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
		if b.Len() >= 32 {
			break
		}
	}
	return b.String()
}
