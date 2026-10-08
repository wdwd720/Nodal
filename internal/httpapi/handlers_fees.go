package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/feeledger"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// The fee ledger routes (internal/feeledger). Handlers translate shapes only:
// the fee ledger and the fee engine decide every figure and every refusal.
// Published schedules and the quote calculator are any signed-in customer's
// (transparency); an account's quotes, bookings and performance periods are
// its owner's, or an operator's who may read any account; a record read by
// its own id is scoped by its own account and answers NOT_FOUND otherwise.
// Publishing, recording quotes, reversing, the spread audit and the
// performance-period lifecycle are an operator's (authz.go).

const feesFeature = "the fee ledger"

// --- schedules and the quote calculator --------------------------------------------

// GetAdvancedFeesSchedules pages every schedule key with the version in force.
func (s *Server) GetAdvancedFeesSchedules(ctx context.Context, request api.GetAdvancedFeesSchedulesRequestObject) (api.GetAdvancedFeesSchedulesResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	page, err := s.opts.Ports.Fees.Schedules(ctx, marginCursorParam(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.FeeScheduleSummaryPage{ValueDomain: api.SIMULATED, Items: make([]api.FeeScheduleSummary, 0, len(page.Items)),
		NextCursor: nextCursor(page.NextCursor)}
	for _, sm := range page.Items {
		item := api.FeeScheduleSummary{Key: sm.Key, LatestVersion: sm.Latest, VersionCount: sm.Count}
		if sm.Effective != nil {
			v := toAPIFeeScheduleVersion(*sm.Effective)
			item.Effective = &v
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedFeesSchedules200JSONResponse(out), nil
}

// GetAdvancedFeesSchedulesHistory returns every version of one schedule key.
func (s *Server) GetAdvancedFeesSchedulesHistory(ctx context.Context, request api.GetAdvancedFeesSchedulesHistoryRequestObject) (api.GetAdvancedFeesSchedulesHistoryResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	key := strings.TrimSpace(request.Params.Key)
	if key == "" || key != request.Params.Key {
		return nil, validationError("key", "key names a fee schedule")
	}
	h, err := s.opts.Ports.Fees.ScheduleHistory(ctx, key)
	if err != nil {
		return nil, err
	}
	out := api.FeeScheduleHistory{ValueDomain: api.SIMULATED, Key: h.Key, Versions: make([]api.FeeScheduleVersion, 0, len(h.Versions))}
	if h.EffectiveVersion > 0 {
		v := h.EffectiveVersion
		out.EffectiveVersion = &v
	}
	for _, v := range h.Versions {
		out.Versions = append(out.Versions, toAPIFeeScheduleVersion(v))
	}
	return api.GetAdvancedFeesSchedulesHistory200JSONResponse(out), nil
}

// GetAdvancedFeesQuotePreview prices an order exactly under the schedule in
// force. It is a read: nothing is recorded, reserved or charged.
func (s *Server) GetAdvancedFeesQuotePreview(ctx context.Context, request api.GetAdvancedFeesQuotePreviewRequestObject) (api.GetAdvancedFeesQuotePreviewResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	p := request.Params
	in, err := feeQuoteInputFrom(p.ScheduleKey, p.At, p.Side, p.BaseQuantity, p.BaseDecimals, p.ReferencePrice, p.TrailingVolumeUsd,
		p.HasSubscription, p.StrategyOriginated)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Fees.PreviewQuote(ctx, in)
	if err != nil {
		return nil, err
	}
	sched := v.Schedule
	out := api.FeeQuotePreview{
		ValueDomain: api.SIMULATED,
		Schedule: api.FeeScheduleRef{Id: toUUID(sched.ID), Key: sched.Key, Version: sched.Version, Label: sched.Label,
			ScheduleHash: sched.Hash, EffectiveFrom: sched.EffectiveFrom.UTC()},
		Disclosure: toAPIFeeDisclosure(v.Disclosure), DisclosureHash: v.DisclosureHash, Charges: toAPIFeeCharges(v.Charges),
		Recorded: false,
	}
	return api.GetAdvancedFeesQuotePreview200JSONResponse(out), nil
}

// PostAdvancedFeesSchedules publishes the next version of a schedule key.
func (s *Server) PostAdvancedFeesSchedules(ctx context.Context, request api.PostAdvancedFeesSchedulesRequestObject) (api.PostAdvancedFeesSchedulesResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	sched, err := feeScheduleFrom(b.Schedule)
	if err != nil {
		return nil, err
	}
	var effective time.Time
	if b.EffectiveFrom != nil {
		effective = b.EffectiveFrom.UTC()
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := feeledger.PublishRequest{Key: b.Key, Schedule: sched, EffectiveFrom: effective, Reason: b.Reason, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeeScheduleVersion, commandMeta, error) {
			v, perr := s.opts.Ports.Fees.Publish(ctx, req)
			if perr != nil {
				return api.FeeScheduleVersion{}, commandMeta{}, perr
			}
			return toAPIFeeScheduleVersion(v), commandMeta{Status: http.StatusOK, ResourceType: "fee_schedule", ResourceID: v.Schedule.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesSchedules200JSONResponse(res.Value), nil
}

// --- quote disclosures ------------------------------------------------------------------

// GetAdvancedFeesQuotes pages the quote disclosures bound to an account.
func (s *Server) GetAdvancedFeesQuotes(ctx context.Context, request api.GetAdvancedFeesQuotesRequestObject) (api.GetAdvancedFeesQuotesResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Fees.Quotes(ctx, account, marginCursorParam(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.FeeQuotePage{ValueDomain: api.SIMULATED, Items: make([]api.FeeQuoteRecord, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, q := range page.Items {
		out.Items = append(out.Items, toAPIFeeQuoteRecord(q))
	}
	return api.GetAdvancedFeesQuotes200JSONResponse(out), nil
}

// GetAdvancedFeesQuotesFeeDisclosureId returns one stored disclosure,
// re-proved from the record. A disclosure bound to an account is its owner's
// (or an operator's who may read any account); one bound to no account is an
// operator's. Anything else answers exactly as an absent one.
func (s *Server) GetAdvancedFeesQuotesFeeDisclosureId(ctx context.Context, request api.GetAdvancedFeesQuotesFeeDisclosureIdRequestObject) (api.GetAdvancedFeesQuotesFeeDisclosureIdResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	did, err := feeledger.ParseDisclosureID(request.FeeDisclosureId.String())
	if err != nil || did.IsZero() {
		return nil, validationError("feeDisclosureId", "feeDisclosureId must be a canonical UUID")
	}
	notFound := errs.New(errs.CodeNotFound, "no such quote disclosure").WithField("disclosure_id", did.String())
	v, err := s.opts.Ports.Fees.Quote(ctx, did)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if v.Quote.AccountID != nil {
		if security.RequireAccount(ctx, v.Quote.AccountID.String()) != nil {
			return nil, notFound
		}
	} else if security.Require(ctx, security.PermSandboxOperate) != nil {
		return nil, notFound
	}
	return api.GetAdvancedFeesQuotesFeeDisclosureId200JSONResponse(toAPIFeeQuoteRecord(v)), nil
}

// PostAdvancedFeesQuotes records a verified quote disclosure.
func (s *Server) PostAdvancedFeesQuotes(ctx context.Context, request api.PostAdvancedFeesQuotesRequestObject) (api.PostAdvancedFeesQuotesResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	in, err := feeQuoteInputFrom(b.ScheduleKey, b.At, b.Side, b.BaseQuantity, b.BaseDecimals, b.ReferencePrice, b.TrailingVolumeUsd,
		b.HasSubscription, b.StrategyOriginated)
	if err != nil {
		return nil, err
	}
	sub := econ.Subsystem(b.Subsystem)
	if !sub.Valid() {
		return nil, validationError("subsystem", "subsystem is not one of the advanced subsystems")
	}
	mode := gates.Mode(b.Mode)
	if !mode.Valid() {
		return nil, validationError("mode", "mode must be one of PAPER, SANDBOX, DEVNET, LIVE")
	}
	cmd := FeeQuoteCommand{Input: in, Subsystem: sub, Mode: mode, IdempotencyKey: request.Params.IdempotencyKey}
	if b.AccountId != nil {
		acct, err := accounts.ParseAccountID(b.AccountId.String())
		if err != nil || acct.IsZero() {
			return nil, validationError("account_id", "account_id must be a canonical UUID")
		}
		cmd.AccountID = &acct
	}
	if b.BaseAssetId != nil {
		aid, err := treasuryAsset(*b.BaseAssetId, "base_asset_id")
		if err != nil {
			return nil, err
		}
		cmd.BaseAsset = &aid
	}
	if b.Source != nil {
		cmd.Source = strings.TrimSpace(*b.Source)
	}
	if cmd.Actor, err = treasuryActor(ctx); err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeeQuoteRecord, commandMeta, error) {
			v, qerr := s.opts.Ports.Fees.RecordQuote(ctx, cmd)
			if qerr != nil {
				return api.FeeQuoteRecord{}, commandMeta{}, qerr
			}
			return toAPIFeeQuoteRecord(v), commandMeta{Status: http.StatusOK, ResourceType: "fee_quote_disclosure", ResourceID: v.Quote.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesQuotes200JSONResponse(res.Value), nil
}

// --- bookings --------------------------------------------------------------------------

// GetAdvancedFeesBookings pages the fees charged to an account.
func (s *Server) GetAdvancedFeesBookings(ctx context.Context, request api.GetAdvancedFeesBookingsRequestObject) (api.GetAdvancedFeesBookingsResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Fees.Bookings(ctx, account, marginCursorParam(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.FeeBookingPage{ValueDomain: api.SIMULATED, Items: make([]api.FeeBooking, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, b := range page.Items {
		out.Items = append(out.Items, toAPIFeeBooking(b))
	}
	return api.GetAdvancedFeesBookings200JSONResponse(out), nil
}

// GetAdvancedFeesBookingsFeeBookingId returns one booking with its disclosure,
// re-proved end to end. A booking of another account answers exactly as one
// that does not exist.
func (s *Server) GetAdvancedFeesBookingsFeeBookingId(ctx context.Context, request api.GetAdvancedFeesBookingsFeeBookingIdRequestObject) (api.GetAdvancedFeesBookingsFeeBookingIdResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	bid, err := feeBookingID(request.FeeBookingId)
	if err != nil {
		return nil, err
	}
	notFound := errs.New(errs.CodeNotFound, "no such fee booking").WithField("booking_id", bid.String())
	v, err := s.opts.Ports.Fees.Booking(ctx, bid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if security.RequireAccount(ctx, v.Booking.AccountID.String()) != nil {
		return nil, notFound
	}
	b := toAPIFeeBooking(v)
	out := api.FeeBookingDetail{
		ValueDomain: b.ValueDomain, Id: b.Id, DisclosureId: b.DisclosureId, AccountId: b.AccountId, Mode: b.Mode, BookingMode: b.BookingMode,
		JournalKind: b.JournalKind, JournalTransactionId: b.JournalTransactionId, EffectId: b.EffectId, IntentId: b.IntentId,
		Charges: b.Charges, Status: b.Status, EvidenceRef: b.EvidenceRef, BookedAt: b.BookedAt,
		ReversalJournalTransactionId: b.ReversalJournalTransactionId, ReversalEffectId: b.ReversalEffectId,
		ReversalReasonCode: b.ReversalReasonCode, ReversedAt: b.ReversedAt, SpreadLine: b.SpreadLine,
		Disclosure: toAPIFeeDisclosure(v.Quote.Disclosure), Verification: toAPIFeeVerification(v.Verification),
	}
	return api.GetAdvancedFeesBookingsFeeBookingId200JSONResponse(out), nil
}

// PostAdvancedFeesBookingsFeeBookingIdReverse reverses a booking.
func (s *Server) PostAdvancedFeesBookingsFeeBookingIdReverse(ctx context.Context, request api.PostAdvancedFeesBookingsFeeBookingIdReverseRequestObject) (api.PostAdvancedFeesBookingsFeeBookingIdReverseResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	bid, err := feeBookingID(request.FeeBookingId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := feeledger.ReverseRequest{BookingID: bid, ReasonCode: request.Body.ReasonCode, Reason: request.Body.Reason, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeeBooking, commandMeta, error) {
			v, rerr := s.opts.Ports.Fees.ReverseBooking(ctx, req)
			if rerr != nil {
				return api.FeeBooking{}, commandMeta{}, rerr
			}
			return toAPIFeeBooking(v), commandMeta{Status: http.StatusOK, ResourceType: "fee_booking", ResourceID: bid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesBookingsFeeBookingIdReverse200JSONResponse(res.Value), nil
}

// GetAdvancedFeesSpreadAudit lists every charged spread the filter selects.
func (s *Server) GetAdvancedFeesSpreadAudit(ctx context.Context, request api.GetAdvancedFeesSpreadAuditRequestObject) (api.GetAdvancedFeesSpreadAuditResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	p := request.Params
	f := feeledger.SpreadFilter{Limit: pageLimit(p.Limit, defaultPageLimit, maxPageLimit), IncludeReversed: treasuryBool(p.IncludeReversed)}
	if p.QuoteAssetId != nil {
		aid, err := treasuryAsset(*p.QuoteAssetId, "quote_asset_id")
		if err != nil {
			return nil, err
		}
		f.QuoteAsset = &aid
	}
	if p.AccountId != nil {
		acct, err := accounts.ParseAccountID(p.AccountId.String())
		if err != nil || acct.IsZero() {
			return nil, validationError("account_id", "account_id must be a canonical UUID")
		}
		f.AccountID = &acct
	}
	if p.Since != nil {
		f.Since = p.Since.UTC()
	}
	if p.Until != nil {
		f.Until = p.Until.UTC()
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		return nil, validationError("until", "until must be after since")
	}
	items, err := s.opts.Ports.Fees.SpreadAudit(ctx, f)
	if err != nil {
		return nil, err
	}
	out := api.FeeSpreadAudit{ValueDomain: api.SIMULATED, Items: make([]api.FeeSpreadCharge, 0, len(items))}
	for _, c := range items {
		item := api.FeeSpreadCharge{
			BookingId: toUUID(c.BookingID), DisclosureId: toUUID(c.DisclosureID), AccountId: toUUID(c.AccountID), ScheduleKey: c.ScheduleKey,
			ScheduleVersion: c.ScheduleVersion, ScheduleHash: c.ScheduleHash, DisclosureHash: c.DisclosureHash, Side: api.FeeSide(c.Side),
			BaseQuantity: c.BaseQuantity.String(), ReferencePrice: toAPITreasuryPrice(c.ReferencePrice),
			ExecutionPrice: toAPITreasuryPrice(c.ExecutionPrice), SpreadBps: int(c.SpreadBPS), SpreadAmount: c.SpreadAmount.String(),
			QuoteAssetId: toUUID(c.QuoteAsset), BookingMode: api.FeeBookingMode(c.BookingMode), Status: api.FeeBookingStatus(c.Status),
			JournalTransactionId: toUUID(c.JournalTransactionID), BookedAt: c.BookedAt.UTC(),
		}
		if c.ReversalJournalID != nil {
			item.ReversalJournalTransactionId = uuidPtr(*c.ReversalJournalID)
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedFeesSpreadAudit200JSONResponse(out), nil
}

// GetAdvancedFeesSpreadTotals returns the spread booked and reversed in one
// quote asset.
func (s *Server) GetAdvancedFeesSpreadTotals(ctx context.Context, request api.GetAdvancedFeesSpreadTotalsRequestObject) (api.GetAdvancedFeesSpreadTotalsResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	aid, err := treasuryAsset(request.Params.QuoteAssetId, "quote_asset_id")
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Fees.SpreadTotals(ctx, aid)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedFeesSpreadTotals200JSONResponse(api.FeeSpreadTotals{ValueDomain: api.SIMULATED, QuoteAssetId: toUUID(v.QuoteAsset),
		Booked: v.Booked.String(), Reversed: v.Reversed.String()}), nil
}

// --- performance periods --------------------------------------------------------------------

// GetAdvancedFeesPeriods pages an account's performance-fee periods.
func (s *Server) GetAdvancedFeesPeriods(ctx context.Context, request api.GetAdvancedFeesPeriodsRequestObject) (api.GetAdvancedFeesPeriodsResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	strategy := ""
	if request.Params.StrategyRef != nil {
		strategy = *request.Params.StrategyRef
		if strings.TrimSpace(strategy) == "" {
			return nil, validationError("strategy_ref", "strategy_ref names a strategy")
		}
	}
	page, err := s.opts.Ports.Fees.Periods(ctx, account, strategy, marginCursorParam(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.FeePeriodPage{ValueDomain: api.SIMULATED, Items: make([]api.FeePeriod, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, p := range page.Items {
		out.Items = append(out.Items, toAPIFeePeriod(p))
	}
	return api.GetAdvancedFeesPeriods200JSONResponse(out), nil
}

// GetAdvancedFeesPeriodsFeePeriodId returns one period; another account's
// answers exactly as one that does not exist.
func (s *Server) GetAdvancedFeesPeriodsFeePeriodId(ctx context.Context, request api.GetAdvancedFeesPeriodsFeePeriodIdRequestObject) (api.GetAdvancedFeesPeriodsFeePeriodIdResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	pid, err := feePeriodID(request.FeePeriodId)
	if err != nil {
		return nil, err
	}
	notFound := errs.New(errs.CodeNotFound, "no such performance period").WithField("period_id", pid.String())
	v, err := s.opts.Ports.Fees.Period(ctx, pid)
	if errs.HasCode(err, errs.CodeNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if security.RequireAccount(ctx, v.Period.AccountID.String()) != nil {
		return nil, notFound
	}
	return api.GetAdvancedFeesPeriodsFeePeriodId200JSONResponse(toAPIFeePeriod(v)), nil
}

// PostAdvancedFeesPeriods opens the next period of an (account, strategy)
// pair. An operator acts on any account under sandbox:operate.
func (s *Server) PostAdvancedFeesPeriods(ctx context.Context, request api.PostAdvancedFeesPeriodsRequestObject) (api.PostAdvancedFeesPeriodsResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	acct, err := accounts.ParseAccountID(b.AccountId.String())
	if err != nil || acct.IsZero() {
		return nil, validationError("account_id", "account_id must be a canonical UUID")
	}
	mode, err := treasuryMode(b.Mode)
	if err != nil {
		return nil, err
	}
	if b.HurdleAnnualBps < 0 || b.FeeRateBps < 0 {
		return nil, validationError("fee_rate_bps", "rates are non-negative basis points")
	}
	req := feeledger.OpenPeriodRequest{AccountID: acct, StrategyRef: b.StrategyRef, Mode: mode, TermsVersion: b.TermsVersion,
		PeriodEnd: b.PeriodEnd.UTC(), HurdleAnnualBPS: money.BPS(b.HurdleAnnualBps), FeeRateBPS: money.BPS(b.FeeRateBps),
		IdempotencyKey: request.Params.IdempotencyKey}
	if b.FeeAssetId != nil {
		if req.FeeAsset, err = treasuryAsset(*b.FeeAssetId, "fee_asset_id"); err != nil {
			return nil, err
		}
	}
	if b.PeriodStart != nil {
		req.PeriodStart = b.PeriodStart.UTC()
	}
	if b.OpeningNav != nil {
		if req.OpeningNAV, err = feeUSD(*b.OpeningNav, "opening_nav"); err != nil {
			return nil, err
		}
	}
	if req.Actor, err = treasuryActor(ctx); err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeePeriod, commandMeta, error) {
			v, perr := s.opts.Ports.Fees.OpenPeriod(ctx, req)
			if perr != nil {
				return api.FeePeriod{}, commandMeta{}, perr
			}
			return toAPIFeePeriod(v), commandMeta{Status: http.StatusOK, ResourceType: "performance_fee_period", ResourceID: v.Period.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesPeriods200JSONResponse(res.Value), nil
}

// PostAdvancedFeesPeriodsFeePeriodIdFlows records a deposit or withdrawal.
func (s *Server) PostAdvancedFeesPeriodsFeePeriodIdFlows(ctx context.Context, request api.PostAdvancedFeesPeriodsFeePeriodIdFlowsRequestObject) (api.PostAdvancedFeesPeriodsFeePeriodIdFlowsResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	pid, err := feePeriodID(request.FeePeriodId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	amount, err := feeUSD(b.Amount, "amount")
	if err != nil {
		return nil, err
	}
	nav, err := feeUSD(b.NavBefore, "nav_before")
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := feeledger.FlowRequest{PeriodID: pid, At: b.At.UTC(), Amount: amount, NAVBefore: nav, IdempotencyKey: request.Params.IdempotencyKey, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeeFlowResult, commandMeta, error) {
			f, ferr := s.opts.Ports.Fees.RecordFlow(ctx, req)
			if ferr != nil {
				return api.FeeFlowResult{}, commandMeta{}, ferr
			}
			return api.FeeFlowResult{ValueDomain: api.SIMULATED, PeriodId: toUUID(pid), Flow: toAPIFeeFlow(f)},
				commandMeta{Status: http.StatusOK, ResourceType: "performance_fee_flow", ResourceID: f.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesPeriodsFeePeriodIdFlows200JSONResponse(res.Value), nil
}

// PostAdvancedFeesPeriodsFeePeriodIdAccruals values an open period.
func (s *Server) PostAdvancedFeesPeriodsFeePeriodIdAccruals(ctx context.Context, request api.PostAdvancedFeesPeriodsFeePeriodIdAccrualsRequestObject) (api.PostAdvancedFeesPeriodsFeePeriodIdAccrualsResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	pid, err := feePeriodID(request.FeePeriodId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	nav, err := feeUSD(request.Body.Nav, "nav")
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := feeledger.AccrueRequest{PeriodID: pid, At: request.Body.At.UTC(), NAV: nav, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeeLineResult, commandMeta, error) {
			l, aerr := s.opts.Ports.Fees.Accrue(ctx, req)
			if aerr != nil {
				return api.FeeLineResult{}, commandMeta{}, aerr
			}
			return api.FeeLineResult{ValueDomain: api.SIMULATED, PeriodId: toUUID(pid), Line: toAPIFeeLine(l)},
				commandMeta{Status: http.StatusOK, ResourceType: "performance_fee_line", ResourceID: l.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesPeriodsFeePeriodIdAccruals200JSONResponse(res.Value), nil
}

// PostAdvancedFeesPeriodsFeePeriodIdCrystallize crystallizes a period whose
// end has passed.
func (s *Server) PostAdvancedFeesPeriodsFeePeriodIdCrystallize(ctx context.Context, request api.PostAdvancedFeesPeriodsFeePeriodIdCrystallizeRequestObject) (api.PostAdvancedFeesPeriodsFeePeriodIdCrystallizeResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	pid, err := feePeriodID(request.FeePeriodId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	closing, err := feeUSD(request.Body.ClosingNav, "closing_nav")
	if err != nil {
		return nil, err
	}
	req := feeledger.CrystallizeRequest{PeriodID: pid, ClosingNAV: closing}
	if request.Body.Contributions != nil {
		for _, c := range *request.Body.Contributions {
			pnl, err := feeUSD(c.GrossPnl, "contributions.gross_pnl")
			if err != nil {
				return nil, err
			}
			req.Contributions = append(req.Contributions, fees.StrategyContribution{Strategy: c.Strategy, GrossPnL: pnl})
		}
	}
	if req.Actor, err = treasuryActor(ctx); err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeePeriod, commandMeta, error) {
			v, cerr := s.opts.Ports.Fees.Crystallize(ctx, req)
			if cerr != nil {
				return api.FeePeriod{}, commandMeta{}, cerr
			}
			return toAPIFeePeriod(v), commandMeta{Status: http.StatusOK, ResourceType: "performance_fee_period", ResourceID: pid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesPeriodsFeePeriodIdCrystallize200JSONResponse(res.Value), nil
}

// PostAdvancedFeesPeriodsFeePeriodIdCancel cancels an open period.
func (s *Server) PostAdvancedFeesPeriodsFeePeriodIdCancel(ctx context.Context, request api.PostAdvancedFeesPeriodsFeePeriodIdCancelRequestObject) (api.PostAdvancedFeesPeriodsFeePeriodIdCancelResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	pid, err := feePeriodID(request.FeePeriodId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	reason := request.Body.Reason
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeePeriod, commandMeta, error) {
			v, cerr := s.opts.Ports.Fees.CancelPeriod(ctx, pid, reason, actor)
			if cerr != nil {
				return api.FeePeriod{}, commandMeta{}, cerr
			}
			return toAPIFeePeriod(v), commandMeta{Status: http.StatusOK, ResourceType: "performance_fee_period", ResourceID: pid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesPeriodsFeePeriodIdCancel200JSONResponse(res.Value), nil
}

// PostAdvancedFeesPeriodsFeePeriodIdReverse reverses a crystallization.
func (s *Server) PostAdvancedFeesPeriodsFeePeriodIdReverse(ctx context.Context, request api.PostAdvancedFeesPeriodsFeePeriodIdReverseRequestObject) (api.PostAdvancedFeesPeriodsFeePeriodIdReverseResponseObject, error) {
	if s.opts.Ports.Fees == nil {
		return nil, errNotWired(feesFeature)
	}
	pid, err := feePeriodID(request.FeePeriodId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := feeledger.ReversePeriodRequest{PeriodID: pid, ReasonCode: request.Body.ReasonCode, Reason: request.Body.Reason, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.FeePeriod, commandMeta, error) {
			v, rerr := s.opts.Ports.Fees.ReverseCrystallization(ctx, req)
			if rerr != nil {
				return api.FeePeriod{}, commandMeta{}, rerr
			}
			return toAPIFeePeriod(v), commandMeta{Status: http.StatusOK, ResourceType: "performance_fee_period", ResourceID: pid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedFeesPeriodsFeePeriodIdReverse200JSONResponse(res.Value), nil
}

// --- request helpers ---------------------------------------------------------------------

func feeBookingID(u api.UUID) (feeledger.BookingID, error) {
	var bid feeledger.BookingID
	if err := bid.UnmarshalText([]byte(u.String())); err != nil || bid.IsZero() {
		return feeledger.BookingID{}, validationError("feeBookingId", "feeBookingId must be a canonical UUID")
	}
	return bid, nil
}

func feePeriodID(u api.UUID) (feeledger.PeriodID, error) {
	pid, err := feeledger.ParsePeriodID(u.String())
	if err != nil || pid.IsZero() {
		return feeledger.PeriodID{}, validationError("feePeriodId", "feePeriodId must be a canonical UUID")
	}
	return pid, nil
}

// feeUSDShape is components.schemas.USD: a decimal string with exactly two
// fraction digits. money.ParseUSD is more forgiving than the contract.
var feeUSDShape = regexp.MustCompile(`^-?[0-9]+\.[0-9]{2}$`)

func feeUSD(s, field string) (money.USD, error) {
	if !feeUSDShape.MatchString(s) {
		return money.USD{}, validationError(field, field+" must be a decimal string with exactly two fraction digits")
	}
	u, err := money.ParseUSD(s)
	if err != nil {
		return money.USD{}, validationError(field, field+" must be a decimal string with exactly two fraction digits")
	}
	return u, nil
}

// feeQuoteInputFrom reads the shape of a quote input; the fee engine checks
// everything the shape cannot.
func feeQuoteInputFrom(key string, at *api.Timestamp, side api.FeeSide, qty string, baseDecimals int, price string, trailing *api.USD,
	subscription, strategy *bool) (FeeQuoteInput, error) {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
		return FeeQuoteInput{}, validationError("schedule_key", "schedule_key names a fee schedule")
	}
	sd := fees.Side(side)
	if sd != fees.SideBuy && sd != fees.SideSell {
		return FeeQuoteInput{}, validationError("side", "side must be BUY or SELL")
	}
	q, err := treasuryQuantity(qty, "base_quantity")
	if err != nil {
		return FeeQuoteInput{}, err
	}
	if baseDecimals < 0 || baseDecimals > 38 {
		return FeeQuoteInput{}, validationError("base_decimals", "base_decimals must be within 0..38")
	}
	if err := treasuryDecimal(price, "reference_price"); err != nil {
		return FeeQuoteInput{}, err
	}
	in := FeeQuoteInput{ScheduleKey: key, Side: sd, BaseQty: q, BaseDecimals: uint8(baseDecimals), ReferencePrice: price, // #nosec G115 -- bounded to 0..38 above
		HasSubscription: treasuryBool(subscription), StrategyOriginated: treasuryBool(strategy)}
	if at != nil {
		in.At = at.UTC()
	}
	if trailing != nil {
		if in.TrailingVolume, err = feeUSD(*trailing, "trailing_volume_usd"); err != nil {
			return FeeQuoteInput{}, err
		}
	}
	return in, nil
}

func feeRounding(field, s string) (money.RoundingMode, error) {
	m, err := money.ParseRoundingMode(s)
	if err != nil {
		return 0, validationError("schedule.rounding."+field, "a rounding mode is one of down, up, half_even, half_up, floor, ceil, exact")
	}
	return m, nil
}

// feeScheduleFrom reads a schedule's shape. fees.Schedule.Validate (run by
// the fee ledger) decides whether it is a schedule at all.
func feeScheduleFrom(b api.FeeSchedule) (fees.Schedule, error) {
	quote, err := treasuryAsset(b.QuoteAssetId, "schedule.quote_asset_id")
	if err != nil {
		return fees.Schedule{}, err
	}
	if b.QuoteDecimals < 0 || b.QuoteDecimals > 38 {
		return fees.Schedule{}, validationError("schedule.quote_decimals", "quote_decimals must be within 0..38")
	}
	s := fees.Schedule{
		Version: b.Label, QuoteAsset: quote, QuoteDecimals: uint8(b.QuoteDecimals), // #nosec G115 -- bounded to 0..38 above
		SpreadBPS: money.BPS(b.SpreadBps), PercentageFeeBPS: money.BPS(b.PercentageFeeBps), VenuePassThroughBPS: money.BPS(b.VenuePassThroughBps),
		SlippageReserveBPS: money.BPS(b.SlippageReserveBps), SubscriptionDiscountBPS: money.BPS(b.SubscriptionDiscountBps),
		StrategyFeeBPS: money.BPS(b.StrategyFeeBps), MaxTotalFeeBPS: money.BPS(b.MaxTotalFeeBps),
	}
	for _, f := range []struct {
		dst   *money.Quantity
		src   string
		field string
	}{
		{&s.FixedFee, b.FixedFee, "schedule.fixed_fee"}, {&s.NetworkFee.Amount, b.NetworkFeeAmount, "schedule.network_fee_amount"},
		{&s.MaxTotalFee, b.MaxTotalFee, "schedule.max_total_fee"}, {&s.MinFee, b.MinFee, "schedule.min_fee"},
	} {
		if *f.dst, err = treasuryQuantity(f.src, f.field); err != nil {
			return fees.Schedule{}, err
		}
	}
	if b.NetworkFeeAssetId != nil {
		if s.NetworkFee.Asset, err = treasuryAsset(*b.NetworkFeeAssetId, "schedule.network_fee_asset_id"); err != nil {
			return fees.Schedule{}, err
		}
	}
	for _, t := range b.Tiers {
		vol, err := feeUSD(t.MinTrailingVolumeUsd, "schedule.tiers.min_trailing_volume_usd")
		if err != nil {
			return fees.Schedule{}, err
		}
		s.Tiers = append(s.Tiers, fees.VolumeTier{MinTrailingVolume: vol, OverridesPercentageFee: t.OverridesPercentageFee,
			PercentageFeeBPS: money.BPS(t.PercentageFeeBps), OverridesSpread: t.OverridesSpread, SpreadBPS: money.BPS(t.SpreadBps)})
	}
	r := b.Rounding
	s.Rounding.AllowPlatformFavourable = r.AllowPlatformFavourable
	for _, m := range []struct {
		dst   *money.RoundingMode
		src   string
		field string
	}{
		{&s.Rounding.BuyNotional, r.BuyNotional, "buy_notional"}, {&s.Rounding.SellNotional, r.SellNotional, "sell_notional"},
		{&s.Rounding.Spread, r.Spread, "spread"}, {&s.Rounding.PercentageFee, r.PercentageFee, "percentage_fee"},
		{&s.Rounding.VenuePassThrough, r.VenuePassThrough, "venue_pass_through"}, {&s.Rounding.StrategyFee, r.StrategyFee, "strategy_fee"},
		{&s.Rounding.FeeCap, r.FeeCap, "fee_cap"}, {&s.Rounding.SlippageReserve, r.SlippageReserve, "slippage_reserve"},
	} {
		if *m.dst, err = feeRounding(m.field, m.src); err != nil {
			return fees.Schedule{}, err
		}
	}
	return s, nil
}

// --- response shapes --------------------------------------------------------------------------

func toAPIFeeSchedule(s fees.Schedule) api.FeeSchedule {
	out := api.FeeSchedule{
		Label: s.Version, QuoteAssetId: toUUID(s.QuoteAsset), QuoteDecimals: int(s.QuoteDecimals), SpreadBps: int(s.SpreadBPS),
		FixedFee: s.FixedFee.String(), PercentageFeeBps: int(s.PercentageFeeBPS), VenuePassThroughBps: int(s.VenuePassThroughBPS),
		NetworkFeeAmount: s.NetworkFee.Amount.String(), SlippageReserveBps: int(s.SlippageReserveBPS),
		SubscriptionDiscountBps: int(s.SubscriptionDiscountBPS), Tiers: make([]api.FeeTier, 0, len(s.Tiers)),
		StrategyFeeBps: int(s.StrategyFeeBPS), MaxTotalFeeBps: int(s.MaxTotalFeeBPS), MaxTotalFee: s.MaxTotalFee.String(),
		MinFee: s.MinFee.String(),
		Rounding: api.FeeRounding{
			BuyNotional: s.Rounding.BuyNotional.String(), SellNotional: s.Rounding.SellNotional.String(), Spread: s.Rounding.Spread.String(),
			PercentageFee: s.Rounding.PercentageFee.String(), VenuePassThrough: s.Rounding.VenuePassThrough.String(),
			StrategyFee: s.Rounding.StrategyFee.String(), FeeCap: s.Rounding.FeeCap.String(), SlippageReserve: s.Rounding.SlippageReserve.String(),
			AllowPlatformFavourable: s.Rounding.AllowPlatformFavourable,
		},
	}
	if !s.NetworkFee.Asset.IsZero() {
		out.NetworkFeeAssetId = uuidPtr(s.NetworkFee.Asset)
	}
	for _, t := range s.Tiers {
		out.Tiers = append(out.Tiers, api.FeeTier{MinTrailingVolumeUsd: t.MinTrailingVolume.String(), OverridesPercentageFee: t.OverridesPercentageFee,
			PercentageFeeBps: int(t.PercentageFeeBPS), OverridesSpread: t.OverridesSpread, SpreadBps: int(t.SpreadBPS)})
	}
	return out
}

func toAPIFeeScheduleVersion(v FeeScheduleVersionView) api.FeeScheduleVersion {
	p := v.Schedule
	return api.FeeScheduleVersion{
		ValueDomain: api.SIMULATED, Id: toUUID(p.ID), Key: p.Key, Version: p.Version, Schedule: toAPIFeeSchedule(p.Schedule),
		ScheduleHash: p.Hash, QuoteAsset: toAPITreasuryAssetRef(v.QuoteAsset), EffectiveFrom: p.EffectiveFrom.UTC(),
		PublishedAt: p.PublishedAt.UTC(), ActorType: string(p.Actor.Type), ActorId: p.Actor.ID, Reason: p.Reason, InForce: v.InForce,
	}
}

func toAPIFeePriceRecord(r fees.PriceRecord) api.FeePriceRecord {
	return api.FeePriceRecord{Mantissa: r.Mantissa.String(), Scale: int(r.Scale), QuoteAsset: r.QuoteAsset, Source: r.Source, At: r.At.UTC()}
}

func toAPIFeeDisclosure(d fees.Disclosure) api.FeeDisclosure {
	out := api.FeeDisclosure{
		Kind: d.Kind, ScheduleVersion: d.ScheduleVersion, ScheduleHash: d.ScheduleHash, QuoteAsset: toUUID(d.QuoteAsset),
		Input: api.FeeDisclosedInput{
			Side: api.FeeSide(d.Input.Side), BaseQty: d.Input.BaseQty.String(), BaseDecimals: int(d.Input.BaseDecimals),
			QuoteDecimals: int(d.Input.QuoteDecimals), ReferencePrice: toAPIFeePriceRecord(d.Input.ReferencePrice),
			TrailingVolumeUsd: d.Input.TrailingVolume.String(), HasSubscription: d.Input.HasSubscription,
			StrategyOriginated: d.Input.StrategyOriginated,
		},
		ReferencePrice: toAPIFeePriceRecord(d.ReferencePrice), ExecutionPrice: toAPIFeePriceRecord(d.ExecutionPrice),
		NotionalAtReference: d.NotionalAtReference.String(), NotionalAtExecution: d.NotionalAtExecution.String(),
		Fees: api.FeeDisclosedFees{
			Lines: make([]api.FeeDisclosedFeeLine, 0, len(d.Fees.Lines)), MinimumFeeTopUp: d.Fees.MinimumTopUp.String(),
			ComputedTotal: d.Fees.ComputedTotal.String(), Total: d.Fees.Total.String(), MinFee: d.Fees.MinFee.String(),
			MaxTotalFeeBps: int(d.Fees.MaxTotalFeeBPS), MaxTotalFeeBpsAmount: d.Fees.MaxBPSAmount.String(),
			MaxTotalFeeBpsRounding: d.Fees.MaxBPSRounding, MaxTotalFee: d.Fees.MaxTotalFee.String(), MinimumApplied: d.Fees.MinimumApplied,
			MaxBpsApplied: d.Fees.MaxBPSApplied, MaxAbsoluteApplied: d.Fees.MaxAbsoluteApplied,
		},
		TotalDebit: d.TotalDebit.String(), TotalCredit: d.TotalCredit.String(), WorstCaseTotal: d.WorstCaseTotal.String(),
		EffectivePrice: toAPIFeePriceRecord(d.EffectivePrice),
	}
	out.Tier.Index, out.Tier.MinTrailingVolumeUsd = d.Tier.Index, d.Tier.MinTrailingVolume.String()
	out.Tier.PercentageFeeBps, out.Tier.SpreadBps = int(d.Tier.PercentageFeeBPS), int(d.Tier.SpreadBPS)
	out.Spread.Bps, out.Spread.Amount, out.Spread.Rounding = int(d.Spread.BPS), d.Spread.Amount.String(), d.Spread.Rounding
	out.Network.Asset, out.Network.Amount, out.Network.InQuoteAsset = toUUID(d.Network.Asset), d.Network.Amount.String(), d.Network.InQuoteAsset
	out.SlippageReserve.Bps, out.SlippageReserve.Amount = int(d.SlippageReserve.BPS), d.SlippageReserve.Amount.String()
	out.SlippageReserve.Rounding, out.SlippageReserve.HeldNotCharged = d.SlippageReserve.Rounding, d.SlippageReserve.HeldNotCharged
	out.Rounding.Components = make([]api.FeeDisclosedMode, 0, len(d.Rounding.Components))
	for _, c := range d.Rounding.Components {
		out.Rounding.Components = append(out.Rounding.Components, api.FeeDisclosedMode{Component: string(c.Component), Mode: c.Mode,
			FavoursPlatform: c.FavoursPlatform})
	}
	out.Rounding.PlatformFavourableAllowed, out.Rounding.AnyFavoursPlatform = d.Rounding.PlatformFavourableAllowed, d.Rounding.AnyFavoursPlatform
	for _, l := range d.Fees.Lines {
		out.Fees.Lines = append(out.Fees.Lines, api.FeeDisclosedFeeLine{
			Component: string(l.Component), RateBps: int(l.RateBPS), DiscountBps: int(l.DiscountBPS), FixedAmount: l.FixedAmount.String(),
			Rounding: l.Rounding, Computed: l.Computed.String(), CapReduction: l.CapReduction.String(), Charged: l.Charged.String(),
		})
	}
	return out
}

func toAPIFeeCharges(c feeledger.Charges) api.FeeCharges {
	out := api.FeeCharges{QuoteAssetId: toUUID(c.QuoteAsset), Spread: c.Spread.String(), Platform: c.Platform.String(),
		Strategy: c.Strategy.String(), Venue: c.Venue.String(), Network: c.Network.String(), NetworkInQuote: c.NetworkInQuote}
	if !c.NetworkAsset.IsZero() {
		out.NetworkAssetId = uuidPtr(c.NetworkAsset)
	}
	return out
}

func toAPIFeeVerification(v *FeeVerification) api.FeeVerification {
	if v == nil {
		return api.FeeVerification{Verified: false, Reason: strPtr("NOT_VERIFIED")}
	}
	return api.FeeVerification{Verified: v.Verified, Code: strPtr(v.Code), Reason: strPtr(v.Reason), Detail: strPtr(v.Detail)}
}

func toAPIFeeQuoteRecord(v FeeQuoteView) api.FeeQuoteRecord {
	q := v.Quote
	out := api.FeeQuoteRecord{
		ValueDomain: api.SIMULATED, Id: toUUID(q.ID), ScheduleId: toUUID(q.ScheduleID), ScheduleKey: q.ScheduleKey,
		ScheduleVersion: q.ScheduleVersion, ScheduleHash: q.ScheduleHash, Subsystem: string(q.Subsystem), Side: api.FeeSide(q.Side),
		Disclosure: toAPIFeeDisclosure(q.Disclosure), DisclosureHash: q.DisclosureHash, Charges: toAPIFeeCharges(q.Charges),
		QuotedAt: q.QuotedAt.UTC(), ValidUntil: q.ValidUntil.UTC(),
	}
	if q.AccountID != nil {
		out.AccountId = uuidPtr(*q.AccountID)
	}
	if q.BaseAsset != nil {
		out.BaseAssetId = uuidPtr(*q.BaseAsset)
	}
	if v.Verification != nil {
		ver := toAPIFeeVerification(v.Verification)
		out.Verification = &ver
	}
	return out
}

func toAPIFeeBooking(v FeeBookingView) api.FeeBooking {
	b, q := v.Booking, v.Quote
	d := q.Disclosure
	out := api.FeeBooking{
		ValueDomain: api.SIMULATED, Id: toUUID(b.ID), DisclosureId: toUUID(b.DisclosureID), AccountId: toUUID(b.AccountID),
		Mode: api.AdvancedMode(b.Mode), BookingMode: api.FeeBookingMode(b.BookingMode), JournalKind: string(b.Kind),
		JournalTransactionId: toUUID(b.JournalTransactionID), EffectId: toUUID(b.EffectID), Charges: toAPIFeeCharges(b.Charges),
		Status: api.FeeBookingStatus(b.Status), EvidenceRef: b.EvidenceRef, BookedAt: b.BookedAt.UTC(),
		ReversalReasonCode: strPtr(b.ReversalReasonCode), ReversedAt: timePtr(b.ReversedAt),
		SpreadLine: api.FeeSpreadLine{
			ScheduleKey: q.ScheduleKey, ScheduleVersion: q.ScheduleVersion, ScheduleHash: q.ScheduleHash, DisclosureHash: q.DisclosureHash,
			Side: api.FeeSide(q.Side), BaseQuantity: d.Input.BaseQty.String(), ReferencePrice: toAPITreasuryPrice(d.ReferencePrice.Price()),
			ExecutionPrice: toAPITreasuryPrice(d.ExecutionPrice.Price()), SpreadBps: int(d.Spread.BPS), SpreadAmount: b.Charges.Spread.String(),
			QuoteAssetId: toUUID(b.Charges.QuoteAsset),
		},
	}
	if b.IntentID != nil {
		out.IntentId = uuidPtr(*b.IntentID)
	}
	if b.ReversalJournalID != nil {
		out.ReversalJournalTransactionId = uuidPtr(*b.ReversalJournalID)
	}
	if b.ReversalEffectID != nil {
		out.ReversalEffectId = uuidPtr(*b.ReversalEffectID)
	}
	return out
}

func toAPIFeeFlow(f feeledger.Flow) api.FeeFlow {
	return api.FeeFlow{Id: toUUID(f.ID), Seq: f.Seq, At: f.At.UTC(), Amount: f.Amount.String(), NavBefore: f.NAVBefore.String(),
		ActorType: string(f.Actor.Type), ActorId: f.Actor.ID}
}

func toAPIFeeLine(l feeledger.Line) api.FeePerformanceLine {
	out := api.FeePerformanceLine{
		Id: toUUID(l.ID), Seq: l.Seq, Kind: api.FeeLineKind(l.Kind), At: l.At.UTC(), Nav: l.NAV.String(), HwmAdjusted: l.HWMAdjusted.String(),
		HurdleThreshold: l.HurdleThreshold.String(), FeeBase: l.FeeBase.String(), Amount: l.Amount.String(),
		AccrualBefore: l.AccrualBefore.String(), AccrualAfter: l.AccrualAfter.String(), EffectId: toUUID(l.EffectID),
	}
	if l.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*l.JournalTransactionID)
	}
	return out
}

func toAPIFeeStatement(st fees.PerformanceStatement) api.FeeStatement {
	out := api.FeeStatement{
		Kind: st.Kind, Mode: st.Mode, PeriodStart: st.PeriodStart.UTC(), PeriodEnd: st.PeriodEnd.UTC(), OpeningNav: st.OpeningNAV.String(),
		Flows: make([]api.FeeStatementFlow, 0, len(st.Flows)), NetFlows: st.NetFlows.String(), ClosingNavGross: st.ClosingNAVGross.String(),
		GrossPnl: st.GrossPnL.String(), HwmBefore: st.HWMBefore.String(), HwmAdjusted: st.HWMAdjusted.String(),
		HurdleAnnualBps: int64(st.Hurdle.AnnualBPS), HurdleThreshold: st.Hurdle.Threshold.String(), FeeBase: st.FeeBase.String(),
		FeeRateBps: int64(st.FeeRateBPS), FeeAmount: st.FeeAmount.String(), ClosingNavNet: st.ClosingNAVNet.String(),
		HwmAfter: st.HWMAfter.String(), PriorAccruedFee: st.PriorAccruedFee.String(),
		Attribution: make([]api.FeeAttribution, 0, len(st.Attribution)),
		Rounding: map[string]string{
			"hwm_withdrawal": st.Rounding.HWMWithdrawal, "hurdle_threshold": st.Rounding.HurdleThreshold, "fee": st.Rounding.Fee,
			"attribution": st.Rounding.Attribution,
		},
	}
	for _, f := range st.Flows {
		out.Flows = append(out.Flows, api.FeeStatementFlow{At: f.At.UTC(), Amount: f.Amount.String(), NavBefore: f.NAVBefore.String(),
			NavAfter: f.NAVAfter.String(), HwmBefore: f.HWMBefore.String(), HwmAfter: f.HWMAfter.String()})
	}
	for _, a := range st.Attribution {
		out.Attribution = append(out.Attribution, api.FeeAttribution{Strategy: a.Strategy, GrossPnl: a.GrossPnL.String(), Eligible: a.Eligible,
			FeeShare: a.FeeShare.String(), RemainderAllocated: a.RemainderAllocated.String()})
	}
	return out
}

func toAPIFeePeriod(v FeePeriodView) api.FeePeriod {
	p := v.Period
	t := p.Terms
	out := api.FeePeriod{
		ValueDomain: api.SIMULATED, Id: toUUID(p.ID), AccountId: toUUID(p.AccountID), StrategyRef: p.StrategyRef, Seq: p.Seq,
		Mode: api.AdvancedMode(p.Mode), FeeAssetId: toUUID(p.FeeAsset),
		Terms: api.FeePeriodTerms{
			AccountRef: t.AccountRef, TermsVersion: t.TermsVersion, PeriodStart: t.PeriodStart.UTC(), PeriodEnd: t.PeriodEnd.UTC(),
			OpeningNav: t.OpeningNAV.String(), PriorHwm: t.PriorHWM.String(), HurdleAnnualBps: int64(t.HurdleAnnualBPS),
			FeeRateBps: int64(t.FeeRateBPS), Crystallization: string(t.Crystallization), PriorAccruedFee: t.PriorAccruedFee.String(),
		},
		TermsHash: p.TermsHash, Status: api.FeePeriodStatus(p.Status), Accrued: p.Accrued.String(), LastValuationAt: timePtr(p.LastValuationAt),
		Flows: make([]api.FeeFlow, 0, len(p.Flows)), Lines: make([]api.FeePerformanceLine, 0, len(p.Lines)),
		StatementHash: strPtr(p.StatementHash), CrystallizedAt: timePtr(p.CrystallizedAt), ReversalReasonCode: strPtr(p.ReversalReasonCode),
		ReversedAt: timePtr(p.ReversedAt),
	}
	if p.PreviousID != nil {
		out.PreviousPeriodId = uuidPtr(*p.PreviousID)
	}
	for _, f := range p.Flows {
		out.Flows = append(out.Flows, toAPIFeeFlow(f))
	}
	for _, l := range p.Lines {
		out.Lines = append(out.Lines, toAPIFeeLine(l))
	}
	if p.Statement != nil {
		st := toAPIFeeStatement(*p.Statement)
		out.Statement = &st
	}
	if p.CrystallizationJournalID != nil {
		out.CrystallizationJournalTransactionId = uuidPtr(*p.CrystallizationJournalID)
	}
	if p.CrystallizationEffectID != nil {
		out.CrystallizationEffectId = uuidPtr(*p.CrystallizationEffectID)
	}
	if p.ReversalJournalID != nil {
		out.ReversalJournalTransactionId = uuidPtr(*p.ReversalJournalID)
	}
	if p.GrossReturnBPS != nil {
		g := int64(*p.GrossReturnBPS)
		out.GrossReturnBps = &g
	}
	if p.NetReturnBPS != nil {
		n := int64(*p.NetReturnBPS)
		out.NetReturnBps = &n
	}
	if v.Verification != nil {
		ver := toAPIFeeVerification(v.Verification)
		out.Verification = &ver
	}
	return out
}
