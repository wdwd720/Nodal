package terms

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// versionPattern is migration 00759's CHECK, transcribed. A version the schema
// refuses is a document nobody can ever accept, and the failure would land at
// the moment somebody tried.
var versionPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}\.[0-9]+$`)

func TestRegistry_EveryDeclaredDocumentLoads(t *testing.T) {
	t.Parallel()
	docs, err := Current()
	require.NoError(t, err)
	require.Len(t, docs, len(sources))
	for _, d := range docs {
		assert.NotEmpty(t, d.Body, "%s has no text", d.ID)
		assert.NotEmpty(t, d.Title, "%s has no title", d.ID)
		assert.True(t, versionPattern.MatchString(d.Version),
			"%s version %q is not the shape the database CHECK accepts", d.ID, d.Version)
		assert.Len(t, d.ContentHash, 64, "%s hash is not a sha256 hex digest", d.ID)
	}
}

// The hash is what an acceptance records, so two documents must never share one
// and the value must not depend on how the file happened to be checked out.
func TestRegistry_ContentHashesAreDistinctAndLineEndingIndependent(t *testing.T) {
	t.Parallel()
	seen := map[string]DocumentID{}
	for _, d := range MustCurrent() {
		if other, dup := seen[d.ContentHash]; dup {
			t.Fatalf("%s and %s hash to the same value", d.ID, other)
		}
		seen[d.ContentHash] = d.ID
		assert.NotContains(t, d.Body, "\r", "%s kept a carriage return; the hash would differ per checkout", d.ID)
	}
}

// PART 48: do not fabricate final legal terms, and mark what needs counsel.
func TestRegistry_EveryDocumentIsMarkedForCounselReview(t *testing.T) {
	t.Parallel()
	for _, d := range MustCurrent() {
		assert.True(t, d.CounselReviewRequired, "%s is not marked as needing counsel review", d.ID)
		assert.Contains(t, strings.ToLower(d.Body), "counsel",
			"%s does not say in its own text that it awaits counsel; the flag and the copy must agree", d.ID)
	}
}

// The Credits document is the one PART 6 requires a user to acknowledge before
// they hold Credits, and its first job is to say Credits are not withdrawable.
func TestRegistry_CreditsTermsSayCreditsAreNotWithdrawable(t *testing.T) {
	t.Parallel()
	d, ok := Get(CreditsTerms)
	require.True(t, ok)
	body := strings.ToLower(d.Body)
	assert.Contains(t, body, "not withdrawable")
	assert.Contains(t, body, "not money")
}

func TestRegistry_OnboardingAsksForFourDocumentsAndNotTheWithdrawalOne(t *testing.T) {
	t.Parallel()
	var ids []DocumentID
	for _, d := range RequiredAt(AtOnboarding) {
		ids = append(ids, d.ID)
	}
	assert.ElementsMatch(t, []DocumentID{TermsOfService, PrivacyPolicy, RiskDisclosure, CreditsTerms}, ids)

	// Frontloading the withdrawal disclosure would be exactly the frontloaded
	// KYC the onboarding section forbids.
	var later []DocumentID
	for _, d := range RequiredAt(BeforeWithdrawal) {
		later = append(later, d.ID)
	}
	assert.Equal(t, []DocumentID{WithdrawalDisclosure}, later)
}

func TestRegistry_AllDocumentIDsIsSortedAndComplete(t *testing.T) {
	t.Parallel()
	ids := AllDocumentIDs()
	require.Len(t, ids, len(sources))
	for i := 1; i < len(ids); i++ {
		assert.Less(t, string(ids[i-1]), string(ids[i]), "AllDocumentIDs is not sorted")
	}
	for _, id := range ids {
		assert.True(t, id.Valid(), "%s is listed but does not resolve", id)
	}
	assert.False(t, DocumentID("NOT_A_DOCUMENT").Valid())
}

func TestRegistry_GetIsUnknownForAnUndeclaredID(t *testing.T) {
	t.Parallel()
	_, ok := Get("COOKIE_POLICY")
	assert.False(t, ok)
}
