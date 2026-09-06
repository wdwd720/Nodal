package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/strategy/ir"
)

// get issues one GET through the guarded client.
func get(t *testing.T, client *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	return client.Do(req)
}

// countingTransport records every request that actually reached the network.
type countingTransport struct {
	calls int
	inner http.RoundTripper
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls++
	return t.inner.RoundTrip(req)
}

// TestEgressAllowlistFailsClosed: a tool configured for one host cannot reach
// any other, and the refusal happens before the connection is made.
func TestEgressAllowlistFailsClosed(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	counter := &countingTransport{inner: http.DefaultTransport}
	tool := Tool{
		Code: "price-oracle", Version: 1, Effect: ir.EffectReadMarketData, Provider: "test",
		EgressHosts: []string{srv.Listener.Addr().String()},
		Timeout:     2 * time.Second, Status: ToolActive,
		MaxCallsPerMinute: 60, MaxCallsPerRun: 4,
	}
	client := NewToolHTTPClient(tool, counter)

	// The declared host is reachable.
	resp, err := get(t, client, srv.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, counter.calls)

	// Every other host is refused, and nothing is dialed.
	for _, url := range []string{
		"https://evil.example/steal",
		"http://127.0.0.1:1/",
		"https://metadata.google.internal/computeMetadata/v1/",
		"http://169.254.169.254/latest/meta-data/",
	} {
		refused, err := get(t, client, url)
		if refused != nil {
			_ = refused.Body.Close()
		}
		require.Errorf(t, err, "%s must be refused", url)
		assert.Truef(t, IsEgressRefused(err), "%s must be refused by the allowlist, got %v", url, err)
	}
	assert.Equal(t, 1, counter.calls, "a refused host must never reach the network")
}

func TestEgressEmptyAllowlistBlocksEverything(t *testing.T) {
	t.Parallel()
	counter := &countingTransport{inner: http.DefaultTransport}
	tool := Tool{Code: "unconfigured", Version: 1, Timeout: time.Second, Status: ToolActive}
	client := NewToolHTTPClient(tool, counter)
	resp, err := get(t, client, "https://example.com/")
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.True(t, IsEgressRefused(err), "a tool that declared no host may dial none")
	assert.Zero(t, counter.calls)
}

func TestEgressTransportHostMatching(t *testing.T) {
	t.Parallel()
	tr := NewEgressTransport("t@1", []string{"api.example.com", "other.example.com:8443"}, http.DefaultTransport)
	assert.True(t, tr.Allows("api.example.com"))
	assert.True(t, tr.Allows("api.example.com:443"), "a port may be added to a bare allowlist host")
	assert.True(t, tr.Allows("API.EXAMPLE.COM"), "host matching is case-insensitive")
	assert.True(t, tr.Allows("other.example.com:8443"))
	assert.False(t, tr.Allows("other.example.com"), "an allowlist entry with a port pins that port")
	assert.False(t, tr.Allows("evil.example.com"))
	assert.False(t, tr.Allows("api.example.com.evil.com"), "a suffix is not a match")
	assert.False(t, tr.Allows(""))
}

func TestToolAllowsHostMirrorsTheTransport(t *testing.T) {
	t.Parallel()
	tool := Tool{EgressHosts: []string{"api.example.com"}}
	assert.True(t, tool.AllowsHost("api.example.com"))
	assert.False(t, tool.AllowsHost("evil.example.com"))
	assert.False(t, tool.AllowsHost(""))
	assert.False(t, Tool{}.AllowsHost("anything"), "no declared hosts means no egress")
}

func TestToolStatusGatesInvocation(t *testing.T) {
	t.Parallel()
	assert.True(t, ToolActive.Invocable())
	assert.False(t, ToolDegraded.Invocable(), "a DEGRADED tool is refused, not silently retried")
	assert.False(t, ToolDisabled.Invocable())
	for _, s := range ToolStatuses() {
		assert.True(t, s.Valid())
	}
	assert.False(t, ToolStatus("SOMETIMES").Valid())
}

func TestToolEffectsAreClosedAndReadOnly(t *testing.T) {
	t.Parallel()
	// The tools table declares five effects. Every one of them is a read or a
	// model call: there is deliberately no write effect anywhere in the set.
	require.Len(t, ToolEffects(), 5)
	for _, e := range ToolEffects() {
		assert.Truef(t, e.Allowed(), "%s must be an allowed effect", e)
		assert.Falsef(t, e.Forbidden(), "%s must not be a forbidden effect", e)
	}
	for _, e := range ir.ForbiddenEffects() {
		assert.Falsef(t, IsToolEffect(e), "no tool may ever declare %s", e)
	}
	// The two runtime actions are effects of the agent, not of a tool.
	assert.False(t, IsToolEffect(ir.EffectCommitPrediction))
	assert.False(t, IsToolEffect(ir.EffectCreateTradeIntent))
}
