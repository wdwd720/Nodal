package docsref

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The runbooks describe the system an operator will actually find.
//
// F-55: 140 `PENDING` markers had accumulated across 21 runbooks, and every one
// naming a Go package or a binary named something that exists. The index said
// `cmd/api` had "no serving binary, no handler wiring" and told an operator that
// the only way to activate a kill switch was to write a Go program against the
// database; `POST /admin/kill-switches` had been served, permission-gated and
// step-up-protected for a long time. Twelve of the fifteen HTTP routes the
// runbooks name were live, each under a marker saying it was not.
//
// Each marker was true when written. That is the whole problem: a runbook is
// read once a year, under time pressure, by somebody who was not there when it
// was written, and it decays silently in between. F-51 fixed three documents by
// hand and the rest went on rotting.
//
// So the two claims a runbook makes about the system are checked here.
//
// # The marker vocabulary
//
//	PENDING           missing from this repository; somebody here can close it
//	BLOCKED_EXTERNAL  needs a cloud account, a credential, a funded wallet
//
// The distinction is the point. Filing "we have no AWS account" under the same
// word as "this package does not exist" makes the second invisible, and a
// reader who learns that most PENDINGs are unactionable stops reading them.

var (
	// A marker is the bare word. `PENDING` in backticks is a mention -- the
	// index explains the convention and has to be able to name it, and
	// RISK_RECONCILIATION_PENDING is a risk reason code, not a marker. Same
	// rule as the citation check next door: a document may name the thing it
	// is talking about, so long as it is quoting rather than asserting.
	markerRe = regexp.MustCompile("(^|[^`_A-Z])PENDING([^`_A-Z]|$)")

	// Tokens inside a marker's clause that name something checkable.
	pathRe = regexp.MustCompile("`(cmd/[a-z0-9-]+|internal/[a-z0-9/]+|make [a-z0-9-]+)`")

	// A path this document tells an operator to call.
	routeRe = regexp.MustCompile(`\b(GET|POST|PUT|PATCH|DELETE) (/[A-Za-z0-9_{}/:.-]*)`)
)

func runbookFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, dir := range []string{"docs/runbooks", "docs/operations"} {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		require(t, err == nil, "reading %s: %v", dir, err)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".md") {
				out = append(out, dir+"/"+e.Name())
			}
		}
	}
	require(t, len(out) > 15, "found only %d runbooks; the enumeration is broken, not the tree", len(out))
	sort.Strings(out)
	return out
}

// TestDocs_NothingMarkedPendingAlreadyExists: a PENDING marker may not name a
// package, command or make target that is on disk.
//
// This forces the marker to say what is actually missing. "PENDING: `cmd/api`
// route" is the shape that rotted -- `cmd/api` exists, the route was the
// pending part, and once the route landed nothing distinguished the sentence
// from a true one. A marker that names only what is absent cannot survive the
// thing arriving.
func TestDocs_NothingMarkedPendingAlreadyExists(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, doc := range runbookFiles(t) {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(doc)))
		require(t, err == nil, "reading %s: %v", doc, err)
		for i, line := range strings.Split(string(body), "\n") {
			for _, m := range markerRe.FindAllStringIndex(line, -1) {
				for _, tok := range pathRe.FindAllStringSubmatch(clauseAt(line, m[1]), -1) {
					if what, live := existsInRepo(root, tok[1]); live {
						t.Errorf("%s:%d marks %q as PENDING, but %s\n    %s",
							doc, i+1, tok[1], what, strings.TrimSpace(line))
					}
				}
			}
		}
	}
}

// clauseAt returns the text from a marker to the end of its clause. A marker
// governs what follows it up to the next separator, not the whole line: runbook
// lines routinely carry a BLOCKED_EXTERNAL clause and a PENDING clause and a
// paragraph of true statements, and a check that read the whole line would
// blame the marker for all of them.
//
// A sentence boundary ends a clause too. Without that, a line reading
// "PENDING: emitters for X. The adapter (`internal/provider/privy`) exists"
// is read as marking the adapter pending, which is the opposite of what it
// says -- the first version of this check made exactly that complaint.
func clauseAt(line string, from int) string {
	rest := line[from:]
	if i := strings.IndexAny(rest, ";|"); i >= 0 {
		rest = rest[:i]
	}
	// A sentence may end into markup rather than into a space: "not wired.**"
	// is the end of a bold clause, and reading past it attributes the next
	// sentence's package names to this marker. The first version missed that
	// and blamed a marker for a package the sentence after it merely mentioned.
	if i := sentenceEnd(rest); i >= 0 {
		rest = rest[:i]
	}
	// A marker inside a parenthetical ends with that parenthetical.
	if i := strings.IndexByte(rest, ')'); i >= 0 && !strings.Contains(rest[:i], "(") {
		rest = rest[:i]
	}
	return rest
}

// sentenceEnd finds where a marker's sentence stops, or -1. A sentence ends at
// a full stop followed by a space or by markup -- runbooks bold their markers,
// so "not wired.**" is an ending and a rule that only knew about ". " read
// straight through it into the next sentence.
func sentenceEnd(s string) int {
	for i := 0; i < len(s)-1; i++ {
		if s[i] != '.' {
			continue
		}
		switch s[i+1] {
		case ' ', '*', '\t':
			return i
		}
	}
	return -1
}

// existsInRepo reports whether a token names something on disk, and what.
func existsInRepo(root, token string) (string, bool) {
	if target, ok := strings.CutPrefix(token, "make "); ok {
		body, err := os.ReadFile(filepath.Join(root, "Makefile"))
		if err != nil {
			return "", false
		}
		if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:`).Match(body) {
			return "the Makefile declares that target", true
		}
		return "", false
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(token)))
	if err != nil || !info.IsDir() {
		return "", false
	}
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(token)))
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			return "the directory exists and holds Go source", true
		}
	}
	return "", false
}

// TestDocs_EveryRunbookRouteIsServed: a runbook may not tell an operator to
// call an endpoint the API does not declare.
//
// The reverse of the marker check, and the one with teeth. A stale PENDING
// wastes a responder's time; a route that does not exist wastes it and then
// leaves them with no containment action at all.
func TestDocs_EveryRunbookRouteIsServed(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	declared := declaredOperations(t, root)

	seen := 0
	for _, doc := range runbookFiles(t) {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(doc)))
		require(t, err == nil, "reading %s: %v", doc, err)
		for i, line := range strings.Split(string(body), "\n") {
			for _, m := range routeRe.FindAllStringSubmatch(line, -1) {
				verb, path := m[1], normalizePath(strings.TrimRight(m[2], ".,;`"))
				if !strings.HasPrefix(path, "/v1/admin") && !strings.HasPrefix(path, "/v1/") {
					continue
				}
				seen++
				if !served(declared, verb, path) {
					t.Errorf("%s:%d tells an operator to call %s %s, which openapi.yaml does not declare\n    %s",
						doc, i+1, verb, m[2], strings.TrimSpace(line))
				}
			}
		}
	}
	// The positive signal. A regex that stopped matching, or a path
	// normalisation that stopped agreeing with the spec, would leave this test
	// passing over nothing at all (F-32, F-46).
	require(t, seen > 20, "only %d routes found across the runbooks; the scan is broken, not the documents", seen)
}

// served reports whether the spec declares an operation the runbook's path
// would reach. A runbook writes the concrete call an operator makes --
// `/admin/gates/{capability}/suspend` -- where the spec parameterises the last
// segment as `{action}`, so a literal segment is allowed to fill a `{}` slot.
//
// The limit that leaves: this says the endpoint exists, not that `suspend` is
// one of the values it accepts. Enum membership is the API's job and is tested
// against the generated server; duplicating it from a line scanner over YAML
// would be a second, weaker copy of a rule that already has an owner.
func served(declared map[string]bool, verb, path string) bool {
	if declared[verb+" "+path] {
		return true
	}
	want := strings.Split(path, "/")
	for op := range declared {
		opVerb, opPath, _ := strings.Cut(op, " ")
		if opVerb != verb {
			continue
		}
		got := strings.Split(opPath, "/")
		if len(got) != len(want) {
			continue
		}
		match := true
		for i := range got {
			if got[i] != want[i] && got[i] != "{}" {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// declaredOperations reads the paths and methods out of the spec. This is a
// line scanner rather than a YAML parse because the shape it needs is two
// levels deep and fixed; the count assertion below is what stops that being a
// silent assumption.
func declaredOperations(t *testing.T, root string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "openapi", "openapi.yaml"))
	require(t, err == nil, "reading openapi.yaml: %v", err)

	pathLine := regexp.MustCompile(`^  (/[^:\s]*):\s*$`)
	verbLine := regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)

	out := map[string]bool{}
	current := ""
	for _, line := range strings.Split(string(body), "\n") {
		if m := pathLine.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if m := verbLine.FindStringSubmatch(line); m != nil && current != "" {
			out[strings.ToUpper(m[1])+" "+normalizePath(current)] = true
		}
	}
	require(t, len(out) > 40, "openapi.yaml parsed to %d operations; the scanner is broken", len(out))
	return out
}

// normalizePath makes a runbook's path comparable with the spec's: parameter
// names differ between them ({id} against {accountId}), and runbooks sometimes
// write the version prefix and sometimes assume it.
func normalizePath(p string) string {
	p = regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(p, "{}")
	if !strings.HasPrefix(p, "/v1") {
		p = "/v1" + p
	}
	return strings.TrimSuffix(p, "/")
}
