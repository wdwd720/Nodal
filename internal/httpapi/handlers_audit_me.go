package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/security"
)

// GetMeAudit pages the caller's own security and account history (product goal
// §52: "every consequential action should still produce evidence").
//
// # Why this is not "the audit trail"
//
// internal/audit's `audit_events` is a hash-chained record of what the platform
// did, with operator identities, policy versions, evidence references and
// payloads on every row. Handing that to a browser would publish how the
// controls work and who operated them. What a customer is owed is the record of
// what happened to THEM, so this reads the same rows through a narrow
// projection: the action, what it was about, the actor's TYPE, and when.
//
// # Why it reads two tables
//
// A person's history is in two places and neither alone is honest. Sign-ins,
// step-ups and session events are `security_events`, keyed by user. What was
// done to their accounts -- a freeze, a payout, a moderation verdict -- is
// `audit_events`, keyed by the account stream. The union is what a person means
// by "what happened on my account", and the keyset runs over both.
//
// The financial history is deliberately NOT here: the ledger, the activity
// timeline and the export already render it, in the shapes that carry exact
// figures. Repeating it in a third shape is how two answers to one question
// start to disagree.
func (s *Server) GetMeAudit(ctx context.Context, request api.GetMeAuditRequestObject) (api.GetMeAuditResponseObject, error) {
	if s.opts.Ports.MeAudit == nil {
		return nil, errNotWired("the account history")
	}
	userID, err := meUserID(ctx)
	if err != nil {
		return nil, err
	}
	p, _ := security.PrincipalFrom(ctx)
	// The principal's own account ids, and no others. An operator holding
	// account:read_any reaches their OWN history here too: this route has no
	// "any" mode, because a customer's own view is not an investigation tool.
	accountIDs := append([]string(nil), p.AccountIDs...)

	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.opts.Ports.MeAudit.Audit(ctx, userID, accountIDs,
		cursor, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	items := make([]api.MeAuditEntry, 0, len(page.Items))
	for _, it := range page.Items {
		items = append(items, api.MeAuditEntry{
			Id:           it.ID,
			Source:       api.MeAuditEntrySource(it.Source),
			Action:       it.Action,
			Severity:     strPtr(it.Severity),
			ResourceType: strPtr(it.ResourceType),
			ResourceId:   strPtr(it.ResourceID),
			ActorType:    strPtr(it.ActorType),
			Ip:           strPtr(it.IP),
			UserAgent:    strPtr(it.UserAgent),
			OccurredAt:   it.OccurredAt.UTC(),
		})
	}
	return api.GetMeAudit200JSONResponse(api.MeAuditPage{
		Items:      items,
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}
