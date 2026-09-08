// Package deps decides whether a missing external dependency is a reason to
// skip a test or a reason to fail it.
//
// Several integration suites need something the Go test binary cannot start:
// ClickHouse, Redpanda, MinIO, Temporal, Redis. Each guarded itself with a
// `t.Skip` when its address was not in the environment, which is right on a
// laptop with nothing running and wrong in CI, where the job's whole purpose is
// to run them. The `integration` job started Postgres alone and set the two
// database variables, so five subsystems' tests were invoked, skipped, and
// reported as a pass -- while fifteen rows of REQUIREMENTS_TRACEABILITY.md
// cited those exact tests as the evidence for VERIFIED.
//
// The repository already knew the hazard. The chaos job carries a comment
// saying that without two environment variables its tests "t.Skip() silently
// and the job finishes in under half a second while reporting success". That
// warning was written for one job and not applied to the other.
//
// # The rule
//
// A job that promises a dependency sets CP_TEST_REQUIRE_EXTERNAL_DEPS. In that
// job a missing dependency is a failure. Anywhere else it is a skip.
//
// The variable is what makes this safe to apply everywhere: a fast unit job
// that never starts Redis is not lying when it skips a Redis test, because it
// never claimed to have Redis. Only a job that claims it can be caught out.
package deps

import (
	"os"
	"strings"
	"testing"
)

// RequireEnv is the variable a CI job sets to say it has started the external
// stack. Its presence turns every skip in this package into a failure.
const RequireEnv = "CP_TEST_REQUIRE_EXTERNAL_DEPS"

// Required reports whether the current job has promised the external stack.
func Required() bool { return strings.TrimSpace(os.Getenv(RequireEnv)) != "" }

// Need returns the value of an environment variable naming an external
// dependency. It skips the test when the variable is unset, or fails when the
// job said it would provide it.
//
// `what` names the dependency in the operator's terms -- "ClickHouse", not the
// variable -- because the failure is read by somebody deciding whether CI or
// the test is broken.
func Need(t *testing.T, envVar, what string) string {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v
	}
	if Required() {
		t.Fatalf("%s is unset, but %s is set: this job started %s and the test must run. "+
			"Either the service failed to come up or the job stopped exporting the variable; "+
			"skipping here would report a pass for a test that never ran.",
			envVar, RequireEnv, what)
	}
	t.Skipf("%s not set; skipping the %s test (start the stack with `make infra-up`)", envVar, what)
	return ""
}

// Unavailable handles a dependency that is reached rather than addressed -- one
// whose absence shows up as a failed health check instead of an empty variable.
// Same rule: a job that promised the stack does not get to pass without it.
func Unavailable(t *testing.T, what string, err error) {
	t.Helper()
	if Required() {
		t.Fatalf("%s is unreachable (%v), but %s is set: this job started it and the test must run. "+
			"Skipping here would report a pass for a test that never ran.", what, err, RequireEnv)
	}
	t.Skipf("%s is unreachable (%v); skipping (start the stack with `make infra-up`)", what, err)
}
