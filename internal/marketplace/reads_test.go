package marketplace

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// TestReads_RefuseWhatTheyCannotBound: every list read refuses, before it
// queries anything (there is no database here), a page it cannot bound, a key
// that names no row and a look-up of more than MaxReadLimit rows.
func TestReads_RefuseWhatTheyCannotBound(t *testing.T) {
	ctx := context.Background()
	key := &PageKey{At: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), ID: "not-a-row"}
	tooMany := make([]VersionID, MaxReadLimit+1)
	for i := range tooMany {
		tooMany[i] = id.New[versionKind]()
	}
	installs := make([]InstallID, MaxReadLimit+1)
	for i := range installs {
		installs[i] = id.New[installKind]()
	}
	refusals := map[string]func() error{
		"an empty page":            func() error { _, _, err := Catalogue(ctx, nil, nil, 0); return err },
		"a page beyond the bound":  func() error { _, _, err := Catalogue(ctx, nil, nil, MaxReadLimit+1); return err },
		"a key that names no row":  func() error { _, _, err := AccountListings(ctx, nil, accounts.NewAccountID(), key, 10); return err },
		"a queue of no status":     func() error { _, _, err := Queue(ctx, nil, nil, nil, 10); return err },
		"a queue of an odd status": func() error { _, _, err := Queue(ctx, nil, []Status{"ARCHIVED"}, nil, 10); return err },
		"too many versions' reviews": func() error {
			_, err := Reviews(ctx, nil, tooMany)
			return err
		},
		"too many versions' evaluations": func() error {
			_, err := Evaluations(ctx, nil, tooMany, 10)
			return err
		},
		"too many versions' performance": func() error {
			_, err := PaperPerformance(ctx, nil, tooMany, 10)
			return err
		},
		"too many installs' performance": func() error {
			_, err := LatestPaperPerformance(ctx, nil, installs)
			return err
		},
		"an unbounded version list": func() error { _, err := ListingVersionIDs(ctx, nil, id.New[listingKind](), 0); return err },
		"an installs page beyond the bound": func() error {
			_, _, err := AccountInstalls(ctx, nil, accounts.NewAccountID(), nil, MaxReadLimit+1)
			return err
		},
		"a follow page with a key that names no row": func() error {
			_, _, err := AccountFollowRuns(ctx, nil, accounts.NewAccountID(), nil, key, 10)
			return err
		},
	}
	for name, read := range refusals {
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(read()), name)
	}

	// Nothing to look up is an empty answer, still without a query.
	reviews, err := Reviews(ctx, nil, nil)
	assert.NoError(t, err)
	assert.Empty(t, reviews)
	latest, err := LatestPaperPerformance(ctx, nil, nil)
	assert.NoError(t, err)
	assert.Empty(t, latest)
}

func TestKeyed_CutsOneRowMoreThanAPageIntoAPageAndMore(t *testing.T) {
	page, more := keyed([]int{1, 2, 3}, 2)
	assert.Equal(t, []int{1, 2}, page)
	assert.True(t, more)
	page, more = keyed([]int{1, 2}, 2)
	assert.Equal(t, []int{1, 2}, page)
	assert.False(t, more)
	page, more = keyed([]int(nil), 2)
	assert.Empty(t, page)
	assert.False(t, more)
}
