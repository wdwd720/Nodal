package marketplace

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nodal/controlplane/internal/settlement"
)

// TestRouteRefusalReasons_NameTheSkippedStep: the reasons of a route that may
// not execute now are the compiler's refusals, or -- for a permitted route
// that needs a person -- that step; a route that may execute has none.
func TestRouteRefusalReasons_NameTheSkippedStep(t *testing.T) {
	assert.Empty(t, routeRefusalReasons(settlement.Route{Permitted: true, RequiredConfirmation: settlement.ConfirmNone}))
	assert.Equal(t, []string{"REQUIRES_USER_CONFIRMATION"},
		routeRefusalReasons(settlement.Route{Permitted: true, RequiredConfirmation: settlement.ConfirmUserReview}))
	assert.Equal(t, []string{"REQUIRES_MANUAL_REVIEW"},
		routeRefusalReasons(settlement.Route{Permitted: true, RequiredConfirmation: settlement.ConfirmManualReview}))
	assert.Equal(t, []string{"A", "B"}, routeRefusalReasons(settlement.Route{Reasons: []string{"B", "A"}}))
}
