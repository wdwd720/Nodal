package httpapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// The admin reconciliation routes return every record, of every kind, mode
// and status the reconciliation package defines. The API's enums must name
// each of them and nothing else, or a record of a newer kind breaks the
// contract -- and every generated client's validation -- the day the first
// one is written. (The advanced kinds were missing until migration 00760.)
func TestReconciliationRecord_TheAPIEnumsAreExactlyThePackagesValues(t *testing.T) {
	spec, err := api.GetSwagger()
	require.NoError(t, err)
	schema := spec.Components.Schemas["ReconciliationRecord"]
	require.NotNil(t, schema, "ReconciliationRecord schema")

	enum := func(field string) []string {
		prop := schema.Value.Properties[field]
		require.NotNil(t, prop, field)
		var out []string
		for _, v := range prop.Value.Enum {
			s, ok := v.(string)
			require.True(t, ok, "%s enum value %v", field, v)
			out = append(out, s)
		}
		return out
	}

	var kinds, modes, statuses []string
	for _, k := range reconciliation.AllKinds() {
		kinds = append(kinds, string(k))
		assert.True(t, api.ReconciliationRecordKind(k).Valid(), "kind %s", k)
	}
	for _, m := range reconciliation.AllModes() {
		modes = append(modes, string(m))
		assert.True(t, api.ReconciliationRecordMode(m).Valid(), "mode %s", m)
	}
	for _, s := range reconciliation.AllStatuses() {
		statuses = append(statuses, string(s))
		assert.True(t, api.ReconciliationRecordStatus(s).Valid(), "status %s", s)
	}
	assert.ElementsMatch(t, kinds, enum("kind"), "ReconciliationRecord.kind")
	assert.ElementsMatch(t, modes, enum("mode"), "ReconciliationRecord.mode")
	assert.ElementsMatch(t, statuses, enum("status"), "ReconciliationRecord.status")
}
