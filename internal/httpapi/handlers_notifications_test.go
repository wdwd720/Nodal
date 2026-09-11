package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/security"
)

// fakeNotifications records what the boundary asked for, which is the half of
// these tests that matters: every call must name the CALLER's user id, never an
// id taken from a path or a body, because there is no path or body here that
// could carry one.
type fakeNotifications struct {
	page       notifications.Page
	unread     int
	one        notifications.Notification
	updated    int
	prefs      []notifications.Preference
	err        error
	askedUser  accounts.UserID
	askedFiltr notifications.Filter
	askedWant  map[notifications.Kind]bool
}

func (f *fakeNotifications) List(_ context.Context, userID accounts.UserID, filter notifications.Filter, _ string, _ int) (notifications.Page, error) {
	f.askedUser, f.askedFiltr = userID, filter
	return f.page, f.err
}

func (f *fakeNotifications) UnreadCount(_ context.Context, userID accounts.UserID) (int, error) {
	f.askedUser = userID
	return f.unread, f.err
}

func (f *fakeNotifications) MarkRead(_ context.Context, userID accounts.UserID, _ notifications.ID) (notifications.Notification, error) {
	f.askedUser = userID
	return f.one, f.err
}

func (f *fakeNotifications) MarkAllRead(_ context.Context, userID accounts.UserID) (int, error) {
	f.askedUser = userID
	return f.updated, f.err
}

func (f *fakeNotifications) Preferences(_ context.Context, userID accounts.UserID) ([]notifications.Preference, error) {
	f.askedUser = userID
	return f.prefs, f.err
}

func (f *fakeNotifications) SavePreferences(_ context.Context, userID accounts.UserID, want map[notifications.Kind]bool) ([]notifications.Preference, error) {
	f.askedUser, f.askedWant = userID, want
	return f.prefs, f.err
}

type fakeMeAudit struct {
	page          MeAuditPage
	err           error
	askedUser     accounts.UserID
	askedAccounts []string
}

func (f *fakeMeAudit) Audit(_ context.Context, userID accounts.UserID, accountIDs []string, _ string, _ int) (MeAuditPage, error) {
	f.askedUser, f.askedAccounts = userID, accountIDs
	return f.page, f.err
}

func sampleNotification() notifications.Notification {
	acct := testAccountID
	return notifications.Notification{
		ID:         testNotificationID,
		UserID:     testUserID,
		AccountID:  &acct,
		Kind:       notifications.KindNativeTradeFilled,
		Severity:   notifications.SeverityInfo,
		Title:      "You bought NODL",
		Body:       "Your buy order for NODL filled.",
		Ref:        notifications.Ref{Type: "native_market_fill", ID: "fill-1"},
		Data:       json.RawMessage(`{"symbol":"NODL"}`),
		OccurredAt: testNow,
	}
}

func TestNotifications_ListIsScopedToTheCallerAndCarriesTheFilter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ports.notifs.page = notifications.Page{Items: []notifications.Notification{sampleNotification()}, NextCursor: "c2"}

	res := h.do(http.MethodGet, "/v1/me/notifications?unread=true&kinds=NATIVE_TRADE_FILLED,PAYOUT_SETTLED&limit=10", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	assert.Equal(t, testUserID, h.ports.notifs.askedUser, "the port must be asked for the CALLER's notifications")
	assert.True(t, h.ports.notifs.askedFiltr.UnreadOnly)
	assert.Equal(t, []notifications.Kind{notifications.KindNativeTradeFilled, notifications.KindPayoutSettled},
		h.ports.notifs.askedFiltr.Kinds)

	var page struct {
		Items []struct {
			ID         string          `json:"id"`
			Kind       string          `json:"kind"`
			Severity   string          `json:"severity"`
			Title      string          `json:"title"`
			Sandbox    bool            `json:"sandbox"`
			ReadAt     *string         `json:"read_at"`
			Data       json.RawMessage `json:"data"`
			ResourceID string          `json:"resource_id"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, "NATIVE_TRADE_FILLED", page.Items[0].Kind)
	assert.Equal(t, "INFO", page.Items[0].Severity)
	assert.Equal(t, "You bought NODL", page.Items[0].Title)
	assert.False(t, page.Items[0].Sandbox)
	assert.Nil(t, page.Items[0].ReadAt, "an unread notification reports read_at as null, not as a zero time")
	assert.JSONEq(t, `{"symbol":"NODL"}`, string(page.Items[0].Data))
	assert.Equal(t, "fill-1", page.Items[0].ResourceID)
	require.NotNil(t, page.NextCursor)
	assert.Equal(t, "c2", *page.NextCursor)
}

func TestNotifications_AnUnknownKindFilterIsRefusedAtTheEdge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	// FUNDING_AVAILABLE is a real value of the CHECK and is deliberately not a
	// product kind: the surface must not accept it as a filter.
	res := h.do(http.MethodGet, "/v1/me/notifications?kinds=FUNDING_AVAILABLE", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
}

func TestNotifications_UnreadCount(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ports.notifs.unread = 7
	res := h.do(http.MethodGet, "/v1/me/notifications/unread-count", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.JSONEq(t, `{"count":7}`, res.Body.String())
	assert.Equal(t, testUserID, h.ports.notifs.askedUser)
}

func TestNotifications_MarkReadAndReadAllAreCommands(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	read := testNow.Add(time.Minute)
	n := sampleNotification()
	n.ReadAt = &read
	h.ports.notifs.one = n
	h.ports.notifs.updated = 3

	res := h.do(http.MethodPost, "/v1/me/notifications/"+testNotificationID.String()+"/read", nil,
		"Idempotency-Key", "mark-read-000001")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	var got struct {
		ReadAt *time.Time `json:"read_at"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &got))
	require.NotNil(t, got.ReadAt)
	assert.True(t, read.Equal(*got.ReadAt))

	res = h.do(http.MethodPost, "/v1/me/notifications/read-all", nil, "Idempotency-Key", "read-all-000001")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.JSONEq(t, `{"updated":3}`, res.Body.String())
}

func TestNotifications_ACommandWithoutAnIdempotencyKeyIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res := h.do(http.MethodPost, "/v1/me/notifications/read-all", nil)
	assert.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
}

func TestNotifications_PreferencesRoundTripAndReportWhatIsEnforced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ports.notifs.prefs = []notifications.Preference{
		{Kind: notifications.KindNativeTradeFilled, Channel: notifications.ChannelInApp, Enabled: false, Enforced: true},
		{Kind: notifications.KindSecurityNewSession, Channel: notifications.ChannelInApp, Enabled: true, Enforced: false},
	}
	res := h.do(http.MethodGet, "/v1/me/notification-preferences", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	var prefs struct {
		Items []struct {
			Kind     string `json:"kind"`
			Channel  string `json:"channel"`
			Enabled  bool   `json:"enabled"`
			Enforced bool   `json:"enforced"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &prefs))
	require.Len(t, prefs.Items, 2)
	assert.Equal(t, "IN_APP", prefs.Items[0].Channel, "there is one channel and the API says which")
	assert.False(t, prefs.Items[1].Enforced, "a kind that cannot be switched off must say so")

	res = h.do(http.MethodPut, "/v1/me/notification-preferences",
		`{"items":[{"kind":"NATIVE_TRADE_FILLED","enabled":false}]}`, "Idempotency-Key", "prefs-000001")
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())
	assert.Equal(t, map[notifications.Kind]bool{notifications.KindNativeTradeFilled: false}, h.ports.notifs.askedWant)
	assert.Equal(t, testUserID, h.ports.notifs.askedUser)

	res = h.do(http.MethodPut, "/v1/me/notification-preferences",
		`{"items":[{"kind":"FUNDING_AVAILABLE","enabled":false}]}`, "Idempotency-Key", "prefs-000002")
	require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
}

// TestNotifications_AnAgentHasNoNotificationCentre. The authz table refuses an
// AGENT everywhere; this asserts the handler's own refusal too, so the second
// line of defence is a line and not a comment.
func TestNotifications_AnAgentHasNoNotificationCentre(t *testing.T) {
	t.Parallel()
	ctx := security.WithPrincipal(context.Background(), agentPrincipal())
	_, err := meUserID(ctx)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// And a session whose subject is not a customer identity at all.
	ctx = security.WithPrincipal(context.Background(), security.Principal{
		SubjectID: "not-a-uuid", ActorType: security.ActorOperator,
	})
	_, err = meUserID(ctx)
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// TestNotifications_TheDomainsRefusalIsRenderedNotSwallowed: internal/
// notifications refuses another person's row with FORBIDDEN, and the boundary
// must render that rather than turning it into a 500 or an empty page.
func TestNotifications_TheDomainsRefusalIsRenderedNotSwallowed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ports.notifs.err = errs.New(errs.CodeForbidden, "notifications belong to the person they were addressed to")
	res := h.do(http.MethodGet, "/v1/me/notifications", nil)
	require.Equal(t, http.StatusForbidden, res.Code, res.Body.String())
	assert.Equal(t, errs.CodeForbidden, res.problem().Code)
}

func TestNotifications_ANilPortAnswersUnsupportedRatherThanInventingAnEmptyInbox(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.opts.Ports.Notifications = nil
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/v1/me/notifications"},
		{http.MethodGet, "/v1/me/notifications/unread-count"},
		{http.MethodGet, "/v1/me/notification-preferences"},
	} {
		res := h.do(probe.method, probe.path, nil)
		assert.Equal(t, http.StatusUnprocessableEntity, res.Code, probe.path+": "+res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, probe.path)
	}
}

func TestMeAudit_ReadsTheCallersOwnHistoryAndNamesNoOperator(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ports.meAudit.page = MeAuditPage{
		Items: []MeAuditItem{
			{ID: "s-1", Source: "SECURITY", Action: "login", Severity: "INFO", IP: "203.0.113.7", UserAgent: "Firefox", OccurredAt: testNow},
			{ID: "a-1", Source: "ACCOUNT", Action: "account.freeze", ResourceType: "account", ResourceID: testAccountID.String(), ActorType: "OPERATOR", OccurredAt: testNow},
		},
		NextCursor: "next",
	}
	res := h.do(http.MethodGet, "/v1/me/audit?limit=25", nil)
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	assert.Equal(t, testUserID, h.ports.meAudit.askedUser)
	assert.Equal(t, []string{testAccountID.String()}, h.ports.meAudit.askedAccounts,
		"only the principal's own accounts reach the reader")

	var page struct {
		Items []struct {
			ID        string  `json:"id"`
			Source    string  `json:"source"`
			Action    string  `json:"action"`
			ActorType *string `json:"actor_type"`
			IP        *string `json:"ip"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page))
	require.Len(t, page.Items, 2)
	assert.Equal(t, "SECURITY", page.Items[0].Source)
	assert.Equal(t, "login", page.Items[0].Action)
	require.NotNil(t, page.Items[1].ActorType)
	assert.Equal(t, "OPERATOR", *page.Items[1].ActorType)
	assert.NotContains(t, res.Body.String(), "actor_id", "an operator's identity is never rendered to a customer")
	require.NotNil(t, page.NextCursor)
}

func TestMeAudit_ANilPortAnswersUnsupported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.server.opts.Ports.MeAudit = nil
	res := h.do(http.MethodGet, "/v1/me/audit", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, res.Code)
	assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
}
