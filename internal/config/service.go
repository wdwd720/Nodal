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
	// DepRedis is the rate-limit store. NOTHING constructs a Redis client
	// today: ratelimit.NewRedisStore exists and no composition root calls it,
	// and cmd/api explicitly chooses ratelimit.NewMemoryStore. It is modelled
	// anyway, because the store exists and a binary will one day use it, and a
	// dependency that appears later should arrive as one line here rather than
	// as a rediscovery of why the variable was required.
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
)

var allDependencies = []Dependency{DepRedis, DepRedpanda, DepClickHouse, DepTemporal, DepArchive}

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
// The two disagree in one place that matters: cmd/api links go-redis through
// internal/ratelimit and then chooses the in-memory store, so it is linked and
// unused.
//
// Adding a dependency to a binary belongs here, in one line, next to the
// evidence.
var serviceDeps = map[Service][]Dependency{
	// Dials Postgres. Writes raw Stripe deliveries to the evidence bucket
	// before parsing them. Chooses ratelimit.NewMemoryStore, links no
	// ClickHouse, Temporal or Redpanda client at all.
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
	ServiceAuditWorker: {DepArchive},

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
