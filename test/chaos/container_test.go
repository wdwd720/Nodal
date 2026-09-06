//go:build integration && chaos

package chaos

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Container names on the LOCAL docker-compose stack. Overridable so the suite
// can run against a differently-named stack without editing tests.
const (
	envPostgresContainer = "CP_TEST_POSTGRES_CONTAINER"
	envRedpandaContainer = "CP_TEST_REDPANDA_CONTAINER"
	envMinIOContainer    = "CP_TEST_MINIO_CONTAINER"
)

func containerName(env, def string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	return def
}

// PostgresContainer, RedpandaContainer and MinIOContainer name the containers
// the chaos suite is allowed to fault.
func PostgresContainer() string { return containerName(envPostgresContainer, "cp-postgres") }
func RedpandaContainer() string { return containerName(envRedpandaContainer, "cp-redpanda") }
func MinIOContainer() string    { return containerName(envMinIOContainer, "cp-minio") }

// fault controls one container. It is deliberately conservative: the only
// operations are pause/unpause (SIGSTOP of every process in the container,
// which is what an unresponsive node or a network black hole looks like to a
// client) and stop/start. Restoration is idempotent, registered in Cleanup
// before the fault is applied, and verified — leaving a shared container
// paused would break every other suite on this host.
type fault struct {
	t         *testing.T
	container string

	mu      sync.Mutex
	paused  bool
	stopped bool
}

// newFault returns a controller for the named container, skipping the test
// when docker or the container is unavailable, and registers restoration.
func newFault(t *testing.T, container string) *fault {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker not on PATH; cannot fault %s", container)
	}
	if err := exec.Command("docker", "inspect", container).Run(); err != nil {
		t.Skipf("container %s not found; cannot fault it", container)
	}
	f := &fault{t: t, container: container}
	// Registered BEFORE any fault is applied so a panic between the pause and
	// the deferred restore still restores the container.
	t.Cleanup(f.Restore)
	return f
}

// Pause SIGSTOPs the container. The client sees an open TCP connection that
// never answers, which is the interesting failure: a closed port is easy and
// every client handles it, a silent one is what actually breaks producers.
func (f *fault) Pause() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.paused {
		return
	}
	out, err := exec.Command("docker", "pause", f.container).CombinedOutput()
	require.NoErrorf(f.t, err, "docker pause %s: %s", f.container, out)
	f.paused = true
	f.t.Logf("chaos: paused %s", f.container)
}

// Unpause resumes a paused container. Safe to call when not paused.
func (f *fault) Unpause() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unpauseLocked()
}

func (f *fault) unpauseLocked() {
	if !f.paused {
		return
	}
	// Tolerant: restoring shared infrastructure must never be the reason a
	// run fails, and the body may already have unpaused.
	_ = exec.Command("docker", "unpause", f.container).Run()
	f.paused = false
}

// Stop terminates the container's processes (SIGTERM then SIGKILL). Unlike
// Pause this closes the listening socket, so clients see connection refused
// rather than a silent stall — a different and also worth-testing failure.
func (f *fault) Stop() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return
	}
	out, err := exec.Command("docker", "stop", "-t", "1", f.container).CombinedOutput()
	require.NoErrorf(f.t, err, "docker stop %s: %s", f.container, out)
	f.stopped = true
	f.t.Logf("chaos: stopped %s", f.container)
}

// Start restarts a stopped container. Safe to call when not stopped.
func (f *fault) Start() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startLocked()
}

func (f *fault) startLocked() {
	if !f.stopped {
		return
	}
	_ = exec.Command("docker", "start", f.container).Run()
	f.stopped = false
}

// Restore returns the container to a running state and waits for docker to
// report it healthy. It is idempotent and never fails the test on the restore
// path itself — but it does fail the test if the container never comes back,
// because a suite that leaves the stack broken must say so loudly.
func (f *fault) Restore() {
	f.mu.Lock()
	f.unpauseLocked()
	f.startLocked()
	f.mu.Unlock()
	if err := waitHealthy(f.container, 3*time.Minute); err != nil {
		f.t.Errorf("chaos: %s did not return to healthy after the test: %v", f.container, err)
		return
	}
	f.t.Logf("chaos: %s restored to healthy", f.container)
}

// health returns the container's docker health status, or "running"/"exited"
// when the image declares no healthcheck.
func health(container string) (string, error) {
	out, err := exec.Command("docker", "inspect", "-f",
		"{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", container).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", container, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// waitHealthy blocks until the container reports healthy (or running, for an
// image with no healthcheck).
func waitHealthy(container string, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		s, err := health(container)
		if err != nil {
			return err
		}
		last = s
		if s == "healthy" || s == "running" {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("%s still %q after %s", container, last, within)
}

// requireHealthyStack asserts every container the suite may fault is healthy
// before a test starts. Running a chaos test against an already-broken stack
// produces a failure that means nothing.
func requireHealthyStack(t *testing.T, containers ...string) {
	t.Helper()
	for _, c := range containers {
		s, err := health(c)
		require.NoErrorf(t, err, "cannot inspect %s", c)
		require.Containsf(t, []string{"healthy", "running"}, s,
			"%s is %q before the test; chaos tests need a healthy stack to mean anything", c, s)
	}
}
