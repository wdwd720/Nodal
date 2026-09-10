// Command configcheck answers "would this binary start with this environment?"
// without starting it.
//
//	go run ./scripts/configcheck -service api path\to\candidate.env
//
// It exists because the alternative is deploying and reading the crash. A
// deployment's environment is assembled by hand in a provider's dashboard or a
// Terraform module, and internal/config reports every problem at once rather
// than the first -- so the fast loop is to ask it here, fix the whole list, and
// deploy once.
//
// The file is parsed the way a .env file is: KEY=value, blank lines and lines
// beginning with # ignored, surrounding quotes stripped. Nothing is printed but
// variable names and the loader's own messages, so a file containing real
// secrets can be checked without those secrets reaching a terminal.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nodal/controlplane/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "configcheck:", err)
		os.Exit(1)
	}
}

func run() error {
	service := flag.String("service", "api", "which cmd/ binary to check as")
	flag.Parse()
	if flag.NArg() != 1 {
		return errors.New("usage: configcheck [-service api] <env-file>")
	}

	svc, err := config.ParseService(*service)
	if err != nil {
		return err
	}
	env, err := parseEnvFile(flag.Arg(0))
	if err != nil {
		return err
	}

	if _, err := config.Load(context.Background(), svc, config.LookupFromMap(env)); err != nil {
		// Every problem, one per line, in the loader's own words. They already
		// name the variable and say what is wrong with it.
		fmt.Printf("%s: %d variable(s) supplied, and the configuration is NOT valid:\n\n", svc, len(env))
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Println("  " + strings.TrimPrefix(line, "config: "))
		}
		return errors.New("configuration invalid")
	}

	fmt.Printf("%s: %d variable(s) supplied, and the configuration is valid.\n", svc, len(env))
	return nil
}

// parseEnvFile reads KEY=value lines. It is deliberately not a full .env
// parser: no interpolation, no export prefixes, no multi-line values. A
// deployment environment that needs those is a deployment environment nobody
// can read.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 -- the path is the operator's own argument; this program reads no untrusted input
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: not KEY=value", path, n)
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
