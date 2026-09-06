//go:build integration

package security

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db/migrate"
)

// The API-level half of this suite drives the REAL cmd/api binary over real
// HTTP against a real database. It is provisioned by
//
//	go run ./scripts/testdb -name advsec2 -export
//
// and NEVER the shared controlplane_test or controlplane: the suite writes
// intents, deposits, admin actions, sessions and operator-role grants, and
// posts to the append-only ledger, none of which can be undone. testMain
// refuses to start against either shared name.
//
// The suite must pass twice in a row against ONE database with nothing cleaned
// in between (a suite that only passes on a fresh database hides defects — see
// D-029). Every fixture is therefore idempotent and every assertion is scoped
// either to a row this run created (runToken) or to a time window this run
// opened.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")

	testPool   *pgxpool.Pool
	testDBName string

	apiBaseURL string
	apiProcess *exec.Cmd
	apiLog     bytes.Buffer
	apiLogMu   sync.Mutex

	// runToken makes every key, reason and label this run writes unique, so a
	// second run against the same database collides with nothing.
	runToken string
	// suiteStart is the instant the process started; time-windowed assertions
	// never look at rows an earlier run left behind.
	suiteStart time.Time
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run() // every API test skips with a reason
	}
	name, err := databaseName(testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "security: parse CP_TEST_DATABASE_URL:", err)
		return 1
	}
	testDBName = name
	if strings.EqualFold(name, "controlplane_test") || strings.EqualFold(name, "controlplane") {
		fmt.Fprintf(os.Stderr, "security: refusing to run against the shared database %q; "+
			"provision one with `go run ./scripts/testdb -name advsec2 -export`\n", name)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "security: migrate up:", err)
		return 1
	}
	testPool, err = pgxpool.New(ctx, testMigrateURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "security: open pool:", err)
		return 1
	}
	defer testPool.Close()

	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "security:", err)
		return 1
	}
	// Reference data (assets, the SOL/USDC instrument, the seed journal
	// posting for customer-a) comes from the repository's own seeder, which is
	// idempotent. Identities are created by the dev IdP on first login.
	if out, serr := runSeed(ctx, root); serr != nil {
		fmt.Fprintf(os.Stderr, "security: seed: %v\n%s\n", serr, out)
		return 1
	}

	binDir, err := os.MkdirTemp("", "advsec-api-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "security: temp dir:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(binDir) }()

	bin := filepath.Join(binDir, "api-advsec"+exeSuffix())
	if out, berr := buildAPI(ctx, root, bin); berr != nil {
		fmt.Fprintf(os.Stderr, "security: build cmd/api: %v\n%s\n", berr, out)
		return 1
	}

	stop, err := startAPI(ctx, root, bin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "security: start api: %v\n%s\n", err, apiLogText())
		return 1
	}
	defer stop()

	runToken = strings.ReplaceAll(fmt.Sprintf("%d", time.Now().UnixNano()), " ", "")
	suiteStart = time.Now().UTC()

	code := m.Run()
	if code != 0 {
		fmt.Fprintln(os.Stderr, "security: api stderr follows\n"+tailLines(apiLogText(), 80))
	}
	return code
}

// requireAPI skips a test when the harness could not be brought up, with the
// reason, rather than passing vacuously.
func requireAPI(t *testing.T) {
	t.Helper()
	if apiBaseURL == "" {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; " +
			"provision with `go run ./scripts/testdb -name advsec2 -export`")
	}
}

func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, serr := os.Stat(filepath.Join(dir, "go.mod")); serr == nil {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("security: go.mod not found above %s", wd)
		}
	}
}

func exeSuffix() string {
	if os.PathSeparator == '\\' {
		return ".exe"
	}
	return ""
}

func runSeed(ctx context.Context, root string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "run", "./scripts/seed")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CP_ENV=LOCAL", "CP_DATABASE_APP_URL="+testAppURL)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// buildAPI builds cmd/api into the suite's own temporary directory. It must
// never write bin/api.exe: other suites and other people run that binary.
func buildAPI(ctx context.Context, root, out string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/api")
	cmd.Dir = root
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// freePort asks the kernel for an unused loopback port and returns it. The
// listener is closed before the child binds; the window is small and the
// alternative (a fixed port) collides with the other suites running here.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		_ = l.Close()
		return 0, fmt.Errorf("security: listener address is %T, not *net.TCPAddr", l.Addr())
	}
	return addr.Port, l.Close()
}

func startAPI(ctx context.Context, root, bin string) (func(), error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	cmd := exec.Command(bin) //nolint:gosec // the path is built by this test
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"CP_ENV=LOCAL",
		"CP_HTTP_ADDR=127.0.0.1:"+fmt.Sprint(port),
		"CP_HTTP_PUBLIC_BASE_URL="+base,
		"CP_DATABASE_APP_URL="+testAppURL,
		"CP_DATABASE_MIGRATE_URL="+testMigrateURL,
		"CP_AUTH_MODE=dev",
		"CP_AUTH_COOKIE_SECURE=false",
		"CP_API_SETTLEMENT_CHAIN=solana-devnet",
		"CP_API_SETTLEMENT_MINT=4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
		// The transport rate limits are raised, not disabled: this suite
		// authenticates and probes far more often than a human would, and the
		// stock 30/1m auth limit would answer 429 where an authorization
		// answer is what is being measured. They stay finite and every probe
		// asserts it did not receive 429 (assertNotThrottled), so a limit that
		// does bite fails the suite loudly instead of mis-measuring it.
		"CP_API_RATE_LIMIT_GENERAL=200000/1m",
		"CP_API_RATE_LIMIT_AUTH=200000/1m",
		"CP_API_RATE_LIMIT_QUOTE=200000/1m",
		"CP_API_RATE_LIMIT_COMMAND=200000/1m",
	)
	cmd.Stdout = &lockedWriter{}
	cmd.Stderr = &lockedWriter{}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	apiProcess = cmd

	stop := func() {
		if apiProcess == nil || apiProcess.Process == nil {
			return
		}
		// Kill only this child, by PID: other agents are running here.
		_ = apiProcess.Process.Kill()
		_, _ = apiProcess.Process.Wait()
		apiProcess = nil
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			stop()
			return nil, ctx.Err()
		}
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/healthz", nil)
		if rerr != nil {
			stop()
			return nil, rerr
		}
		resp, herr := http.DefaultClient.Do(req)
		if herr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				apiBaseURL = base
				return stop, nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	stop()
	return nil, errors.New("security: api did not become healthy within 60s")
}

// lockedWriter collects the child's output for a failure report without
// interleaving writes from its two pipes.
type lockedWriter struct{}

func (*lockedWriter) Write(p []byte) (int, error) {
	apiLogMu.Lock()
	defer apiLogMu.Unlock()
	return apiLog.Write(p)
}

func apiLogText() string {
	apiLogMu.Lock()
	defer apiLogMu.Unlock()
	return apiLog.String()
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(u.Path, "/"), nil
}

// TestHarness_RefusesTheSharedDatabase proves the refusal above is a rule and
// not a comment: the same predicate testMain uses is asserted here, and the
// database actually in use is asserted not to be a shared one.
func TestHarness_RefusesTheSharedDatabase(t *testing.T) {
	requireAPI(t)
	for _, shared := range []string{"controlplane", "controlplane_test", "CONTROLPLANE_TEST"} {
		require.True(t, isSharedDatabase(shared), "%q must be recognized as a shared database", shared)
	}
	require.False(t, isSharedDatabase(testDBName), "the suite is running against %q", testDBName)
	require.True(t, strings.HasPrefix(testDBName, "controlplane_test_"),
		"expected an isolated controlplane_test_<name> database, got %q", testDBName)
}

func isSharedDatabase(name string) bool {
	return strings.EqualFold(name, "controlplane_test") || strings.EqualFold(name, "controlplane")
}
