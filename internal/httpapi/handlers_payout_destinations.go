package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/payout"
)

// Payout destinations and the pre-commitment quote (goal §19, §22, §25).
//
// The rule these routes exist to enforce is stated once and enforced three
// times: **Nodal never accepts a bank account number, a card number, an IBAN, a
// routing number, a private key or a seed phrase.** The request schema has no
// field for one, `internal/payout.ValidateDestinationToken` refuses an input
// that looks like one, and the table stores a provider token and a mask.
//
// §25 also calls a destination change a high-risk operation, so registering and
// disabling one both require a recent strong authentication (authz.go marks
// them StepUp).

func toAPIDestination(d payout.Destination) api.PayoutDestination {
	out := api.PayoutDestination{
		DestinationId: toUUID(d.ID),
		AccountId:     toUUID(d.AccountID),
		Kind:          api.PayoutDestinationKind(d.Kind),
		Provider:      d.Provider,
		Status:        api.PayoutDestinationStatus(d.Status),
		Sandbox:       d.Sandbox,
		Usable:        ptr(d.Status.Usable()),
		CreatedAt:     d.CreatedAt.UTC(),
	}
	if d.DisplayLabel != "" {
		out.DisplayLabel = ptr(d.DisplayLabel)
	}
	if d.MaskedDisplay != "" {
		out.MaskedDisplay = ptr(d.MaskedDisplay)
	}
	if d.Currency != "" {
		out.Currency = ptr(d.Currency)
	}
	if d.Country != "" {
		out.Country = ptr(d.Country)
	}
	if d.Region != "" {
		out.Region = ptr(d.Region)
	}
	if d.VerifiedAt != nil {
		out.VerifiedAt = ptr(d.VerifiedAt.UTC())
	}
	// There is deliberately no provider_reference field. The token is the
	// provider's credential for moving money to somebody's account, and a
	// response that echoed it would put it in every browser cache and every
	// proxy log that ever saw this route.
	return out
}

func toAPIProvenance(slices []payout.ProvenanceSlice) []api.PayoutProvenanceSlice {
	out := make([]api.PayoutProvenanceSlice, 0, len(slices))
	for _, s := range slices {
		item := api.PayoutProvenanceSlice{
			Origin:          api.CreditOrigin(s.Origin),
			Quantity:        qtyString(s.Quantity),
			ConsumptionRank: s.ConsumptionRank,
		}
		if s.OriginFloor != "" {
			item.OriginFloor = ptr(api.CreditOrigin(s.OriginFloor))
		}
		if s.Returned {
			item.Returned = ptr(true)
		}
		out = append(out, item)
	}
	return out
}

// countryAlpha2 is the shape a country code has to have before the provider
// can be asked anything about it.
var countryAlpha2 = regexp.MustCompile(`^[A-Z]{2}$`)

// GetMePayoutDestinations lists where an account has asked value to be sent.
func (s *Server) GetMePayoutDestinations(ctx context.Context, request api.GetMePayoutDestinationsRequestObject) (api.GetMePayoutDestinationsResponseObject, error) {
	if s.opts.Ports.Conversion == nil {
		return nil, errNotWired("payout destinations")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	limit := defaultPageLimit
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	list, err := s.opts.Ports.Conversion.Destinations(ctx, accountID, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.PayoutDestination, 0, len(list))
	for _, d := range list {
		items = append(items, toAPIDestination(d))
	}
	return api.GetMePayoutDestinations200JSONResponse(items), nil
}

// PostMePayoutDestinations registers a destination from a provider token.
func (s *Server) PostMePayoutDestinations(ctx context.Context, request api.PostMePayoutDestinationsRequestObject) (api.PostMePayoutDestinationsResponseObject, error) {
	if s.opts.Ports.Conversion == nil {
		return nil, errNotWired("payout destinations")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	kind := payout.DestinationKind(strings.ToUpper(strings.TrimSpace(string(request.Body.Kind))))
	if !kind.Valid() {
		return nil, validationError("kind", "kind must be one of BANK, CARD_PUSH, FIAT_WALLET, CRYPTO_WALLET")
	}
	// Refused here as well as in the domain, because the whole point is that
	// the value never becomes ours: catching it at the boundary means it is
	// not carried through three more function calls and into an error message
	// on the way to being refused.
	if err := payout.ValidateDestinationToken(request.Body.ProviderToken); err != nil {
		return nil, err
	}
	cmd := AddPayoutDestination{
		AccountID:      accountID,
		Kind:           kind,
		ProviderToken:  strings.TrimSpace(request.Body.ProviderToken),
		IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID:  observability.CorrelationID(ctx),
	}
	if request.Body.DisplayLabel != nil {
		cmd.DisplayLabel = strings.TrimSpace(*request.Body.DisplayLabel)
	}
	if request.Body.MaskedDisplay != nil {
		cmd.MaskedDisplay = strings.TrimSpace(*request.Body.MaskedDisplay)
		if err := payout.ValidateMaskedDisplay(cmd.MaskedDisplay); err != nil {
			return nil, err
		}
	}
	if request.Body.Currency != nil {
		cmd.Currency = strings.ToUpper(strings.TrimSpace(*request.Body.Currency))
	}
	// Required (D-122). `CanPayRecipient` is asked unconditionally now, and it
	// cannot be answered about a recipient whose country nobody stated: the
	// version of this route that let the field be omitted accepted -- and
	// marked VERIFIED -- destinations the provider had already said in as many
	// words it could not pay (F-228).
	cmd.Country = strings.ToUpper(strings.TrimSpace(request.Body.Country))
	if !countryAlpha2.MatchString(cmd.Country) {
		return nil, validationError("country",
			"country must be an ISO 3166-1 alpha-2 code; the provider is asked whether it can pay a recipient there")
	}
	if request.Body.Region != nil {
		cmd.Region = strings.ToUpper(strings.TrimSpace(*request.Body.Region))
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PayoutDestination, commandMeta, error) {
			dest, cerr := s.opts.Ports.Conversion.AddDestination(ctx, cmd)
			if cerr != nil {
				return api.PayoutDestination{}, commandMeta{}, cerr
			}
			return toAPIDestination(dest), commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "payout_destination",
				ResourceID:   dest.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostMePayoutDestinations200JSONResponse(res.Value), nil
	}
	return api.PostMePayoutDestinations201JSONResponse(res.Value), nil
}

// DeleteMePayoutDestinationsDestinationId stops using a destination.
//
// It disables rather than deletes. A destination value has left through is
// financial history, and a row that can be removed is a row an incident cannot
// be reconstructed from. The disable is one-way: adding the same destination
// again is a new registration with its own creation time, which is what lets a
// cooldown on a changed destination be a fact about the data rather than a
// field somebody remembers to reset.
func (s *Server) DeleteMePayoutDestinationsDestinationId(ctx context.Context, request api.DeleteMePayoutDestinationsDestinationIdRequestObject) (api.DeleteMePayoutDestinationsDestinationIdResponseObject, error) {
	if s.opts.Ports.Conversion == nil {
		return nil, errNotWired("payout destinations")
	}
	accountID, err := accountScopeWrite(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	destinationID, err := payout.ParseDestinationID(request.DestinationId.String())
	if err != nil || destinationID.IsZero() {
		return nil, validationError("destinationId", "destinationId must be a canonical UUID")
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PayoutDestination, commandMeta, error) {
			dest, open, cerr := s.opts.Ports.Conversion.DisableDestination(ctx, accountID, destinationID)
			if cerr != nil {
				return api.PayoutDestination{}, commandMeta{}, cerr
			}
			body := toAPIDestination(dest)
			// Only on this route, and only when there is something to say. A
			// payout still pointing at a destination that has just stopped
			// being usable is refused at submission and keeps its value
			// reserved until somebody cancels it; leaving the holder to
			// discover that is how a person's money goes quiet (F-263).
			if len(open) > 0 {
				ids := make([]api.UUID, 0, len(open))
				for _, r := range open {
					ids = append(ids, toUUID(r))
				}
				body.OpenPayoutIds = &ids
			}
			return body, commandMeta{
				Status:       http.StatusOK,
				ResourceType: "payout_destination",
				ResourceID:   dest.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.DeleteMePayoutDestinationsDestinationId200JSONResponse(res.Value), nil
}

// PostPayoutsQuote is the pre-commitment step of §19 and §22.
//
// It answers three questions a person is entitled to have answered before they
// commit: what leaves, what the provider charges, and what actually arrives.
// The minimum is judged NET of fees, because sub-minimum dust is destroyed
// rather than returned, and the response says so with `minimum_ok` rather than
// silently rounding.
//
// Nothing is reserved. A quote is a statement about what WOULD happen; the
// reservation happens when POST /payouts names the quote.
func (s *Server) PostPayoutsQuote(ctx context.Context, request api.PostPayoutsQuoteRequestObject) (api.PostPayoutsQuoteResponseObject, error) {
	if s.opts.Ports.Conversion == nil {
		return nil, errNotWired("payout quotes")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	destinationID, err := payout.ParseDestinationID(request.Body.DestinationId.String())
	if err != nil || destinationID.IsZero() {
		return nil, validationError("destination_id", "destination_id must be a canonical UUID")
	}
	amount, err := money.ParseQuantity(request.Body.Amount)
	if err != nil {
		return nil, validationError("amount", "amount must be an integer string of base units")
	}
	if amount.Sign() <= 0 {
		return nil, validationError("amount", "amount must be positive")
	}

	cmd := CreatePayoutQuote{
		AccountID:      accountID,
		DestinationID:  destinationID,
		Amount:         amount,
		IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID:  observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PayoutQuote, commandMeta, error) {
			quote, provenance, cerr := s.opts.Ports.Conversion.Quote(ctx, cmd)
			if cerr != nil {
				return api.PayoutQuote{}, commandMeta{}, cerr
			}
			return toAPIQuote(quote, provenance), commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "payout_quote",
				ResourceID:   quote.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostPayoutsQuote200JSONResponse(res.Value), nil
	}
	return api.PostPayoutsQuote201JSONResponse(res.Value), nil
}

func toAPIQuote(q payout.Quote, provenance []payout.ProvenanceSlice) api.PayoutQuote {
	out := api.PayoutQuote{
		QuoteId:            toUUID(q.ID),
		AccountId:          toUUID(q.AccountID),
		DestinationId:      toUUID(q.DestinationID),
		Provider:           q.Provider,
		GrossQuantity:      qtyString(q.GrossQuantity),
		FeeQuantity:        qtyString(q.FeeQuantity),
		NetQuantity:        qtyString(q.NetQuantity),
		Currency:           q.Currency,
		GrossAmountMinor:   q.GrossAmountMinor,
		FeeAmountMinor:     q.FeeAmountMinor,
		NetAmountMinor:     q.NetAmountMinor,
		MinimumAmountMinor: ptr(q.MinimumAmountMinor),
		MinimumOk:          q.MinimumOK,
		ExpiresAt:          q.ExpiresAt.UTC(),
		Sandbox:            q.Sandbox,
		CreatedAt:          ptr(q.CreatedAt.UTC()),
		Provenance:         ptr(toAPIProvenance(provenance)),
	}
	if q.PricingVersion != "" {
		out.PricingVersion = ptr(q.PricingVersion)
	}
	if q.FeeModelVersion != "" {
		out.FeeModelVersion = ptr(q.FeeModelVersion)
	}
	if q.PolicyVersion != "" {
		out.PolicyVersion = ptr(q.PolicyVersion)
	}
	if q.ConsumedAt != nil {
		out.ConsumedAt = ptr(q.ConsumedAt.UTC())
	}
	return out
}
