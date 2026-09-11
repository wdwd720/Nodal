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
	}
	// Unreachable: TestEveryKindHasASummaryTemplate proves the switch is total.
	// Returning the kind rather than an empty string keeps an unknown item
	// legible if one ever arrives from a newer writer.
	return string(k)
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
