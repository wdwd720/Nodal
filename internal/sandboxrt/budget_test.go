package sandboxrt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func TestBudget_ValidateRefusesWhatIsOutsideThePolicy(t *testing.T) {
	require.NoError(t, DefaultBudget().Validate())
	assert.Equal(t, 20, DefaultBudget().MaxRunsPerCall, "never a thousand runs in one call")
	cases := map[string]func(*Budget){
		"no version":                func(b *Budget) { b.Version = "" },
		"window too short":          func(b *Budget) { b.Window = time.Second },
		"window too long":           func(b *Budget) { b.Window = 30 * 24 * time.Hour },
		"window not whole ms":       func(b *Budget) { b.Window = time.Hour + time.Microsecond },
		"no runs per call":          func(b *Budget) { b.MaxRunsPerCall = 0 },
		"a thousand runs in a call": func(b *Budget) { b.MaxRunsPerCall = 1000 },
		"no runs per window":        func(b *Budget) { b.MaxRunsPerWindow = 0 },
		"no CPU per call":           func(b *Budget) { b.MaxCPUMSPerCall = 0 },
		"no wall per window":        func(b *Budget) { b.MaxWallMSPerWindow = 0 },
		"call bigger than a window": func(b *Budget) { b.MaxRunsPerWindow = 10 },
		"call CPU above window CPU": func(b *Budget) { b.MaxCPUMSPerWindow = 30_000 },
	}
	for name, mutate := range cases {
		b := DefaultBudget()
		mutate(&b)
		err := b.Validate()
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), name)
	}
	reasons := AllBudgetReasons()
	assert.Len(t, reasons, 6)
	for _, r := range reasons {
		assert.Regexp(t, `^SANDBOX_BUDGET_[A-Z_]+$`, r, "a stable, machine-readable reason")
	}
}
