//go:build integration

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/advancedjobs"
	"github.com/nodal/controlplane/internal/advancedmetrics"
)

// TestIntegration_OnceRunsEveryJobThroughTheWiredServices: with the sandbox
// chain configured every job is wired, and `once` runs each of them -- and
// the collector -- through the services build constructs, against a migrated
// database, printing its report and exiting 0. This is the proof that the
// composition root wires every job it lists.
func TestIntegration_OnceRunsEveryJobThroughTheWiredServices(t *testing.T) {
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" || os.Getenv("CP_TEST_MIGRATE_DATABASE_URL") == "" {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	var seed [32]byte
	_, err := rand.Read(seed[:])
	require.NoError(t, err)
	lookup := exampleEnv(t, map[string]string{
		"CP_ENV":                     "LOCAL",
		"CP_DATABASE_APP_URL":        url,
		"CP_SANDBOX_DEVNET_NETWORK":  "advw-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		"CP_SANDBOX_DEVNET_SEED_REF": hex.EncodeToString(seed[:]),
	})
	names := []string{advancedmetrics.Name}
	for _, d := range advancedjobs.Catalog() {
		names = append(names, d.Name)
	}
	for _, name := range names {
		var out, errOut bytes.Buffer
		code := run([]string{"once", name}, lookup, &out, &errOut)
		require.Equal(t, exitOK, code, "%s: %s\n%s", name, out.String(), errOut.String())
		var report struct {
			Job      string           `json:"job"`
			Failed   bool             `json:"failed"`
			Failures []map[string]any `json:"failures"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &report), "%s prints its report as JSON", name)
		assert.Equal(t, name, report.Job)
		assert.False(t, report.Failed, "%s: %+v", name, report.Failures)
		t.Logf("%s: %s", name, strings.Join(strings.Fields(out.String()), " "))
	}
}
