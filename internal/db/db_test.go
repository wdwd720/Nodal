package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSLMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"url disable", "postgres://u:p@h:5432/d?sslmode=disable", "disable"},
		{"url verify-full", "postgres://u:p@h:5432/d?sslmode=verify-full", "verify-full"},
		{"url verify-ca mixed case", "postgresql://u:p@h/d?sslmode=Verify-CA", "verify-ca"},
		{"url require", "postgres://u:p@h/d?sslmode=require&application_name=x", "require"},
		{"url absent defaults to prefer", "postgres://u:p@h/d", "prefer"},
		{"kv verify-full", "host=h user=u password=p sslmode=verify-full", "verify-full"},
		{"kv quoted", "host=h sslmode='verify-ca'", "verify-ca"},
		{"kv absent", "host=h dbname=d", "prefer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := SSLMode(tt.dsn)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	_, err := SSLMode("postgres://u:p@h:not-a-port/d?sslmode=verify-full")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "u:p", "errors never echo credentials")
}

func TestIsVerifiedSSLMode(t *testing.T) {
	t.Parallel()
	assert.True(t, IsVerifiedSSLMode("verify-full"))
	assert.True(t, IsVerifiedSSLMode("verify-ca"))
	assert.True(t, IsVerifiedSSLMode("VERIFY-FULL"))
	for _, m := range []string{"require", "prefer", "allow", "disable", ""} {
		assert.False(t, IsVerifiedSSLMode(m), m)
	}
}

func TestOpen_RequireTLSFailsClosedBeforeDialling(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Unroutable host: if Open ever tried to dial, this would hang until the
	// deadline instead of returning ErrTLSRequired immediately.
	for _, mode := range []string{"disable", "require", "prefer", ""} {
		url := "postgres://cp_app:secret-value@192.0.2.1:5432/controlplane"
		if mode != "" {
			url += "?sslmode=" + mode
		}
		start := time.Now()
		dbh, err := Open(ctx, Config{URL: url, RequireTLS: true})
		require.Nil(t, dbh)
		require.ErrorIs(t, err, ErrTLSRequired, "mode %q", mode)
		assert.NotContains(t, err.Error(), "secret-value")
		assert.Less(t, time.Since(start), time.Second)
	}
}

func TestOpen_RejectsEmptyURLAndBadPoolBounds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, err := Open(ctx, Config{})
	require.Error(t, err)

	_, err = Open(ctx, Config{URL: "postgres://u:p@192.0.2.1/d?sslmode=disable", MaxConns: 2, MinConns: 5})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MinConns")
}

func TestSessionSetup(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "SET TIME ZONE 'UTC'; SET statement_timeout = 30000; SET lock_timeout = 5000", sessionSetup(0, 0))
	assert.Equal(t, "SET TIME ZONE 'UTC'; SET statement_timeout = 7000; SET lock_timeout = 250", sessionSetup(7*time.Second, 250*time.Millisecond))
	assert.Equal(t, "SET TIME ZONE 'UTC'", sessionSetup(-1, -1))
	assert.Equal(t, "SET TIME ZONE 'UTC'; SET statement_timeout = 1; SET lock_timeout = 5000", sessionSetup(time.Nanosecond, 0), "sub-millisecond rounds up to 1ms rather than disabling")
}
