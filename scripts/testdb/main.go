// Command testdb provisions an isolated, fully migrated PostgreSQL database for
// integration tests on the LOCAL docker-compose stack and prints the
// environment variables tests expect.
//
//	go run ./scripts/testdb -name ledger
//	# → CP_TEST_DATABASE_URL=... CP_TEST_MIGRATE_DATABASE_URL=...
//
// It refuses to run unless the admin DSN points at 127.0.0.1/localhost, so it
// can never touch a shared environment (see internal/testkit/localdb).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/nodal/controlplane/internal/testkit/localdb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "testdb:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		name      = flag.String("name", "", "suffix for the database name (controlplane_test_<name>)")
		admin     = flag.String("admin", envOr("CP_TEST_ADMIN_DATABASE_URL", localdb.DefaultAdminDSN), "admin DSN (LOCAL only)")
		drop      = flag.Bool("drop", false, "drop the database instead of creating it")
		export    = flag.Bool("export", false, "print `export` statements for bash")
		noMigrate = flag.Bool("no-migrate", false, "create the database but do not apply migrations")
	)
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if *drop {
		if err := localdb.Drop(ctx, *name, *admin); err != nil {
			return err
		}
		fmt.Printf("dropped controlplane_test_%s\n", *name)
		return nil
	}
	d, err := localdb.Provision(ctx, *name, localdb.Options{AdminDSN: *admin, Migrate: !*noMigrate})
	if err != nil {
		return err
	}
	prefix := ""
	if *export {
		prefix = "export "
	}
	fmt.Printf("%sCP_TEST_DATABASE_URL=%s\n", prefix, d.AppDSN)
	fmt.Printf("%sCP_TEST_MIGRATE_DATABASE_URL=%s\n", prefix, d.MigrateDSN)
	return nil
}

func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}
