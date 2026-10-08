//go:build integration

package marketplace

import (
	"context"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/sandboxrt"
)

var checkLiteral = regexp.MustCompile(`'([^']*)'::text`)

func checkValues(t *testing.T, constraint string) []string {
	t.Helper()
	var def string
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, constraint).Scan(&def),
		"constraint %s", constraint)
	var out []string
	for _, m := range checkLiteral.FindAllStringSubmatch(def, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func sortedStrings[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	sort.Strings(out)
	return out
}

// TestIntegration_EveryCheckListHasItsGoTwin pairs each CHECK (col IN (...))
// of migrations 00750 and 00757 with the Go function that lists the same
// values.
func TestIntegration_EveryCheckListHasItsGoTwin(t *testing.T) {
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	creators := sortedStrings(CreatorActorTypes())
	movers := sortedStrings(TransitionActorTypes())
	pairs := map[string][]string{
		"marketplace_listings_creator_actor_type_check":                 creators,
		"marketplace_listing_versions_status_check":                     sortedStrings(AllStatuses()),
		"marketplace_listing_versions_moderation_state_check":           sortedStrings(AllModerationStates()),
		"marketplace_listing_versions_risk_class_check":                 sortedStrings(AllRiskClasses()),
		"marketplace_listing_versions_created_by_actor_type_check":      creators,
		"marketplace_version_validations_outcome_check":                 sortedStrings(AllValidationOutcomes()),
		"marketplace_version_validations_failure_reason_check":          sortedStrings(AllValidationFailureReasons()),
		"marketplace_version_validations_validated_by_actor_type_check": movers,
		"marketplace_reviews_reviewer_actor_type_check":                 sortedStrings(ReviewerActorTypes()),
		"marketplace_reviews_decision_check":                            sortedStrings(AllReviewDecisions()),
		"marketplace_version_transitions_actor_type_check":              movers,
		"marketplace_moderation_transitions_actor_type_check":           movers,
		"marketplace_installs_mode_check":                               sortedStrings(AllInstallModes()),
		"marketplace_installs_status_check":                             sortedStrings(AllInstallStatuses()),
		"marketplace_installs_installed_by_actor_type_check":            creators,
		"marketplace_install_transitions_actor_type_check":              movers,
		"marketplace_follow_runs_trigger_check":                         sortedStrings(AllFollowTriggers()),
		"marketplace_follow_runs_actor_type_check":                      movers,
		"marketplace_follow_intents_outcome_check":                      sortedStrings(AllFollowOutcomes()),
		"marketplace_evaluations_kind_check":                            sortedStrings(AllEvaluationKinds()),
		"marketplace_evaluations_actor_type_check":                      movers,
		"marketplace_paper_fills_side_check":                            sortedStrings(sandboxrt.AllSides()),
		"marketplace_validation_claims_claimed_by_actor_type_check":     movers,
	}
	for constraint, goList := range pairs {
		assert.Equal(t, goList, checkValues(t, constraint), constraint)
	}
}
