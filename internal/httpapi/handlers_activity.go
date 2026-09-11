package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/activity"
	"github.com/nodal/controlplane/internal/gen/api"
)

// The unified activity timeline (product goal §16).
//
// Every item's sentence is built in internal/activity from a fixed template,
// never here and never in the browser: a summary composed in the client is a
// second implementation that eventually disagrees with the amounts beside it.
//
// Nothing provider-specific reaches this response. The reference on an item is
// a Nodal row's type and id; a provider reference, a webhook payload or an
// evidence document is not on the item, is not in the summary, and has no field
// to be put in.

// GetMeActivity returns one page of an account's timeline.
func (s *Server) GetMeActivity(ctx context.Context, request api.GetMeActivityRequestObject) (api.GetMeActivityResponseObject, error) {
	if s.opts.Ports.ActivityFeed == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	r := activity.Request{AccountID: accountID}
	if request.Params.Kind != nil {
		for _, k := range *request.Params.Kind {
			r.Kinds = append(r.Kinds, activity.Kind(k))
		}
	}
	if request.Params.Cursor != nil {
		r.Cursor = *request.Params.Cursor
	}
	if request.Params.Limit != nil {
		r.Limit = *request.Params.Limit
	}
	page, err := s.opts.Ports.ActivityFeed.Feed(ctx, r)
	if err != nil {
		return nil, err
	}
	items := make([]api.ActivityFeedItem, 0, len(page.Items))
	for _, it := range page.Items {
		items = append(items, toAPIActivityFeedItem(it))
	}
	return api.GetMeActivity200JSONResponse(api.ActivityFeedPage{
		Items:      items,
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

func toAPIActivityFeedItem(it activity.Item) api.ActivityFeedItem {
	amounts := make([]api.ActivityAmount, 0, len(it.Amounts))
	for _, a := range it.Amounts {
		amount := api.ActivityAmount{
			Unit:        api.ActivityAmountUnit(a.Unit),
			Value:       a.Value,
			Temperature: api.ValueTemperature(a.Temperature),
		}
		if a.Currency != "" {
			amount.Currency = ptr(a.Currency)
		}
		if a.Symbol != "" {
			amount.Symbol = ptr(a.Symbol)
		}
		if a.Origin != "" {
			amount.Origin = ptr(api.CreditOrigin(a.Origin))
		}
		amounts = append(amounts, amount)
	}
	out := api.ActivityFeedItem{
		Id:         it.ID,
		Kind:       api.ActivityFeedKind(it.Kind),
		OccurredAt: it.OccurredAt.UTC(),
		Summary:    it.Summary,
		Amounts:    amounts,
		Simulated:  it.Simulated,
	}
	out.Reference.Type = it.Reference.Type
	out.Reference.Id = it.Reference.ID
	if it.Status != "" {
		out.Status = ptr(it.Status)
	}
	return out
}
