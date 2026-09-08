package valuedomain

import (
	"fmt"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// Custom SQLSTATEs raised by the value-domain triggers in migration 00710.
//
// Thirteen raise sites across five codes, and until F-58 not one of them
// appeared in any Go file -- production or test. This package had no `SQLState`
// call, no `pgconn` reference, and no error mapping at all, so a posting refused
// because it moved value between domains with no declared conversion reached its
// caller as an unclassified INTERNAL: a 500 to the customer, a page to whoever
// is on call, and nothing in the message saying which rule fired.
//
// These are not obscure failures. VD003 is what refuses an undeclared movement
// between simulated and real capital, which is the boundary the whole domain
// model exists to hold. It deserves better than "internal error".
const (
	// SQLStateUnknownDomain: an asset with no value domain cannot hold a
	// ledger account.
	SQLStateUnknownDomain = "VD001"
	// SQLStateForbiddenPair: two domains that may never move together.
	SQLStateForbiddenPair = "VD002"
	// SQLStateUndeclaredConversion: a cross-domain movement that no declared
	// conversion covers, or one whose declaration does not match what it
	// touched.
	SQLStateUndeclaredConversion = "VD003"
	// SQLStateTooManyDomains: a transaction touching more than the two domains
	// a declared conversion can describe.
	SQLStateTooManyDomains = "VD004"
	// SQLStateBadDomain: a transaction touching no value domain at all.
	SQLStateBadDomain = "VD005"
)

// MapError turns a value-domain SQLSTATE into an application error. It returns
// nil for an error that is not one, so a caller can fall through to its own
// mapping -- the shape ledger.MapError uses and the credit service composes
// with.
//
// The codes chosen say what a caller can do about it. A forbidden pair or a
// missing domain is a validation failure: the request was malformed against the
// domain model and will never succeed. An undeclared conversion is the same,
// and names the missing declaration, because the fix is a policy row rather
// than a different request.
func MapError(err error) error {
	if err == nil {
		return nil
	}
	switch db.SQLState(err) {
	case SQLStateUnknownDomain:
		return errs.Wrap(err, errs.CodeValidationFailed,
			"an asset with no value domain cannot hold a ledger account")
	case SQLStateForbiddenPair:
		return errs.Wrap(err, errs.CodeValidationFailed,
			"these value domains may never move together")
	case SQLStateUndeclaredConversion:
		return errs.Wrap(err, errs.CodeValidationFailed,
			"a movement between value domains needs a declared conversion that matches it")
	case SQLStateTooManyDomains:
		return errs.Wrap(err, errs.CodeValidationFailed,
			"a transaction may touch at most the two value domains a declared conversion describes")
	case SQLStateBadDomain:
		return errs.Wrap(err, errs.CodeValidationFailed,
			"a transaction must touch at least one value domain")
	}
	return nil
}

// IsValueDomainViolation reports whether an error came from one of these
// triggers, for callers that need to know only that the domain model refused
// rather than which rule did.
func IsValueDomainViolation(err error) bool { return MapError(err) != nil }

// Describe names the rule an error violated, for a log line or an operator
// message. It returns "" for an error that is not a value-domain refusal.
func Describe(err error) string {
	code := db.SQLState(err)
	switch code {
	case SQLStateUnknownDomain, SQLStateForbiddenPair, SQLStateUndeclaredConversion,
		SQLStateTooManyDomains, SQLStateBadDomain:
		return fmt.Sprintf("value domain rule %s", code)
	}
	return ""
}
