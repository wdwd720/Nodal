package httpapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
)

// The published contract's enums are the code's. The operator console found
// the Capability enum naming ten of twenty capabilities: the generated server
// binds the path parameter as a plain string and gates.Capability.Valid does
// the checking, so nothing failed -- a client generated from the contract
// simply could not name half the gates. Both enums are held here to the Go
// lists the database is already held to (test/integration/enums).
func TestOpenAPI_GateEnumsMatchTheCode(t *testing.T) {
	t.Parallel()
	spec, err := api.GetSpec()
	require.NoError(t, err)

	capability := spec.Components.Schemas["Capability"]
	require.NotNil(t, capability, "the contract declares a Capability schema")
	want := make([]string, 0, len(gates.AllCapabilities()))
	for _, c := range gates.AllCapabilities() {
		want = append(want, string(c))
	}
	assert.Equal(t, want, enumStrings(t, capability.Value.Enum), "Capability enum")

	gate := spec.Components.Schemas["CapabilityGate"]
	require.NotNil(t, gate, "the contract declares a CapabilityGate schema")
	state := gate.Value.Properties["state"]
	require.NotNil(t, state, "CapabilityGate.state")
	wantStates := make([]string, 0, len(gates.AllStates()))
	for _, s := range gates.AllStates() {
		wantStates = append(wantStates, string(s))
	}
	assert.Equal(t, wantStates, enumStrings(t, state.Value.Enum), "CapabilityGate.state enum")
}

func enumStrings(t *testing.T, enum []any) []string {
	t.Helper()
	out := make([]string, 0, len(enum))
	for _, e := range enum {
		s, ok := e.(string)
		require.True(t, ok, "enum value %v is not a string", e)
		out = append(out, s)
	}
	return out
}
