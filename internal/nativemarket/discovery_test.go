package nativemarket

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

// TestCandleRequest_TheWindowIsBounded: a chart may ask for a window, not for
// a table scan a client can request by typing a date.
func TestCandleRequest_TheWindowIsBounded(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	base := CandleRequest{MarketID: NewMarketID(), Interval: Interval1m, From: from, To: from.Add(time.Hour)}
	require.NoError(t, base.Validate())

	// Exactly MaxCandles buckets is permitted; one more is not.
	atCap := base
	atCap.To = from.Add(MaxCandles * time.Minute)
	require.NoError(t, atCap.Validate())
	over := base
	over.To = from.Add((MaxCandles + 1) * time.Minute)
	require.Error(t, over.Validate())

	// The same span at a wider interval fits, which is the remedy the message
	// names.
	wide := over
	wide.Interval = Interval1h
	require.NoError(t, wide.Validate())

	for name, mutate := range map[string]func(*CandleRequest){
		"no market":      func(r *CandleRequest) { r.MarketID = MarketID{} },
		"unknown window": func(r *CandleRequest) { r.Interval = "3m" },
		"no from":        func(r *CandleRequest) { r.From = time.Time{} },
		"no to":          func(r *CandleRequest) { r.To = time.Time{} },
		"reversed":       func(r *CandleRequest) { r.From, r.To = r.To, r.From },
		"empty window":   func(r *CandleRequest) { r.To = r.From },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := base
			mutate(&r)
			err := r.Validate()
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestIntervals_AreAClosedSetWithRealDurations.
func TestIntervals_AreAClosedSetWithRealDurations(t *testing.T) {
	t.Parallel()
	want := map[Interval]time.Duration{
		Interval1m: time.Minute, Interval5m: 5 * time.Minute, Interval15m: 15 * time.Minute,
		Interval1h: time.Hour, Interval1d: 24 * time.Hour,
	}
	all := AllIntervals()
	require.Len(t, all, len(want))
	last := time.Duration(0)
	for _, i := range all {
		d, ok := i.Duration()
		require.True(t, ok, "%s has no duration", i)
		assert.Equal(t, want[i], d)
		assert.Greater(t, d, last, "AllIntervals must be shortest first")
		last = d
	}
	_, ok := Interval("2h").Duration()
	assert.False(t, ok)
}

// TestSortBy_OnlyNewestIsStable: the response tells a client which orderings it
// can page through safely, so the caveat does not have to live in a comment
// nobody reads.
func TestSortBy_OnlyNewestIsStable(t *testing.T) {
	t.Parallel()
	require.Len(t, AllSorts(), 5)
	stable := 0
	for _, s := range AllSorts() {
		require.True(t, s.Valid())
		require.Contains(t, sortKeyExpressions, s, "%s has no SQL sort key", s)
		if s.Stable() {
			stable++
		}
	}
	assert.Equal(t, 1, stable, "exactly one ordering may claim stability")
	assert.True(t, SortNewest.Stable())
	assert.False(t, SortBy("BY_VIBES").Valid())
}

// TestSortKeyExpressions_AreCompiledInAndNeverBuiltFromCallerText: the closed
// map is the only thing that varies in the list statement, so a value that
// carried a caller's text would be the one injection point.
func TestSortKeyExpressions_AreCompiledInAndNeverBuiltFromCallerText(t *testing.T) {
	t.Parallel()
	for sort, expr := range sortKeyExpressions {
		assert.NotContains(t, expr, ";", "%s: a sort key may not end a statement", sort)
		assert.NotContains(t, expr, "--", "%s: a sort key may not start a comment", sort)
	}
	// Every parameter the statement binds appears in it, and none is skipped:
	// a gap would mean an argument silently landing in the wrong slot.
	full := listQueryHead + sortKeyExpressions[SortNewest] + listQueryTail
	for i := 1; i <= 10; i++ {
		assert.Contains(t, full, dollar(i), "the list statement does not bind $%d", i)
	}
	assert.NotContains(t, full, "$11")
}

func dollar(n int) string {
	if n < 10 {
		return "$" + string(rune('0'+n))
	}
	return "$1" + string(rune('0'+n-10))
}

// TestEscapeLike_NeutralisesTheWildcards: without it, a search for "%" matches
// every market and a search for "_" matches every one-character symbol, which
// is a search box answering a question nobody asked.
func TestEscapeLike_NeutralisesTheWildcards(t *testing.T) {
	t.Parallel()
	assert.Equal(t, `\%`, escapeLike("%"))
	assert.Equal(t, `\_`, escapeLike("_"))
	assert.Equal(t, `\\`, escapeLike(`\`))
	assert.Equal(t, `a\%b\_c`, escapeLike("a%b_c"))
	assert.Equal(t, "plain", escapeLike("plain"))
}

// TestListCursor_IsOpaqueAndRefusesTampering: a cursor a client can construct
// from a guessed id is a cursor a client can use to page into somebody else's
// ordering, and a malformed one must be a validation error rather than a page
// that silently starts from the beginning.
func TestListCursor_IsOpaqueAndRefusesTampering(t *testing.T) {
	t.Parallel()
	id := NewMarketID()
	encoded := encodeListCursor(listCursor{Key: "1234.5678", ID: id.String()})
	require.NotEmpty(t, encoded)
	assert.NotContains(t, encoded, id.String(), "the cursor must not be readable as its parts")

	got, ok, err := decodeListCursor(encoded)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "1234.5678", got.Key)
	assert.Equal(t, id.String(), got.ID)

	_, ok, err = decodeListCursor("")
	require.NoError(t, err)
	assert.False(t, ok, "no cursor is the first page, not an error")

	for name, bad := range map[string]string{
		"not base64":    "!!!!",
		"not json":      "bm90LWpzb24",
		"key not a rat": encodeListCursor(listCursor{Key: "drop table", ID: id.String()}),
		"id not an id":  encodeListCursor(listCursor{Key: "1", ID: "../../etc/passwd"}),
		"no id at all":  encodeListCursor(listCursor{Key: "1"}),
		"empty key":     encodeListCursor(listCursor{ID: id.String()}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := decodeListCursor(bad)
			require.Error(t, err)
			assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// TestSummaryProjections_NameTheSameColumns: the inner projection and the outer
// one are written separately, so a column added to one and forgotten in the
// other would be a scan error at runtime rather than a compile error.
func TestSummaryProjections_NameTheSameColumns(t *testing.T) {
	t.Parallel()
	outer := outerNames(summaryOut)
	// One scan target per outer column, plus the sort key the outer SELECT
	// puts in front of them.
	require.Len(t, outer, 26)
	for _, name := range outer {
		assert.Contains(t, summaryColumns, name,
			"the outer projection selects r.%s and the inner one never produces it", name)
	}
}

// outerNames reduces `r.a, r.b AS c` to the names the rows come out under.
func outerNames(projection string) []string {
	var out []string
	for _, part := range strings.Split(projection, ",") {
		f := strings.Fields(strings.TrimSpace(part))
		if len(f) == 0 {
			continue
		}
		name := f[len(f)-1]
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
		out = append(out, name)
	}
	return out
}
