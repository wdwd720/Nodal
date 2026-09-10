//go:build integration

package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
)

// The single-instance lock is the one control in this binary that is about the
// world outside the process, so it is the one that most needs to be observed
// working rather than reasoned about.
//
// `newRateLimitStore` refuses process-local counters when CP_HTTP_REPLICAS is
// not 1, which compares configuration against configuration. Its own comment
// admitted the gap: nothing there can tell whether the platform really runs one
// instance, and render.yaml sets no numInstances, so a second instance can
// appear without any file in this repository changing (F-93).
func TestIntegration_TheSecondInstanceIsRefused(t *testing.T) {
	appURL := os.Getenv("CP_TEST_DATABASE_URL")
	if appURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Two pools, because two instances are two processes: one pool would let
	// both callers share a session and the lock would be re-entrant.
	first := openPool(ctx, t, appURL)
	second := openPool(ctx, t, appURL)

	release, err := holdSingleInstanceLock(ctx, first, log)
	require.NoError(t, err, "the first instance must be able to start")
	require.NotNil(t, release)

	_, err = holdSingleInstanceLock(ctx, second, log)
	require.Error(t, err, "a second instance started while the first held the lock; every rate limit is now doubled")
	assert.ErrorIs(t, err, errSecondInstance)
	assert.Contains(t, err.Error(), "CP_RATELIMIT_BACKEND=redis",
		"the refusal must say what to do about it, not only that it happened")

	// And the lock is released, so a redeploy is not locked out by its
	// predecessor. This is the half that would turn a safety control into an
	// outage if it were wrong.
	release()
	releaseAgain, err := holdSingleInstanceLock(ctx, second, log)
	require.NoError(t, err, "the next deployment could not take the lock the previous one released")
	releaseAgain()
}

func openPool(ctx context.Context, t *testing.T, url string) *db.DB {
	t.Helper()
	pool, err := db.Open(ctx, db.Config{URL: url, AppName: "single-instance-itest", MaxConns: 2})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}
