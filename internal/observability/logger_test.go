package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
)

const sensitive = "SENSITIVE-VALUE-1234567890"

// capture runs fn against a JSON logger and returns the decoded last line
// plus the raw output.
func capture(t *testing.T, env config.Environment, fn func(*slog.Logger)) (map[string]any, string) {
	t.Helper()
	var buf bytes.Buffer
	fn(NewLogger(env, &buf))
	raw := buf.String()
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	last := lines[len(lines)-1]
	var m map[string]any
	if strings.HasPrefix(last, "{") {
		require.NoError(t, json.Unmarshal([]byte(last), &m), raw)
	}
	return m, raw
}

// jsonObject returns m[key] as a JSON object, failing the test when it is not one.
func jsonObject(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := m[key].(map[string]any)
	require.True(t, ok, "%q is not a JSON object: %T", key, m[key])
	return o
}

func TestNewLogger_FormatByEnvironment(t *testing.T) {
	t.Parallel()
	_, jsonOut := capture(t, config.EnvTest, func(l *slog.Logger) { l.Info("hello", "password", sensitive, "k", "v") })
	assert.True(t, strings.HasPrefix(jsonOut, "{"), "non-LOCAL is JSON: %s", jsonOut)
	assert.Contains(t, jsonOut, `"password":"[REDACTED]"`)
	assert.NotContains(t, jsonOut, sensitive)

	_, textOut := capture(t, config.EnvLocal, func(l *slog.Logger) { l.Debug("hello", "password", sensitive, "k", "v") })
	assert.False(t, strings.HasPrefix(textOut, "{"), "LOCAL is text: %s", textOut)
	assert.Contains(t, textOut, "msg=hello")
	assert.Contains(t, textOut, "password=[REDACTED]")
	assert.NotContains(t, textOut, sensitive)

	_, prodDebug := capture(t, config.EnvProd, func(l *slog.Logger) { l.Debug("hidden") })
	assert.Empty(t, prodDebug, "Info level outside LOCAL")

	assert.NotNil(t, NewLogger(config.EnvProd, nil), "nil writer is tolerated")
	var buf bytes.Buffer
	NewLoggerWithOptions(config.EnvProd, &buf, LoggerOptions{Level: slog.LevelDebug}).Debug("shown")
	assert.Contains(t, buf.String(), "shown")
}

func TestRedaction_DenylistKeys(t *testing.T) {
	t.Parallel()
	keys := []string{
		"private_key", "privatekey", "PrivateKey", "privateKey", "PRIVATE-KEY", "wallet.private_key",
		"seed", "seed_phrase", "seedPhrase", "mnemonic", "secret", "client_secret", "clientSecret",
		"token", "access_token", "refresh_token", "id_token", "csrf_token", "authorization", "Authorization",
		"cookie", "Cookie", "set-cookie", "Set-Cookie", "password", "db_password", "passwd", "api_key",
		"apikey", "apiKey", "APIKey", "x-api-key", "card", "card_number", "pan", "PAN", "cvv", "ssn",
		"signing_token", "webhook_secret", "provider_webhook_secret",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			m, raw := capture(t, config.EnvTest, func(l *slog.Logger) { l.Info("x", key, sensitive) })
			assert.Equal(t, RedactedMarker, m[key], raw)
			assert.NotContains(t, raw, sensitive)
			assert.True(t, IsDeniedKey(key))
		})
	}
}

func TestRedaction_AllowedKeysPassThrough(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"company", "cardinality", "span_id", "tokens_used", "expand", "seeded_at", "user_id", "amount", "panel", "discard", "secretary"} {
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			m, raw := capture(t, config.EnvTest, func(l *slog.Logger) { l.Info("x", key, "visible") })
			assert.Equal(t, "visible", m[key], raw)
			assert.False(t, IsDeniedKey(key))
		})
	}
}

func TestRedaction_NestedGroups(t *testing.T) {
	t.Parallel()
	t.Run("inline group member", func(t *testing.T) {
		t.Parallel()
		m, raw := capture(t, config.EnvTest, func(l *slog.Logger) {
			l.Info("x", slog.Group("db", slog.String("password", sensitive), slog.String("host", "h"),
				slog.Group("inner", slog.String("api_key", sensitive), slog.Int("port", 5))))
		})
		db := jsonObject(t, m, "db")
		assert.Equal(t, RedactedMarker, db["password"])
		assert.Equal(t, "h", db["host"])
		inner := jsonObject(t, db, "inner")
		assert.Equal(t, RedactedMarker, inner["api_key"])
		assert.Equal(t, float64(5), inner["port"])
		assert.NotContains(t, raw, sensitive)
	})
	t.Run("WithGroup member", func(t *testing.T) {
		t.Parallel()
		m, raw := capture(t, config.EnvTest, func(l *slog.Logger) {
			l.WithGroup("auth").WithGroup("session").Info("x", "token", sensitive, "id", "s1")
		})
		session := jsonObject(t, jsonObject(t, m, "auth"), "session")
		assert.Equal(t, RedactedMarker, session["token"])
		assert.Equal(t, "s1", session["id"])
		assert.NotContains(t, raw, sensitive)
	})
	t.Run("denied group name redacts everything beneath", func(t *testing.T) {
		t.Parallel()
		m, raw := capture(t, config.EnvTest, func(l *slog.Logger) {
			l.WithGroup("secret").Info("x", "anything", sensitive, "other", "also hidden")
		})
		g := jsonObject(t, m, "secret")
		assert.Equal(t, RedactedMarker, g["anything"])
		assert.Equal(t, RedactedMarker, g["other"])
		assert.NotContains(t, raw, sensitive)
	})
	t.Run("denied inline group is replaced whole", func(t *testing.T) {
		t.Parallel()
		m, raw := capture(t, config.EnvTest, func(l *slog.Logger) {
			l.Info("x", slog.Group("cookie", slog.String("name", "cp"), slog.String("value", sensitive)))
		})
		assert.Equal(t, RedactedMarker, m["cookie"])
		assert.NotContains(t, raw, sensitive)
	})
	t.Run("WithAttrs", func(t *testing.T) {
		t.Parallel()
		m, raw := capture(t, config.EnvTest, func(l *slog.Logger) {
			l.With("api_key", sensitive, "region", "eu").WithGroup("g").With("password", sensitive).Info("x")
		})
		assert.Equal(t, RedactedMarker, m["api_key"])
		assert.Equal(t, "eu", m["region"])
		assert.Equal(t, RedactedMarker, jsonObject(t, m, "g")["password"])
		assert.NotContains(t, raw, sensitive)
	})
}

const (
	jwt        = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	solanaKey  = "4NMwxzmYj2uvHuq8xoqhY8RXg63KSVJM1DXkpbmkUY7YQWuoyQgFnnzn6yo3CMnqZasnNPNuAT2TLwQsCaKkUddp"
	solanaPub  = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	pemBlock   = "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7\n-----END PRIVATE KEY-----"
	pemPartial = "-----BEGIN EC PRIVATE KEY-----\nMHQCAQEEIBkQ3Yq2hZ6LQm"
)

func TestRedaction_ValuePatterns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		in         string
		wantMasked bool
		keep       string
	}{
		{"bearer jwt", "Authorization: Bearer " + jwt, true, "Bearer [REDACTED]"},
		{"bearer lowercase", "bearer " + jwt + " sent", true, "sent"},
		{"bearer short is not a token", "Bearer abc", false, "Bearer abc"},
		{"solana secret key", "key=" + solanaKey + " loaded", true, "loaded"},
		{"solana public key kept", "pubkey=" + solanaPub, false, solanaPub},
		{"pem block", "cert: " + pemBlock + " end", true, "end"},
		{"pem truncated", "partial " + pemPartial, true, "partial"},
		{"plain text", "nothing to see here at all", false, "nothing to see here at all"},

		// Connection strings. The password goes; the host, port and database
		// stay, because those are what an operator reads a connection log line
		// for. Adversarial security testing found that nothing covered these:
		// not exploitable today, since config holds every DSN as a SecretRef and
		// neither internal/db nor pgconn echoes one, but that is three separate
		// behaviors all having to keep holding, and one stray
		// log.Info("connecting", "dsn", url) would print a live credential.
		{
			"postgres dsn", "postgres://cp_app:s3cr3t-pgpass-9f2a@db.internal.example:5432/controlplane?sslmode=require",
			true, "@db.internal.example:5432/controlplane?sslmode=require",
		},
		{"dsn inside a sentence", "connecting to redis://user:hunter2hunter2@cache.internal:6379/0 now", true, "@cache.internal:6379/0 now"},
		{"dsn keeps the username", "amqps://svc_ingest:pa55word-long@broker.internal:5671/vhost", true, "amqps://svc_ingest:"},
		// A URL with no credential is not a secret and must survive intact,
		// or every logged endpoint becomes unreadable.
		{"url without userinfo", "https://api.example.com/v1/health?probe=1", false, "https://api.example.com/v1/health?probe=1"},
		{"url with user but no password", "postgres://cp_app@db.internal.example:5432/controlplane", false, "postgres://cp_app@db.internal.example:5432/controlplane"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := MaskString(tc.in)
			if tc.wantMasked {
				assert.Contains(t, out, RedactedMarker)
				assert.NotEqual(t, tc.in, out)
			} else {
				assert.Equal(t, tc.in, out)
			}
			assert.Contains(t, out, tc.keep)
			for _, secret := range []string{jwt, solanaKey, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7", "MHQCAQEEIBkQ3Yq2hZ6LQm"} {
				if tc.wantMasked {
					assert.NotContains(t, out, secret)
				}
			}

			// The same masking applies to messages, string attributes and errors.
			m, raw := capture(t, config.EnvTest, func(l *slog.Logger) {
				l.Info(tc.in, "detail", tc.in, "err", errors.New(tc.in))
			})
			assert.Equal(t, out, m["msg"], raw)
			assert.Equal(t, out, m["detail"], raw)
			assert.Equal(t, out, m["err"], raw)
		})
	}
}

func TestSecret_NeverRenders(t *testing.T) {
	t.Parallel()
	s := Secret(sensitive)
	m, raw := capture(t, config.EnvTest, func(l *slog.Logger) { l.Info("x", "note", s, slog.Any("any", s)) })
	assert.Equal(t, RedactedMarker, m["note"])
	assert.Equal(t, RedactedMarker, m["any"])
	assert.NotContains(t, raw, sensitive)
	assert.Equal(t, RedactedMarker, fmt.Sprint(s))
	assert.Equal(t, RedactedMarker, fmt.Sprintf("%s|%v", s, s)[:len(RedactedMarker)])
	b, err := json.Marshal(struct{ S Secret }{s})
	require.NoError(t, err)
	assert.Equal(t, `{"S":"[REDACTED]"}`, string(b))
	assert.Equal(t, sensitive, s.Reveal())

	_, text := capture(t, config.EnvLocal, func(l *slog.Logger) { l.Info("x", "note", s) })
	assert.NotContains(t, text, sensitive)
}

func TestSplitKey(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"private_key":   {"private", "key"},
		"PrivateKey":    {"private", "key"},
		"privateKey":    {"private", "key"},
		"APIKey":        {"api", "key"},
		"x-api-key":     {"x", "api", "key"},
		"Set-Cookie":    {"set", "cookie"},
		"SSN":           {"ssn"},
		"wallet.seed":   {"wallet", "seed"},
		"HTTPServer2Go": {"http", "server2", "go"},
		"":              nil,
		"___":           nil,
	}
	for in, want := range cases {
		assert.Equal(t, want, splitKey(in), in)
	}
}

func TestRedactHandler_Composition(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	r := NewRedactHandler(base)
	assert.Same(t, r, NewRedactHandler(r), "wrapping is idempotent")
	assert.False(t, r.Enabled(t.Context(), slog.LevelInfo))
	assert.True(t, r.Enabled(t.Context(), slog.LevelError))
	assert.Same(t, r, r.WithGroup(""), "empty group is a no-op")
	c := NewContextHandler(r)
	assert.Same(t, c, NewContextHandler(c))
	assert.Same(t, c, c.WithGroup(""))

	// A pre-resolved LogValuer inside a group is still inspected.
	l := slog.New(NewContextHandler(r))
	l.Error("x", slog.Group("g", slog.Any("token", slog.StringValue(sensitive))))
	assert.Contains(t, buf.String(), RedactedMarker)
	assert.NotContains(t, buf.String(), sensitive)
}
