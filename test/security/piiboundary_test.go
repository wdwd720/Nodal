package security

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Personal data is written by the encrypting store and by nothing else (F-47).
//
// `identity_pii` holds `email_encrypted`, `legal_name_encrypted`, `dob_encrypted`
// and `key_version`. For a year nothing in the repository wrote those columns
// -- the encryption was DESIGNED -- so this test was a fuse: it failed the
// moment anything did, because writing personal data before deciding who may
// read it would have been a standing exposure.
//
// internal/pii is the encryption now, and migration 00754 is the decision. What
// this test holds from here on is the invariant that makes the decision sound:
// every statement that writes `identity_pii` lives in `internal/pii/store.go`,
// where the value has already been sealed. A writer anywhere else is a path by
// which plaintext could reach the column, and this fails naming the file.

// piiTables are the tables only the encrypting store may write. `sessions` is
// deliberately absent: it is written by the session store, holds no encrypted
// column, and its half of F-47 was about who may READ it, which 00754 settled
// and test/integration/migrations asserts.
var piiTables = []string{"identity_pii"}

// piiWriters are the only files allowed to write those tables.
var piiWriters = map[string]bool{"internal/pii/store.go": true}

// writeRe finds a statement that puts data into a table.
var writeRe = regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE|COPY)\s+([a-z_]+)`)

func TestPII_OnlyTheEncryptingStoreWritesPersonalData(t *testing.T) {
	root := repoRoot(t)
	writers := map[string][]string{}

	for _, tree := range []string{"internal", "cmd", "scripts"} {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
				return err
			}
			if strings.HasSuffix(d.Name(), "_test.go") {
				return nil
			}
			body, rerr := os.ReadFile(path) // #nosec G304 -- walking the repository
			if rerr != nil {
				return rerr
			}
			for _, m := range writeRe.FindAllStringSubmatch(string(body), -1) {
				for _, tbl := range piiTables {
					if strings.EqualFold(m[2], tbl) {
						rel, _ := filepath.Rel(root, path)
						writers[tbl] = append(writers[tbl], filepath.ToSlash(rel))
					}
				}
			}
			return nil
		})
		require.NoError(t, err)
	}

	for _, tbl := range piiTables {
		sort.Strings(writers[tbl])
		var outside []string
		for _, w := range writers[tbl] {
			if !piiWriters[w] {
				outside = append(outside, w)
			}
		}
		assert.Emptyf(t, outside,
			"%s is written outside the encrypting store (%s).\n"+
				"Every write to this table must go through internal/pii, where the value is sealed\n"+
				"under the keyring before it reaches the column. A writer anywhere else is a path\n"+
				"for plaintext personal data into the database (F-47).",
			tbl, strings.Join(outside, ", "))
		// And the store really is a writer, or the allowlist above is guarding
		// a table nothing populates and the assertion means nothing.
		assert.Containsf(t, writers[tbl], "internal/pii/store.go",
			"%s has no writer in internal/pii/store.go; the encrypting store stopped writing it", tbl)
	}
}

// TestPII_TheBoundaryScanCanActuallySeeAWriter is the positive control. A
// regex that stopped matching, or a walk that stopped finding Go files, would
// leave the test above passing over nothing at all -- which is the shape of
// F-32 and F-46, an assertion about an absence with no positive signal behind
// it.
func TestPII_TheBoundaryScanCanActuallySeeAWriter(t *testing.T) {
	root := repoRoot(t)

	// The scan finds writers of a table that certainly has them.
	found := 0
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return err
		}
		body, rerr := os.ReadFile(path) // #nosec G304 -- walking the repository
		if rerr != nil {
			return rerr
		}
		for _, m := range writeRe.FindAllStringSubmatch(string(body), -1) {
			if strings.EqualFold(m[2], "journal_transactions") {
				found++
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Positivef(t, found,
		"the scan found no writer of journal_transactions, which certainly has one; "+
			"the regex or the walk is broken, and the absence asserted above means nothing")

	// And the table under test is named in the schema, so a rename cannot make
	// the check quietly vacuous either.
	schema, err := os.ReadFile(filepath.Join(root, "migrations", "00010_identity_accounts.sql"))
	require.NoError(t, err)
	for _, tbl := range piiTables {
		assert.Containsf(t, string(schema), "CREATE TABLE "+tbl,
			"%s is not created by 00010 any more; this check is guarding a table that no longer exists", tbl)
	}
}
