package docsref

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A runbook step that names an unreachable function must say it is unreachable.
//
// F-55 corrected 140 stale `PENDING` markers and introduced three claims of its
// own that were wrong in the other direction. The sharpest:
//
//	1. Revoke every session of the suspect principal(s):
//	   `auth.Manager.RevokeAllForSubject(subject)` ..., served by `cmd/api`.
//
// `RevokeAllForSubject` has no caller outside its own package, and
// `SessionsPort` exposes only per-session revoke, so no such route exists. The
// original marker said "PENDING: `cmd/api` route"; the correction pass removed
// it on the strength of `cmd/api` existing -- which is precisely the reasoning
// F-55 was about, committed while fixing it.
//
// Neither existing check could catch it. `TestDocs_EveryRunbookRouteIsServed`
// looks for HTTP paths and the sentence names none;
// `TestDocs_NothingMarkedPendingAlreadyExists` looks at markers and there was no
// longer a marker.
//
// # Why this is one named claim rather than a scan
//
// The general form -- "every Go call a runbook names must be reachable" -- was
// written first and abandoned. Across every runbook there is exactly ONE
// function written in call form, so the scan would guard a corpus of one while
// carrying a name-based call index that cannot tell a package's own internals
// from an external caller. It got that wrong on the first run, reporting
// `RevokeAllForSubject` as reachable because `auth.Manager` calls its own store
// method of the same name.
//
// So this is a curated claim, in the shape `countClaims` uses next door, and it
// is honest about being one: it watches the specific thing that went wrong and
// fires when the situation changes.

// reachedFromOutside reports whether anything outside a symbol's own package
// calls it. That is the definition the first attempt got wrong: a wrapper
// calling its own store method of the same name is not a caller.
func reachedFromOutside(t *testing.T, root, symbol string) (bool, []string) {
	t.Helper()
	def := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(symbol) + `\(`)
	use := regexp.MustCompile(`\.` + regexp.QuoteMeta(symbol) + `\(`)

	defining := map[string]bool{}
	var callers []string
	scan := func(collect bool) {
		for _, tree := range []string{"internal", "cmd", "scripts"} {
			err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") ||
					strings.HasSuffix(d.Name(), "_test.go") {
					return err
				}
				body, rerr := os.ReadFile(path) // #nosec G304 -- walking the repository
				if rerr != nil {
					return rerr
				}
				dir := filepath.Dir(path)
				if !collect {
					if def.Match(body) {
						defining[dir] = true
					}
					return nil
				}
				if defining[dir] || !use.Match(body) {
					return nil
				}
				rel, _ := filepath.Rel(root, path)
				callers = append(callers, filepath.ToSlash(rel))
				return nil
			})
			require(t, err == nil, "walking %s: %v", tree, err)
		}
	}
	scan(false) // find the defining package(s)
	scan(true)  // find callers outside them
	require(t, len(defining) > 0, "no definition of %s found; this control is guarding a symbol that no longer exists", symbol)
	return len(callers) > 0, callers
}

func TestDocs_SessionRevocationIsStillUnreachable(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)

	if reachable, callers := reachedFromOutside(t, root, "RevokeAllForSubject"); reachable {
		t.Errorf("RevokeAllForSubject now has a caller outside its package (%s).\n"+
			"    That is good news: an operator can revoke a compromised principal's sessions.\n"+
			"    Delete this control and take the PENDING marker out of docs/runbooks/admin-compromise.md.",
			strings.Join(callers, ", "))
	}

	// So the runbook must still say so, in the step that tells an operator to
	// do it. Without this half the check would pass on a runbook that had
	// quietly gone back to claiming a route.
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash("docs/runbooks/admin-compromise.md")))
	require(t, err == nil, "reading admin-compromise.md: %v", err)
	step := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.Contains(line, "RevokeAllForSubject") {
			step = line
			break
		}
	}
	require(t, step != "", "admin-compromise.md no longer names RevokeAllForSubject; this control is guarding a sentence that moved")
	require(t, strings.Contains(step, "PENDING"),
		"admin-compromise.md tells an operator to revoke every session of a suspect principal without saying "+
			"there is no route for it:\n    %s", strings.TrimSpace(step))
}

// TestDocs_TheReachabilityScanCanSeeACaller is the positive control. A scan
// that found no caller for anything would report every symbol unreachable and
// agree with any runbook at all.
func TestDocs_TheReachabilityScanCanSeeACaller(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, symbol := range []string{"Activate", "Authenticate"} {
		reachable, callers := reachedFromOutside(t, root, symbol)
		require(t, reachable, "the scan found no external caller of %s, which certainly has one; the walk or the regex is broken", symbol)
		require(t, len(callers) > 0, "%s reported reachable with no caller named", symbol)
	}
}
