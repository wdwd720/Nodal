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

// mountedOutside reports the files outside internal/auth/httpmw that reference
// one of its exported guards. A caller in another package must write the
// qualified form, so that is what is looked for -- and it has to be, because
// RequireStepUp is also a function in internal/security with real callers, and
// a bare-name scan would report it reachable and the check would pass for the
// wrong reason.
func mountedOutside(t *testing.T, root, guard string) []string {
	t.Helper()
	use := regexp.MustCompile(`httpmw\.` + regexp.QuoteMeta(guard) + `\b`)
	var callers []string
	for _, tree := range []string{"internal", "cmd", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/auth/httpmw/") {
				return nil // the package's own tests are not the API mounting it
			}
			body, rerr := os.ReadFile(path) // #nosec G304 -- walking the repository
			if rerr != nil {
				return rerr
			}
			if use.Match(body) {
				callers = append(callers, rel)
			}
			return nil
		})
		require(t, err == nil, "walking %s: %v", tree, err)
	}
	return callers
}

// TestDocs_TheHTTPMiddlewareGuardsAreStillUnused.
//
// internal/auth/httpmw exports six route guards -- RequireAuth, RequireRole,
// RequireRoleAt, RequirePermission, RequirePermissionAt, RequireStepUp -- and
// the API mounts none of them. Authorization is decided in
// internal/httpapi/authz.go, per operation, from the generated operation id.
//
// The package doc advertised the six as the enforcement without saying that
// (F-76). Six exported guards that look live are an invitation to mount one
// beside the real enforcement layer, and two authorization paths over one route
// is the shape that produces a route guarded in one of them.
//
// So the doc now says which layer enforces, and this keeps that sentence true:
// the day somebody wires one of these, the doc has to change with it.
func TestDocs_TheHTTPMiddlewareGuardsAreStillUnused(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	guards := []string{"RequireAuth", "RequireRole", "RequireRoleAt", "RequirePermission", "RequirePermissionAt", "RequireStepUp"}
	for _, g := range guards {
		if callers := mountedOutside(t, root, g); len(callers) > 0 {
			t.Errorf("httpmw.%s is now referenced outside its package (%s).\n"+
				"    If the API has started mounting it, take the paragraph out of internal/auth/httpmw/doc.go\n"+
				"    that says it does not -- and check that the route is not also guarded by httpapi/authz.go,\n"+
				"    because two authorization paths over one route is how a route ends up guarded in only one.",
				g, strings.Join(callers, ", "))
		}
	}

	// The other half: the doc must still say so. Without it this check would
	// pass on a doc that had quietly gone back to advertising them.
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash("internal/auth/httpmw/doc.go")))
	require(t, err == nil, "reading httpmw/doc.go: %v", err)
	require(t, strings.Contains(string(body), "THE API DOES NOT MOUNT THEM"),
		"httpmw/doc.go no longer says the API does not mount its guards, and nothing else tells a reader "+
			"that internal/httpapi/authz.go is the enforcement layer")

	// And the positive control: the scan can see a reference it should see.
	// Without this the walk could be broken and every guard would look unused.
	require(t, len(mountedOutside(t, root, "Session")) > 0,
		"the scan found no reference to httpmw.Session, which cmd/api certainly makes; the walk or the regex is broken")
}
