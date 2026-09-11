//go:build integration && e2e

package e2e

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serverHeartbeat is the interval cmd/api gives stream.NewHandler. No producer
// is attached to the hub in this binary (cmd/api/wire.go says so explicitly),
// so a heartbeat comment is the only frame that can arrive — which is exactly
// what the brief allows, and what proves the connection is live rather than
// merely open.
const (
	serverHeartbeat = 15 * time.Second
	frameDeadline   = 30 * time.Second
	// headerDeadline is how quickly a real client must be able to observe the
	// response headers. It is far below the heartbeat interval on purpose: if
	// headers only appeared with the first frame, this would fail.
	headerDeadline = 5 * time.Second
)

// TestE2E_SSEStreamDeliversAcrossProcesses opens GET /v1/events/stream with a
// client that has no timeout and reads the body incrementally, the way a
// browser's EventSource does.
//
// test/load/README.md records that k6 could never observe the streaming
// headers on this endpoint. A Go client can, which is why this test belongs
// here: it distinguishes "the server does not send them" from "that client
// cannot see them".
func TestE2E_SSEStreamDeliversAcrossProcesses(t *testing.T) {
	requireEnv(t)
	srv := startAPI(t)
	srv.dumpLogs(t)
	c := newClient(t, srv)
	ctx := t.Context()

	s := c.signIn(ctx, "customer-a:mfa")

	// A streaming client: no Timeout, because a stream that is cut off by the
	// client's own deadline proves nothing about the server.
	stream := &http.Client{Timeout: 0}
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, srv.baseURL+"/v1/events/stream", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(&http.Cookie{Name: s.cookieName, Value: s.cookie})

	started := time.Now()
	resp, err := stream.Do(req)
	require.NoError(t, err, "opening the event stream")
	headerLatency := time.Since(started)
	defer func() { _ = resp.Body.Close() }()

	require.Equalf(t, http.StatusOK, resp.StatusCode, "GET /v1/events/stream")

	// --- headers a real client CAN observe ---------------------------------
	assert.Equal(t, "text/event-stream", mediaType(resp.Header.Get("Content-Type")))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "keep-alive", resp.Header.Get("Connection"))
	assert.Equal(t, "no", resp.Header.Get("X-Accel-Buffering"),
		"proxies must be told not to buffer, or the stream arrives in one lump at the end")
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"))
	assert.Lessf(t, headerLatency, headerDeadline,
		"headers must be flushed before the first frame; they took %s, and the first frame cannot arrive for %s",
		headerLatency, serverHeartbeat)

	// --- at least one frame, read incrementally ----------------------------
	deadline := frameDeadline
	if e2eBreak(t, "sse_no_frame") {
		// Shorter than the server's heartbeat interval, so no frame can
		// arrive inside the window. This is a real "nothing arrived in time"
		// condition against the real server, not a mocked one.
		deadline = time.Second
	}

	frames := make(chan string, 1)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				select {
				case frames <- string(buf[:n]):
				default:
				}
				return
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	var frame string
	select {
	case frame = <-frames:
	case err := <-readErr:
		t.Fatalf("the stream ended before delivering a frame: %v\n--- server output ---\n%s",
			err, srv.logs.String())
	case <-time.After(deadline):
		t.Fatalf("no frame arrived on /v1/events/stream within %s (heartbeat is %s); "+
			"an open socket that never delivers is not a stream\n--- server output ---\n%s",
			deadline, serverHeartbeat, srv.logs.String())
	}

	t.Logf("first frame after %s: %q", time.Since(started), frame)
	// SSE framing: a comment (the heartbeat), the retry hint the server sends
	// first so a browser reconnects at the pace it chooses, or a real event
	// block.
	isComment := strings.HasPrefix(frame, ":")
	isRetry := strings.HasPrefix(frame, "retry: ")
	isEvent := strings.Contains(frame, "data: ") && strings.Contains(frame, "event: ")
	assert.Truef(t, isComment || isRetry || isEvent,
		"the first frame is not SSE framing: %q", frame)
	assert.Truef(t, strings.HasSuffix(frame, "\n\n"),
		"an SSE frame is terminated by a blank line: %q", frame)

	// --- close cleanly and prove the server is unharmed --------------------
	require.NoError(t, resp.Body.Close())
	cancelStream()

	require.Truef(t, srv.healthy(10*time.Second),
		"the child stopped answering /v1/healthz after a stream was closed; "+
			"a leaked stream connection is a slow outage\n--- server output ---\n%s", srv.logs.String())

	// And it still serves ordinary authenticated traffic on a fresh
	// connection, which a wedged handler or an exhausted pool would not.
	me := c.get(ctx, "/v1/me", asSession(s))
	require.Equalf(t, http.StatusOK, me.Status,
		"ordinary requests must still work after the stream closed: %s", me.Body)

	// Draining what is left of a closed body must not block or panic.
	_, _ = io.Copy(io.Discard, resp.Body)
}
