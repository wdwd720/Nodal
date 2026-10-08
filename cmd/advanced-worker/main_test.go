package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/advancedjobs"
	"github.com/nodal/controlplane/internal/advancedmetrics"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/gates"
)

func lookupNothing(string) (string, bool) { return "", false }

// exampleEnv is the documented LOCAL environment (.env.example), with
// overrides; a value of "-" removes the variable.
func exampleEnv(t *testing.T, overrides map[string]string) func(string) (string, bool) {
	t.Helper()
	env, err := config.ParseDotEnv(strings.NewReader(config.ExampleEnv()))
	require.NoError(t, err)
	for k, v := range overrides {
		if v == "-" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	return config.LookupFromMap(env)
}

// withoutChain is the example environment with no sandbox chain configured.
var withoutChain = map[string]string{"CP_SANDBOX_DEVNET_NETWORK": "-", "CP_SANDBOX_DEVNET_SEED_REF": "-"}

func TestRun_UsageErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"run-everything"}},
		{"run with arguments", []string{"run", "extra"}},
		{"list with arguments", []string{"list", "extra"}},
		{"once without a job", []string{"once"}},
		{"once with two jobs", []string{"once", advancedjobs.NameMarginInterest, advancedjobs.NameCapitalExpiry}},
		{"once with an unknown job", []string{"once", "margin.intrest"}},
		{"once with a job of no catalog", []string{"once", "everything"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			assert.Equal(t, exitUsage, run(tc.args, lookupNothing, &out, &errOut), errOut.String())
			assert.Empty(t, out.String(), "a usage error prints nothing on stdout")
		})
	}
}

// TestRun_OnceOfAnUnknownJobNamesTheListCommand: the refusal says how to find
// the right name, and is decided before any configuration or database is
// touched.
func TestRun_OnceOfAnUnknownJobNamesTheListCommand(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	require.Equal(t, exitUsage, run([]string{"once", "derivatives.fundng"}, lookupNothing, &out, &errOut))
	assert.Contains(t, errOut.String(), `unknown job "derivatives.fundng"`)
	assert.Contains(t, errOut.String(), "advanced-worker list")
}

func TestRun_HelpIsSuccessAndDocumentsEveryCommandAndVariable(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"help", "-h", "--help"} {
		var out, errOut bytes.Buffer
		require.Equal(t, exitOK, run([]string{arg}, lookupNothing, &out, &errOut))
		text := out.String()
		for _, want := range []string{
			"run", "once <job>", "list",
			"CP_ADVANCED_WORKER_INTERVALS", "CP_ADVANCED_WORKER_BATCHES", "CP_ADVANCED_WORKER_RUN_TIMEOUT",
			"CP_ADVANCED_WORKER_DRAIN_TIMEOUT", "CP_ADVANCED_WORKER_METRICS_INTERVAL",
			"CP_SANDBOX_DEVNET_NETWORK", "kill switch", "exit codes",
		} {
			assert.Contains(t, text, want, "help must document %q", want)
		}
	}
}

// listing runs `list` and returns its rows by job name.
func listing(t *testing.T, lookup func(string) (string, bool)) map[string]string {
	t.Helper()
	var out, errOut bytes.Buffer
	require.Equal(t, exitOK, run([]string{"list"}, lookup, &out, &errOut), errOut.String())
	rows := map[string]string{}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.NotEmpty(t, lines)
	assert.Equal(t, []string{"JOB", "INTERVAL", "BATCH", "STATUS"}, strings.Fields(lines[0]))
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		require.GreaterOrEqual(t, len(f), 4, l)
		rows[f[0]] = l
	}
	return rows
}

// TestRun_ListPrintsEveryJobWithItsResolvedSchedule: every catalog job and
// the collector are listed with the interval and batch this configuration
// gives them -- the documented default, an override, an override clamped to
// the subsystem's bound -- and without a sandbox chain the chain jobs say
// they are not wired and why.
func TestRun_ListPrintsEveryJobWithItsResolvedSchedule(t *testing.T) {
	t.Parallel()
	overrides := map[string]string{
		"CP_ADVANCED_WORKER_INTERVALS":        "margin.interest=30m,derivatives.funding=15s",
		"CP_ADVANCED_WORKER_BATCHES":          "margin.liquidation_queue=5000",
		"CP_ADVANCED_WORKER_METRICS_INTERVAL": "45s",
	}
	for k, v := range withoutChain {
		overrides[k] = v
	}
	rows := listing(t, exampleEnv(t, overrides))
	require.Len(t, rows, len(advancedjobs.Catalog())+1, "every job and the collector")
	for _, d := range advancedjobs.Catalog() {
		require.Contains(t, rows, d.Name)
		f := strings.Fields(rows[d.Name])
		switch d.Name {
		case advancedjobs.NameMarginInterest:
			assert.Equal(t, "30m0s", f[1], "an interval override")
		case advancedjobs.NameDerivativesFunding:
			assert.Equal(t, "15s", f[1])
		case advancedjobs.NameMarginLiquidation:
			assert.Equal(t, "100", f[2], "a batch beyond the queue's bound is clamped to it")
		default:
			assert.Equal(t, d.Default.Interval.String(), f[1], "%s keeps its documented interval", d.Name)
			assert.Equal(t, d.Default.Batch, atoi(t, f[2]), "%s keeps its documented batch", d.Name)
		}
		if d.Needs == advancedjobs.NeedsDevnet {
			assert.Contains(t, rows[d.Name], "not wired: needs CP_SANDBOX_DEVNET_NETWORK", d.Name)
		} else {
			assert.Equal(t, "wired", f[3], "%s runs in LOCAL", d.Name)
		}
	}
	assert.Equal(t, []string{advancedmetrics.Name, "45s", "1", "wired"}, strings.Fields(rows[advancedmetrics.Name]))
}

// TestRun_ListWiresTheChainJobsWhenTheSandboxChainIsConfigured: with the
// network and the seed configured, every job is wired.
func TestRun_ListWiresTheChainJobsWhenTheSandboxChainIsConfigured(t *testing.T) {
	t.Parallel()
	rows := listing(t, exampleEnv(t, nil))
	for name, row := range rows {
		assert.True(t, strings.HasSuffix(row, "wired") && !strings.Contains(row, "not wired"), "%s: %s", name, row)
	}
	assert.Contains(t, rows, advancedjobs.NameDelegationUpkeep)
	assert.Contains(t, rows, advancedjobs.NameEquitiesVenue)
}

// TestRun_AnOverrideOfAJobTheWorkerDoesNotRunIsAFailedStart: a typo in a
// schedule never becomes a silently different schedule, and the collector's
// interval has its own variable.
func TestRun_AnOverrideOfAJobTheWorkerDoesNotRunIsAFailedStart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		want      string
	}{
		{"misspelled interval", map[string]string{"CP_ADVANCED_WORKER_INTERVALS": "margin.intrest=1h"}, "margin.intrest"},
		{"misspelled batch", map[string]string{"CP_ADVANCED_WORKER_BATCHES": "stable.redemption=5"}, "stable.redemption"},
		{"the collector in the job intervals", map[string]string{"CP_ADVANCED_WORKER_INTERVALS": advancedmetrics.Name + "=1m"},
			"CP_ADVANCED_WORKER_METRICS_INTERVAL"},
		{"malformed pair", map[string]string{"CP_ADVANCED_WORKER_INTERVALS": "margin.interest"}, "CP_ADVANCED_WORKER_INTERVALS"},
		{"non-positive batch", map[string]string{"CP_ADVANCED_WORKER_BATCHES": "margin.interest=0"}, "CP_ADVANCED_WORKER_BATCHES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			assert.Equal(t, exitFailure, run([]string{"list"}, exampleEnv(t, tc.overrides), &out, &errOut))
			assert.Contains(t, errOut.String(), tc.want)
		})
	}
}

// TestRun_OnceOfAnUnwiredJobFailsBeforeTheDatabase: a job this configuration
// does not wire is a failure that says why -- not a usage error, and not a
// connection attempt.
func TestRun_OnceOfAnUnwiredJobFailsBeforeTheDatabase(t *testing.T) {
	t.Parallel()
	overrides := map[string]string{"CP_DATABASE_APP_URL": "postgres://nobody@127.0.0.1:1/none?sslmode=disable"}
	for k, v := range withoutChain {
		overrides[k] = v
	}
	var out, errOut bytes.Buffer
	assert.Equal(t, exitFailure, run([]string{"once", advancedjobs.NameStableMonitor}, exampleEnv(t, overrides), &out, &errOut))
	assert.Contains(t, errOut.String(), "not wired")
	assert.Contains(t, errOut.String(), "CP_SANDBOX_DEVNET_NETWORK")
}

// TestPlan_TheEquityJobsRunOnlyWhereThePaperVenueMay: the paper venue runs in
// LOCAL, TEST and DEV and nowhere else, and never in provider mode live --
// the API's own rule -- so the equity jobs are wired exactly there.
func TestPlan_TheEquityJobsRunOnlyWhereThePaperVenueMay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		env   config.Environment
		mode  config.ProviderMode
		wired bool
	}{
		{config.EnvLocal, config.ProviderModeFake, true},
		{config.EnvDev, config.ProviderModeSandbox, true},
		{config.EnvTest, config.ProviderModeFake, true},
		{config.EnvStaging, config.ProviderModeFake, false},
		{config.EnvProd, config.ProviderModeSandbox, false},
		{config.EnvLocal, config.ProviderModeLive, false},
	} {
		cfg := &config.Config{Env: tc.env}
		cfg.Providers.Execution.Mode = tc.mode
		p := newPlan(cfg)
		for _, name := range []string{advancedjobs.NameEquitiesSettlement, advancedjobs.NameEquitiesCorporate, advancedjobs.NameEquitiesVenue} {
			assert.Equal(t, tc.wired, p.wired(name), "%s in %s with %s", name, tc.env, tc.mode)
		}
		assert.True(t, p.wired(advancedjobs.NameMarginLiquidation), "every other job is wired everywhere")
		assert.False(t, p.wired(advancedjobs.NameDelegationUpkeep), "no sandbox chain configured")
	}
}

// TestPlan_DefaultsAreTheCatalogs: with no override the plan is the catalog's
// documented defaults, and the collector runs every minute.
func TestPlan_DefaultsAreTheCatalogs(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Env: config.EnvLocal}
	cfg.Providers.Execution.Mode = config.ProviderModeFake
	cfg.Sandbox.DevnetNetwork = "nodal-sandbox"
	p := newPlan(cfg)
	require.Len(t, p.jobs, len(advancedjobs.Catalog())+1)
	for i, d := range advancedjobs.Catalog() {
		assert.Equal(t, d.Name, p.jobs[i].Name)
		assert.Equal(t, d.Default, p.jobs[i].Settings)
	}
	last := p.jobs[len(p.jobs)-1]
	assert.Equal(t, advancedmetrics.Name, last.Name)
	assert.Equal(t, time.Minute, last.Settings.Interval)
	assert.Empty(t, p.unwired)
}

func TestParseCapabilities_ReadsTheAPIsListTheAPIsWay(t *testing.T) {
	t.Parallel()
	got := parseCapabilities(" credit_purchase, AGENT_SIGNING ,,NOT_A_CAPABILITY")
	assert.Equal(t, map[gates.Capability]struct{}{gates.CreditPurchase: {}, gates.AgentSigning: {}}, got)
	assert.Empty(t, parseCapabilities(""), "an empty list enables nothing")
}

func TestLegalRouterFor_RefusesTheDevelopmentPolicyInProduction(t *testing.T) {
	t.Parallel()
	r, err := legalRouterFor(config.EnvProd, "")
	require.NoError(t, err)
	assert.Nil(t, r, "conservative is the caller's default")
	_, err = legalRouterFor(config.EnvProd, "development")
	require.Error(t, err)
	r, err = legalRouterFor(config.EnvLocal, "DEVELOPMENT")
	require.NoError(t, err)
	assert.NotNil(t, r)
	_, err = legalRouterFor(config.EnvLocal, "lenient")
	assert.Error(t, err)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		require.True(t, r >= '0' && r <= '9', s)
		n = n*10 + int(r-'0')
	}
	return n
}
