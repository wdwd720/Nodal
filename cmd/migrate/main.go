// Command migrate applies, inspects, verifies and (guardedly) rolls back the
// embedded schema migrations under the migration database role.
//
// Usage:
//
//	migrate up                       apply all pending migrations
//	migrate up-to <version>          apply pending migrations up to <version>
//	migrate status                   list migrations and their applied state
//	migrate down-to <version>        roll back above <version>; refuses below the ledger-protected version
//	migrate verify                   compare applied checksums against the embedded files
//	migrate create <name> [-range R] [-dir migrations]
//	                                 write a new SQL skeleton with the next number in range R
//	                                 (foundation|financial|instruments|execution|funding|strategy|reality|audit)
//
// Environment:
//
//	CP_DATABASE_MIGRATE_URL   connection string for the migration role (required outside LOCAL/TEST)
//	CP_ENV                    LOCAL|TEST|DEV|STAGING|PROD; when LOCAL, TEST or unset and the URL is
//	                          missing, the docker-compose default is used with a warning
//
// Exit codes: 0 success, 1 runtime failure, 2 usage error. Connection strings
// are never printed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nodal/controlplane/internal/db/migrate"
)

const (
	envMigrateURL = "CP_DATABASE_MIGRATE_URL"
	envEnv        = "CP_ENV"

	// localDefaultURL mirrors docker-compose.yml / docker/postgres/init/001_roles.sql.
	// It is a LOCAL development credential, not a secret.
	localDefaultURL = "postgres://cp_migrate:cp_migrate_local@127.0.0.1:5433/controlplane?sslmode=disable" //nolint:gosec // G101: LOCAL docker-compose default; resolveMigrateURL substitutes it only when CP_ENV is unset, LOCAL or TEST

	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
)

var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "create":
		err = cmdCreate(rest, stdout)
	case "up", "up-to", "status", "down-to", "verify", "version":
		var url string
		url, err = resolveMigrateURL(getenv, stderr)
		if err == nil {
			err = runDBCommand(ctx, cmd, rest, url, stdout)
		}
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "migrate: unknown command %q\n", cmd)
		usage(stderr)
		return exitUsage
	}
	if err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintln(stderr, "migrate:", err)
			usage(stderr)
			return exitUsage
		}
		fmt.Fprintln(stderr, "migrate:", err)
		return exitFailure
	}
	return exitOK
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `usage: migrate <command> [args]

  up                      apply all pending migrations
  up-to <version>         apply pending migrations up to and including <version>
  status                  list migrations and their applied state
  version                 print the current database version
  down-to <version>       roll back migrations above <version> (refuses below %d once applied)
  verify                  check applied checksums against the embedded migration files
  create <name> [-range R] [-dir DIR]
                          write DIR/NNNNN_<name>.sql with the next number in range R
                          R: %s (default foundation)

env: %s (migration-role connection string), %s
`, migrate.ProtectedVersion, strings.Join(migrate.RangeNames(), "|"), envMigrateURL, envEnv)
}

// resolveMigrateURL returns the migration URL from the environment. The local
// docker-compose default is only substituted when CP_ENV is LOCAL, TEST or
// unset, and then with a warning; every other environment must set it.
func resolveMigrateURL(getenv func(string) string, warn io.Writer) (string, error) {
	if u := strings.TrimSpace(getenv(envMigrateURL)); u != "" {
		return u, nil
	}
	env := strings.ToUpper(strings.TrimSpace(getenv(envEnv)))
	switch env {
	case "", "LOCAL", "TEST":
		fmt.Fprintf(warn, "migrate: WARNING %s is not set; using the LOCAL docker-compose default (CP_ENV=%q)\n", envMigrateURL, env)
		return localDefaultURL, nil
	default:
		return "", fmt.Errorf("%s must be set when %s=%s", envMigrateURL, envEnv, env)
	}
}

func runDBCommand(ctx context.Context, cmd string, args []string, url string, stdout io.Writer) error {
	switch cmd {
	case "up":
		if len(args) != 0 {
			return fmt.Errorf("%w: up takes no arguments", errUsage)
		}
		if err := migrate.Up(ctx, url); err != nil {
			return err
		}
		return printStatus(ctx, url, stdout)
	case "up-to":
		v, err := versionArg(args)
		if err != nil {
			return err
		}
		if err := migrate.UpTo(ctx, url, v); err != nil {
			return err
		}
		return printStatus(ctx, url, stdout)
	case "down-to":
		v, err := versionArg(args)
		if err != nil {
			return err
		}
		if err := migrate.DownTo(ctx, url, v); err != nil {
			return err
		}
		return printStatus(ctx, url, stdout)
	case "status":
		if len(args) != 0 {
			return fmt.Errorf("%w: status takes no arguments", errUsage)
		}
		return printStatus(ctx, url, stdout)
	case "version":
		v, err := migrate.Version(ctx, url)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, v)
		return nil
	case "verify":
		if len(args) != 0 {
			return fmt.Errorf("%w: verify takes no arguments", errUsage)
		}
		if err := migrate.Verify(ctx, url); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "verify: ok (all applied migrations match the embedded files)")
		return nil
	}
	return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
}

func versionArg(args []string) (int64, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("%w: expected exactly one <version> argument", errUsage)
	}
	v, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%w: invalid version %q", errUsage, args[0])
	}
	return v, nil
}

func printStatus(ctx context.Context, url string, w io.Writer) error {
	rows, err := migrate.Status(ctx, url)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%-8s %-10s %-20s %s\n", "VERSION", "STATE", "APPLIED AT (UTC)", "NAME")
	for _, r := range rows {
		state, at := "pending", "-"
		switch {
		case r.Orphaned:
			state = "orphaned"
			at = r.AppliedAt.UTC().Format(time.RFC3339)
		case r.Applied:
			state = "applied"
			at = r.AppliedAt.UTC().Format(time.RFC3339)
		}
		name := r.Name
		if name == "" {
			name = "(no embedded source)"
		}
		fmt.Fprintf(w, "%-8d %-10s %-20s %s\n", r.Version, state, at, name)
	}
	return nil
}

func cmdCreate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rangeName := fs.String("range", "foundation", "numbering range")
	dir := fs.String("dir", "migrations", "directory holding the migration files")
	// Accept both "create name -range x" and "create -range x name".
	var name string
	var positional []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return fmt.Errorf("%w: %v", errUsage, err)
		}
		rest = fs.Args()
		if len(rest) > 0 {
			positional = append(positional, rest[0])
			rest = rest[1:]
		}
	}
	if len(positional) != 1 {
		return fmt.Errorf("%w: create expects exactly one <name>", errUsage)
	}
	name = positional[0]
	if err := migrate.ValidateName(name); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	r, ok := migrate.RangeByName(*rangeName)
	if !ok {
		return fmt.Errorf("%w: unknown range %q (want one of %s)", errUsage, *rangeName, strings.Join(migrate.RangeNames(), "|"))
	}
	entries, err := os.ReadDir(*dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", *dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	version, err := migrate.NextVersion(names, r)
	if err != nil {
		return err
	}
	// Anything at or above the protected version gets the non-destructive Down skeleton.
	r.Protected = r.Protected || version >= migrate.ProtectedVersion
	path := filepath.Join(*dir, migrate.Filename(version, name))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // G302,G304: creates a migration source file to be committed; path is inside the operator-supplied -dir
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.WriteString(migrate.Skeleton(name, r)); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	fmt.Fprintf(stdout, "created %s\n", path)
	return nil
}
