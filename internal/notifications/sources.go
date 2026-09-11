package notifications

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
)

// The eight sources. Each one is a query over rows a domain service already
// wrote, a mapping from a state name to a kind, and the copy a person reads.
//
// Nothing here computes a figure. Quantities are cast to text in SQL and
// carried verbatim into the notification's data, because a base-unit integer is
// exact and a rendered amount is a presentation decision the client owns
// together with the asset's decimals.

// ---------------------------------------------------------------------------
// Credit purchases
// ---------------------------------------------------------------------------

// creditFundingKinds maps a credit funding state to the kind it notifies as.
// States absent from the map are steps the person did not ask to hear about:
// CREATED, AUTHORIZATION_PENDING, AUTHORIZED, CAPTURE_PENDING and REVERSIBLE
// are the machinery of a card payment, and SETTLED means only that the
// reversibility window closed, which is not news to the person who was told
// about the capture days earlier.
var creditFundingKinds = map[string]Kind{
	"CAPTURED": KindCreditPurchaseCaptured,
	"REVERSED": KindCreditPurchaseReversed,
	"REFUNDED": KindCreditPurchaseReversed,
	"DISPUTED": KindCreditPurchaseReversed,
	"FAILED":   KindCreditPurchaseReversed,
}

func readCreditFundings(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `SELECT t.occurred_at, t.id::text, a.owner_user_id, f.account_id, t.to_state,
			f.id::text, f.credit_quantity::text, coalesce(t.correlation_id, ''), coalesce(f.failure_reason, '')
		FROM credit_funding_transitions t
		JOIN credit_fundings f ON f.id = t.funding_id
		JOIN accounts a ON a.id = f.account_id
		WHERE `+keysetOn("t.occurred_at", "t.id")+` AND t.to_state = ANY($3::text[])
		  -- A transition that did not move the verification state is not a
		  -- verification update. Since 00796 a row on this table may record a
		  -- change to the SANCTIONS screen while the state stands still, and
		  -- "your verification was updated" is not what happened to that person.
		  AND t.from_state IS DISTINCT FROM t.to_state
		ORDER BY t.occurred_at, t.id LIMIT $4`,
		at.UTC(), nullable(rowID), keysOf(creditFundingKinds), limit)
	if err != nil {
		return nil, fmt.Errorf("read credit funding transitions: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var (
			occurredAt                             time.Time
			transitionID, toState, fundingID       string
			quantity, correlationID, failureReason string
			userID                                 accounts.UserID
			accountID                              accounts.AccountID
		)
		if err := rows.Scan(&occurredAt, &transitionID, &userID, &accountID, &toState,
			&fundingID, &quantity, &correlationID, &failureReason); err != nil {
			return nil, fmt.Errorf("read credit funding transitions: %w", err)
		}
		kind := creditFundingKinds[toState]
		title, body := creditFundingCopy(toState, failureReason)
		out = append(out, Change{
			At:    occurredAt,
			RowID: transitionID,
			Notify: []Notification{{
				UserID:        userID,
				AccountID:     accountPtr(accountID),
				Kind:          kind,
				Title:         title,
				Body:          body,
				Ref:           Ref{Type: "credit_funding", ID: fundingID},
				Occurrence:    transitionID,
				CorrelationID: correlationID,
				OccurredAt:    occurredAt,
				Data: mustJSON(map[string]any{
					"funding_id":      fundingID,
					"to_state":        toState,
					"credit_quantity": quantity,
				}),
			}},
			Signal: []Signal{{UserID: userID.String(), Scope: ScopeBalance}},
		})
	}
	return out, rows.Err()
}

func creditFundingCopy(state, failureReason string) (title, body string) {
	switch state {
	case "CAPTURED":
		return "Your Credits are available",
			"The payment for your Credit purchase completed and the Credits are in your balance."
	case "REFUNDED":
		return "Your Credit purchase was refunded",
			"The payment was refunded. The Credits it issued are no longer in your balance."
	case "DISPUTED":
		return "Your Credit purchase is disputed",
			"The card issuer has raised a dispute against this payment. The Credits it issued are held while it is resolved."
	case "FAILED":
		body = "The payment did not complete, so no Credits were issued."
		if failureReason != "" {
			body += " Reason recorded: " + failureReason + "."
		}
		return "Your Credit purchase did not complete", body
	default: // REVERSED
		return "Your Credit purchase was reversed",
			"The payment was reversed. The Credits it issued are no longer in your balance."
	}
}

// ---------------------------------------------------------------------------
// Payout requests
// ---------------------------------------------------------------------------

// payoutKinds maps a payout request state to the kind it notifies as. DRAFT,
// ELIGIBILITY_CHECK, VERIFIED and SUBMITTED-adjacent internal steps are absent
// on purpose: a person asked for a payout and wants to know that it left, that
// it arrived, that it stopped, or that it needs them.
var payoutKinds = map[string]Kind{
	"SUBMITTED":             KindPayoutAccepted,
	"PROVIDER_PENDING":      KindPayoutAccepted,
	"SETTLED":               KindPayoutSettled,
	"FAILED":                KindPayoutFailed,
	"REJECTED":              KindPayoutFailed,
	"REVERSED":              KindPayoutFailed,
	"MANUAL_REVIEW":         KindPayoutNeedsReview,
	"VERIFICATION_REQUIRED": KindPayoutNeedsReview,
	"PAYOUT_STATUS_UNKNOWN": KindPayoutNeedsReview,
}

func readPayoutRequests(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `SELECT t.occurred_at, t.id::text, a.owner_user_id, p.account_id, t.to_state,
			p.id::text, p.requested_quantity::text, coalesce(t.correlation_id, ''), coalesce(p.failure_reason, '')
		FROM payout_request_transitions t
		JOIN payout_requests p ON p.id = t.request_id
		JOIN accounts a ON a.id = p.account_id
		WHERE `+keysetOn("t.occurred_at", "t.id")+` AND t.to_state = ANY($3::text[])
		  -- A transition that did not move the verification state is not a
		  -- verification update. Since 00796 a row on this table may record a
		  -- change to the SANCTIONS screen while the state stands still, and
		  -- "your verification was updated" is not what happened to that person.
		  AND t.from_state IS DISTINCT FROM t.to_state
		ORDER BY t.occurred_at, t.id LIMIT $4`,
		at.UTC(), nullable(rowID), keysOf(payoutKinds), limit)
	if err != nil {
		return nil, fmt.Errorf("read payout request transitions: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var (
			occurredAt                             time.Time
			transitionID, toState, payoutID        string
			quantity, correlationID, failureReason string
			userID                                 accounts.UserID
			accountID                              accounts.AccountID
		)
		if err := rows.Scan(&occurredAt, &transitionID, &userID, &accountID, &toState,
			&payoutID, &quantity, &correlationID, &failureReason); err != nil {
			return nil, fmt.Errorf("read payout request transitions: %w", err)
		}
		title, body := payoutCopy(toState, failureReason)
		signals := []Signal{{UserID: userID.String(), Scope: ScopePayout, Ref: payoutID}}
		if toState == "SETTLED" || toState == "REVERSED" || toState == "FAILED" || toState == "REJECTED" {
			signals = append(signals, Signal{UserID: userID.String(), Scope: ScopeBalance})
		}
		out = append(out, Change{
			At:    occurredAt,
			RowID: transitionID,
			Notify: []Notification{{
				UserID:        userID,
				AccountID:     accountPtr(accountID),
				Kind:          payoutKinds[toState],
				Title:         title,
				Body:          body,
				Ref:           Ref{Type: "payout_request", ID: payoutID},
				Occurrence:    transitionID,
				CorrelationID: correlationID,
				OccurredAt:    occurredAt,
				Data: mustJSON(map[string]any{
					"payout_id":          payoutID,
					"to_state":           toState,
					"requested_quantity": quantity,
				}),
			}},
			Signal: signals,
		})
	}
	return out, rows.Err()
}

func payoutCopy(state, failureReason string) (title, body string) {
	switch state {
	case "SUBMITTED", "PROVIDER_PENDING":
		return "Your payout is on its way",
			"Your payout request reached the payout provider. You will be told again when it settles."
	case "SETTLED":
		return "Your payout settled", "The payout provider reported this payout as settled."
	case "MANUAL_REVIEW":
		return "Your payout needs a review",
			"This payout stopped for a manual review. Nothing further is required from you until you are asked."
	case "VERIFICATION_REQUIRED":
		return "Your payout needs verification",
			"This payout cannot continue until your identity verification is complete."
	case "PAYOUT_STATUS_UNKNOWN":
		return "Your payout's status is not yet known",
			"The payout provider has not reported an outcome. It is treated as in flight, never as failed, until it does."
	default: // FAILED, REJECTED, REVERSED
		body = "This payout did not complete. The Credits it reserved are returned to your balance."
		if failureReason != "" {
			body += " Reason recorded: " + failureReason + "."
		}
		return "Your payout did not complete", body
	}
}

// ---------------------------------------------------------------------------
// Native market fills
// ---------------------------------------------------------------------------

func readNativeFills(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `SELECT f.created_at, f.id::text, a.owner_user_id, f.account_id, f.side,
			f.market_id::text, na.symbol,
			f.credits_in::text, f.credits_out::text, f.assets_in::text, f.assets_out::text
		FROM native_market_fills f
		JOIN accounts a ON a.id = f.account_id
		JOIN native_markets m ON m.id = f.market_id
		JOIN native_assets na ON na.asset_id = m.asset_id
		WHERE `+keysetOn("f.created_at", "f.id")+`
		ORDER BY f.created_at, f.id LIMIT $3`,
		at.UTC(), nullable(rowID), limit)
	if err != nil {
		return nil, fmt.Errorf("read native market fills: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var (
			createdAt                                  time.Time
			fillID, side, marketID, symbol             string
			creditsIn, creditsOut, assetsIn, assetsOut string
			userID                                     accounts.UserID
			accountID                                  accounts.AccountID
		)
		if err := rows.Scan(&createdAt, &fillID, &userID, &accountID, &side, &marketID, &symbol,
			&creditsIn, &creditsOut, &assetsIn, &assetsOut); err != nil {
			return nil, fmt.Errorf("read native market fills: %w", err)
		}
		title := "You bought " + symbol
		body := "Your buy order for " + symbol + " filled."
		if side == "SELL" {
			title = "You sold " + symbol
			body = "Your sell order for " + symbol + " filled."
		}
		out = append(out, Change{
			At:    createdAt,
			RowID: fillID,
			Notify: []Notification{{
				UserID:     userID,
				AccountID:  accountPtr(accountID),
				Kind:       KindNativeTradeFilled,
				Title:      title,
				Body:       body + " The exact figures are on the trade itself.",
				Ref:        Ref{Type: "native_market_fill", ID: fillID},
				Occurrence: fillID,
				OccurredAt: createdAt,
				Data: mustJSON(map[string]any{
					"fill_id":     fillID,
					"market_id":   marketID,
					"symbol":      symbol,
					"side":        side,
					"credits_in":  creditsIn,
					"credits_out": creditsOut,
					"assets_in":   assetsIn,
					"assets_out":  assetsOut,
				}),
			}},
			Signal: []Signal{
				{UserID: userID.String(), Scope: ScopePosition, Ref: marketID},
				{UserID: userID.String(), Scope: ScopeBalance},
				// A fill moves the market's price, and a market's price is
				// public: every client may already read it over REST, so
				// telling every client it moved leaks nothing.
				{Scope: ScopeMarket, Ref: marketID, Broadcast: true},
			},
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Native market pauses
// ---------------------------------------------------------------------------

// pausedStatuses are the market statuses that stop a person doing something
// they could do yesterday. ACTIVE and PENDING are not among them, and DELISTED
// is, because a delisted market is the most complete form of a pause.
var pausedStatuses = []string{"CLOSE_ONLY", "HALTED", "FROZEN", "DELISTED"}

func readMarketPauses(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `SELECT t.occurred_at, t.id::text, t.market_id::text, t.to_status, na.symbol,
			coalesce(t.correlation_id, '')
		FROM native_market_transitions t
		JOIN native_markets m ON m.id = t.market_id
		JOIN native_assets na ON na.asset_id = m.asset_id
		WHERE `+keysetOn("t.occurred_at", "t.id")+` AND t.to_status = ANY($3::text[])
		ORDER BY t.occurred_at, t.id LIMIT $4`,
		at.UTC(), nullable(rowID), pausedStatuses, limit)
	if err != nil {
		return nil, fmt.Errorf("read native market transitions: %w", err)
	}
	defer rows.Close()
	type pause struct {
		at                                                    time.Time
		transitionID, marketID, status, symbol, correlationID string
	}
	var pauses []pause
	for rows.Next() {
		var p pause
		if err := rows.Scan(&p.at, &p.transitionID, &p.marketID, &p.status, &p.symbol, &p.correlationID); err != nil {
			return nil, fmt.Errorf("read native market transitions: %w", err)
		}
		pauses = append(pauses, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]Change, 0, len(pauses))
	for _, p := range pauses {
		// Everyone who has ever traded this market. The fan-out is bounded by
		// the launch tier's own account ceiling (CP_CAPACITY_MAX_ACCOUNTS), so
		// it is not capped here: a cap would silently leave some holders
		// untold, which is worse than a slow pass.
		holders, err := marketParticipants(ctx, q, p.marketID)
		if err != nil {
			return nil, err
		}
		title, body := marketPauseCopy(p.symbol, p.status)
		notify := make([]Notification, 0, len(holders))
		signals := []Signal{{Scope: ScopeMarket, Ref: p.marketID, Broadcast: true}}
		for _, h := range holders {
			notify = append(notify, Notification{
				UserID:        h.userID,
				AccountID:     accountPtr(h.accountID),
				Kind:          KindNativeMarketPaused,
				Title:         title,
				Body:          body,
				Ref:           Ref{Type: "native_market", ID: p.marketID},
				Occurrence:    p.transitionID,
				CorrelationID: p.correlationID,
				OccurredAt:    p.at,
				Data: mustJSON(map[string]any{
					"market_id": p.marketID,
					"symbol":    p.symbol,
					"to_status": p.status,
				}),
			})
			signals = append(signals, Signal{UserID: h.userID.String(), Scope: ScopePosition, Ref: p.marketID})
		}
		out = append(out, Change{At: p.at, RowID: p.transitionID, Notify: notify, Signal: signals})
	}
	return out, nil
}

type participant struct {
	userID    accounts.UserID
	accountID accounts.AccountID
}

func marketParticipants(ctx context.Context, q db.Querier, marketID string) ([]participant, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT a.owner_user_id, f.account_id
		FROM native_market_fills f JOIN accounts a ON a.id = f.account_id
		WHERE f.market_id = $1::uuid`, marketID)
	if err != nil {
		return nil, fmt.Errorf("read market participants: %w", err)
	}
	defer rows.Close()
	var out []participant
	for rows.Next() {
		var p participant
		if err := rows.Scan(&p.userID, &p.accountID); err != nil {
			return nil, fmt.Errorf("read market participants: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func marketPauseCopy(symbol, status string) (title, body string) {
	switch status {
	case "CLOSE_ONLY":
		return symbol + " is close-only",
			"This market has stopped accepting buys. You can still sell what you hold."
	case "DELISTED":
		return symbol + " was delisted",
			"This market no longer trades. Your holding is unchanged and still shown in your portfolio."
	case "FROZEN":
		return symbol + " is frozen",
			"This market has stopped accepting buys and sells. Your holding is unchanged."
	default: // HALTED
		return symbol + " is halted",
			"Trading in this market is paused. Your holding is unchanged."
	}
}

// ---------------------------------------------------------------------------
// Account status
// ---------------------------------------------------------------------------

var restrictedStatuses = []string{"RESTRICTED", "FROZEN", "CLOSED"}

func readAccountStatus(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `SELECT t.occurred_at, t.id::text, a.owner_user_id, t.account_id, t.to_status,
			t.reason, coalesce(t.correlation_id, '')
		FROM account_status_transitions t
		JOIN accounts a ON a.id = t.account_id
		WHERE `+keysetOn("t.occurred_at", "t.id")+` AND t.to_status = ANY($3::text[])
		ORDER BY t.occurred_at, t.id LIMIT $4`,
		at.UTC(), nullable(rowID), restrictedStatuses, limit)
	if err != nil {
		return nil, fmt.Errorf("read account status transitions: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var (
			occurredAt                                  time.Time
			transitionID, toStatus, reason, correlation string
			userID                                      accounts.UserID
			accountID                                   accounts.AccountID
		)
		if err := rows.Scan(&occurredAt, &transitionID, &userID, &accountID, &toStatus, &reason, &correlation); err != nil {
			return nil, fmt.Errorf("read account status transitions: %w", err)
		}
		out = append(out, Change{
			At:    occurredAt,
			RowID: transitionID,
			Notify: []Notification{{
				UserID:        userID,
				AccountID:     accountPtr(accountID),
				Kind:          KindAccountRestricted,
				Title:         accountStatusTitle(toStatus),
				Body:          accountStatusBody(toStatus, reason),
				Ref:           Ref{Type: "account", ID: accountID.String()},
				Occurrence:    transitionID,
				CorrelationID: correlation,
				OccurredAt:    occurredAt,
				Data:          mustJSON(map[string]any{"account_id": accountID.String(), "to_status": toStatus}),
			}},
			Signal: []Signal{{UserID: userID.String(), Scope: ScopeAccount, Ref: accountID.String()}},
		})
	}
	return out, rows.Err()
}

func accountStatusTitle(status string) string {
	switch status {
	case "FROZEN":
		return "Your account is frozen"
	case "CLOSED":
		return "Your account is closed"
	default:
		return "Your account is restricted"
	}
}

func accountStatusBody(status, reason string) string {
	base := "Some actions are no longer available on this account."
	switch status {
	case "FROZEN":
		base = "No money can move on this account while it is frozen. Your balances are unchanged."
	case "CLOSED":
		base = "This account is closed and can no longer be used."
	}
	if r := strings.TrimSpace(reason); r != "" {
		base += " Reason recorded: " + r + "."
	}
	return base
}

// ---------------------------------------------------------------------------
// New sessions
// ---------------------------------------------------------------------------

// readNewSessions follows security_events rather than the sessions table.
// A session row is UPDATEd -- last_seen_at moves on every request -- so it is
// not an append-only log and a cursor over it would re-read every active
// session forever. The 'login' security event is written in the same
// transaction as the session it describes (internal/identity), is immutable,
// and is the durable record the retention policy deliberately keeps.
func readNewSessions(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `SELECT e.occurred_at, e.id::text, e.user_id, e.session_id::text,
			coalesce(host(e.ip), ''), coalesce(e.user_agent, '')
		FROM security_events e
		WHERE `+keysetOn("e.occurred_at", "e.id")+`
		  AND e.kind = 'login' AND e.user_id IS NOT NULL AND e.session_id IS NOT NULL
		ORDER BY e.occurred_at, e.id LIMIT $3`,
		at.UTC(), nullable(rowID), limit)
	if err != nil {
		return nil, fmt.Errorf("read login security events: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var (
			occurredAt         time.Time
			eventID, sessionID string
			ip, userAgent      string
			userID             accounts.UserID
		)
		if err := rows.Scan(&occurredAt, &eventID, &userID, &sessionID, &ip, &userAgent); err != nil {
			return nil, fmt.Errorf("read login security events: %w", err)
		}
		body := "A new session was created for your account. If this was not you, sign out every session from Settings and sign in again."
		out = append(out, Change{
			At:    occurredAt,
			RowID: eventID,
			Notify: []Notification{{
				UserID:     userID,
				Kind:       KindSecurityNewSession,
				Title:      "New sign-in to your account",
				Body:       body,
				Ref:        Ref{Type: "session", ID: sessionID},
				Occurrence: eventID,
				OccurredAt: occurredAt,
				// The address and the agent are the person's own, and they are
				// what makes "was this me?" answerable at all.
				Data: mustJSON(map[string]any{"session_id": sessionID, "ip": ip, "user_agent": userAgent}),
			}},
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Verification decisions (D-082)
// ---------------------------------------------------------------------------

// verificationKinds maps a financial verification state to the kind it
// notifies as. Seven of the ten states are here; UNVERIFIED, STARTED and
// PENDING are not, and that is the whole of the editorial decision.
//
// STARTED and PENDING are the machinery of a provider session the person is
// standing in front of: they already know, and telling them "we are thinking
// about it" every time a provider moves an internal state would make the one
// message that matters -- the decision -- arrive in a queue of noise.
// UNVERIFIED is where everybody starts.
//
// EXPIRED is here and is deliberately NOT a failure message. A decision that
// aged out of its validity window is not a rejection (D-061), and the copy has
// to say the difference, because the two land in the same inbox.
var verificationKinds = map[string]Kind{
	"REQUIRED":          KindVerificationUpdated,
	"NEEDS_INFORMATION": KindVerificationUpdated,
	"VERIFIED":          KindVerificationUpdated,
	"REJECTED":          KindVerificationUpdated,
	"EXPIRED":           KindVerificationUpdated,
	"RESTRICTED":        KindVerificationUpdated,
	"SUSPENDED":         KindVerificationUpdated,
}

// readVerificationUpdates follows compliance_profile_transitions.
//
// The notification is addressed to the PERSON and carries no account id.
// Verification is a property of a person -- somebody with three accounts
// verifies once and all three see the same level -- so joining `accounts` here
// would produce three Emit calls that the unique index then collapses into one,
// and the one that survived would name whichever account sorted first.
//
// The sandbox flag rides in the data rather than on the row: Producer stamps
// notifications.sandbox from the DEPLOYMENT (a sandbox tier labels everything
// it writes and a live one may label nothing), so a rehearsal decision says so
// in the words a person reads instead.
func readVerificationUpdates(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `SELECT t.occurred_at, t.id::text, t.user_id, t.to_state,
			coalesce(t.reason, ''), coalesce(t.correlation_id, ''), coalesce(vs.sandbox, false)
		FROM compliance_profile_transitions t
		LEFT JOIN verification_sessions vs ON vs.id = t.session_id
		WHERE `+keysetOn("t.occurred_at", "t.id")+` AND t.to_state = ANY($3::text[])
		  -- A transition that did not move the verification state is not a
		  -- verification update. Since 00796 a row on this table may record a
		  -- change to the SANCTIONS screen while the state stands still, and
		  -- "your verification was updated" is not what happened to that person.
		  AND t.from_state IS DISTINCT FROM t.to_state
		ORDER BY t.occurred_at, t.id LIMIT $4`,
		at.UTC(), nullable(rowID), keysOf(verificationKinds), limit)
	if err != nil {
		return nil, fmt.Errorf("read compliance profile transitions: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var (
			occurredAt            time.Time
			transitionID, toState string
			reason, correlationID string
			sandbox               bool
			userID                accounts.UserID
		)
		if err := rows.Scan(&occurredAt, &transitionID, &userID, &toState,
			&reason, &correlationID, &sandbox); err != nil {
			return nil, fmt.Errorf("read compliance profile transitions: %w", err)
		}
		title, body := verificationCopy(toState, sandbox)
		out = append(out, Change{
			At:    occurredAt,
			RowID: transitionID,
			Notify: []Notification{{
				UserID: userID,
				Kind:   KindVerificationUpdated,
				Title:  title,
				Body:   body,
				// The reference is the person's own profile: there is one, it
				// is what the surface refetches, and a provider reference or a
				// session id would put a vendor's identifier in a customer's
				// inbox (PROVIDER_BOUNDARY SS1 role B).
				Ref:           Ref{Type: "compliance_profile", ID: userID.String()},
				Occurrence:    transitionID,
				CorrelationID: correlationID,
				OccurredAt:    occurredAt,
				Data: mustJSON(map[string]any{
					"to_state": toState,
					"sandbox":  sandbox,
				}),
			}},
			Signal: []Signal{
				{UserID: userID.String(), Scope: ScopeVerification},
				// A verification level is an input to withdrawal eligibility,
				// and the eligibility page is the one a person is most likely
				// to be staring at while this arrives.
				{UserID: userID.String(), Scope: ScopeEligibility},
			},
		})
	}
	return out, rows.Err()
}

// verificationCopy. Nothing here names a provider, quotes a provider's reason,
// or restates a sub-check: Nodal stores a decision and a reference, and what a
// customer is told is the decision.
func verificationCopy(state string, sandbox bool) (title, body string) {
	switch state {
	case "VERIFIED":
		title, body = "Your identity is verified",
			"Your identity verification completed. What you can do with your balance is on the withdraw page."
	case "EXPIRED":
		title, body = "Your identity verification expired",
			"Verification decisions are valid for a year and this one has reached the end of its window. "+
				"This is not a rejection: you can verify again whenever you want to."
	case "REJECTED":
		title, body = "Your identity verification was not approved",
			"The check did not complete in your favour. Your balance is unchanged; what changes is what may leave the platform."
	case "NEEDS_INFORMATION":
		title, body = "Your identity verification needs more information",
			"The check stopped and needs something more from you before it can continue."
	case "RESTRICTED":
		title, body = "Your verification carries a restriction",
			"Your identity is established and something limits what it permits. The withdraw page says which."
	case "SUSPENDED":
		title, body = "Your identity verification is suspended",
			"Verification is stopped pending a review. Your balance is unchanged."
	default: // REQUIRED
		title, body = "Identity verification is needed",
			"Something you asked for needs your identity verified first. Nothing else about your account changed."
	}
	if sandbox {
		body += " This was a SANDBOX rehearsal: no provider assessed anybody and this is not an approval."
	}
	return title, body
}

// ---------------------------------------------------------------------------
// Agent pauses (D-082)
// ---------------------------------------------------------------------------

// readAgentPauses follows agent_pauses, and deliberately not every row of it.
//
// An owner who pauses their own agent pressed the button; a notification saying
// what they just did is the noise that teaches people to ignore the inbox. What
// a person cannot know without being told is that somebody ELSE stopped their
// agent -- an operator, the kill switch, a budget, a risk violation -- so the
// predicate is the pause's own actor type, and it is the same predicate
// cmd/api's publisher uses so the two cannot disagree about who hears what.
//
// It orders on paused_at, which never moves. agent_pauses IS updated -- a
// resume stamps resumed_at -- so a cursor over updated_at would re-read every
// pause forever, which is the mistake readNewSessions documents avoiding.
const agentPauseActorPredicate = `ap.paused_by_actor_type <> 'USER'`

func readAgentPauses(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error) {
	rows, err := q.Query(ctx, `SELECT ap.paused_at, ap.id::text, a.owner_user_id, ag.account_id,
			ap.agent_id::text, ap.reason_code, ap.reason, coalesce(ap.correlation_id, '')
		FROM agent_pauses ap
		JOIN agents ag ON ag.id = ap.agent_id
		JOIN accounts a ON a.id = ag.account_id
		WHERE `+keysetOn("ap.paused_at", "ap.id")+` AND `+agentPauseActorPredicate+`
		ORDER BY ap.paused_at, ap.id LIMIT $3`,
		at.UTC(), nullable(rowID), limit)
	if err != nil {
		return nil, fmt.Errorf("read agent pauses: %w", err)
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var (
			pausedAt                      time.Time
			pauseID, agentID              string
			reasonCode, reason, correlate string
			userID                        accounts.UserID
			accountID                     accounts.AccountID
		)
		if err := rows.Scan(&pausedAt, &pauseID, &userID, &accountID,
			&agentID, &reasonCode, &reason, &correlate); err != nil {
			return nil, fmt.Errorf("read agent pauses: %w", err)
		}
		title, body := AgentPauseCopy(reasonCode, reason)
		out = append(out, Change{
			At:    pausedAt,
			RowID: pauseID,
			Notify: []Notification{{
				UserID:        userID,
				AccountID:     accountPtr(accountID),
				Kind:          KindAgentPaused,
				Title:         title,
				Body:          body,
				Ref:           AgentPauseRef(pauseID),
				Occurrence:    pauseID,
				CorrelationID: correlate,
				OccurredAt:    pausedAt,
				Data: mustJSON(map[string]any{
					"agent_id":    agentID,
					"pause_id":    pauseID,
					"reason_code": reasonCode,
				}),
			}},
			Signal: []Signal{{UserID: userID.String(), Scope: ScopeAgent, Ref: agentID}},
		})
	}
	return out, rows.Err()
}

// AgentPauseRef and AgentPauseCopy are exported because cmd/api emits the same
// notification for the same pause row the instant it is written, and the
// follower emits it up to a tick later as the safety net. They agree on the
// ref, the occurrence and therefore the dedup key by construction rather than
// by two people writing the same string twice.
//
// The pause ROW is the reference rather than the agent, because a second pause
// of the same agent is a second thing that happened and has to be a second
// notification.
func AgentPauseRef(pauseID string) Ref { return Ref{Type: "agent_pause", ID: pauseID} }

// AgentPauseCopy is what the agent's owner reads.
//
// The reason the person who acted supplied is quoted and the person is not
// named -- the same rule accountStatusBody follows, for the same reason: an
// operator's identifier is not a customer's business, and the reason is.
func AgentPauseCopy(reasonCode, reason string) (title, body string) {
	switch reasonCode {
	case "KILL_SWITCH":
		title, body = "Your agent was stopped by a kill switch",
			"Trading was stopped platform-wide and your agent stopped with it."
	case "OPERATOR":
		title, body = "Your agent was paused by Nodal",
			"An operator paused your agent. You can resume it yourself once the reason is resolved."
	case "BUDGET_EXHAUSTED":
		title, body = "Your agent paused: its budget is used up",
			"Your agent reached the spending ceiling you granted it and stopped."
	case "RISK_VIOLATION":
		title, body = "Your agent was paused by a risk control",
			"A risk limit stopped your agent before it could act further."
	case "SECURITY":
		title, body = "Your agent was paused for a security reason",
			"Your agent was stopped pending a security review."
	default:
		title, body = "Your agent was paused",
			"Your agent stopped acting. Nothing it already did is affected."
	}
	if r := strings.TrimSpace(reason); r != "" {
		body += " Reason recorded: " + r + "."
	}
	return title, body
}

// keysOf returns a map's keys as a sorted-enough []string for = ANY(). Order
// does not matter to ANY; determinism does, for a readable query plan and a
// stable test.
func keysOf(m map[string]Kind) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
