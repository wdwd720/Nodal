//go:build integration && e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// healthDeadline bounds how long a test waits for the child to answer
// GET /v1/healthz. A cold binary on Windows opening a pgx pool is comfortably
// inside this; anything slower is a failure worth seeing, not worth waiting
// out.
const healthDeadline = 45 * time.Second

// apiServer is a real `cmd/api` child process. Every test gets its own so a
// failure prints only its own server's log, and so one test's rate-limit
// budget, session store state or crash cannot reach another.
type apiServer struct {
	baseURL string
	port    int
	cmd     *exec.Cmd
	logs    *syncBuffer

	done     chan struct{} // closed when Wait returned
	waitErr  error
	waitOnce sync.Once
}

// startAPI builds nothing (TestMain already did) and starts the API binary on
// a port the kernel just handed us. It returns only after GET /v1/healthz has
// answered 200; a child that dies during startup fails the test with its
// captured output, because a black-box failure with no server log is close to
// useless.
func startAPI(t *testing.T) *apiServer {
	t.Helper()
	requireEnv(t)

	port := freePort(t)
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	logs := &syncBuffer{}
	cmd := exec.Command(apiBinary) //nolint:gosec // apiBinary is built by TestMain into a temp dir
	cmd.Dir = repoRoot
	cmd.Stdout = logs
	cmd.Stderr = logs
	cmd.Env = append(childEnv(),
		"CP_ENV=LOCAL",
		"CP_HTTP_ADDR="+addr,
		"CP_HTTP_PUBLIC_BASE_URL=http://"+addr,
		"CP_DATABASE_APP_URL="+testAppURL,
		"CP_DATABASE_MIGRATE_URL="+testMigrateURL,
		"CP_AUTH_MODE=dev",
		"CP_AUTH_COOKIE_SECURE=false",
		"CP_API_SETTLEMENT_CHAIN="+settlementChain,
		"CP_API_SETTLEMENT_MINT="+settlementMint,
	)

	s := &apiServer{
		baseURL: "http://" + addr,
		port:    port,
		cmd:     cmd,
		logs:    logs,
		done:    make(chan struct{}),
	}
	require.NoErrorf(t, cmd.Start(), "starting %s", apiBinary)
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()

	t.Cleanup(func() { s.stop(t) })

	s.waitHealthy(t)
	return s
}

// waitHealthy blocks until GET /v1/healthz answers 200, the child exits, or
// the deadline passes.
func (s *apiServer) waitHealthy(t *testing.T) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(healthDeadline)
	var last string
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			t.Fatalf("api child exited during startup (%v)\n--- server output ---\n%s",
				s.waitErr, s.logs.String())
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/v1/healthz", nil)
		if err != nil {
			cancel()
			t.Fatalf("build healthz request: %v", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			code := resp.StatusCode
			_ = resp.Body.Close()
			cancel()
			if code == http.StatusOK {
				return
			}
			last = fmt.Sprintf("status %d", code)
		} else {
			cancel()
			last = err.Error()
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("api child never answered GET %s/v1/healthz with 200 within %s (last: %s)\n"+
		"--- server output ---\n%s", s.baseURL, healthDeadline, last, s.logs.String())
}

// stop kills this test's child and confirms it actually exited: Wait must
// return, and the port must stop answering. Only this PID is ever touched —
// other agents' servers on other ports are none of our business.
func (s *apiServer) stop(t *testing.T) {
	t.Helper()
	s.waitOnce.Do(func() {
		select {
		case <-s.done:
			// Already gone; the exit is confirmed below.
		default:
			if s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
		}
		select {
		case <-s.done:
		case <-time.After(20 * time.Second):
			t.Errorf("api child (pid %d) did not exit within 20s of being killed", s.pid())
			return
		}
		if s.cmd.ProcessState == nil {
			t.Errorf("api child (pid %d) was reaped without a process state", s.pid())
			return
		}
		if !s.cmd.ProcessState.Exited() {
			t.Errorf("api child (pid %d) was reaped but reports Exited()==false: %v",
				s.pid(), s.cmd.ProcessState)
		}
		// A reaped PID is not proof the listener is gone; ask the port.
		if s.healthy(2 * time.Second) {
			t.Errorf("api child (pid %d) was reaped but %s/v1/healthz still answers 200",
				s.pid(), s.baseURL)
		}
	})
}

// healthy reports whether GET /v1/healthz answers 200 right now.
func (s *apiServer) healthy(timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/v1/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

func (s *apiServer) pid() int {
	if s.cmd.Process == nil {
		return -1
	}
	return s.cmd.Process.Pid
}

// dumpLogs prints the child's captured output. Tests call it from a cleanup
// that runs only when the test failed, so a green run stays quiet.
func (s *apiServer) dumpLogs(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- api child (pid %d, %s) output ---\n%s", s.pid(), s.baseURL, s.logs.String())
		}
	})
}

// freePort asks the kernel for an unused loopback port and gives it straight
// back. Two agents racing for the same port is possible in principle; in
// practice the kernel does not hand out the same ephemeral port twice in the
// window between Close and the child's Listen, and the alternative — a fixed
// port — is guaranteed to collide.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "probing for a free port")
	addr, ok := l.Addr().(*net.TCPAddr)
	require.Truef(t, ok, "loopback listener reported a %T, not a *net.TCPAddr", l.Addr())
	require.NoError(t, l.Close())
	return addr.Port
}

// syncBuffer is an io.Writer safe for the child's two pipes plus a test
// goroutine reading it for a failure message.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
