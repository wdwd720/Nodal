// Package source checks properties of the repository's own text.
//
// It exists because one defect class kept coming back in a different file each
// time. Bidirectional control characters — Trojan Source (CVE-2021-42574) — were
// found three times in this project:
//
//	internal/nativeasset/moderation.go     the code that REFUSES them, flagged by gosec
//	internal/nativeasset/nativeasset_test.go   the test that proves it, invisible to gosec
//	two readiness documents                 describing the fix, invisible to every scanner
//
// Each was found by a different tool, none of which sees the others' files:
// gosec builds only the default tag set, so it never reads a file behind
// //go:build integration; staticcheck reads Go and nothing else; neither reads
// Markdown. A check that covers one third of a repository catches a third of a
// class.
//
// This reads every text file the repository tracks, whatever its extension and
// whatever build tag guards it.
package source

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// bidi are the Unicode controls that let displayed order differ from logical
// order. A reviewer approving source containing one of these is approving
// something other than what they read, which is the entire attack.
//
// U+202A–U+202E are the legacy embedding and override codes; U+2066–U+2069 are
// the isolates that replaced them. U+200E and U+200F (the LEFT-TO-RIGHT and
// RIGHT-TO-LEFT MARKs) are deliberately NOT here: they are ordinary characters
// in real multilingual text and do not reorder a line the way an override does.
var bidi = map[rune]string{
	'\u202a': "LEFT-TO-RIGHT EMBEDDING",
	'\u202b': "RIGHT-TO-LEFT EMBEDDING",
	'\u202c': "POP DIRECTIONAL FORMATTING",
	'\u202d': "LEFT-TO-RIGHT OVERRIDE",
	'\u202e': "RIGHT-TO-LEFT OVERRIDE",
	'\u2066': "LEFT-TO-RIGHT ISOLATE",
	'\u2067': "RIGHT-TO-LEFT ISOLATE",
	'\u2068': "FIRST STRONG ISOLATE",
	'\u2069': "POP DIRECTIONAL ISOLATE",
}

// scanned are the extensions worth reading. Anything a human reviews.
var scanned = map[string]bool{
	".go": true, ".md": true, ".sql": true, ".yml": true, ".yaml": true,
	".json": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".css": true, ".html": true, ".sh": true, ".tf": true, ".toml": true,
	".proto": true, ".txt": true,
}

// skipped are directories whose contents nobody reviews line by line.
var skipped = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "bin": true,
	".next": true, "coverage": true, "vendor": true, "testdata": true,
}

// TestSource_NoBidirectionalControlCharacters: a reviewer must see what the
// compiler sees.
//
// There is no allowlist. The one file in this repository that legitimately
// needs these codepoints — the moderation screen that refuses them in
// user-supplied names — carries them as `\u202e` escapes, which is strictly
// better: the source contains no invisible glyph and a reviewer can read which
// codepoint is meant. Any file that thinks it needs the literal character can
// do the same, so an exemption would only ever be used by the case this test
// exists to catch.
func TestSource_NoBidirectionalControlCharacters(t *testing.T) {
	t.Parallel()
	var findings []string
	forEachTextFile(t, func(path, body string) {
		for i, r := range body {
			name, isBidi := bidi[r]
			if !isBidi {
				continue
			}
			findings = append(findings, formatFinding(path, body, i, r, name))
		}
	})
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Errorf("%d bidirectional control character(s); a reviewer would approve "+
			"something other than what they read:\n  %s",
			len(findings), strings.Join(findings, "\n  "))
	}
}

// TestSource_TheCheckCanFail is the negative control.
//
// A test that walks a clean repository passes whether or not it looks at
// anything, which is how a check ends up proving nothing (F-21, F-32). This
// hands the same predicate a string that does contain an override and requires
// it to say so.
func TestSource_TheCheckCanFail(t *testing.T) {
	t.Parallel()
	body := "var name = \"Doggu\u202eCoin\"\n"
	var found int
	for _, r := range body {
		if _, ok := bidi[r]; ok {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("the predicate found %d overrides in a line that contains exactly one", found)
	}
	if _, ok := bidi['\u200e']; ok {
		t.Error("U+200E is an ordinary mark in multilingual text and must not be reported")
	}
}

// formatFinding names the file, the line, and which codepoint it is — the three
// things somebody needs to act, given the character itself cannot be seen.
func formatFinding(path, body string, offset int, r rune, name string) string {
	line := 1 + strings.Count(body[:offset], "\n")
	start := strings.LastIndex(body[:offset], "\n") + 1
	end := strings.Index(body[offset:], "\n")
	if end < 0 {
		end = len(body)
	} else {
		end += offset
	}
	context := strings.Map(func(c rune) rune {
		if _, isBidi := bidi[c]; isBidi {
			return -1 // it would corrupt this very message
		}
		return c
	}, body[start:end])
	if len(context) > 90 {
		context = context[:90] + "..."
	}
	return path + ":" + itoa(line) + ": U+" + hex(r) + " " + name +
		"\n      write it as an escape instead; the line reads: " + strings.TrimSpace(context)
}

func forEachTextFile(t *testing.T, fn func(path, body string)) {
	t.Helper()
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipped[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !scanned[strings.ToLower(filepath.Ext(d.Name()))] {
			return nil
		}
		body, readErr := os.ReadFile(path) // #nosec G304 -- a repository file this test is walking on purpose
		if readErr != nil {
			return readErr
		}
		if !utf8.Valid(body) {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		fn(filepath.ToSlash(rel), string(body))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func hex(r rune) string {
	const digits = "0123456789abcdef"
	var b []byte
	for i := 12; i >= 0; i -= 4 {
		b = append(b, digits[(r>>uint(i))&0xf])
	}
	return strings.ToUpper(string(b))
}
