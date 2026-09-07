// Command restoredrill runs the local backup → restore → boot → consistency
// dry-run required by PART 141/219 ("a backup that was never restored is not a
// proven backup") against the LOCAL docker-compose Postgres.
//
//	go run ./scripts/restoredrill [-container cp-postgres] [-keep] [-out dist/restore-drill.json]
//
// Steps:
//  1. provision a fresh, fully migrated source database and seed a small,
//     balanced ledger fixture as the application role;
//  2. back it up with pg_dump (custom format) through `docker exec`, hashing
//     the archive;
//  3. restore the archive into a new empty database with pg_restore;
//  4. "boot": verify migration checksums on the restored database and that
//     its version equals the source's;
//  5. dry-run reconciliation: recompute ledger balances from journal entries
//     on the restored copy, compare row counts of every table with the source,
//     and re-hash the journal transactions on both sides;
//  6. write a JSON report and exit non-zero on any mismatch.
//
// The production procedure (RDS snapshot/PITR restore, application pointed at
// the restored instance, reconciliation against external truth) is documented
// in docs/operations/BACKUP_RESTORE.md; this drill proves the local half of
// it end to end.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/testkit/localdb"
)

type report struct {
	StartedAt        time.Time         `json:"started_at"`
	FinishedAt       time.Time         `json:"finished_at"`
	SourceDB         string            `json:"source_db"`
	RestoredDB       string            `json:"restored_db"`
	DumpBytes        int               `json:"dump_bytes"`
	DumpSHA256       string            `json:"dump_sha256"`
	SourceVersion    int64             `json:"source_version"`
	RestoredVersion  int64             `json:"restored_version"`
	VerifyOK         bool              `json:"migration_verify_ok"`
	RowCountsMatch   bool              `json:"row_counts_match"`
	RowCountDiffs    map[string][2]int `json:"row_count_diffs,omitempty"`
	BalancesMatch    bool              `json:"ledger_balances_recomputed_match"`
	JournalHashMatch bool              `json:"journal_hash_match"`
	SourceJournal    string            `json:"source_journal_hash"`
	RestoredJournal  string            `json:"restored_journal_hash"`
	Steps            []string          `json:"steps"`
	OK               bool              `json:"ok"`
}

func main() {
	os.Exit(run())
}

func run() int {
	var (
		container = flag.String("container", "cp-postgres", "docker container running Postgres")
		admin     = flag.String("admin", localdb.DefaultAdminDSN, "admin DSN (LOCAL only)")
		keep      = flag.Bool("keep", false, "keep the source and restored databases")
		out       = flag.String("out", filepath.Join("dist", "restore-drill.json"), "report path")
	)
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	rep := &report{StartedAt: time.Now().UTC(), RowCountDiffs: map[string][2]int{}}
	step := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		rep.Steps = append(rep.Steps, s)
		fmt.Println("restoredrill:", s)
	}
	fail := func(err error) int {
		step("FAILED: %v", err)
		rep.FinishedAt = time.Now().UTC()
		writeReport(*out, rep)
		return 1
	}

	// 1. Provision + seed.
	src, err := localdb.Provision(ctx, "drill_src", localdb.Options{AdminDSN: *admin, Migrate: true})
	if err != nil {
		return fail(err)
	}
	rep.SourceDB = src.Name
	if !*keep {
		defer func() { _ = localdb.Drop(context.Background(), "drill_src", *admin) }()
	}
	if err := seed(ctx, src.AppDSN); err != nil {
		return fail(fmt.Errorf("seed: %w", err))
	}
	step("provisioned and seeded %s", src.Name)

	// 2. Backup.
	dump, err := dockerOut(ctx, *container, "pg_dump", "-U", "cp_admin", "-Fc", src.Name)
	if err != nil {
		return fail(fmt.Errorf("pg_dump: %w", err))
	}
	sum := sha256.Sum256(dump)
	rep.DumpBytes, rep.DumpSHA256 = len(dump), hex.EncodeToString(sum[:])
	step("backup: %d bytes sha256=%s", rep.DumpBytes, rep.DumpSHA256[:16])

	// 3. Restore into an empty database.
	dst, err := localdb.CreateEmpty(ctx, "drill_restored", *admin)
	if err != nil {
		return fail(err)
	}
	rep.RestoredDB = dst.Name
	if !*keep {
		defer func() { _ = localdb.Drop(context.Background(), "drill_restored", *admin) }()
	}
	if _, err := dockerIn(ctx, dump, *container, "pg_restore", "-U", "cp_admin", "-d", dst.Name, "--exit-on-error"); err != nil {
		return fail(fmt.Errorf("pg_restore: %w", err))
	}
	step("restored into %s", dst.Name)

	// 4. Boot: migration checksums and version.
	if rep.SourceVersion, err = migrate.Version(ctx, src.MigrateDSN); err != nil {
		return fail(err)
	}
	if rep.RestoredVersion, err = migrate.Version(ctx, dst.MigrateDSN); err != nil {
		return fail(fmt.Errorf("restored version: %w", err))
	}
	if err := migrate.Verify(ctx, dst.MigrateDSN); err != nil {
		return fail(fmt.Errorf("restored verify: %w", err))
	}
	rep.VerifyOK = rep.SourceVersion == rep.RestoredVersion
	step("boot: version source=%d restored=%d verify=ok", rep.SourceVersion, rep.RestoredVersion)

	// 5. Dry-run reconciliation.
	srcCounts, err := rowCounts(ctx, src.AdminDSN)
	if err != nil {
		return fail(err)
	}
	dstCounts, err := rowCounts(ctx, dst.AdminDSN)
	if err != nil {
		return fail(err)
	}
	rep.RowCountsMatch = true
	for tbl, n := range srcCounts {
		if m := dstCounts[tbl]; m != n {
			rep.RowCountsMatch = false
			rep.RowCountDiffs[tbl] = [2]int{n, m}
		}
	}
	for tbl, m := range dstCounts {
		if _, ok := srcCounts[tbl]; !ok {
			rep.RowCountsMatch = false
			rep.RowCountDiffs[tbl] = [2]int{0, m}
		}
	}
	drift, err := balanceDrift(ctx, dst.AdminDSN)
	if err != nil {
		return fail(err)
	}
	rep.BalancesMatch = drift == 0
	if rep.SourceJournal, err = journalHash(ctx, src.AdminDSN); err != nil {
		return fail(err)
	}
	if rep.RestoredJournal, err = journalHash(ctx, dst.AdminDSN); err != nil {
		return fail(err)
	}
	rep.JournalHashMatch = rep.SourceJournal == rep.RestoredJournal
	step("reconciliation dry-run: tables=%d rowcounts_match=%v balance_drift_accounts=%d journal_hash_match=%v",
		len(srcCounts), rep.RowCountsMatch, drift, rep.JournalHashMatch)

	rep.OK = rep.VerifyOK && rep.RowCountsMatch && rep.BalancesMatch && rep.JournalHashMatch
	rep.FinishedAt = time.Now().UTC()
	writeReport(*out, rep)
	if !rep.OK {
		fmt.Println("restoredrill: FAILED")
		return 1
	}
	fmt.Printf("restoredrill: OK (%s)\n", rep.FinishedAt.Sub(rep.StartedAt).Round(time.Millisecond))
	return 0
}

// seed writes a small balanced ledger fixture as the application role.
func seed(ctx context.Context, appDSN string) error {
	conn, err := pgx.Connect(ctx, appDSN)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stmts := []string{
		`INSERT INTO users (id, idp_issuer, idp_subject, status) VALUES ('00000000-0000-7000-8000-000000000001','drill','u1','ACTIVE')`,
		`INSERT INTO accounts (id, owner_user_id, kind, status) VALUES ('00000000-0000-7000-8000-000000000010','00000000-0000-7000-8000-000000000001','CUSTOMER','ACTIVE')`,
		`INSERT INTO assets (id, chain, mint_address, kind, symbol, name, decimals, is_stablecoin, peg_currency, risk_class, status, value_domain)
		 VALUES ('00000000-0000-7000-8000-0000000000aa','solana-drill','EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v','SPL_TOKEN','USDC','USD Coin',6,true,'USD','SETTLEMENT','ACTIVE','SELF_CUSTODIAL_CRYPTO')`,
		`INSERT INTO ledger_accounts (id, owner_type, owner_id, code, asset_id, normal_side) VALUES
		 ('00000000-0000-7000-8000-0000000000a1','CUSTOMER','00000000-0000-7000-8000-000000000010','WALLET','00000000-0000-7000-8000-0000000000aa','DEBIT'),
		 ('00000000-0000-7000-8000-0000000000a2','CUSTOMER','00000000-0000-7000-8000-000000000010','CAPITAL','00000000-0000-7000-8000-0000000000aa','CREDIT')`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s[:40], err)
		}
	}
	for i := 1; i <= 25; i++ {
		txID := fmt.Sprintf("00000000-0000-7000-8000-0000000000%02x", 0x10+i)
		if _, err := tx.Exec(ctx, `INSERT INTO journal_transactions (id, kind, idempotency_key, reference_type, reference_id, effective_at, posted_by_actor_type, posted_by_actor_id, content_hash)
			VALUES ($1,'FUNDING_SETTLED',$2,'deposit',$3,now(),'SYSTEM','drill',decode('00','hex'))`, txID, fmt.Sprintf("drill:%d", i), fmt.Sprintf("%d", i)); err != nil {
			return err
		}
		amount := int64(1_000_000 * i)
		if _, err := tx.Exec(ctx, `INSERT INTO journal_entries (id, transaction_id, seq, ledger_account_id, asset_id, side, quantity) VALUES
			(gen_random_uuid(),$1,0,'00000000-0000-7000-8000-0000000000a1','00000000-0000-7000-8000-0000000000aa','DEBIT',$2),
			(gen_random_uuid(),$1,1,'00000000-0000-7000-8000-0000000000a2','00000000-0000-7000-8000-0000000000aa','CREDIT',$2)`, txID, amount); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func rowCounts(ctx context.Context, adminDSN string) (map[string]int, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()
	rows, err := conn.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	out := map[string]int{}
	for _, t := range tables {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{t}.Sanitize()).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

// balanceDrift returns the number of ledger accounts whose stored balance
// differs from the sum of their entries (normal-side signed).
func balanceDrift(ctx context.Context, adminDSN string) (int, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	err = conn.QueryRow(ctx, `
		SELECT count(*) FROM ledger_accounts a
		JOIN ledger_balances b ON b.ledger_account_id = a.id
		WHERE b.balance <> coalesce((
			SELECT sum(CASE WHEN (e.side = 'DEBIT') = (a.normal_side = 'DEBIT') THEN e.quantity ELSE -e.quantity END)
			FROM journal_entries e WHERE e.ledger_account_id = a.id), 0)`).Scan(&n)
	return n, err
}

// journalHash hashes every journal transaction and entry in a deterministic order.
func journalHash(ctx context.Context, adminDSN string) (string, error) {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close(ctx) }()
	var h string
	err = conn.QueryRow(ctx, `
		SELECT encode(sha256(convert_to(coalesce(string_agg(line, E'\n' ORDER BY line), ''), 'UTF8')), 'hex') FROM (
			SELECT t.id::text || '|' || t.kind || '|' || t.idempotency_key || '|' || e.seq || '|' || e.ledger_account_id::text || '|' || e.side || '|' || e.quantity::text AS line
			FROM journal_transactions t JOIN journal_entries e ON e.transaction_id = t.id) x`).Scan(&h)
	return h, err
}

func dockerOut(ctx context.Context, container string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", container}, args...)...) // #nosec G204 -- local restore drill: fixed "docker exec -i" argument shape, container from this tool's -container flag; no shell is involved
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func dockerIn(ctx context.Context, stdin []byte, container string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"exec", "-i", container}, args...)...) // #nosec G204 -- local restore drill: fixed "docker exec -i" argument shape, container from this tool's -container flag; no shell is involved
	var stdout, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func writeReport(path string, rep *report) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		fmt.Fprintln(os.Stderr, "restoredrill: report dir:", err)
		return
	}
	keys := make([]string, 0, len(rep.RowCountDiffs))
	for k := range rep.RowCountDiffs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "restoredrill: encode report:", err)
		return
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "restoredrill: write report:", err)
		return
	}
	fmt.Println("restoredrill: report written to", path)
}
