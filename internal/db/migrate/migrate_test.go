package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/migrations"
)

func TestGuardDownTo(t *testing.T) {
	t.Parallel()
	const p = ProtectedVersion
	tests := []struct {
		name            string
		current, target int64
		wantProtected   bool
		wantErr         bool
	}{
		{"nothing applied, to zero", 0, 0, false, false},
		{"foundation only, to zero", 3, 0, false, false},
		{"foundation only, to protected-1", 3, p - 1, false, false},
		{"foundation only, to protected", 3, p, false, false},
		{"exactly protected, to protected-1 refused", p, p - 1, true, true},
		{"exactly protected, to zero refused", p, 0, true, true},
		{"above protected, to zero refused", p + 50, 0, true, true},
		{"above protected, to protected allowed", p + 50, p, false, false},
		{"above protected, to protected+10 allowed", p + 50, p + 10, false, false},
		{"above protected, to same version allowed", p + 50, p + 50, false, false},
		{"negative target invalid", 3, -1, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := guardDownTo(tt.current, tt.target, p)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantProtected, errors.Is(err, ErrProtectedVersion))
		})
	}
}

func TestProp_GuardDownTo_NeverUndoesProtectedHistory(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		current := rapid.Int64Range(0, 2000).Draw(rt, "current")
		target := rapid.Int64Range(0, 2000).Draw(rt, "target")
		protected := rapid.Int64Range(1, 1000).Draw(rt, "protected")
		err := guardDownTo(current, target, protected)
		undoesProtected := current >= protected && target < protected
		if undoesProtected != (err != nil) {
			rt.Fatalf("current=%d target=%d protected=%d: err=%v", current, target, protected, err)
		}
	})
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"00001_extensions.sql", 1, true},
		{"00100_ledger.sql", 100, true},
		{"migrations/00003_idempotency.sql", 3, true},
		{"12_a.sql", 12, true},
		{"embed.go", 0, false},
		{"README.md", 0, false},
		{"notes.sql", 0, false},
		{"_x.sql", 0, false},
		{"00000_zero.sql", 0, false},
		{"abc_x.sql", 0, false},
	}
	for _, tt := range tests {
		v, ok := parseVersion(tt.in)
		assert.Equal(t, tt.ok, ok, tt.in)
		assert.Equal(t, tt.want, v, tt.in)
	}
}

func TestLoadSources_EmbeddedMigrations(t *testing.T) {
	t.Parallel()
	srcs, err := loadSources(migrations.FS)
	require.NoError(t, err)
	require.NotEmpty(t, srcs)
	for i, s := range srcs {
		assert.Len(t, s.SHA256, 64)
		assert.True(t, strings.HasSuffix(s.Name, ".sql"))
		if i > 0 {
			assert.Greater(t, s.Version, srcs[i-1].Version, "sorted ascending, unique")
		}
	}
	assert.Equal(t, int64(1), srcs[0].Version)
	assert.GreaterOrEqual(t, srcs[len(srcs)-1].Version, ProtectedVersion, "ledger migrations (>= 00100) are expected to be embedded")
	for _, s := range srcs {
		assert.NotContains(t, s.Name, "TODO", "no placeholder migrations")
	}
}

func TestLoadSources_RejectsDuplicateVersions(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"00001_a.sql": {Data: []byte("-- +goose Up\nSELECT 1;\n")},
		"00001_b.sql": {Data: []byte("-- +goose Up\nSELECT 2;\n")},
	}
	_, err := loadSources(fsys)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate version 1")
}

func TestDigest_NormalisesLineEndings(t *testing.T) {
	t.Parallel()
	assert.Equal(t, digest([]byte("a\nb\n")), digest([]byte("a\r\nb\r\n")))
	assert.NotEqual(t, digest([]byte("a\nb\n")), digest([]byte("a\nb\n\n")))
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", digest(nil))
}

func TestEmbeddedMigrations_UseGooseAnnotations(t *testing.T) {
	t.Parallel()
	srcs, err := loadSources(migrations.FS)
	require.NoError(t, err)
	for _, s := range srcs {
		b, err := migrations.FS.ReadFile(s.Name)
		require.NoError(t, err)
		body := string(b)
		assert.Contains(t, body, "-- +goose Up", s.Name)
		assert.Contains(t, body, "-- +goose Down", s.Name)
		if s.Version >= ProtectedVersion {
			// Protected migrations must carry an explicit marker and a Down
			// section that can never destroy accounting history.
			downIdx := strings.Index(body, "-- +goose Down")
			require.GreaterOrEqual(t, downIdx, 0, "%s: missing Down section", s.Name)
			down := body[downIdx:]
			// The "protected" marker is itself a comment, so it is looked for
			// in the full text.
			assert.Contains(t, strings.ToLower(down), "protected", "%s: protected Down must say why it is a no-op", s.Name)
			// The destructive-keyword scan runs on SQL only, with -- comments
			// stripped. Scanning the raw text made it impossible for a
			// migration to explain the rule in the words the rule names: a Down
			// section saying "must not DROP, DELETE, TRUNCATE or ALTER" tripped
			// its own check. A keyword inside a comment cannot destroy
			// anything; one in a statement can, and still fails here.
			for _, kw := range []string{"DROP ", "DELETE ", "TRUNCATE ", "ALTER TABLE"} {
				assert.NotContains(t, strings.ToUpper(stripSQLComments(down)), kw,
					"%s: protected Down must not contain %q", s.Name, kw)
			}
		}
	}
}

// stripSQLComments removes -- line comments so a keyword scan sees statements
// rather than prose. Block comments are not used in this repo's migrations, and
// a naive /* */ strip would be wrong inside string literals.
func stripSQLComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestRanges(t *testing.T) {
	t.Parallel()
	// Contiguous, non-overlapping, starting at 1.
	assert.Equal(t, int64(1), Ranges[0].Lo)
	for i := 1; i < len(Ranges); i++ {
		assert.Equal(t, Ranges[i-1].Hi+1, Ranges[i].Lo, Ranges[i].Name)
		assert.Less(t, Ranges[i].Lo, Ranges[i].Hi)
	}
	fin, ok := RangeByName("FINANCIAL")
	require.True(t, ok)
	assert.Equal(t, ProtectedVersion, fin.Lo, "the protected version is the start of the financial range")
	assert.True(t, fin.Protected)
	_, ok = RangeByName("nope")
	assert.False(t, ok)
	assert.Equal(t, []string{"foundation", "financial", "instruments", "execution", "funding", "strategy", "reality", "audit"}, RangeNames())
}

func TestNextVersion(t *testing.T) {
	t.Parallel()
	found, _ := RangeByName("foundation")
	fin, _ := RangeByName("financial")
	names := []string{"00001_extensions.sql", "00002_outbox_inbox.sql", "00003_idempotency.sql", "embed.go", "00250_orders.sql"}

	// New migrations always take the global maximum + 1, whatever range is asked for.
	v, err := NextVersion(names, found)
	require.NoError(t, err)
	assert.Equal(t, int64(251), v)

	v, err = NextVersion(names, fin)
	require.NoError(t, err)
	assert.Equal(t, int64(251), v, "range does not change the allocation")

	v, err = NextVersion(nil, found)
	require.NoError(t, err)
	assert.Equal(t, int64(1), v)

	v, err = NextVersion(nil, fin)
	require.NoError(t, err)
	assert.Equal(t, int64(100), v, "an empty repository starts at the requested range's lower bound")

	_, err = NextVersion([]string{"99999_last.sql"}, found)
	require.ErrorIs(t, err, ErrRangeFull)
}

func TestValidateNameAndFilename(t *testing.T) {
	t.Parallel()
	require.NoError(t, ValidateName("ledger_accounts"))
	require.NoError(t, ValidateName("a1"))
	for _, bad := range []string{"", "Ledger", "1abc", "has-dash", "has space", "x.sql", strings.Repeat("a", 65)} {
		assert.Error(t, ValidateName(bad), bad)
	}
	assert.Equal(t, "00100_ledger.sql", Filename(100, "ledger"))
	assert.Equal(t, "00004_sessions.sql", Filename(4, "sessions"))
}

func TestSkeleton(t *testing.T) {
	t.Parallel()
	found, _ := RangeByName("foundation")
	fin, _ := RangeByName("financial")

	s := Skeleton("sessions", found)
	assert.True(t, strings.HasPrefix(s, "-- +goose Up\n"))
	assert.Contains(t, s, "-- +goose Down\n")
	assert.NotContains(t, s, "ledger history is never dropped")

	p := Skeleton("ledger", fin)
	assert.Contains(t, p, "-- +goose Down\nSELECT 1; -- ledger history is never dropped\n")
	assert.Contains(t, p, "forbid_mutation")

	// The skeleton is a migration goose can parse: version + name parse back.
	v, ok := parseVersion(Filename(100, "ledger"))
	assert.True(t, ok)
	assert.Equal(t, int64(100), v)
}

func TestPublicFuncs_RejectEmptyURL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	assert.Error(t, Up(ctx, ""))
	assert.Error(t, UpTo(ctx, "", 1))
	assert.Error(t, DownTo(ctx, "", 0))
	assert.Error(t, Verify(ctx, " "))
	_, err := Status(ctx, "")
	assert.Error(t, err)
	_, err = Version(ctx, "")
	assert.Error(t, err)
	assert.Error(t, UpTo(ctx, "postgres://x", -1))
}
