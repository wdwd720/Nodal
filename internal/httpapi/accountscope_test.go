package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/intent"
)

// Every account-scoped WRITE must use ownership-only scoping.
//
// There are two scoping helpers and the difference is the whole control.
// `accountScope` honours the operator override `account:read_any`, which is
// right for a read and was wrong on fourteen write routes: RoleAdmin holds
// that permission AND the customer surface (`native_market:trade`,
// `commerce:buy`, `payout:create`, `withdrawal:create`), so one ADMIN session
// could trade, buy and reserve a payout out of any customer's balance with no
// second signature and no admin action (F-36).
//
// `accountScopeWrite` has no override. This test is the reason the next write
// route cannot quietly get it wrong: a new POST handler that calls the read
// helper fails here, in the fast tier, naming itself.

var (
	handlerDecl  = regexp.MustCompile(`^func \(s \*Server\) (\w+)\(`)
	mutatingVerb = regexp.MustCompile(`^(Post|Put|Patch|Delete)`)
)

// readGradeHelpers are every helper in this package that resolves a tenant
// check through security.RequireAccount, which honours the operator override.
//
// It is a list because the check below is a source scan, and it is a list of
// THREE because the original scan named only the first one. Two write routes
// scoped through the other two and passed this test for months: cancelling any
// customer's intent, and publishing, pausing or withdrawing any seller's
// product, both reachable by one ADMIN session (F-102).
//
// TestAccountScope_TheReadGradeListIsComplete is what stops that recurring: a
// fourth helper cannot appear without joining this list.
// The fourth entry was found by the completeness check below on its first run,
// not by reading: requireOrderScope is read-only today and correct, and nothing
// had ever confirmed that.
var readGradeHelpers = []string{
	"accountScope(ctx",
	"securityRequireAccount(ctx",
	"requireIntentScope(ctx",
	"requireOrderScope(ctx",
}

func TestAccountScope_EveryWriteUsesOwnershipOnly(t *testing.T) {
	t.Parallel()
	var offenders []string
	forEachHandlerSource(t, func(file, fn, line string, lineNo int) {
		if !mutatingVerb.MatchString(fn) {
			return
		}
		for _, helper := range readGradeHelpers {
			if strings.Contains(line, helper) {
				offenders = append(offenders, fn+" via "+strings.TrimSuffix(helper, "(ctx")+
					" ("+file+":"+itoa(lineNo)+")")
			}
		}
	})
	sort.Strings(offenders)
	require.Empty(t, offenders,
		"%d write handler(s) scope through a read-grade helper, which lets any operator holding "+
			"account:read_any act on another customer's account; use the ownership-only form:\n  %s",
		len(offenders), strings.Join(offenders, "\n  "))
}

// TestAccountScope_TheReadGradeListIsComplete is the finding behind the
// finding.
//
// The check above is a string match over handler source, so it sees exactly the
// helper names it was told about. Naming a new one is enough to escape it --
// which is not hypothetical, it is what happened. This derives the set instead:
// every function in the package that calls security.RequireAccount is
// read-grade by definition, and must be declared above.
func TestAccountScope_TheReadGradeListIsComplete(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	funcDecl := regexp.MustCompile(`^(?:func (\w+)\(|\tvar (\w+) = security\.RequireAccount\b|var (\w+) = security\.RequireAccount\b)`)
	var found []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(f) // #nosec G304 -- a source file of this package, walked on purpose
		require.NoError(t, rerr)
		current := ""
		for _, line := range strings.Split(string(body), "\n") {
			if m := funcDecl.FindStringSubmatch(line); m != nil {
				for _, g := range m[1:] {
					if g != "" {
						current = g
					}
				}
			}
			// security.RequireAccount, not ...Owner: the suffix matters and
			// `RequireAccountOwner` must not match.
			if strings.Contains(line, "security.RequireAccount(") && current != "" {
				found = append(found, current)
				current = ""
			}
		}
	}
	require.NotEmpty(t, found,
		"no helper calls security.RequireAccount; either the override was removed "+
			"or this scan stopped working, and both need a person")

	declared := make(map[string]bool, len(readGradeHelpers))
	for _, h := range readGradeHelpers {
		declared[strings.TrimSuffix(h, "(ctx")] = true
	}
	var undeclared []string
	for _, fn := range found {
		if !declared[fn] {
			undeclared = append(undeclared, fn)
		}
	}
	sort.Strings(undeclared)
	require.Empty(t, undeclared,
		"these helpers resolve a tenant check through the operator read override and are not in "+
			"readGradeHelpers, so no write route using them is checked:\n  %s",
		strings.Join(undeclared, "\n  "))
}

// TestAccountScope_TheWriteHelperIsActuallyUsed is the negative control.
//
// The test above passes trivially if nothing calls either helper — if the
// handlers were renamed, or the regexes stopped matching. This requires the
// write helper to be in real use, so a green run means the check looked at
// something.
func TestAccountScope_TheWriteHelperIsActuallyUsed(t *testing.T) {
	t.Parallel()
	writes, reads := 0, 0
	forEachHandlerSource(t, func(_, fn, line string, _ int) {
		switch {
		case strings.Contains(line, "accountScopeWrite(ctx"):
			writes++
			require.True(t, mutatingVerb.MatchString(fn),
				"%s is not a mutating handler and should scope as a read", fn)
		case strings.Contains(line, "accountScope(ctx"):
			reads++
		}
	})
	require.GreaterOrEqual(t, writes, 10, "the write helper is barely used; the check above proves little")
	require.Positive(t, reads, "no handler scopes as a read; the two helpers have collapsed into one")
}

// forEachHandlerSource walks the handler files, tracking which function each
// line belongs to.
func forEachHandlerSource(t *testing.T, fn func(file, handler, line string, lineNo int)) {
	t.Helper()
	files, err := filepath.Glob("handlers_*.go")
	require.NoError(t, err)
	require.NotEmpty(t, files, "no handler files found; this test is looking in the wrong place")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, rerr := os.ReadFile(f) // #nosec G304 -- a source file of this package, walked on purpose
		require.NoError(t, rerr)
		current := ""
		for i, line := range strings.Split(string(body), "\n") {
			if m := handlerDecl.FindStringSubmatch(line); m != nil {
				current = m[1]
			}
			if current != "" {
				fn(f, current, line, i+1)
			}
		}
	}
}

// The behaviour the source scan above stands in for.
//
// TestAccountScope_EveryWriteUsesOwnershipOnly reads handler text; this drives
// the route. Both matter: the scan catches a new route on the day it is
// written, and this proves the helper it names actually refuses.
func TestAccountScope_AnOperatorCannotCancelAnotherAccountsIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const someoneElse = "0193b2e0-0000-7000-8000-0000000000ff"
	h.ports.intents.value = intent.TradeIntent{ID: testIntentID, AccountID: someoneElse}

	// An ADMIN holds account:read_any AND the customer surface, so before the
	// fix this returned 202 and the customer's intent was cancelled by one
	// operator with no reason, no second signature and no admin action (F-102).
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodPost, "/v1/intents/"+testIntentID.String()+"/cancel", map[string]any{},
		"Idempotency-Key", "operator-cancel-0001")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Zero(t, h.ports.intents.cancelCount(), "the cancellation reached the port anyway")

	// The control: the owner still cancels their own.
	owner := customerPrincipal()
	owner.AccountIDs = []string{someoneElse}
	h.as(&owner)
	ok := h.do(http.MethodPost, "/v1/intents/"+testIntentID.String()+"/cancel", map[string]any{},
		"Idempotency-Key", "owner-cancel-0001")
	require.Equal(t, http.StatusAccepted, ok.Code, "body=%s", ok.Body.String())
	assert.Equal(t, 1, h.ports.intents.cancelCount())

	// And an operator may still READ it: the override is a read permission and
	// removing it from reads was never the point.
	h.as(&op)
	got := h.do(http.MethodGet, "/v1/intents/"+testIntentID.String(), nil)
	assert.Equal(t, http.StatusOK, got.Code, "body=%s", got.Body.String())
}
