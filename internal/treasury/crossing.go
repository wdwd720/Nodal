package treasury

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/security"
)

// CrossingRuleRequest changes whether house books may trade with customers on
// one instrument's book. The order book refuses it by default; this is the
// only way the treasury loosens that, and it is always disclosed.
type CrossingRuleRequest struct {
	InstrumentID instruments.InstrumentID
	Allow        bool
	// Disclosure is what customers are told about the house trading against
	// them on this book. It must say the activity is simulated.
	Disclosure string
	Reason     string
	Actor      econ.Actor
}

// CrossingDisclosure is the record of one crossing rule change.
type CrossingDisclosure struct {
	ID             DisclosureID
	InstrumentID   instruments.InstrumentID
	BookID         orders.BookID
	RuleVersion    int
	Allow          bool
	Disclosure     string
	Reason         string
	Actor          econ.Actor
	EvidenceRef    string
	EvidenceSHA256 []byte
	DisclosedAt    time.Time
}

// SetCrossingRule records the next version of a book's crossing rule through
// orders.Service.SetCrossingRule, together with its customer-facing
// disclosure, archived evidence and an admin audit event. Only an operator
// changes it; the database refuses a disclosure that names no matching rule
// version or does not say "simulated" (TY006).
func (s *Service) SetCrossingRule(ctx context.Context, tx pgx.Tx, r CrossingRuleRequest) (CrossingDisclosure, error) {
	if r.Actor.Type != security.ActorOperator || strings.TrimSpace(r.Actor.ID) == "" {
		return CrossingDisclosure{}, rejection(errs.CodeForbidden, ReasonNotAuthorized, "only an operator changes a crossing rule")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return CrossingDisclosure{}, errs.New(errs.CodeValidationFailed, "treasury: a crossing rule change needs a reason")
	}
	d := strings.TrimSpace(r.Disclosure)
	if len(d) < 20 || len(d) > 2000 || !strings.Contains(strings.ToLower(d), "simulated") {
		return CrossingDisclosure{}, errs.New(errs.CodeValidationFailed,
			"treasury: a crossing disclosure of 20..2000 characters that says the activity is simulated is required")
	}
	rule, err := s.orders.SetCrossingRule(ctx, tx, r.InstrumentID, r.Allow, r.Actor, r.Reason)
	if err != nil {
		return CrossingDisclosure{}, err
	}
	now := s.clk.Now().UTC()
	c := CrossingDisclosure{
		ID: id.New[disclosureKind](), InstrumentID: r.InstrumentID, BookID: rule.BookID, RuleVersion: rule.Version,
		Allow: r.Allow, Disclosure: d, Reason: r.Reason, Actor: r.Actor, DisclosedAt: now,
	}
	doc := map[string]any{
		"kind": "treasury.crossing_disclosure.v1", "instrument_id": r.InstrumentID.String(), "book_id": rule.BookID.String(),
		"rule_version": rule.Version, "allow_house_customer_crossing": r.Allow, "disclosure": d, "reason": r.Reason,
		"actor_type": string(r.Actor.Type), "actor_id": r.Actor.ID, "disclosed_at": now, "value_domain": "SIMULATED",
	}
	ev, err := econ.RecordEvidence(ctx, tx, econ.Evidence{
		Key: "treasury/crossing/" + rule.BookID.String() + "/" + c.ID.String(), Document: doc,
	})
	if err != nil {
		return CrossingDisclosure{}, err
	}
	c.EvidenceRef, c.EvidenceSHA256 = ev.URI, ev.SHA256
	if _, err := tx.Exec(ctx, `INSERT INTO treasury_crossing_disclosures (id, instrument_id, book_id, rule_version, allow, disclosure,
		reason, actor_type, actor_id, evidence_ref, evidence_sha256, disclosed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		c.ID, r.InstrumentID, rule.BookID, rule.Version, r.Allow, d, r.Reason, r.Actor.Type, r.Actor.ID, ev.URI, ev.SHA256, now); err != nil {
		return CrossingDisclosure{}, dbErr("record crossing disclosure", err)
	}
	payload, err := json.Marshal(map[string]any{
		"disclosure_id": c.ID.String(), "book_id": rule.BookID.String(), "rule_version": rule.Version, "allow": r.Allow,
	})
	if err != nil {
		return CrossingDisclosure{}, errs.Wrap(err, errs.CodeInternal, "treasury: encode audit payload")
	}
	if _, err := s.audit.Append(ctx, tx, audit.Event{
		Stream: audit.AdminStream, ActorType: string(r.Actor.Type), ActorID: r.Actor.ID,
		Action: "treasury.crossing_rule.set", ResourceType: "book_crossing_rule", ResourceID: rule.BookID.String(),
		Reason: r.Reason, EvidenceRef: ev.URI, Payload: payload, OccurredAt: now,
	}); err != nil {
		return CrossingDisclosure{}, err
	}
	return c, nil
}

// CrossingDisclosures lists the disclosures of one instrument's book, oldest
// first.
func (s *Service) CrossingDisclosures(ctx context.Context, q db.Querier, instrumentID instruments.InstrumentID) ([]CrossingDisclosure, error) {
	rows, err := q.Query(ctx, `SELECT id, instrument_id, book_id, rule_version, allow, disclosure, reason, actor_type, actor_id,
		evidence_ref, evidence_sha256, disclosed_at FROM treasury_crossing_disclosures WHERE instrument_id = $1 ORDER BY rule_version`,
		instrumentID)
	if err != nil {
		return nil, dbErr("crossing disclosures", err)
	}
	defer rows.Close()
	var out []CrossingDisclosure
	for rows.Next() {
		var c CrossingDisclosure
		if err := rows.Scan(&c.ID, &c.InstrumentID, &c.BookID, &c.RuleVersion, &c.Allow, &c.Disclosure, &c.Reason, &c.Actor.Type,
			&c.Actor.ID, &c.EvidenceRef, &c.EvidenceSHA256, &c.DisclosedAt); err != nil {
			return nil, dbErr("scan crossing disclosure", err)
		}
		c.DisclosedAt = c.DisclosedAt.UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}
