// Command gateceremony performs the real three-principal capability-gate
// ceremony against a LOCAL database, so a development or load-testing
// deployment can exercise the paths a gate protects.
//
//	go run ./scripts/gateceremony -capability MARKETPLACE \
//	  -proposer alice -approver bob -activator carol \
//	  -legal LOCAL-DEV-NOT-A-LEGAL-REVIEW -contract LOCAL-DEV-NO-CONTRACT \
//	  -risk LOCAL-DEV-NOT-A-RISK-APPROVAL -security LOCAL-DEV-NOT-A-SECURITY-REVIEW \
//	  -reason "activating MARKETPLACE to measure the committed purchase path"
//
// # Why this exists, and what it is not
//
// `scripts/seedeconomy` deliberately activates nothing, and the reason it gives
// is right: a seeder that wrote an ACTIVE gate row would bypass the control,
// and one that drove the flow with invented legal, provider, risk and security
// references would fill a control with fiction, producing a gate that LOOKS
// approved.
//
// The consequence was that Domain A's committed write path could not be load
// tested at all: `test/load/internal_economy.js` measured 182 purchases refused
// CAPABILITY_NOT_APPROVED and zero committed, and the readiness report recorded
// that as a gap rather than pretending otherwise.
//
// This tool closes the gap without weakening the control, by DRIVING the
// control rather than going around it:
//
//   - It uses `gates.Admin`, so every rule applies: the state machine, the
//     permission checks, the step-up requirement, the four evidence references
//     a high-risk capability needs, and the refusal of a principal who tries to
//     approve or activate their own proposal.
//   - It takes three principal identifiers and REFUSES if they are not
//     distinct. It cannot make one person into three, and it does not try:
//     what it produces is a gate approved by three development identities, and
//     it says so in its output and in the gate's own reason.
//   - It refuses any environment but LOCAL, DEV or TEST, and any database host
//     but a local one.
//   - It requires all four evidence references as arguments with no defaults.
//     An operator who has nothing real to reference must type a string that
//     says so, and that string is what the gate carries forever.
//
// What a reader of a load-test result must therefore keep in mind: the gate was
// activated by a development ceremony, not by three humans with real evidence.
// That distinction does not affect a throughput number. It affects every
// readiness claim, which is why it is printed rather than buried.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/security"
)

// localAppDSN is the LOCAL docker-compose credential; a non-local host is
// refused below.
const localAppDSN = "postgres://cp_app:cp_app_local@127.0.0.1:5433/controlplane?sslmode=disable" // #nosec G101

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateceremony:", err)
		os.Exit(1)
	}
}

type args struct {
	capability                      string
	proposer, approver, activator   string
	legal, contract, risk, security string
	reason                          string
	expiresIn                       time.Duration
}

func run() error {
	var a args
	flag.StringVar(&a.capability, "capability", "", "the capability to activate, e.g. MARKETPLACE")
	flag.StringVar(&a.proposer, "proposer", "", "identifier of the principal proposing")
	flag.StringVar(&a.approver, "approver", "", "identifier of the principal approving")
	flag.StringVar(&a.activator, "activator", "", "identifier of the principal activating")
	flag.StringVar(&a.legal, "legal", "", "legal review reference (required; no default)")
	flag.StringVar(&a.contract, "contract", "", "provider contract reference (required; no default)")
	flag.StringVar(&a.risk, "risk", "", "risk approval reference (required; no default)")
	flag.StringVar(&a.security, "security", "", "security approval reference (required; no default)")
	flag.StringVar(&a.reason, "reason", "", "why this gate is being activated (recorded on every transition)")
	flag.DurationVar(&a.expiresIn, "expires-in", 24*time.Hour, "how long the approval version is valid")
	flag.Parse()

	if err := a.validate(); err != nil {
		return err
	}

	env := strings.ToUpper(strings.TrimSpace(os.Getenv("CP_ENV")))
	if env == "" {
		env = "LOCAL"
	}
	switch env {
	case "LOCAL", "DEV", "TEST":
	default:
		return fmt.Errorf("refusing to run a gate ceremony in environment %q: "+
			"a production gate is activated by three people through the admin plane, not by a script", env)
	}

	dsn := os.Getenv("CP_DATABASE_APP_URL")
	if dsn == "" {
		dsn = localAppDSN
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return err
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" {
		return fmt.Errorf("refusing to touch a gate on the non-local database host %q", h)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	d, err := db.Open(ctx, db.Config{URL: dsn, MaxConns: 4, MinConns: 1, AppName: "gateceremony"})
	if err != nil {
		return err
	}
	defer d.Close()

	clk := clock.System()
	admin, err := gates.NewAdmin(env, clk, gateAudit{})
	if err != nil {
		return err
	}
	capability := gates.Capability(a.capability)

	// The gate row must exist first. Bootstrap creates every declared
	// capability in DISABLED, which is what migration 00701 insists a gate is
	// born as; it is idempotent, so an existing row is left alone.
	if err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		_, berr := gates.Bootstrap(withPrincipal(ctx, "gateceremony-bootstrap", clk, "bootstrap"), tx, env)
		return berr
	}); err != nil {
		return fmt.Errorf("bootstrap gates: %w", err)
	}

	reason := a.reason + " [ACTIVATED BY scripts/gateceremony IN " + env + ": " +
		"three development identities, not three people]"

	steps := []struct {
		who  string
		what string
		run  func(ctx context.Context, tx pgx.Tx) (gates.Gate, error)
	}{
		{a.proposer, "propose", func(ctx context.Context, tx pgx.Tx) (gates.Gate, error) {
			return admin.Propose(ctx, tx, capability, gates.Proposal{
				LegalReviewRef:      a.legal,
				ProviderContractRef: a.contract,
				RiskApprovalRef:     a.risk,
				SecurityApprovalRef: a.security,
				ExpiresAt:           clk.Now().Add(a.expiresIn),
				Reason:              reason,
			})
		}},
		{a.approver, "approve", func(ctx context.Context, tx pgx.Tx) (gates.Gate, error) {
			return admin.Approve(ctx, tx, capability, reason)
		}},
		{a.activator, "activate", func(ctx context.Context, tx pgx.Tx) (gates.Gate, error) {
			return admin.Activate(ctx, tx, capability, reason)
		}},
	}

	var final gates.Gate
	for _, step := range steps {
		var g gates.Gate
		if err := d.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			g, serr = step.run(withPrincipal(ctx, step.who, clk, step.what), tx)
			return serr
		}); err != nil {
			return fmt.Errorf("%s %s as %q: %w", step.what, capability, step.who, err)
		}
		fmt.Printf("  %-9s %-12s -> %s\n", step.what, step.who, g.State)
		final = g
	}

	fmt.Println()
	fmt.Printf("%s is %s in %s.\n", capability, final.State, env)
	fmt.Printf("  approvers: %s\n", strings.Join(final.DistinctApprovers(), ", "))
	fmt.Printf("  evidence:  legal=%s contract=%s risk=%s security=%s\n", a.legal, a.contract, a.risk, a.security)
	if final.ExpiresAt != nil {
		fmt.Printf("  expires:   %s\n", final.ExpiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Println()
	fmt.Println("This gate was activated by a development ceremony. Three DEVELOPMENT")
	fmt.Println("identities agreed; three people did not. Any number measured against it")
	fmt.Println("is a number about throughput, never about approval.")
	return nil
}

func (a args) validate() error {
	missing := []string{}
	for name, v := range map[string]string{
		"capability": a.capability, "proposer": a.proposer, "approver": a.approver,
		"activator": a.activator, "legal": a.legal, "contract": a.contract,
		"risk": a.risk, "security": a.security, "reason": a.reason,
	} {
		if strings.TrimSpace(v) == "" {
			missing = append(missing, "-"+name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("these are required and have no defaults: %s\n"+
			"an evidence reference with a default is a control filled in by the tool rather than by a person",
			strings.Join(sorted(missing), " "))
	}
	if a.proposer == a.approver || a.approver == a.activator || a.proposer == a.activator {
		return fmt.Errorf("the three principals must be distinct; got proposer=%q approver=%q activator=%q\n"+
			"dual control is the control, and a tool that let one identity fill all three roles "+
			"would be the tool that removed it", a.proposer, a.approver, a.activator)
	}
	if !gates.Capability(a.capability).Valid() {
		return fmt.Errorf("unknown capability %q", a.capability)
	}
	if a.expiresIn <= 0 {
		return fmt.Errorf("-expires-in must be positive; an approval with no expiry is one nobody revisits")
	}
	return nil
}

// withPrincipal builds the operator principal a gate step requires, with the
// roles that step actually needs and no more.
//
// The two sides carry DIFFERENT authority, and that is the control rather than
// a detail:
//
//   - proposing needs `gate:propose`, a standing permission held by RISK,
//     COMPLIANCE and ADMIN;
//   - approving and activating need `gate:approve`, which is a dual-control
//     permission NO standing role holds. It exists only while a live
//     BREAK_GLASS elevation does.
//
// Giving every step break-glass would have papered over that difference. The
// first version of this tool did, and the propose step failed with "missing
// permission gate:propose" — the roles were wrong in the safe direction, and
// the state machine said so.
//
// These are development identities named on the command line, and every
// transition row and audit event records them by the name the operator typed.
func withPrincipal(ctx context.Context, subject string, clk clock.Clock, step string) context.Context {
	p := security.Principal{
		SubjectID: subject,
		ActorType: security.ActorOperator,
		AuthTime:  clk.Now(),
		AMR:       []string{"mfa"},
	}
	if step == "propose" {
		p.Roles = []security.Role{security.RoleRisk}
	} else {
		until := clk.Now().Add(time.Hour)
		p.Roles = []security.Role{security.RoleOperations, security.RoleBreakGlass}
		p.BreakGlassUntil = &until
	}
	return security.WithPrincipal(ctx, p)
}

// gateAudit appends the gate's audit event. The Admin requires an appender and
// refuses to transition without one, which is the point: a gate change that
// left no audit row would be a gate change nobody could find later.
type gateAudit struct{}

func (gateAudit) Append(ctx context.Context, tx pgx.Tx, e gates.AuditEvent) error {
	_, err := audit.NewWriter().Append(ctx, tx, audit.Event{
		Stream:       audit.AdminStream,
		ActorType:    string(e.ActorType),
		ActorID:      e.ActorID,
		Action:       "capability_gate." + e.Action,
		ResourceType: "capability_gate",
		ResourceID:   e.ResourceID,
		Reason:       e.Reason,
		OccurredAt:   e.OccurredAt,
	})
	return err
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
