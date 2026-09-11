package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/oapi-codegen/nullable"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/security"
)

// The notification centre (product goal §36).
//
// Every route here is scoped to the caller and to nobody else. There is no
// accountId in any path and no account:read_any branch in any handler: a
// notification is addressed to a person, and the only question the boundary can
// answer is "are you that person". internal/notifications asks it again against
// the principal, so a handler bug cannot widen the answer.

// meUserID resolves the caller's own user id from the principal.
func meUserID(ctx context.Context) (accounts.UserID, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return accounts.UserID{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	if p.ActorType == security.ActorAgent {
		return accounts.UserID{}, errs.New(errs.CodeForbidden, "an agent has no notification centre")
	}
	userID, err := accounts.ParseUserID(p.SubjectID)
	if err != nil {
		return accounts.UserID{}, errs.New(errs.CodeForbidden, "this session's subject is not a customer identity")
	}
	return userID, nil
}

// GetMeNotifications pages the caller's notification centre, newest first.
func (s *Server) GetMeNotifications(ctx context.Context, request api.GetMeNotificationsRequestObject) (api.GetMeNotificationsResponseObject, error) {
	if s.opts.Ports.Notifications == nil {
		return nil, errNotWired("notifications")
	}
	userID, err := meUserID(ctx)
	if err != nil {
		return nil, err
	}
	f := notifications.Filter{}
	if request.Params.Unread != nil {
		f.UnreadOnly = *request.Params.Unread
	}
	if request.Params.Kinds != nil {
		for _, k := range *request.Params.Kinds {
			kind := notifications.Kind(k)
			if !kind.IsProduct() {
				return nil, validationError("kinds", "unknown notification kind")
			}
			f.Kinds = append(f.Kinds, kind)
		}
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.opts.Ports.Notifications.List(ctx, userID, f,
		cursor, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	items := make([]api.Notification, 0, len(page.Items))
	for _, n := range page.Items {
		items = append(items, toAPINotification(n))
	}
	return api.GetMeNotifications200JSONResponse(api.NotificationPage{
		Items:      items,
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

// GetMeNotificationsUnreadCount is the badge.
func (s *Server) GetMeNotificationsUnreadCount(ctx context.Context, _ api.GetMeNotificationsUnreadCountRequestObject) (api.GetMeNotificationsUnreadCountResponseObject, error) {
	if s.opts.Ports.Notifications == nil {
		return nil, errNotWired("notifications")
	}
	userID, err := meUserID(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.opts.Ports.Notifications.UnreadCount(ctx, userID)
	if err != nil {
		return nil, err
	}
	return api.GetMeNotificationsUnreadCount200JSONResponse(api.UnreadCount{Count: n}), nil
}

// PostMeNotificationsNotificationIdRead marks one notification read.
//
// It carries an Idempotency-Key like every other command even though marking
// read is idempotent in the database (the first instant is kept). The key is
// not what makes it safe; it is what makes a replay return the same body, and a
// surface where some commands take one and some do not is a surface a client
// has to remember exceptions for.
func (s *Server) PostMeNotificationsNotificationIdRead(ctx context.Context, request api.PostMeNotificationsNotificationIdReadRequestObject) (api.PostMeNotificationsNotificationIdReadResponseObject, error) {
	if s.opts.Ports.Notifications == nil {
		return nil, errNotWired("notifications")
	}
	userID, err := meUserID(ctx)
	if err != nil {
		return nil, err
	}
	notificationID, err := notifications.ParseID(request.NotificationId.String())
	if err != nil {
		return nil, validationError("notificationId", "notificationId must be a notification identifier")
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Notification, commandMeta, error) {
			n, merr := s.opts.Ports.Notifications.MarkRead(ctx, userID, notificationID)
			if merr != nil {
				return api.Notification{}, commandMeta{}, merr
			}
			return toAPINotification(n), commandMeta{
				Status: http.StatusOK, ResourceType: "notification", ResourceID: n.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostMeNotificationsNotificationIdRead200JSONResponse(res.Value), nil
}

// PostMeNotificationsReadAll marks every unread notification read.
func (s *Server) PostMeNotificationsReadAll(ctx context.Context, request api.PostMeNotificationsReadAllRequestObject) (api.PostMeNotificationsReadAllResponseObject, error) {
	if s.opts.Ports.Notifications == nil {
		return nil, errNotWired("notifications")
	}
	userID, err := meUserID(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.MarkedRead, commandMeta, error) {
			n, merr := s.opts.Ports.Notifications.MarkAllRead(ctx, userID)
			if merr != nil {
				return api.MarkedRead{}, commandMeta{}, merr
			}
			return api.MarkedRead{Updated: n}, commandMeta{Status: http.StatusOK, ResourceType: "notification"}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostMeNotificationsReadAll200JSONResponse(res.Value), nil
}

// GetMeNotificationPreferences returns one entry per kind.
func (s *Server) GetMeNotificationPreferences(ctx context.Context, _ api.GetMeNotificationPreferencesRequestObject) (api.GetMeNotificationPreferencesResponseObject, error) {
	if s.opts.Ports.Notifications == nil {
		return nil, errNotWired("notifications")
	}
	userID, err := meUserID(ctx)
	if err != nil {
		return nil, err
	}
	prefs, err := s.opts.Ports.Notifications.Preferences(ctx, userID)
	if err != nil {
		return nil, err
	}
	return api.GetMeNotificationPreferences200JSONResponse(toAPIPreferences(prefs)), nil
}

// PutMeNotificationPreferences replaces the caller's answers for the kinds named.
func (s *Server) PutMeNotificationPreferences(ctx context.Context, request api.PutMeNotificationPreferencesRequestObject) (api.PutMeNotificationPreferencesResponseObject, error) {
	if s.opts.Ports.Notifications == nil {
		return nil, errNotWired("notifications")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	userID, err := meUserID(ctx)
	if err != nil {
		return nil, err
	}
	want := make(map[notifications.Kind]bool, len(request.Body.Items))
	for _, item := range request.Body.Items {
		kind := notifications.Kind(item.Kind)
		if !kind.IsProduct() {
			return nil, validationError("items", "unknown notification kind")
		}
		want[kind] = item.Enabled
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.NotificationPreferences, commandMeta, error) {
			prefs, serr := s.opts.Ports.Notifications.SavePreferences(ctx, userID, want)
			if serr != nil {
				return api.NotificationPreferences{}, commandMeta{}, serr
			}
			return toAPIPreferences(prefs), commandMeta{
				Status: http.StatusOK, ResourceType: "notification_preferences", ResourceID: userID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PutMeNotificationPreferences200JSONResponse(res.Value), nil
}

func toAPINotification(n notifications.Notification) api.Notification {
	out := api.Notification{
		Id:           toUUID(n.ID),
		Kind:         api.NotificationKind(n.Kind),
		Severity:     api.NotificationSeverity(n.Severity),
		Title:        n.Title,
		Body:         n.Body,
		ResourceType: strPtr(n.Ref.Type),
		ResourceId:   strPtr(n.Ref.ID),
		OccurredAt:   n.OccurredAt.UTC(),
		Sandbox:      n.Sandbox,
	}
	out.AccountId = nullable.NewNullNullable[api.UUID]()
	if n.AccountID != nil {
		out.AccountId = nullable.NewNullableWithValue(toUUID(*n.AccountID))
	}
	out.ReadAt = nullable.NewNullNullable[time.Time]()
	if n.ReadAt != nil {
		out.ReadAt = nullable.NewNullableWithValue(n.ReadAt.UTC())
	}
	// The data blob is written by this system, read back, and re-marshalled;
	// a value that will not decode is a bug here rather than a client's
	// problem, so the field is dropped and the notification still renders.
	var data map[string]any
	if len(n.Data) > 0 && json.Unmarshal(n.Data, &data) == nil && len(data) > 0 {
		out.Data = &data
	}
	return out
}

func toAPIPreferences(prefs []notifications.Preference) api.NotificationPreferences {
	items := make([]api.NotificationPreference, 0, len(prefs))
	for _, p := range prefs {
		items = append(items, api.NotificationPreference{
			Kind:     api.NotificationKind(p.Kind),
			Channel:  api.NotificationPreferenceChannel(p.Channel),
			Enabled:  p.Enabled,
			Enforced: p.Enforced,
		})
	}
	return api.NotificationPreferences{Items: items}
}
