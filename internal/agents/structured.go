package agents

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/strategy"
)

// StructuredConstraintsRequired is the failure code recorded on a compile
// attempt, and returned to the caller, when this deployment's compiler reads a
// DECLARED strategy and the caller declared none, or declared an incomplete
// one.
//
// It is a failure code rather than an outcome for the reason
// CompilerUnavailable is: compile_attempts.outcome is a closed CHECK (00500),
// and the honest member here is REJECTED — a compiler was present, it read the
// input, and it refused it. The code says WHY: the input was not a strategy,
// as opposed to a strategy the platform's rules turned down. Every field it
// needed and did not get is named on the attempt, because "say more" is only
// actionable advice when it says which more.
const StructuredConstraintsRequired = "STRUCTURED_CONSTRAINTS_REQUIRED"

// structuredConstraintsDetail is the sentence the API returns. Like
// compilerUnavailableDetail it says what did not happen, what was recorded, and
// what would change it.
const structuredConstraintsDetail = "This deployment's compiler builds a strategy from the fields you state, one by one, " +
	"and never from your description: nothing was read out of the words you wrote and nothing was assumed. " +
	"The strategy is incomplete, so no IR was produced and no version exists. The attempt is recorded. " +
	"State the fields listed below and compile again."

// StructuredCompileRequest is one compile of a DECLARED strategy.
//
// Every identifier on it is this service's, for the reason persist's comment
// gives: a backend is trusted for the IR and for nothing around it. The
// constraints are the caller's own words in the only sense that matters here —
// they are a structured document the caller filled in, passed through verbatim.
type StructuredCompileRequest struct {
	StrategyID     strategy.StrategyID
	RequestID      string
	AttemptNo      int
	OwnerAccountID string
	OwnerUserID    string
	// Version is the version number this compile would produce, reserved by the
	// service before the call.
	Version int
	// Constraints is strategies.constraints verbatim: the structured strategy
	// the owner declared. It is NOT the description, and the description is not
	// passed to a structured compiler at all.
	Constraints json.RawMessage
	// Refs is the registry snapshot the pipeline's TYPE and RISK_COMPAT stages
	// validate against.
	Refs strategy.ValidationRefs
	// Registry is what ValidationRefs does not carry and a compiler that must
	// resolve a NAME needs: the canonical name of each instrument and the
	// venues that list it.
	Registry StructuredRegistry
	// Environment is the deployment that is compiling, recorded on the version
	// so migration 00812's CHECK can refuse a sandbox row in PROD.
	Environment string
}

// StructuredRegistry is the name-resolution half of the registry.
//
// strategy.ValidationRefs is keyed by identifier because Validate checks a
// document that already names identifiers. A person names an instrument the way
// it is written on the screen — "SOL/USDC" — so a compiler that turns their
// words into a document has to resolve that name first, and resolve it against
// the registry rather than against a convention.
type StructuredRegistry struct {
	// InstrumentIDsByCanonicalName maps "SOL/USDC" to the instruments row id.
	InstrumentIDsByCanonicalName map[string]string
	// VenueCodesByInstrumentID lists, per instrument id, the venue codes whose
	// listing of that instrument may take new actions.
	VenueCodesByInstrumentID map[string][]string
}

// StructuredCompilerBackend compiles a fully declared structured strategy.
//
// It is a SECOND seam rather than a widening of CompilerBackend, and the reason
// is that the two read different things. CompilerBackend is handed the text a
// person wrote; this one is handed a document they filled in and is never
// handed the text at all. Folding them into one interface would put the
// description within reach of a compiler whose entire claim is that it cannot
// see it.
type StructuredCompilerBackend interface {
	// CompileStructured returns exactly what CompileNL returns: one attempt, a
	// version on success, and never an ACCEPTED one.
	CompileStructured(ctx context.Context, req StructuredCompileRequest) (strategy.Result, error)
	// CompilerName identifies the compiler this deployment has, so the product
	// can say which one answered rather than implying there is only one.
	CompilerName() string
	// SandboxCompiler reports whether everything this compiler produces is a
	// rehearsal. A true answer is rendered as a label everywhere a version or
	// an agent built from it is shown.
	SandboxCompiler() bool
}

// StructuredRefsLoader is a RefsLoader that can also answer the name-resolution
// questions a structured compiler asks.
//
// A structured backend is refused at construction unless the loader beside it
// implements this, which is the same rule the pair already lives under: a
// compiler with no registry would reject every instrument a person named and
// blame them for it.
type StructuredRefsLoader interface {
	RefsLoader
	Registry(ctx context.Context, q db.Querier, now time.Time) (StructuredRegistry, error)
}

// CompilerInfo is what this deployment can compile with, as the product reports
// it.
//
// The web asks one question before it draws the create-agent form — "can this
// deployment compile, and is what it produces real?" — and a boolean could only
// answer the first half. A page that showed a sandbox-compiled strategy without
// the second half would be showing a rehearsal as though it were the product.
type CompilerInfo struct {
	// Name is empty when there is no compiler.
	Name string
	// Sandbox is true when every version this compiler produces is a rehearsal.
	Sandbox bool
	// Structured is true when the compiler reads declared fields rather than
	// natural language, so the page knows to show a form instead of relying on
	// the description alone.
	Structured bool
}
