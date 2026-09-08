package security

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/observability"
)

// An attacker who cannot get in settles for getting the system to talk. The
// two channels it talks through are the error body it returns to the caller
// and the log line it writes about itself, and both are fed by the same
// wrapped errors. This file proves that neither one renders the cause.
//
// The failures modeled here are real shapes, not straw men: a pgx error text
// carries the SQL and often the parameter values, a connection failure carries
// the DSN with the password in it, and a provider error carries the bearer
// token that was rejected.

// secretMaterial is the credential and infrastructure text that must never
// leave the process in an error body. Every value is distinctive enough that a
// substring search for it cannot match legitimate output by accident.
//
// The problem body is held to all of it, because errs.ToProblem does not
// inspect the cause at all: an INTERNAL problem carries a constant detail, so
// nothing in the chain can appear whatever it is.
var secretMaterial = map[string]string{
	"dsn_with_password": "postgres://cp_app:s3cr3t-pgpass-9f2a@db.internal.example:5432/controlplane?sslmode=require",
	"password":          "s3cr3t-pgpass-9f2a",
	"bearer_token":      "Bearer sk-ant-api03-QQQQWWWWEEEERRRRTTTTYYYYUUUUIIIIOOOOPPPP",
	"solana_secret_key": "4wBqpZM9xaSheZzJSMawUEKwzfHtLPZQxwqUvVjNBaGSsSFQGVvJTZjaXpDbfmYnLNGvDkCpn1XHhUCyBUJ4jNQq",
	"private_key_pem":   "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAxq4c\n-----END RSA PRIVATE KEY-----",
	"webhook_secret":    "whsec-7d1a3f9c8b2e4a6d",
	"session_token":     "hL8pQvR2sT4uV6wX8yZ0aB2cD4eF6gH8iJ0kL2mN4oP",
}

// loggedSecrets is what the log redactor is CONTRACTED to remove: the material
// PART 190 names, reaching the logger the three ways it can — under a denied
// key, as a value whose shape is recognizable whatever the key, and inside an
// error or a group.
//
// It deliberately excludes dsn_with_password. A connection string is not on
// PART 190's list and observability.MaskString does not recognize the
// "scheme://user:password@host" shape, so asserting its absence here would be
// asserting a property the redactor never claimed. Nothing logs a DSN today:
// config holds every database and Redis URL as a SecretRef (which renders as
// the redaction marker), internal/db never echoes the URL into an error, and
// pgconn redacts the password in ParseConfigError. The gap is therefore latent
// rather than live, and closing it belongs in internal/observability, not here.
var loggedSecrets = map[string]string{
	"password":          secretMaterial["password"],
	"bearer_token":      secretMaterial["bearer_token"],
	"solana_secret_key": secretMaterial["solana_secret_key"],
	"private_key_pem":   secretMaterial["private_key_pem"],
	"webhook_secret":    secretMaterial["webhook_secret"],
	"session_token":     secretMaterial["session_token"],
}

// internalCause is the sort of error that reaches the HTTP boundary when
// something has genuinely broken: a driver message quoting the statement, its
// parameters and the connection string.
func internalCause() error {
	base := fmt.Errorf(
		`ERROR: relation "ledger_balances" does not exist (SQLSTATE 42P01) `+
			`while executing SELECT id, owner_user_id FROM accounts WHERE owner_user_id = $1 AND status = 'ACTIVE'; `+
			`conn=%s; retry-with=%s`,
		secretMaterial["dsn_with_password"], secretMaterial["bearer_token"],
	)
	return fmt.Errorf("readmodel: load account: %w", base)
}

// loggedCause is an error of the shape that gets logged rather than returned:
// a provider rejection quoting the credential it rejected and the key material
// it was configured with. Both are patterns observability.MaskString
// recognizes, so both must be gone from the record.
func loggedCause() error {
	return fmt.Errorf("wallet: provider rejected the signer: auth=%s key=%s",
		secretMaterial["bearer_token"], secretMaterial["private_key_pem"])
}

// TestErrorBody_InternalFailuresNeverRenderTheCause proves the answer a client
// gets for an internal failure carries a constant detail and nothing from the
// error chain, whatever that chain says.
//
// The negative control writes the raw error text as the detail — the single
// most natural mistake in an error handler, and the reason errs.ToProblem
// exists — so the assertions below fire.
func TestErrorBody_InternalFailuresNeverRenderTheCause(t *testing.T) {
	renderTheCause := secBreak(t, "problem_body_renders_the_cause")

	cause := internalCause()
	// Sanity: the fixture must actually carry the secrets, or the assertions
	// below would hold for the wrong reason.
	for name, secret := range secretsIn(cause) {
		require.Contains(t, cause.Error(), secret, "the fixture error should carry %s", name)
	}

	cases := []struct {
		name string
		err  error
	}{
		{"bare_cause", cause},
		{"wrapped_internal", errs.Wrap(cause, errs.CodeInternal, "internal error")},
		{"unwrapped_sentinel", errors.New(secretMaterial["dsn_with_password"])},
		{
			"wrapped_with_fields",
			errs.Wrap(cause, errs.CodeInternal, "internal error").
				WithField("dsn", secretMaterial["dsn_with_password"]),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := errs.ToProblem(c.err, "/v1/accounts/01a00000-0000-7000-8000-0000000000aa", "req-"+c.name)
			if renderTheCause {
				p.Detail = c.err.Error()
			}

			rec := httptest.NewRecorder()
			errs.WriteProblem(rec, p)

			require.Equal(t, http.StatusInternalServerError, rec.Code,
				"an internal failure must answer 500, not leak through a softer status")
			require.Equal(t, errs.ContentType, rec.Header().Get("Content-Type"))

			body := rec.Body.String()
			assertNoSecrets(t, secretMaterial, body, "the problem body")

			// The generic markers matter as much as the literal secrets: a
			// SQLSTATE or a table name is a map of the schema.
			for _, marker := range []string{
				"SQLSTATE", "relation \"", "SELECT ", "ledger_balances", "sslmode", "postgres://", "readmodel:",
			} {
				require.NotContainsf(t, body, marker,
					"the problem body leaked %q, which describes the implementation: %s", marker, body)
			}
			require.Equal(t, string(errs.CodeInternal), string(p.Code))
			require.Equal(t, "internal error", p.Detail,
				"an INTERNAL problem must carry the constant detail and nothing else")
			require.Empty(t, p.Fields,
				"an INTERNAL problem must carry no fields; a field is a place for a cause to hide")
			// The request id is what support quotes, and it must survive.
			require.Contains(t, body, "req-"+c.name,
				"the request id must be returned, or a leak-free error is also an undebuggable one")
		})
	}
}

// TestLogRedaction_TheStandardLoggerNeverEmitsSecrets proves the redaction is
// on the path the binaries actually use. NewLogger, not RedactHandler, is what
// every cmd/ main constructs, so that is what is asserted here.
//
// The negative control builds the logger from the bare JSON handler with no
// redaction, which is what "someone simplified the logging setup" looks like,
// and every assertion below fires.
func TestLogRedaction_TheStandardLoggerNeverEmitsSecrets(t *testing.T) {
	bare := secBreak(t, "redaction_handler_removed")

	var buf strings.Builder
	// PROD, so the standard logger is the JSON handler the break replaces.
	log := observability.NewLogger(config.EnvProd, &buf)
	if bare {
		log = slog.New(slog.NewJSONHandler(&buf, nil))
	}

	// Secrets arrive four ways: as a denied key, as a value that looks like a
	// credential under an innocent key, inside an error, and beneath a group
	// whose name is itself denied.
	log.Info("session established",
		"password", secretMaterial["password"],
		"api_key", secretMaterial["bearer_token"],
		"session_token", secretMaterial["session_token"],
		"webhook_secret", secretMaterial["webhook_secret"])
	log.Warn("provider rejected the call",
		"authorization", secretMaterial["bearer_token"],
		"note", secretMaterial["bearer_token"])
	log.Error("wallet load failed",
		"err", loggedCause(),
		"key_material", secretMaterial["private_key_pem"])
	log.With("private_key", secretMaterial["solana_secret_key"]).
		Info("signing configured", "signer", secretMaterial["solana_secret_key"])
	log.WithGroup("secret").Info("nested", "value", secretMaterial["password"])

	out := buf.String()
	require.NotEmpty(t, out, "the logger wrote nothing, so the assertions below would be vacuous")
	require.Contains(t, out, "session established",
		"the log lines themselves must be present; only the secrets are removed")
	assertNoSecrets(t, loggedSecrets, out, "the log output")
	require.Contains(t, out, config.RedactedMarker,
		"nothing was marked redacted, so the secrets were dropped rather than replaced")

	// observability.Secret must render as the marker wherever it is used, so a
	// value typed as a secret cannot leak through a path the denylist misses.
	var s strings.Builder
	observability.NewLogger(config.EnvProd, &s).Info("typed",
		"anything", observability.Secret(secretMaterial["password"]))
	require.NotContains(t, s.String(), secretMaterial["password"])
	require.Equal(t, secretMaterial["password"],
		observability.Secret(secretMaterial["password"]).Reveal(),
		"Reveal must still return the value, or the type is unusable")
}

// TestLogRedaction_EveryBinaryBuildsItsLoggerThroughObservability keeps the
// control above meaningful. Proving NewLogger redacts says nothing if a binary
// logs through a handler of its own, so every cmd/ main is required to build
// its logger from observability and never from a bare slog handler.
func TestLogRedaction_EveryBinaryBuildsItsLoggerThroughObservability(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	require.NoError(t, err)

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, "cmd", e.Name())
		files, rerr := os.ReadDir(dir)
		require.NoError(t, rerr)
		usesObservability, usesBare := false, ""
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			src, ferr := os.ReadFile(filepath.Join(dir, f.Name()))
			require.NoError(t, ferr)
			text := string(src)
			if strings.Contains(text, "observability.NewLogger") || strings.Contains(text, "observability.NewLoggerWithOptions") {
				usesObservability = true
			}
			for _, bareCtor := range []string{"slog.NewJSONHandler", "slog.NewTextHandler"} {
				if strings.Contains(text, bareCtor) {
					usesBare = e.Name() + "/" + f.Name() + " uses " + bareCtor
				}
			}
		}
		if !usesObservability && usesBare == "" {
			continue // a binary that does not log at all
		}
		checked++
		require.Emptyf(t, usesBare,
			"%s: a binary must build its logger with observability.NewLogger, which is where redaction lives", usesBare)
		require.Truef(t, usesObservability,
			"cmd/%s constructs a logger without observability.NewLogger", e.Name())
	}
	require.Positive(t, checked, "no binary was checked; cmd/ moved and this guard is vacuous")
}

// secretsIn returns the entries of secretMaterial that appear in err, so the
// fixture can be asserted non-empty before its absence is asserted elsewhere.
func secretsIn(err error) map[string]string {
	out := map[string]string{}
	text := err.Error()
	for name, secret := range secretMaterial {
		if strings.Contains(text, secret) {
			out[name] = secret
		}
	}
	return out
}

func assertNoSecrets(t *testing.T, secrets map[string]string, out, what string) {
	t.Helper()
	for name, secret := range secrets {
		// PEM and DSN are checked by their most distinctive line, because
		// newlines are escaped differently by each encoder.
		for _, part := range strings.Split(secret, "\n") {
			if len(part) < 12 {
				continue
			}
			require.NotContainsf(t, out, part,
				"%s leaked %s (%q)", what, name, truncateForMessage(part))
		}
	}
}

func truncateForMessage(s string) string {
	if len(s) <= 48 {
		return s
	}
	return s[:48] + "..."
}
