package config

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite the repository .env.example from ExampleEnv()")

const exampleEnvPath = "../../.env.example"

func TestExampleEnv_Golden(t *testing.T) {
	want := ExampleEnv()
	path := filepath.Clean(exampleEnvPath)
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(want), 0o644))
		t.Logf("wrote %s", path)
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err, "missing %s: run `go test ./internal/config -run TestExampleEnv -update`", path)
	assert.Equal(t, want, strings.ReplaceAll(string(got), "\r\n", "\n"),
		"%s is stale: run `go test ./internal/config -run TestExampleEnv -update`", path)
}

func TestExampleEnv_ContainsEveryVariableOnce(t *testing.T) {
	t.Parallel()
	text := ExampleEnv()
	for _, s := range Vars() {
		assert.Equal(t, 1, strings.Count(text, "\n"+s.Name+"="), "%s must appear exactly once as an assignment", s.Name)
		assert.Contains(t, text, s.Doc[:min(len(s.Doc), 40)], "%s documentation present", s.Name)
	}
	assert.True(t, strings.HasPrefix(text, "# .env.example"))
	assert.Contains(t, text, "CP_ENV=LOCAL")
	vars, err := ParseDotEnv(strings.NewReader(text))
	require.NoError(t, err)
	for k, v := range vars {
		assert.NotContains(t, v, "aws-sm://", "%s: the example never points at production secrets", k)
		assert.NotContains(t, v, "example.com", "%s: the example never points at deployed hosts", k)
	}
}

func TestExampleEnv_LoadsAsValidLocalConfig(t *testing.T) {
	t.Parallel()
	vars, err := ParseDotEnv(strings.NewReader(ExampleEnv()))
	require.NoError(t, err)
	assert.Len(t, vars, len(Vars()))
	c, err := Load(context.Background(), ServiceAPI, LookupFromMap(vars))
	require.NoError(t, err)
	assert.Equal(t, EnvLocal, c.Env)
	assert.Equal(t, "Control Plane", c.PublicProductName, "quoted values round-trip")
	assert.Equal(t, []string{"http://localhost:3000"}, c.HTTP.CORSOrigins)
	assert.Equal(t, SecretRef("cp_minio"), c.Archive.AccessKeyRef)
	assert.Equal(t, "", c.Telemetry.OTLPEndpoint, "empty assignments count as unset")
	assert.NoError(t, c.Validate())

	// The same file must not be usable as a production configuration.
	vars["CP_ENV"] = "PROD"
	_, err = Load(context.Background(), ServiceAPI, LookupFromMap(vars))
	require.Error(t, err)
	assert.True(t, HasViolation(err, RuleNoFakeProviders))
	assert.True(t, HasViolation(err, RuleSecretRefScheme))
	assert.True(t, HasViolation(err, RuleDatabaseTLS))
}

func TestParseDotEnv(t *testing.T) {
	t.Parallel()
	in := `
# comment
export CP_A=1
CP_B = two words
CP_C="quoted # not a comment"
CP_D='single'
CP_E=
CP_F="esc \"q\""
`
	got, err := ParseDotEnv(strings.NewReader(in))
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"CP_A": "1", "CP_B": "two words", "CP_C": "quoted # not a comment", "CP_D": "single", "CP_E": "", "CP_F": `esc "q"`,
	}, got)

	_, err = ParseDotEnv(strings.NewReader("NOEQUALS\n"))
	assert.Error(t, err)
	_, err = ParseDotEnv(strings.NewReader("1BAD=x\n"))
	assert.Error(t, err)
}
