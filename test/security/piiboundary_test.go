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

// Nothing writes personal data until it is decided who may read it (F-47).
//
// `identity_pii` holds `email_encrypted`, `legal_name_encrypted`, `dob_encrypted`
// and `key_version`. Migration 00010 deliberately withholds it from `cp_readonly`
// and `cp_ops`; the role bootstrap's blanket
// `ALTER DEFAULT PRIVILEGES ... GRANT SELECT ON TABLES` grants it to them
// anyway, and silently wins. Two deliberate statements in this repository
// disagree, and `test/integration/migrations/privileges_test.go` asserts the
// second one, so the first has never had any effect.
//
// That contradiction is recorded as F-47 and is not resolved here: who may read
// encrypted personal data is a deployment policy question, it has an
// operational constraint attached (`cp_ops` performs session retention
// cleanup), and it probably has different answers for the two roles.
//
// What IS decidable is when it stops being safe to leave open. Today the table
// has no reader and no writer in any Go file: the encryption is, in
// SECURITY.md's own word, DESIGNED, and R-121-1 records that "the column
// encryption is schema-shaped but has no Go implementation". An unresolved grant
// on an empty table costs nothing. The same grant on a populated one is a
// standing exposure of every customer's name and date of birth to two roles that
// were meant not to have it.
//
// So this test holds the two facts together: while nothing writes the table, the
// question may stay open; the moment something does, this fails and says what
// has to be decided first. It is the fuse on a recorded finding rather than a
// substitute for deciding it.

// piiTables are the tables whose readability by cp_readonly and cp_ops is the
// open question in F-47. `sessions` is deliberately absent: cp_ops genuinely
// needs it, that need is written down in privileges_test.go's
// `opsHousekeeping`, and it is written to constantly. Its half of F-47 is a
// question about column-level grants, not about whether to write the table.
var piiTables = []string{"identity_pii"}

// writeRe finds a statement that puts data into a table.
var writeRe = regexp.MustCompile(`(?i)\b(INSERT\s+INTO|UPDATE|COPY)\s+([a-z_]+)`)

func TestPII_NothingWritesPersonalDataWhileTheGrantIsUnresolved(t *testing.T) {
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
		assert.Emptyf(t, writers[tbl],
			"%s now has a writer (%s), so it will hold real personal data.\n"+
				"F-47 must be resolved before this lands: cp_readonly and cp_ops can SELECT this\n"+
				"table through the role bootstrap's blanket default, while migration 00010's grant\n"+
				"list deliberately withholds it. Decide which statement is right, make the schema\n"+
				"say it, and update privileges_test.go's contract to match.",
			tbl, strings.Join(writers[tbl], ", "))
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
