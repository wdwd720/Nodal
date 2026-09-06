//go:build integration && e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nodal/controlplane/internal/db"
)

// The suite runs against an isolated database provisioned by
//
//	go run ./scripts/testdb -name e2e -export
//
// never the shared controlplane_test or controlplane: it starts a real API
// binary against that database and writes real rows through it.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")

	testDB     *db.DB
	testDBName string

	repoRoot  string
	apiBinary string

	// runToken namespaces every fixture this process creates so the suite
	// can be run repeatedly against one database without cleaning it.
	runToken string

	// seeded identifiers, read from the database after scripts/seed ran.
	seeded seedFixture
)

// seedFixture is what `go run ./scripts/seed` leaves behind. The identifiers
// are read from the database rather than from the API so that a test asserting
// on an API response is not checking the API against itself.
type seedFixture struct {
	CustomerAAccountID string
	CustomerAUserID    string
	CustomerBAccountID string
	CustomerBUserID    string
	AdminUserID        string
	USDCAssetID        string
	InstrumentID       string
}

// Settlement asset of the local seed. cmd/api refuses to build its buying
// power and holdings ports without it, and answers 422 UNSUPPORTED on those
// routes when it is absent.
const (
	settlementChain = "solana-devnet"
	settlementMint  = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run() // every test skips with a reason
	}

	name, err := databaseName(testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: parse CP_TEST_DATABASE_URL:", err)
		return 1
	}
	testDBName = name
	// A suite that starts a server against a database and writes to it must
	// never be pointed at a shared one. This is a refusal, not a skip: a
	// silent skip here would look like a pass.
	if strings.EqualFold(name, "controlplane_test") || strings.EqualFold(name, "controlplane") {
		fmt.Fprintf(os.Stderr, "e2e: refusing to run against the shared database %q; "+
			"provision one with `go run ./scripts/testdb -name e2e -export`\n", name)
		return 1
	}

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	repoRoot = root
	runToken = newToken()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// The API binary does not migrate; scripts/testdb already did.
	// scripts/seed is idempotent and is what makes the SEED posting,
	// the dev identities and the settlement asset exist. cmd/api refuses to
	// start without the settlement asset, so this must run first.
	if err := runSeed(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: seed:", err)
		return 1
	}

	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "e2e-suite", MaxConns: 16})
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: open app pool:", err)
		return 1
	}
	defer testDB.Close()

	if seeded, err = readSeedFixture(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: read seed fixture:", err)
		return 1
	}

	tmp, err := os.MkdirTemp("", "cp-e2e-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: temp dir:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	apiBinary = filepath.Join(tmp, "api"+exeSuffix())
	if err := buildAPI(ctx, apiBinary); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: build cmd/api:", err)
		return 1
	}

	return m.Run()
}

// requireEnv skips with an explicit reason when the suite was not given a
// database, exactly as internal/httpapi/integration_test.go does.
func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil || apiBinary == "" {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; " +
			"provision with `go run ./scripts/testdb -name e2e -export`")
	}
}

// runSeed executes scripts/seed against this suite's database with os/exec.
// The seed refuses any environment other than LOCAL/DEV/TEST and any
// non-local host, so it cannot be aimed at a shared database by accident.
func runSeed(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "go", "run", "./scripts/seed")
	cmd.Dir = repoRoot
	cmd.Env = append(childEnv(),
		"CP_ENV=LOCAL",
		"CP_DATABASE_APP_URL="+testAppURL,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go run ./scripts/seed: %w\n%s", err, out)
	}
	return nil
}

func buildAPI(ctx context.Context, out string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/api")
	cmd.Dir = repoRoot
	cmd.Env = childEnv()
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build ./cmd/api: %w\n%s", err, b)
	}
	if _, err := os.Stat(out); err != nil {
		return fmt.Errorf("built binary is missing: %w", err)
	}
	return nil
}

// readSeedFixture resolves the seeded identifiers straight from the database.
// Reading them from the API instead would make several tests check the API
// against its own answer.
func readSeedFixture(ctx context.Context) (seedFixture, error) {
	var f seedFixture
	q := testDB.Pool()

	const accountBySubject = `
		SELECT a.id::text, u.id::text
		FROM accounts a JOIN users u ON u.id = a.owner_user_id
		WHERE u.idp_issuer = 'devidp' AND u.idp_subject = $1`
	if err := q.QueryRow(ctx, accountBySubject, "dev:customer-a").
		Scan(&f.CustomerAAccountID, &f.CustomerAUserID); err != nil {
		return f, fmt.Errorf("customer-a account: %w", err)
	}
	if err := q.QueryRow(ctx, accountBySubject, "dev:customer-b").
		Scan(&f.CustomerBAccountID, &f.CustomerBUserID); err != nil {
		return f, fmt.Errorf("customer-b account: %w", err)
	}
	var ignored string
	if err := q.QueryRow(ctx, accountBySubject, "dev:admin").
		Scan(&ignored, &f.AdminUserID); err != nil {
		return f, fmt.Errorf("admin account: %w", err)
	}
	if err := q.QueryRow(ctx,
		`SELECT id::text FROM assets WHERE chain = $1 AND mint_address = $2`,
		settlementChain, settlementMint).Scan(&f.USDCAssetID); err != nil {
		return f, fmt.Errorf("usdc asset: %w", err)
	}
	if err := q.QueryRow(ctx,
		`SELECT id::text FROM instruments WHERE canonical_name = 'SOL/USDC'`).
		Scan(&f.InstrumentID); err != nil {
		return f, fmt.Errorf("sol/usdc instrument: %w", err)
	}
	return f, nil
}

// childEnv is the parent environment with every CP_* variable removed, so a
// child process sees exactly the configuration a test hands it and nothing
// leaked from the developer's shell. The rest of the environment (PATH,
// SystemRoot, TEMP, GOCACHE, …) is kept because `go build` and the runtime
// need it.
func childEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if strings.HasPrefix(kv, "CP_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(u.Path, "/"), nil
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// newToken returns a short random hex string used to namespace fixtures.
func newToken() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; a time-based
		// fallback keeps the suite runnable rather than panicking here.
		return fmt.Sprintf("%012x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
