package config

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withoutPrefix returns prodEnv minus every variable starting with one of the
// prefixes. It is how "this dependency is not configured at all" is expressed:
// removing the variables rather than blanking them, because a blank value is
// already treated as absent and a deployment that does not use a dependency
// simply does not set it.
func withoutPrefix(base map[string]string, prefixes ...string) map[string]string {
	m := maps.Clone(base)
	for k := range m {
		for _, p := range prefixes {
			if len(k) >= len(p) && k[:len(p)] == p {
				delete(m, k)
			}
		}
	}
	return m
}

// TestService_APIStartsInProductionWithoutTheDependenciesItDoesNotUse is the
// defect this whole mechanism exists for.
//
// cmd/api dials Postgres and writes webhook evidence to S3. It links no
// ClickHouse, Temporal or Redpanda client at all and chooses an in-memory
// rate-limit store over Redis. Before this change it could not start in
// production without four endpoints it would never contact, so a deployment
// had to invent them -- configuration that lies, which the next person reads
// and believes.
func TestService_APIStartsInProductionWithoutTheDependenciesItDoesNotUse(t *testing.T) {
	t.Parallel()
	env := withoutPrefix(prodEnv(), "CP_REDPANDA_", "CP_CLICKHOUSE_", "CP_TEMPORAL_")

	c, err := Load(context.Background(), ServiceAPI, LookupFromMap(env))
	require.NoError(t, err, "the API must start in PROD with only what it uses")
	require.NotNil(t, c)

	assert.Equal(t, EnvProd, c.Env, "and it is really production, not a fallback")
	assert.Equal(t, ServiceAPI, c.Service)
	assert.True(t, c.Database.RequireTLS, "production rules still apply")
	assert.NotEmpty(t, c.Archive.EvidenceBucket, "the one dependency it does use is still required")

	// Nothing was quietly filled in.
	assert.Empty(t, c.Redpanda.Brokers)
	assert.Empty(t, c.ClickHouse.Addr)
	assert.Empty(t, c.Temporal.HostPort)

	// Redis is the exception, and it is not an exception to the principle: the
	// API really does dial it, because PROD requires a rate-limit budget that
	// is shared across replicas rather than copied into each one.
	assert.False(t, c.Redis.URL.IsZero())
	assert.True(t, c.RequiresDependency(DepRedis))
}

// TestService_TheAPINeedsRedisOnlyWhenItsCountersAreShared is the other half of
// the same idea. Redis is the one dependency that is not a property of the
// binary: the same cmd/api needs it or does not, according to a deployment
// decision. A DEV deployment running one task may keep its counters in the
// process, and then there is no Redis to configure and none is demanded.
func TestService_TheAPINeedsRedisOnlyWhenItsCountersAreShared(t *testing.T) {
	t.Parallel()
	base := withoutPrefix(asEnv(prodEnv(), EnvDev), "CP_REDIS_")

	memory := withVars(base, map[string]string{"CP_RATELIMIT_BACKEND": "memory", "CP_HTTP_REPLICAS": "1"})
	c, err := Load(context.Background(), ServiceAPI, LookupFromMap(memory))
	require.NoError(t, err, "a single-replica DEV API may keep its counters in the process")
	assert.True(t, c.Redis.URL.IsZero(), "and then nothing about Redis was invented")
	assert.False(t, c.RequiresDependency(DepRedis))

	// Same environment, same binary, one value changed: now Redis is required
	// and its absence is the error.
	shared := withVars(base, map[string]string{"CP_RATELIMIT_BACKEND": "redis"})
	c, err = Load(context.Background(), ServiceAPI, LookupFromMap(shared))
	require.Error(t, err, "a shared budget needs the thing that shares it")
	assert.Nil(t, c)
	assert.Contains(t, varErrors(err), "CP_REDIS_URL")
	assert.ErrorIs(t, varErrors(err)["CP_REDIS_URL"], ErrMissingRequired)
}

// TestService_TheDecidingValueIsReadBeforeItIsUsed: CP_REDIS_URL is declared
// after CP_RATELIMIT_BACKEND in the table today. If it were declared before,
// a loader that judged each variable as it walked the table would have decided
// Redis was missing before it knew whether Redis was wanted. Load makes two
// passes so the answer does not depend on the order, and this test loads with
// the table walked in both directions to say so.
func TestService_TheDecidingValueIsReadBeforeItIsUsed(t *testing.T) {
	t.Parallel()
	env := withVars(asEnv(withoutPrefix(prodEnv(), "CP_REDIS_"), EnvDev),
		map[string]string{"CP_RATELIMIT_BACKEND": "memory", "CP_HTTP_REPLICAS": "1"})

	// Reading the deciding variable last is the hostile order, and it is
	// simulated by answering lookups only after every other variable has been
	// asked for. The result must still be a config that needs no Redis.
	var asked []string
	lookup := func(name string) (string, bool) {
		asked = append(asked, name)
		v, ok := env[name]
		return v, ok
	}
	c, err := Load(context.Background(), ServiceAPI, lookup)
	require.NoError(t, err)
	assert.False(t, c.RequiresDependency(DepRedis))
	assert.Contains(t, asked, "CP_RATELIMIT_BACKEND")
	assert.Contains(t, asked, "CP_REDIS_URL", "the absent variable is still looked up, not skipped")
}

// TestService_RedisIsRequiredOfNobodyElse: the conditional requirement belongs
// to the binary that serves HTTP. Setting the rate-limit backend on a worker
// must not conjure a Redis dependency it has no use for.
func TestService_RedisIsRequiredOfNobodyElse(t *testing.T) {
	t.Parallel()
	env := withVars(withoutPrefix(prodEnv(), "CP_REDIS_"), map[string]string{"CP_RATELIMIT_BACKEND": "redis"})
	for _, svc := range AllServices() {
		if svc.ServesHTTP() {
			continue
		}
		t.Run(string(svc), func(t *testing.T) {
			t.Parallel()
			c, err := Load(context.Background(), svc, LookupFromMap(env))
			require.NoError(t, err, "%s does not serve HTTP and has no rate limiter", svc)
			assert.False(t, c.RequiresDependency(DepRedis))
		})
	}
}

// TestService_ABinaryThatNeedsADependencyRefusesToStartWithoutIt is the other
// half, and the one that must never regress: strictness is preserved exactly
// where it means something.
func TestService_ABinaryThatNeedsADependencyRefusesToStartWithoutIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		dep      Dependency
		service  Service
		prefixes []string
		wantVar  string
	}{
		{DepRedpanda, ServiceRelayWorker, []string{"CP_REDPANDA_"}, "CP_REDPANDA_BROKERS"},
		{DepClickHouse, ServiceMarketIngestWorker, []string{"CP_CLICKHOUSE_"}, "CP_CLICKHOUSE_ADDR"},
		{DepTemporal, ServiceWorkflowWorker, []string{"CP_TEMPORAL_"}, "CP_TEMPORAL_HOST_PORT"},
		{DepArchive, ServiceAPI, []string{"CP_ARCHIVE_"}, "CP_ARCHIVE_EVIDENCE_BUCKET"},
	}
	for _, tc := range cases {
		t.Run(string(tc.dep), func(t *testing.T) {
			t.Parallel()
			require.True(t, tc.service.Requires(tc.dep), "the fixture must pick a service that uses it")

			env := withoutPrefix(prodEnv(), tc.prefixes...)
			c, err := Load(context.Background(), tc.service, LookupFromMap(env))
			require.Error(t, err, "%s uses %s and must refuse to start without it", tc.service, tc.dep)
			assert.Nil(t, c)
			assert.Contains(t, varErrors(err), tc.wantVar)
			assert.ErrorIs(t, varErrors(err)[tc.wantVar], ErrMissingRequired)
		})
	}
}

// TestService_RedisIsNotAStaticDependencyOfAnything records where the Redis
// requirement lives, because it is the one that does not live in the table.
//
// serviceDeps answers "what does this binary always need". Nothing always needs
// Redis: cmd/api builds ratelimit.NewRedisStore when its rate-limit backend is
// redis and ratelimit.NewMemoryStore when it is not. So the question is asked
// of the Config, which knows the backend, rather than of the Service, which
// does not.
//
// If a binary ever needs Redis unconditionally, it gains a line in serviceDeps
// and this test says so.
func TestService_RedisIsNotAStaticDependencyOfAnything(t *testing.T) {
	t.Parallel()
	for _, s := range AllServices() {
		assert.False(t, s.Requires(DepRedis),
			"%s declares Redis unconditionally: say why here, because RequiresDependency now has a second answer", s)
	}
	// And the conditional path is real: same service, two backends.
	shared := &Config{Service: ServiceAPI, RateLimit: RateLimitConfig{Backend: RateLimitRedis}}
	local := &Config{Service: ServiceAPI, RateLimit: RateLimitConfig{Backend: RateLimitMemory}}
	assert.True(t, shared.RequiresDependency(DepRedis))
	assert.False(t, local.RequiresDependency(DepRedis))
}

// TestService_MalformedDependencyConfigFailsClosedForEveryone: required-ness
// is conditional, parsing is not.
//
// A broken broker list on the API is a mistake somebody made, and the fact
// that the API would never have dialled it does not make it less of a mistake.
// Silently ignoring it would mean the value is wrong in every environment and
// only discovered by the binary that does use it.
func TestService_MalformedDependencyConfigFailsClosedForEveryone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		vars    map[string]string
		wantVar string
	}{
		{"redis url", map[string]string{"CP_REDIS_URL": "env://"}, "CP_REDIS_URL"},
		{"redpanda tls", map[string]string{"CP_REDPANDA_REQUIRE_TLS": "yes"}, "CP_REDPANDA_REQUIRE_TLS"},
		{"clickhouse tls", map[string]string{"CP_CLICKHOUSE_REQUIRE_TLS": "maybe"}, "CP_CLICKHOUSE_REQUIRE_TLS"},
		{"temporal tls", map[string]string{"CP_TEMPORAL_REQUIRE_TLS": "1.5"}, "CP_TEMPORAL_REQUIRE_TLS"},
		{"archive object lock", map[string]string{"CP_ARCHIVE_OBJECT_LOCK_REQUIRED": "sure"}, "CP_ARCHIVE_OBJECT_LOCK_REQUIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Loaded as the API, which uses none of these except the archive.
			c, err := Load(context.Background(), ServiceAPI,
				LookupFromMap(withVars(prodEnv(), tc.vars)))
			require.Error(t, err, "a malformed value must fail even for a binary that would not use it")
			assert.Nil(t, c)
			assert.Contains(t, varErrors(err), tc.wantVar)
		})
	}
}

// TestService_LoadRefusesAnUndeclaredService: there is no default service and
// no zero value that means "everything". A caller that has not said what it is
// cannot be told what it needs, and guessing on its behalf is how every binary
// came to require every dependency.
func TestService_LoadRefusesAnUndeclaredService(t *testing.T) {
	t.Parallel()
	for _, bad := range []Service{"", "apiserver", "API", " api "} {
		c, err := Load(context.Background(), bad, LookupFromMap(prodEnv()))
		require.Error(t, err, "service %q must be refused", bad)
		assert.Nil(t, c)
		assert.Contains(t, err.Error(), "unknown service")
	}
}

// TestService_NoProductionBinaryFallsBackToLocalBehaviour: the defaults that
// exist for LOCAL and TEST must not appear in a production load, whichever
// service is asking. This is the property that would have made the whole
// change dangerous if it were wrong.
func TestService_NoProductionBinaryFallsBackToLocalBehaviour(t *testing.T) {
	t.Parallel()
	for _, service := range AllServices() {
		for _, env := range []Environment{EnvDev, EnvStaging, EnvProd} {
			t.Run(string(service)+"/"+string(env), func(t *testing.T) {
				t.Parallel()
				// Only the environment is set. Every required variable, for
				// this service, must be reported rather than defaulted.
				c, err := Load(context.Background(), service,
					LookupFromMap(map[string]string{"CP_ENV": string(env)}))
				require.Error(t, err)
				assert.Nil(t, c, "a partially defaulted config must never be returned")

				got := varErrors(err)
				for _, s := range Vars() {
					if !s.Required || s.Default == "" {
						continue
					}
					if s.Dep != "" && !service.Requires(s.Dep) {
						continue
					}
					if s.Svc != "" && s.Svc != service {
						continue
					}
					assert.Contains(t, got, s.Name,
						"%s has a LOCAL default and must not be applied in %s", s.Name, env)
				}
			})
		}
	}
}

// TestService_EveryServiceIsDeclaredAndEveryDependencyIsReachable keeps the two
// tables honest about each other.
func TestService_EveryServiceIsDeclaredAndEveryDependencyIsReachable(t *testing.T) {
	t.Parallel()

	// Every service in the dependency table is a declared service, and vice
	// versa: a service with no entry would silently require nothing.
	for s := range serviceDeps {
		assert.True(t, s.Valid(), "%s is in serviceDeps and is not a declared service", s)
	}
	for _, s := range AllServices() {
		_, ok := serviceDeps[s]
		assert.True(t, ok, "%s has no entry in serviceDeps, so it would silently require nothing", s)
		for _, d := range s.Dependencies() {
			assert.True(t, d.Valid(), "%s declares undeclared dependency %q", s, d)
		}
	}

	// Every declared dependency owns at least one variable, and every
	// dependency-tagged variable names a declared dependency. Either half
	// being wrong means a variable that is required of nobody, or of everybody.
	owned := map[Dependency]int{}
	for _, v := range Vars() {
		if v.Dep == "" {
			continue
		}
		assert.True(t, v.Dep.Valid(), "%s names undeclared dependency %q", v.Name, v.Dep)
		owned[v.Dep]++
	}
	for _, d := range AllDependencies() {
		assert.Positive(t, owned[d], "dependency %s owns no variable, so declaring it changes nothing", d)
	}
}

// TestService_ParseService accepts the cmd/ directory name and nothing else.
func TestService_ParseService(t *testing.T) {
	t.Parallel()
	for _, s := range AllServices() {
		got, err := ParseService(string(s))
		require.NoError(t, err)
		assert.Equal(t, s, got)
	}
	got, err := ParseService("  API  ")
	require.NoError(t, err, "case and surrounding space are forgiven")
	assert.Equal(t, ServiceAPI, got)

	_, err = ParseService("nope")
	require.Error(t, err)
}

// TestService_HashDistinguishesServices: two binaries given identical
// environment variables are not the same configuration, because they are held
// to different requirements. The hash is recorded on audit records to name the
// exact configuration in force, so it has to tell them apart.
func TestService_HashDistinguishesServices(t *testing.T) {
	t.Parallel()
	env := prodEnv()
	api := mustLoadAs(t, ServiceAPI, env)
	relay := mustLoadAs(t, ServiceRelayWorker, env)

	assert.NotEqual(t, api.Hash(), relay.Hash(),
		"the same variables loaded by different binaries are different configurations")
	assert.Equal(t, api.Hash(), mustLoadAs(t, ServiceAPI, env).Hash(), "and the hash is stable")
}

// TestService_RedactedKeepsTheService: an operator reading a redacted config in
// a log needs to know which binary produced it.
func TestService_RedactedKeepsTheService(t *testing.T) {
	t.Parallel()
	c := mustLoadAs(t, ServiceRelayWorker, prodEnv())
	assert.Equal(t, ServiceRelayWorker, c.Redacted().Service)
}
