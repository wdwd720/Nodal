package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestAccountScope_EveryWriteUsesOwnershipOnly(t *testing.T) {
	t.Parallel()
	var offenders []string
	forEachHandlerSource(t, func(file, fn, line string, lineNo int) {
		if !mutatingVerb.MatchString(fn) {
			return
		}
		if strings.Contains(line, "accountScope(ctx") {
			offenders = append(offenders, fn+" ("+file+":"+itoa(lineNo)+")")
		}
	})
	sort.Strings(offenders)
	require.Empty(t, offenders,
		"%d write handler(s) scope by accountScope, which lets any operator holding "+
			"account:read_any act on another customer's account; use accountScopeWrite:\n  %s",
		len(offenders), strings.Join(offenders, "\n  "))
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
