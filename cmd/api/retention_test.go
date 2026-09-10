package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
)

// A retention pass that cannot run says so (F-105).
//
// login_attempts holds a plaintext OIDC nonce and PKCE verifier per attempt,
// and its purge lives in cmd/audit-worker -- a binary this deployment does not
// run, because render.yaml declares one web service and no workers. So the
// purge has never run and those secrets are kept forever, on a database whose
// ceiling halts every financial action when it fills.
//
// The purge now runs here. When it CANNOT -- no ops role configured, or a
// retention of zero -- it must not start silently: "a control that reports
// success having run nothing" is the defect class this repository keeps
// finding, and a retention pass that quietly does not exist is the same shape.
//
// It must also not refuse to boot. cmd/audit-worker refuses because purging is
// why that binary was deployed; this is a web service whose job is serving
// requests, and taking it down over an unconfigured retention pass trades a
// disclosure risk for an outage.
func TestLoginAttemptRetention_SaysSoWhenItCannotRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		expect string
	}{
		{
			name: "no ops role",
			cfg: &config.Config{
				Retention: config.RetentionConfig{LoginAttemptDays: 2},
				Database:  config.DatabaseConfig{OpsURL: ""},
			},
			expect: "CP_DATABASE_OPS_URL is not set",
		},
		{
			name: "no retention window",
			cfg: &config.Config{
				Retention: config.RetentionConfig{LoginAttemptDays: 0},
				Database:  config.DatabaseConfig{OpsURL: "postgres://x"},
			},
			expect: "CP_RETENTION_LOGIN_ATTEMPT_DAYS is not positive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, nil))

			// It returns rather than blocking: a ticker that never starts must
			// not hold the goroutine open either.
			done := make(chan struct{})
			go func() {
				runOpsRetention(context.Background(), tc.cfg, func(string) (string, bool) { return "", false }, log)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("it neither ran nor returned")
			}

			out := buf.String()
			require.NotEmpty(t, out, "it declined silently")
			assert.Contains(t, out, tc.expect)

			var rec map[string]any
			require.NoError(t, json.Unmarshal([]byte(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]), &rec))
			assert.Equal(t, "WARN", rec["level"], "an unconfigured control is a warning, not an aside")
			assert.Contains(t, rec["consequence"], "kept indefinitely",
				"the log line must say what will not happen, not just that something did not start")
		})
	}
}
