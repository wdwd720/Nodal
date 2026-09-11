package httpapi

import (
	"time"

	"github.com/google/uuid"
	"github.com/oapi-codegen/nullable"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// The conversions below are pure shape changes. Money never becomes a number:
// USD renders through money.USD.String() ("1234.56") and quantities through
// money.Quantity.String() (exact base units), exactly as the spec and the
// TypeScript client require.

func toUUID[K any](v id.ID[K]) api.UUID {
	return uuid.UUID(v.Bytes())
}

func uuidPtr[K any](v id.ID[K]) *api.UUID {
	if v.IsZero() {
		return nil
	}
	u := toUUID(v)
	return &u
}

// parseUUIDText parses a canonical UUID string into an api.UUID pointer,
// returning nil for the empty string. Unparsable text yields nil rather than
// a fabricated identifier.
func parseUUIDText(s string) *api.UUID {
	if s == "" {
		return nil
	}
	u, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return &u
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timePtr(t *time.Time) *api.Timestamp {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}

func usdPtr(u *money.USD) *api.USD {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

func qtyPtr(q *money.Quantity) *api.Quantity {
	if q == nil {
		return nil
	}
	s := q.String()
	return &s
}

func nextCursor(c string) nullable.Nullable[string] {
	if c == "" {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(c)
}

// pageLimit clamps a client-supplied limit to the spec's bounds.
func pageLimit(l *api.Limit, def, maxLimit int) int {
	if l == nil {
		return def
	}
	switch {
	case *l < 1:
		return 1
	case *l > maxLimit:
		return maxLimit
	default:
		return *l
	}
}

// --- accounts ---------------------------------------------------------------

func toAPIAccount(a accounts.Account) api.Account {
	return api.Account{
		Id: toUUID(a.ID),
		// The owning user, so an operator surface can go from an account to the
		// person who holds it without guessing. It is read-only: the only route
		// that sets an owner is the first login, in internal/identity.
		OwnerUserId:  uuidPtr(a.OwnerUserID),
		Kind:         api.AccountKind(a.Kind),
		Status:       api.AccountStatus(a.Status),
		StatusReason: strPtr(a.StatusReason),
		CreatedAt:    a.CreatedAt.UTC(),
	}
}

func toAPIAccounts(in []accounts.Account) []api.Account {
	out := make([]api.Account, 0, len(in))
	for _, a := range in {
		out = append(out, toAPIAccount(a))
	}
	return out
}

// stepUpWindow is the window the caller is TOLD about, and it must be the one
// authorize enforces.
//
// It used to be the package constant while authorize used
// effectiveStepUpMaxAge(configured), which takes the minimum. Under the
// deployed CP_AUTH_STEP_UP_MAX_AGE=5m the API told an operator their step-up
// was good for fifteen minutes and the boundary refused after five -- during a
// three-principal gate ceremony, a control misreporting its own window
// (F-104).
func toAPIPrincipal(p security.Principal, stepUpWindow time.Duration) api.Principal {
	roles := make([]string, 0, len(p.Roles))
	for _, r := range p.Roles {
		roles = append(roles, string(r))
	}
	accountIDs := make([]api.UUID, 0, len(p.AccountIDs))
	for _, a := range p.AccountIDs {
		if u := parseUUIDText(a); u != nil {
			accountIDs = append(accountIDs, *u)
		}
	}
	subject := uuid.Nil
	if u := parseUUIDText(p.SubjectID); u != nil {
		subject = *u
	}
	amr := p.AMR
	if amr == nil {
		amr = []string{}
	}
	out := api.Principal{
		SubjectId:  subject,
		ActorType:  api.PrincipalActorType(p.ActorType),
		Roles:      roles,
		AccountIds: accountIDs,
		AuthTime:   p.AuthTime.UTC(),
		Amr:        amr,
	}
	if security.HasStrongAMR(p.AMR) && !p.AuthTime.IsZero() {
		until := p.AuthTime.Add(stepUpWindow).UTC()
		out.StepUpValidUntil = &until
	}
	// A live break-glass elevation reports when it expires. The session has
	// carried the value since 00011 and the roles array already names
	// BREAK_GLASS; only the expiry was missing, which left a console able to say
	// that an elevation existed but not how long it had left.
	if p.BreakGlassUntil != nil {
		until := p.BreakGlassUntil.UTC()
		out.BreakGlassUntil = &until
	}
	return out
}

func toAPISessions(in []auth.Summary, currentID string) []api.SessionSummary {
	out := make([]api.SessionSummary, 0, len(in))
	for _, s := range in {
		item := api.SessionSummary{
			CreatedAt:   s.CreatedAt.UTC(),
			ExpiresAt:   s.ExpiresAt.UTC(),
			LastSeenAt:  s.LastSeenAt.UTC(),
			Ip:          strPtr(s.IP),
			UserAgent:   strPtr(s.UserAgent),
			DeviceLabel: strPtr(s.DeviceLabel),
			RevokedAt:   timePtr(s.RevokedAt),
		}
		if u := parseUUIDText(s.ID); u != nil {
			item.Id = *u
		}
		if s.ID == currentID {
			cur := true
			item.Current = &cur
		}
		out = append(out, item)
	}
	return out
}

// --- buying power -----------------------------------------------------------

func toAPIBuyingPower(b buyingpower.BuyingPower) api.BuyingPower {
	out := api.BuyingPower{
		PortfolioValue: b.PortfolioValue.String(),
		BuyingPower:    b.BuyingPower.String(),
		AvailableNow:   b.AvailableNow.String(),
		Reserved:       b.Reserved.String(),
		Pending:        b.Pending.String(),
		Withdrawable:   b.Withdrawable.String(),
		PolicyVersion:  b.PolicyVersion,
		AsOf:           b.AsOf.UTC(),
		Purpose:        api.BuyingPowerPurpose(b.Purpose),
	}
	for _, u := range b.UnderlyingBalances {
		out.UnderlyingBalances = append(out.UnderlyingBalances, struct {
			Asset    api.UUID     `json:"asset"`
			Decimals int          `json:"decimals"`
			PriceRef string       `json:"price_ref"`
			Quantity api.Quantity `json:"quantity"`
			Status   string       `json:"status"`
			Symbol   string       `json:"symbol"`
			UsdValue api.USD      `json:"usd_value"`
		}{
			Asset:    toUUID(u.AssetID),
			Decimals: int(u.Decimals),
			PriceRef: u.PriceRef,
			Quantity: u.Quantity.String(),
			Status:   u.Status,
			Symbol:   u.Symbol,
			UsdValue: u.USDValue.String(),
		})
	}
	if out.UnderlyingBalances == nil {
		out.UnderlyingBalances = []struct {
			Asset    api.UUID     `json:"asset"`
			Decimals int          `json:"decimals"`
			PriceRef string       `json:"price_ref"`
			Quantity api.Quantity `json:"quantity"`
			Status   string       `json:"status"`
			Symbol   string       `json:"symbol"`
			UsdValue api.USD      `json:"usd_value"`
		}{}
	}
	for _, h := range b.Haircuts {
		out.Haircuts = append(out.Haircuts, struct {
			Asset     api.UUID `json:"asset"`
			FactorBps api.BPS  `json:"factor_bps"`
			Reason    string   `json:"reason"`
		}{
			Asset:     toUUID(h.AssetID),
			FactorBps: int(h.FactorBPS),
			Reason:    h.Reason,
		})
	}
	if out.Haircuts == nil {
		out.Haircuts = []struct {
			Asset     api.UUID `json:"asset"`
			FactorBps api.BPS  `json:"factor_bps"`
			Reason    string   `json:"reason"`
		}{}
	}
	for _, rst := range b.Restrictions {
		item := struct {
			Asset    *api.UUID                        `json:"asset,omitempty"`
			Blocking bool                             `json:"blocking"`
			Code     string                           `json:"code"`
			Detail   string                           `json:"detail"`
			Scope    api.BuyingPowerRestrictionsScope `json:"scope"`
		}{
			Blocking: rst.Blocking,
			Code:     string(rst.Code),
			Detail:   rst.Detail,
			Scope:    api.BuyingPowerRestrictionsScope(rst.Scope),
		}
		if !rst.AssetID.IsZero() {
			item.Asset = uuidPtr(rst.AssetID)
		}
		out.Restrictions = append(out.Restrictions, item)
	}
	if out.Restrictions == nil {
		out.Restrictions = []struct {
			Asset    *api.UUID                        `json:"asset,omitempty"`
			Blocking bool                             `json:"blocking"`
			Code     string                           `json:"code"`
			Detail   string                           `json:"detail"`
			Scope    api.BuyingPowerRestrictionsScope `json:"scope"`
		}{}
	}
	return out
}

// --- holdings ---------------------------------------------------------------

func toAPIHoldings(v HoldingsView) api.HoldingsResponse {
	out := api.HoldingsResponse{AsOf: v.AsOf.UTC()}
	for _, h := range v.Holdings {
		out.Holdings = append(out.Holdings, struct {
			Asset            api.UUID     `json:"asset"`
			Chain            *string      `json:"chain,omitempty"`
			CostBasisUsd     api.USD      `json:"cost_basis_usd"`
			Decimals         int          `json:"decimals"`
			Location         string       `json:"location"`
			MintAddress      *string      `json:"mint_address,omitempty"`
			PriceRef         *string      `json:"price_ref,omitempty"`
			Quantity         api.Quantity `json:"quantity"`
			RealizedPnlUsd   *api.USD     `json:"realized_pnl_usd,omitempty"`
			Symbol           string       `json:"symbol"`
			UnrealizedPnlUsd api.USD      `json:"unrealized_pnl_usd"`
			UsdMark          api.USD      `json:"usd_mark"`
		}{
			Asset:            toUUID(h.AssetID),
			Chain:            strPtr(h.Chain),
			CostBasisUsd:     h.CostBasisUSD.String(),
			Decimals:         int(h.Decimals),
			Location:         h.CustodyAddress,
			MintAddress:      strPtr(h.MintAddress),
			PriceRef:         strPtr(h.PriceRef),
			Quantity:         h.Quantity.String(),
			RealizedPnlUsd:   usdPtr(h.RealizedUSD),
			Symbol:           h.Symbol,
			UnrealizedPnlUsd: h.UnrealizedUSD.String(),
			UsdMark:          h.USDMark.String(),
		})
	}
	if out.Holdings == nil {
		out.Holdings = []struct {
			Asset            api.UUID     `json:"asset"`
			Chain            *string      `json:"chain,omitempty"`
			CostBasisUsd     api.USD      `json:"cost_basis_usd"`
			Decimals         int          `json:"decimals"`
			Location         string       `json:"location"`
			MintAddress      *string      `json:"mint_address,omitempty"`
			PriceRef         *string      `json:"price_ref,omitempty"`
			Quantity         api.Quantity `json:"quantity"`
			RealizedPnlUsd   *api.USD     `json:"realized_pnl_usd,omitempty"`
			Symbol           string       `json:"symbol"`
			UnrealizedPnlUsd api.USD      `json:"unrealized_pnl_usd"`
			UsdMark          api.USD      `json:"usd_mark"`
		}{}
	}
	return out
}

// --- ledger -----------------------------------------------------------------

func toAPIJournalTransaction(t ledger.Transaction) api.JournalTransaction {
	out := api.JournalTransaction{
		Id:          toUUID(t.ID),
		Kind:        string(t.Kind),
		EffectiveAt: t.EffectiveAt.UTC(),
		PostedAt:    t.PostedAt.UTC(),
		Description: strPtr(t.Description),
		ReasonCode:  strPtr(t.ReasonCode),
		ContentHash: hexString(t.ContentHash),
		Reference: struct {
			Id   string `json:"id"`
			Type string `json:"type"`
		}{Id: t.Reference.ID, Type: t.Reference.Type},
	}
	if t.ReversalOf != nil {
		out.ReversalOf = uuidPtr(*t.ReversalOf)
	}
	for _, e := range t.Entries {
		entry := struct {
			AccountCode string                            `json:"account_code"`
			Asset       api.UUID                          `json:"asset"`
			Quantity    api.Quantity                      `json:"quantity"`
			Seq         int                               `json:"seq"`
			Side        api.JournalTransactionEntriesSide `json:"side"`
			UsdValue    *api.USD                          `json:"usd_value,omitempty"`
		}{
			AccountCode: string(e.Account.Code),
			Asset:       toUUID(e.Account.AssetID),
			Quantity:    e.Quantity.String(),
			Seq:         int(e.Seq),
			Side:        api.JournalTransactionEntriesSide(e.Side),
		}
		if e.USDValueMinor != nil {
			v := money.USDFromMinor(*e.USDValueMinor).String()
			entry.UsdValue = &v
		}
		out.Entries = append(out.Entries, entry)
	}
	if out.Entries == nil {
		out.Entries = []struct {
			AccountCode string                            `json:"account_code"`
			Asset       api.UUID                          `json:"asset"`
			Quantity    api.Quantity                      `json:"quantity"`
			Seq         int                               `json:"seq"`
			Side        api.JournalTransactionEntriesSide `json:"side"`
			UsdValue    *api.USD                          `json:"usd_value,omitempty"`
		}{}
	}
	return out
}

const hexDigits = "0123456789abcdef"

func hexString(b []byte) string {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return string(out)
}

// --- markets ----------------------------------------------------------------

func toAPIAsset(a assets.Asset) api.Asset {
	out := api.Asset{
		Id:           toUUID(a.ID),
		Chain:        a.Chain,
		MintAddress:  a.MintAddress,
		Kind:         api.AssetKind(a.Kind),
		Symbol:       a.Symbol,
		Name:         a.Name,
		Decimals:     int(a.Decimals),
		IsStablecoin: a.IsStablecoin,
		PegCurrency:  strPtr(a.PegCurrency),
		RiskClass:    string(a.RiskClass),
		Status:       api.AssetStatus(a.Status),
	}
	if len(a.TokenExtensions) > 0 {
		ext := append([]string(nil), a.TokenExtensions...)
		out.TokenExtensions = &ext
	}
	return out
}

func toAPIInstrument(i instruments.Instrument) api.Instrument {
	out := api.Instrument{
		Id:              toUUID(i.ID),
		Type:            api.InstrumentType(i.Type),
		CanonicalName:   i.CanonicalName,
		SettlementAsset: toUUID(i.SettlementAssetID),
		RiskClass:       string(i.RiskClass),
		Status:          api.InstrumentStatus(i.Status),
		ActiveFrom:      i.ActiveFrom.UTC(),
		ActiveUntil:     timePtr(i.ActiveUntil),
	}
	if i.BaseAssetID != nil {
		out.BaseAsset = uuidPtr(*i.BaseAssetID)
	}
	if i.QuoteAssetID != nil {
		out.QuoteAsset = uuidPtr(*i.QuoteAssetID)
	}
	return out
}

func toAPIInstrumentDetail(d InstrumentDetail) api.InstrumentDetail {
	base := toAPIInstrument(d.Instrument)
	out := api.InstrumentDetail{
		Id:              base.Id,
		Type:            api.InstrumentDetailType(base.Type),
		CanonicalName:   base.CanonicalName,
		BaseAsset:       base.BaseAsset,
		QuoteAsset:      base.QuoteAsset,
		SettlementAsset: base.SettlementAsset,
		RiskClass:       base.RiskClass,
		Status:          api.InstrumentDetailStatus(base.Status),
		ActiveFrom:      base.ActiveFrom,
		ActiveUntil:     base.ActiveUntil,
		Base:            toAPIAsset(d.Base),
		Quote:           toAPIAsset(d.Quote),
		Listings:        make([]api.VenueListing, 0, len(d.Listings)),
	}
	for _, l := range d.Listings {
		item := api.VenueListing{
			Id:               toUUID(l.Listing.ID),
			Venue:            l.Venue.Code,
			VenueStatus:      api.VenueListingVenueStatus(l.Venue.Status),
			Status:           api.VenueListingStatus(l.Listing.Status),
			Network:          l.Listing.Network,
			BaseMint:         strPtr(l.Listing.BaseMint),
			QuoteMint:        strPtr(l.Listing.QuoteMint),
			BasePrecision:    int(l.Listing.BasePrecision),
			QuotePrecision:   int(l.Listing.QuotePrecision),
			MinNotionalQuote: l.Listing.MinNotionalQuote.String(),
		}
		if l.Listing.MaxNotionalQuote != nil {
			v := l.Listing.MaxNotionalQuote.String()
			item.MaxNotionalQuote = &v
		}
		out.Listings = append(out.Listings, item)
	}
	return out
}

// --- trading ----------------------------------------------------------------

func toAPIConstraints(c intent.Constraints) *api.IntentConstraints {
	empty := c.MaxSlippageBPS == 0 && c.MaxFeeBPS == 0 && c.MaxPriceImpactBPS == 0 &&
		c.MinReceive == nil && len(c.AllowedVenues) == 0 && c.QuoteFreshness == 0 &&
		c.ExecutionDeadline.IsZero()
	if empty {
		return nil
	}
	out := &api.IntentConstraints{}
	if c.MaxSlippageBPS != 0 {
		v := int(c.MaxSlippageBPS)
		out.MaxSlippageBps = &v
	}
	if c.MaxFeeBPS != 0 {
		v := int(c.MaxFeeBPS)
		out.MaxFeeBps = &v
	}
	if c.MaxPriceImpactBPS != 0 {
		v := int(c.MaxPriceImpactBPS)
		out.MaxPriceImpactBps = &v
	}
	if c.MinReceive != nil {
		out.MinReceive = qtyPtr(c.MinReceive)
	}
	if len(c.AllowedVenues) > 0 {
		v := append([]string(nil), c.AllowedVenues...)
		out.AllowedVenues = &v
	}
	if c.QuoteFreshness > 0 {
		v := int(c.QuoteFreshness / time.Millisecond)
		out.QuoteFreshnessMs = &v
	}
	if !c.ExecutionDeadline.IsZero() {
		v := c.ExecutionDeadline.UTC()
		out.ExecutionDeadline = &v
	}
	return out
}

func fromAPIConstraints(c *api.IntentConstraints) (intent.Constraints, error) {
	var out intent.Constraints
	if c == nil {
		return out, nil
	}
	if c.MaxSlippageBps != nil {
		out.MaxSlippageBPS = money.BPS(*c.MaxSlippageBps)
	}
	if c.MaxFeeBps != nil {
		out.MaxFeeBPS = money.BPS(*c.MaxFeeBps)
	}
	if c.MaxPriceImpactBps != nil {
		out.MaxPriceImpactBPS = money.BPS(*c.MaxPriceImpactBps)
	}
	if c.MinReceive != nil {
		q, err := money.ParseQuantity(*c.MinReceive)
		if err != nil {
			return out, validationError("constraints.min_receive", "min_receive must be an exact integer base-unit string")
		}
		out.MinReceive = &q
	}
	if c.AllowedVenues != nil {
		out.AllowedVenues = append([]string(nil), *c.AllowedVenues...)
	}
	if c.QuoteFreshnessMs != nil {
		if *c.QuoteFreshnessMs < 0 {
			return out, validationError("constraints.quote_freshness_ms", "quote_freshness_ms must not be negative")
		}
		out.QuoteFreshness = time.Duration(*c.QuoteFreshnessMs) * time.Millisecond
	}
	if c.ExecutionDeadline != nil {
		out.ExecutionDeadline = c.ExecutionDeadline.UTC()
	}
	return out, nil
}

func toAPIIntent(t intent.TradeIntent) api.TradeIntent {
	out := api.TradeIntent{
		Id:                toUUID(t.ID),
		Action:            api.IntentAction(t.Action),
		ActorType:         api.TradeIntentActorType(t.ActorType),
		InstrumentId:      toUUID(t.InstrumentID),
		Status:            api.TradeIntentStatus(t.Status),
		Mode:              api.TradeIntentMode(t.Mode),
		RequestedAt:       t.RequestedAt.UTC(),
		ReceivedAt:        t.ReceivedAt.UTC(),
		CorrelationId:     t.CorrelationID,
		NotionalUsd:       usdPtr(t.NotionalUSD),
		TargetExposureUsd: usdPtr(t.TargetExposureUSD),
		Quantity:          qtyPtr(t.Quantity),
		Constraints:       toAPIConstraints(t.Constraints),
		RejectionCode:     strPtr(t.RejectionCode),
		TerminalAt:        timePtr(t.TerminalAt),
	}
	if u := parseUUIDText(t.AccountID); u != nil {
		out.AccountId = *u
	}
	if t.AgentID != nil {
		out.AgentId = parseUUIDText(*t.AgentID)
	}
	if !t.Deadline.IsZero() {
		d := t.Deadline.UTC()
		out.Deadline = &d
	}
	out.OrderId = parseUUIDText(t.Links.OrderID)
	out.PlanId = parseUUIDText(t.Links.PlanID)
	return out
}

func toAPIIntentDetail(d IntentDetail) api.TradeIntentDetail {
	b := toAPIIntent(d.Intent)
	out := api.TradeIntentDetail{
		Id:                b.Id,
		AccountId:         b.AccountId,
		Action:            b.Action,
		ActorType:         api.TradeIntentDetailActorType(b.ActorType),
		AgentId:           b.AgentId,
		Constraints:       b.Constraints,
		CorrelationId:     b.CorrelationId,
		Deadline:          b.Deadline,
		InstrumentId:      b.InstrumentId,
		Mode:              api.TradeIntentDetailMode(b.Mode),
		NotionalUsd:       b.NotionalUsd,
		OrderId:           b.OrderId,
		PlanId:            b.PlanId,
		Quantity:          b.Quantity,
		ReceivedAt:        b.ReceivedAt,
		RejectionCode:     b.RejectionCode,
		RequestedAt:       b.RequestedAt,
		Status:            api.TradeIntentDetailStatus(b.Status),
		TargetExposureUsd: b.TargetExposureUsd,
		TerminalAt:        b.TerminalAt,
	}
	if d.Order != nil {
		o := toAPIOrder(*d.Order)
		out.Order = &o
	}
	if len(d.Transitions) > 0 {
		ts := make([]struct {
			From       string        `json:"from"`
			OccurredAt api.Timestamp `json:"occurred_at"`
			Reason     *string       `json:"reason,omitempty"`
			To         string        `json:"to"`
		}, 0, len(d.Transitions))
		for _, t := range d.Transitions {
			ts = append(ts, struct {
				From       string        `json:"from"`
				OccurredAt api.Timestamp `json:"occurred_at"`
				Reason     *string       `json:"reason,omitempty"`
				To         string        `json:"to"`
			}{From: t.From, To: t.To, Reason: strPtr(t.Reason), OccurredAt: t.OccurredAt.UTC()})
		}
		out.Transitions = &ts
	}
	return out
}

func toAPIOrder(o execution.Order) api.Order {
	out := api.Order{
		Id:                   toUUID(o.ID),
		AccountId:            toUUID(o.AccountID),
		Side:                 api.OrderSide(o.Side),
		Mode:                 string(o.Mode),
		Status:               api.OrderStatus(o.Status),
		InputAsset:           toUUID(o.InputAssetID),
		InputQuantity:        o.InputQuantity.String(),
		OutputAsset:          toUUID(o.OutputAssetID),
		MinOutputQuantity:    o.MinOutputQuantity.String(),
		FilledInputQuantity:  o.FilledInputQuantity.String(),
		FilledOutputQuantity: o.FilledOutputQuantity.String(),
		RejectionCode:        strPtr(o.RejectionCode),
		CreatedAt:            o.CreatedAt.UTC(),
		TerminalAt:           timePtr(o.TerminalAt),
	}
	if u := parseUUIDText(o.IntentID); u != nil {
		out.IntentId = *u
	}
	if u := parseUUIDText(o.InstrumentID); u != nil {
		out.InstrumentId = *u
	}
	return out
}

func toAPIOrderDetail(d OrderDetail) api.OrderDetail {
	b := toAPIOrder(d.Order)
	out := api.OrderDetail{
		Id:                   b.Id,
		AccountId:            b.AccountId,
		IntentId:             b.IntentId,
		InstrumentId:         b.InstrumentId,
		Side:                 api.OrderDetailSide(b.Side),
		Mode:                 b.Mode,
		Status:               api.OrderDetailStatus(b.Status),
		InputAsset:           b.InputAsset,
		InputQuantity:        b.InputQuantity,
		OutputAsset:          b.OutputAsset,
		MinOutputQuantity:    b.MinOutputQuantity,
		FilledInputQuantity:  b.FilledInputQuantity,
		FilledOutputQuantity: b.FilledOutputQuantity,
		RejectionCode:        b.RejectionCode,
		CreatedAt:            b.CreatedAt,
		TerminalAt:           b.TerminalAt,
	}
	if len(d.Attempts) > 0 {
		as := make([]struct {
			AttemptNo   int            `json:"attempt_no"`
			ConfirmedAt *api.Timestamp `json:"confirmed_at,omitempty"`
			CreatedAt   api.Timestamp  `json:"created_at"`
			Finality    *string        `json:"finality,omitempty"`
			FinalizedAt *api.Timestamp `json:"finalized_at,omitempty"`
			Id          api.UUID       `json:"id"`
			Status      string         `json:"status"`
			SubmittedAt *api.Timestamp `json:"submitted_at,omitempty"`
			TxSignature *string        `json:"tx_signature,omitempty"`
		}, 0, len(d.Attempts))
		for _, a := range d.Attempts {
			as = append(as, struct {
				AttemptNo   int            `json:"attempt_no"`
				ConfirmedAt *api.Timestamp `json:"confirmed_at,omitempty"`
				CreatedAt   api.Timestamp  `json:"created_at"`
				Finality    *string        `json:"finality,omitempty"`
				FinalizedAt *api.Timestamp `json:"finalized_at,omitempty"`
				Id          api.UUID       `json:"id"`
				Status      string         `json:"status"`
				SubmittedAt *api.Timestamp `json:"submitted_at,omitempty"`
				TxSignature *string        `json:"tx_signature,omitempty"`
			}{
				AttemptNo:   int(a.AttemptNo),
				ConfirmedAt: timePtr(a.ConfirmedAt),
				CreatedAt:   a.CreatedAt.UTC(),
				Finality:    strPtr(string(a.Finality)),
				FinalizedAt: timePtr(a.FinalizedAt),
				Id:          toUUID(a.ID),
				Status:      string(a.Status),
				SubmittedAt: timePtr(a.SubmittedAt),
				TxSignature: strPtr(a.TxSignature),
			})
		}
		out.Attempts = &as
	}
	if len(d.Fills) > 0 {
		fs := make([]api.Fill, 0, len(d.Fills))
		for _, f := range d.Fills {
			item := api.Fill{
				Id:             toUUID(f.ID),
				Venue:          f.Venue,
				ExternalFillId: f.ExternalFillID,
				TxSignature:    strPtr(f.TxSignature),
				InputQuantity:  f.InputQuantity.String(),
				OutputQuantity: f.OutputQuantity.String(),
				Finality:       api.FillFinality(f.Finality),
				ObservedAt:     f.ObservedAt.UTC(),
			}
			if f.Slot != nil {
				s := int(*f.Slot)
				item.Slot = &s
			}
			netFee := f.NetworkFeeQuantity.String()
			item.NetworkFeeQuantity = &netFee
			venueFee := f.VenueFeeQuantity.String()
			item.VenueFeeQuantity = &venueFee
			platformFee := f.PlatformFeeQuantity.String()
			item.PlatformFeeQuantity = &platformFee
			item.JournalTransactionId = parseUUIDText(f.JournalTransactionID)
			fs = append(fs, item)
		}
		out.Fills = &fs
	}
	return out
}

func toAPIQuoteDisclosure(v QuoteView) api.QuoteDisclosure {
	d := v.Disclosure
	out := api.QuoteDisclosure{
		QuoteId:            toUUID(d.QuoteID),
		Provider:           d.Provider,
		InputAsset:         toUUID(d.PayAssetID),
		InputQuantity:      d.PayQuantity.String(),
		OutputAsset:        toUUID(d.ReceiveAssetID),
		ExpectedOutput:     d.ExpectedReceive.String(),
		MinimumOutput:      d.MinimumReceive.String(),
		PriceImpactBps:     int(d.PriceImpactBPS),
		SlippageBps:        int(d.SlippageBPS),
		VenueFee:           d.VenueFee.Amount.String(),
		NetworkFeeEstimate: d.NetworkEstimate.Amount.String(),
		PlatformFee:        d.PlatformFee.Amount.String(),
		PlatformFeeBps:     int(d.PlatformFeeBPS),
		FeePolicyVersion:   strPtr(d.FeePolicyVersion),
		ReceivedAt:         d.ReceivedAt.UTC(),
		ExpiresAt:          d.ExpiresAt.UTC(),
		Route:              []map[string]interface{}{},

		Venue:                 v.Venue,
		TotalEstimatedCostUsd: v.TotalEstimatedCostUSD.String(),
	}
	if d.VenueFee.AssetID != nil {
		out.VenueFeeAsset = uuidPtr(*d.VenueFee.AssetID)
	}
	if d.NetworkEstimate.AssetID != nil {
		out.NetworkFeeAsset = uuidPtr(*d.NetworkEstimate.AssetID)
	}
	// effective_price is a bare decimal string, quote per base: the price's
	// own String() carries the quote-asset name, which the contract does not.
	if scale := d.EffectivePrice.Scale; scale >= 0 && scale <= 38 {
		price := d.EffectivePrice.Mantissa.ToDecimalString(uint8(scale))
		out.EffectivePrice = &price
	}
	return out
}

// --- funding ----------------------------------------------------------------

func toAPIDeposit(d funding.Deposit, clientSecret string) api.Deposit {
	out := api.Deposit{
		Id:                  toUUID(d.ID),
		AccountId:           toUUID(d.AccountID),
		Provider:            d.Provider,
		ProviderSessionId:   strPtr(d.ProviderSessionID),
		Status:              api.DepositStatus(d.Status),
		FiatCurrency:        strPtr(d.FiatCurrency),
		ExpectedAsset:       toUUID(d.ExpectedAssetID),
		ExpectedQuantity:    qtyPtr(d.ExpectedQuantity),
		ObservedQuantity:    qtyPtr(d.ObservedQuantity),
		TxSignature:         strPtr(d.TxSignature),
		BuyingPowerEligible: d.BuyingPowerEligible,
		WithdrawalEligible:  d.WithdrawalEligible,
		ReversibleUntil:     timePtr(d.ReversibleUntil),
		CreatedAt:           d.CreatedAt.UTC(),
		AvailableAt:         timePtr(d.AvailableAt),
		ClientSecretRef:     strPtr(clientSecret),
	}
	if d.FiatAmountMinor != nil {
		v := money.USDFromMinor(*d.FiatAmountMinor).String()
		out.FiatAmount = &v
	}
	return out
}

func toAPIDepositDetail(d DepositDetail) api.DepositDetail {
	b := toAPIDeposit(d.Deposit, "")
	out := api.DepositDetail{
		Id:                  b.Id,
		AccountId:           b.AccountId,
		Provider:            b.Provider,
		ProviderSessionId:   b.ProviderSessionId,
		Status:              api.DepositDetailStatus(b.Status),
		FiatAmount:          b.FiatAmount,
		FiatCurrency:        b.FiatCurrency,
		ExpectedAsset:       b.ExpectedAsset,
		ExpectedQuantity:    b.ExpectedQuantity,
		ObservedQuantity:    b.ObservedQuantity,
		TxSignature:         b.TxSignature,
		BuyingPowerEligible: b.BuyingPowerEligible,
		WithdrawalEligible:  b.WithdrawalEligible,
		ReversibleUntil:     b.ReversibleUntil,
		CreatedAt:           b.CreatedAt,
		AvailableAt:         b.AvailableAt,
	}
	if d.Deposit.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*d.Deposit.JournalTransactionID)
	}
	if len(d.Transitions) > 0 {
		ts := make([]struct {
			From       string        `json:"from"`
			OccurredAt api.Timestamp `json:"occurred_at"`
			Reason     *string       `json:"reason,omitempty"`
			To         string        `json:"to"`
		}, 0, len(d.Transitions))
		for _, t := range d.Transitions {
			ts = append(ts, struct {
				From       string        `json:"from"`
				OccurredAt api.Timestamp `json:"occurred_at"`
				Reason     *string       `json:"reason,omitempty"`
				To         string        `json:"to"`
			}{From: t.From, To: t.To, Reason: strPtr(t.Reason), OccurredAt: t.OccurredAt.UTC()})
		}
		out.Transitions = &ts
	}
	return out
}

// --- admin ------------------------------------------------------------------

func toAPIGate(v GateView) api.CapabilityGate {
	out := api.CapabilityGate{
		Capability:          api.Capability(v.Gate.Capability),
		Environment:         v.Gate.Environment,
		State:               api.CapabilityGateState(v.Gate.State),
		Active:              v.Verdict.Active,
		Sandbox:             &v.Verdict.Sandbox,
		InactiveReason:      strPtr(v.Verdict.Reason),
		ApprovalVersion:     v.Gate.ApprovalVersion,
		LegalReviewRef:      strPtr(v.Gate.LegalReviewRef),
		ProviderContractRef: strPtr(v.Gate.ProviderContractRef),
		RiskApprovalRef:     strPtr(v.Gate.RiskApprovalRef),
		SecurityApprovalRef: strPtr(v.Gate.SecurityApprovalRef),
		EffectiveAt:         timePtr(v.Gate.EffectiveAt),
		ExpiresAt:           timePtr(v.Gate.ExpiresAt),
	}
	if len(v.Gate.Approvers) > 0 {
		apps := make([]map[string]interface{}, 0, len(v.Gate.Approvers))
		for _, a := range v.Gate.Approvers {
			apps = append(apps, map[string]interface{}{
				"user_id": a.UserID, "role": a.Role, "step": a.Step,
				"at": a.At.UTC().Format(time.RFC3339Nano), "evidence_hash": a.EvidenceHash,
			})
		}
		out.Approvers = &apps
	}
	return out
}

func toAPIKillSwitch(s killswitch.Switch) api.KillSwitch {
	return api.KillSwitch{
		Kind:        api.KillSwitchKind(s.Kind),
		ScopeId:     s.ScopeID,
		Active:      s.Active,
		Severity:    api.KillSwitchSeverity(s.Severity),
		Reason:      strPtr(s.Reason),
		ActivatedAt: timePtr(s.ActivatedAt),
		ReleasedAt:  timePtr(s.ReleasedAt),
	}
}

func toAPIAdminAction(a admin.Action) api.AdminAction {
	out := api.AdminAction{
		Id:           toUUID(a.ID),
		Kind:         string(a.Kind),
		TargetType:   a.TargetType,
		TargetId:     a.TargetID,
		ParamsHash:   strPtr(hexString(a.ParamsHash)),
		Reason:       strPtr(a.Reason),
		RequiresDual: a.RequiresDual,
		Status:       api.AdminActionStatus(a.Status),
		ProposedAt:   a.ProposedAt.UTC(),
		ApprovedAt:   timePtr(a.ApprovedAt),
		ExecutedAt:   timePtr(a.ExecutedAt),
		ExpiresAt:    a.ExpiresAt.UTC(),
	}
	if u := parseUUIDText(a.ProposedBy); u != nil {
		out.ProposedBy = *u
	}
	if a.ApprovedBy != nil {
		out.ApprovedBy = parseUUIDText(*a.ApprovedBy)
	}
	if len(a.Params) > 0 {
		if m, err := decodeObject(a.Params); err == nil {
			out.Params = &m
		}
	}
	return out
}

func toAPIReconciliationRecord(r ReconciliationRecord) api.ReconciliationRecord {
	out := api.ReconciliationRecord{
		Kind:                  api.ReconciliationRecordKind(r.Kind),
		Mode:                  api.ReconciliationRecordMode(r.Mode),
		ScopeType:             r.ScopeType,
		ScopeId:               r.ScopeID,
		Status:                api.ReconciliationRecordStatus(r.Status),
		Material:              r.Material,
		BlocksNewRisk:         r.BlocksNewRisk,
		OpenedAt:              r.OpenedAt.UTC(),
		ResolvedAt:            timePtr(r.ResolvedAt),
		ResolutionReason:      strPtr(r.ResolutionReason),
		ResolutionEvidenceRef: strPtr(r.ResolutionEvidenceRef),
		AccountId:             parseUUIDText(r.AccountID),
		AssetId:               parseUUIDText(r.AssetID),
	}
	if u := parseUUIDText(r.ID); u != nil {
		out.Id = *u
	}
	out.CompensatingJournalTransactionId = parseUUIDText(r.CompensatingJournalTx)
	if len(r.Expected) > 0 {
		m := r.Expected
		out.Expected = &m
	}
	if len(r.Observed) > 0 {
		m := r.Observed
		out.Observed = &m
	}
	if len(r.Difference) > 0 {
		m := r.Difference
		out.Difference = &m
	}
	return out
}

func toAPIProvider(p ProviderView) api.ProviderStatus {
	out := api.ProviderStatus{
		Name:          p.Name,
		Role:          p.Role,
		Health:        api.ProviderStatusHealth(p.Health),
		Verification:  api.ProviderStatusVerification(p.Verification),
		DisableReason: strPtr(p.DisableReason),
	}
	if p.Mode != "" {
		m := api.ProviderStatusMode(p.Mode)
		out.Mode = &m
	}
	if p.ErrorRateBPS > 0 {
		v := int(p.ErrorRateBPS)
		out.ErrorRateBps = &v
	}
	if p.P95Millis > 0 {
		v := int(p.P95Millis)
		out.P95Ms = &v
	}
	if !p.LastSuccessAt.IsZero() {
		t := p.LastSuccessAt.UTC()
		out.LastSuccessAt = &t
	}
	return out
}

func toAPIActivity(items []ActivityItem) []api.ActivityItem {
	out := make([]api.ActivityItem, 0, len(items))
	for _, it := range items {
		item := api.ActivityItem{
			Id:            it.ID,
			Kind:          api.ActivityItemKind(it.Kind),
			OccurredAt:    it.OccurredAt.UTC(),
			Summary:       it.Summary,
			CorrelationId: strPtr(it.CorrelationID),
		}
		if len(it.References) > 0 {
			refs := it.References
			item.References = &refs
		}
		out = append(out, item)
	}
	return out
}
