package config

import (
	"fmt"
	"slices"
	"strings"
)

// Dependency is an external system a binary may need in order to work.
//
// It exists because "required in production" was previously a property of a
// variable and not of a caller, so every binary required every dependency. The
// API required a Temporal address it never dialled, a Redpanda broker list it
// never connected to, and a Redis URL that nothing anywhere constructs a client
// from. A deployment therefore had to be given four endpoints that would never
// be contacted, which is configuration that lies -- and the next person reads
// it and believes it.
//
// Postgres is deliberately absent from this list. Every binary that loads
// configuration uses it, so making it conditional would model a choice nobody
// has.
type Dependency string

// Dependencies.
const (
	// DepRedis is the shared rate-limit store, and it is the one dependency
	// that no entry in serviceDeps can express. cmd/api builds
	// ratelimit.NewRedisStore when CP_RATELIMIT_BACKEND is redis and
	// ratelimit.NewMemoryStore when it is not, so whether the binary needs
	// Redis is a deployment decision rather than a property of the binary.
	// Config.RequiresDependency is where that is decided.
	DepRedis Dependency = "redis"
	// DepRedpanda is the Kafka-protocol event bus.
	DepRedpanda Dependency = "redpanda"
	// DepClickHouse is the analytics store.
	DepClickHouse Dependency = "clickhouse"
	// DepTemporal is the workflow engine.
	DepTemporal Dependency = "temporal"
	// DepArchive is the S3-compatible object store that holds raw payloads,
	// provider evidence and the WORM audit archive.
	DepArchive Dependency = "archive"
	// DepKMS is the key management service that signs the audit chain.
	//
	// Only cmd/audit-worker signs: it is the only binary that constructs a
	// proof.Signer or a proof.Checkpointer. Every other binary links the KMS
	// SDK through internal/proof and never calls it.
	//
	// This matters more than the usual unused-endpoint argument, because the
	// requirement it carries is a real control. proof.NewLocalECDSASigner
	// refuses STAGING and PROD outright: a software key held by the signing
	// process can be copied by anything that compromises that process, and the
	// audit chain's whole value is that it cannot be rewritten by whoever
	// rewrote the data. So the control stays exactly where the signing happens,
	// and stops being demanded of binaries that do not sign.
	DepKMS Dependency = "kms"
)

var allDependencies = []Dependency{DepRedis, DepRedpanda, DepClickHouse, DepTemporal, DepArchive, DepKMS}

// AllDependencies returns every declared dependency (a copy).
func AllDependencies() []Dependency { return slices.Clone(allDependencies) }

// Valid reports whether d is declared.
func (d Dependency) Valid() bool { return slices.Contains(allDependencies, d) }

func (d Dependency) String() string { return string(d) }

// Service is one executable in this repository.
//
// Load takes one. There is no default and no zero value that means "all",
// because a caller that has not said what it is cannot be told what it needs,
// and guessing on its behalf is how the previous behaviour came about.
type Service string

// Services, one per cmd/ directory that loads configuration.
const (
	ServiceAPI                  Service = "api"
	ServiceAgentWorker          Service = "agent-worker"
	ServiceAuditWorker          Service = "audit-worker"
	ServiceExecutionWorker      Service = "execution-worker"
	ServiceMarketIngestWorker   Service = "market-ingest-worker"
	ServiceReconciliationWorker Service = "reconciliation-worker"
	ServiceRelayWorker          Service = "relay-worker"
	ServiceWorkflowWorker       Service = "workflow-worker"

	// ServiceTooling is for one-off operator commands and test harnesses that
	// load configuration without being a deployed binary. It declares no
	// external dependency, which is the conservative reading: a tool that
	// needs one asks for it by naming the service it stands in for.
	ServiceTooling Service = "tooling"
)

// serviceDeps is the audit, as a table.
//
// Each entry was established by reading what the binary actually constructs,
// not by reading its documentation. `go list -deps` says which clients are even
// linked into a binary, and the composition root says which of those it builds.
//
// It answers one question: what does this binary ALWAYS need. A dependency the
// binary needs only under some configuration cannot be stated here and is
// decided in Config.RequiresDependency instead -- Redis is the only one, and
// the only one so far that a deployment gets to choose.
//
// Adding an unconditional dependency belongs here, in one line, next to the
// evidence.
var serviceDeps = map[Service][]Dependency{
	// Dials Postgres. Writes raw Stripe deliveries to the evidence bucket
	// before parsing them. Links no ClickHouse, Temporal or Redpanda client at
	// all. Redis is absent here and required of it anyway whenever
	// CP_RATELIMIT_BACKEND is redis, which STAGING and PROD insist on: see
	// RequiresDependency.
	ServiceAPI: {DepArchive},

	// Postgres only.
	ServiceAgentWorker:          {},
	ServiceExecutionWorker:      {},
	ServiceReconciliationWorker: {},

	// Its S3 Object Lock archive is not wired yet -- buildArchive returns a
	// filesystem archive when CP_AUDIT_ARCHIVE_DIR is set and nil otherwise --
	// so the archive is declared because that is where it is going and because
	// the audit bucket is the one place Object Lock is a compliance control
	// rather than a convenience.
	//
	// It is also the only binary that signs the audit chain, which is why it
	// alone declares KMS.
	ServiceAuditWorker: {DepArchive, DepKMS},

	// Solana -> normalizer -> Redpanda -> ClickHouse, with raw payloads and
	// evidence archived.
	ServiceMarketIngestWorker: {DepRedpanda, DepClickHouse, DepArchive},

	// Publishes to the event bus.
	ServiceRelayWorker: {DepRedpanda},

	// Hosts Temporal workflows and activities.
	ServiceWorkflowWorker: {DepTemporal},

	ServiceTooling: {},
}

// allServices is declaration order, which is also the order AllServices
// returns and the order tests iterate in.
var allServices = []Service{
	ServiceAPI, ServiceAgentWorker, ServiceAuditWorker, ServiceExecutionWorker,
	ServiceMarketIngestWorker, ServiceReconciliationWorker, ServiceRelayWorker,
	ServiceWorkflowWorker, ServiceTooling,
}

// AllServices returns every declared service (a copy).
func AllServices() []Service { return slices.Clone(allServices) }

// Valid reports whether s is declared.
func (s Service) Valid() bool { return slices.Contains(allServices, s) }

func (s Service) String() string { return string(s) }

// Dependencies returns what s needs, in declaration order (a copy). An unknown
// service returns nil, which no caller reaches: Load refuses one first.
func (s Service) Dependencies() []Dependency { return slices.Clone(serviceDeps[s]) }

// Requires reports whether s needs d.
//
// An unknown dependency is never required, and an unknown service requires
// nothing -- but neither can reach a decision, because Load refuses an unknown
// service and the only dependencies in the table are declared constants.
func (s Service) Requires(d Dependency) bool {
	return slices.Contains(serviceDeps[s], d)
}

// ParseService parses the canonical name, which is the cmd/ directory name.
func ParseService(v string) (Service, error) {
	s := Service(strings.ToLower(strings.TrimSpace(v)))
	if !s.Valid() {
		return "", fmt.Errorf("config: unknown service %q", v)
	}
	return s, nil
}

// httpServices lists the binaries that serve public HTTP and therefore run the
// transport rate limiter.
//
// It is a list rather than an equality check so that a second HTTP surface --
// an admin API, say -- joins by being named here instead of by somebody
// remembering that a condition existed somewhere.
var httpServices = []Service{ServiceAPI}

// ServesHTTP reports whether s serves public HTTP, which is the same question
// as whether the transport rate limiter runs in it.
func (s Service) ServesHTTP() bool { return slices.Contains(httpServices, s) }

// RequiresDependency reports whether this configuration needs an external
// system, combining what the binary always needs with what its configuration
// makes it need.
//
// Most dependencies are a property of the binary alone. Redis is not: the API
// needs it only when the rate-limit backend is the distributed one, which is a
// deployment decision rather than a compile-time fact. Modelling that as a
// static "the API needs Redis" would put the variable back into every LOCAL
// developer's environment for a client that a memory-backed run never builds.
func (c *Config) RequiresDependency(d Dependency) bool {
	switch d {
	case DepRedis:
		// The API needs Redis only when its rate-limit counters are shared.
		if c.Service.ServesHTTP() && c.RateLimit.Backend.Distributed() {
			return true
		}
	case DepArchive:
		// A binary that archives needs an OBJECT STORE only when the archive
		// backend is one. With the Postgres backend the store it needs is the
		// database it already has, and demanding an endpoint, a region and
		// three bucket names would be four values that are never contacted.
		if c.Service.Requires(DepArchive) && !c.Archive.Backend.NeedsObjectStore() {
			return false
		}
	}
	return c.Service.Requires(d)
}

// requiresVar reports whether an absent variable is an error for this
// configuration.
func (c *Config) requiresVar(s varSpec) bool {
	switch {
	case !s.Required:
		return false
	case s.Svc != "" && s.Svc != c.Service:
		// A setting only one binary has any use for is not missing from the
		// others.
		return false
	case s.Dep == "":
		return true
	default:
		return c.RequiresDependency(s.Dep)
	}
}
