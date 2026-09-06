//go:build integration

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
)

// TestIntegration_ServeAndDrain runs the whole composition root against the
// isolated database: it loads configuration, opens the pool, constructs every
// dependency, serves the v1 surface, and then drains cleanly when its context
// is cancelled — which is exactly what SIGTERM does in production.
func TestIntegration_ServeAndDrain(t *testing.T) {
	appURL := os.Getenv("CP_TEST_DATABASE_URL")
	if appURL == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; provision one with `go run ./scripts/testdb -name httpapi`")
	}
	addr := freeAddr(t)
	env := map[string]string{
		"CP_ENV":                  "LOCAL",
		"CP_HTTP_ADDR":            addr,
		"CP_DATABASE_APP_URL":     appURL,
		"CP_DATABASE_MIGRATE_URL": os.Getenv("CP_TEST_MIGRATE_DATABASE_URL"),
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { _ = devNull.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- run(ctx, config.LookupFromMap(env), devNull) }()

	base := "http://" + addr
	client := &http.Client{Timeout: 5 * time.Second}
	waitReady(t, client, base)

	// The unauthenticated surface answers, and everything else fails closed.
	require.Equal(t, http.StatusOK, get(t, client, base+"/v1/healthz"))
	require.Equal(t, http.StatusOK, get(t, client, base+"/v1/readyz"))
	require.Equal(t, http.StatusOK, get(t, client, base+"/v1/version"))
	require.Equal(t, http.StatusUnauthorized, get(t, client, base+"/v1/accounts"))
	require.Equal(t, http.StatusUnauthorized, get(t, client, base+"/v1/admin/gates"))
	require.Equal(t, http.StatusNotFound, get(t, client, base+"/v1/nope"))

	cancel()
	select {
	case code := <-exit:
		assert.Equal(t, exitOK, code, "a cancelled context must drain cleanly")
	case <-time.After(40 * time.Second):
		t.Fatal("the server did not shut down within the drain window")
	}

	// The listener is closed.
	res, err := client.Get(base + "/v1/healthz")
	if err == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}
	assert.Error(t, err, "the listener must be closed after shutdown")
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

func waitReady(t *testing.T, client *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get(base + "/v1/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the server never became reachable")
}

func get(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	res, err := client.Get(url)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}
