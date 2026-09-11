package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/notifications"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stream"
)

// Notification follower cadence.
//
// Fifteen seconds is the latency a person waits between a fill and the toast
// that names it. It is the same order as the SSE heartbeat, which is what a
// client is already paying for on an open connection.
//
// # Why a poll costs nothing on this tier
//
// The same argument as runCreditSettlement (D-046) and runInternalVerification
// (F-118), and it is the reason a poll is acceptable here rather than merely
// convenient: the free instance spins down when idle, so this ticker stops when
// nobody is using the product and the database is not held awake by it. While
// the process IS up it is already being polled every five minutes by the
// verification sweep, so the marginal cost is six short indexed SELECTs a
// quarter-minute against a database in the same region.
//
// # Why a poll at all, rather than LISTEN/NOTIFY
//
// See D-071. LISTEN needs a connection held open outside the pool for the
// process's lifetime, and it delivers nothing to a process that was asleep when
// the NOTIFY fired -- which on a free instance is most of them. The poll's
// cursor is what makes a restart recover; NOTIFY would still need it.
const (
	notificationInterval = 15 * time.Second
	// notificationResumeLimit bounds how many missed notifications a
	// reconnecting client is sent before it is told to resync instead. Past
	// this, replaying is slower than refetching the first page.
	notificationResumeLimit = 100
)

// runNotificationFollower turns domain rows into notifications for as long as
// this process is up.
//
// # Why this runs in the API process
//
// The launch tier deploys one web service and no workers (render.yaml). The
// alternative to running it here is not running it somewhere better; it is not
// running it at all, which means the notification centre stays what it has been
// since migration 00640: a table nothing writes.
//
// It is safe to add a worker tier later with no coordination. Each pass takes a
// try-advisory lock per source, so a second runner skips rather than duplicates,
// and every notification is deduplicated on (user, dedup_key) in the database
// regardless of who wrote it.
func runNotificationFollower(ctx context.Context, database *db.DB, f *notifications.Follower, pub notifications.Publisher, log *slog.Logger) {
	if database == nil || f == nil {
		return
	}
	log.Info("notification follower started",
		"interval", notificationInterval, "sources", f.SourceNames())
	t := time.NewTicker(notificationInterval)
	defer t.Stop()
	for {
		// Once at start as well as on the tick, for the same reason as the
		// settlement sweep: a free instance that wakes, serves and spins down
		// would otherwise never drain the backlog its own downtime created.
		followOnce(ctx, database, f, pub, log)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// followOnce is one pass. A failure is logged and the next tick tries again: a
// notification that arrives late is a worse product, and a follower that stops
// is a worse one still.
func followOnce(ctx context.Context, database *db.DB, f *notifications.Follower, pub notifications.Publisher, log *slog.Logger) {
	n, err := f.RunOnce(ctx, database, pub)
	switch {
	case err != nil && ctx.Err() != nil:
		return // shutdown, not a failure
	case err != nil:
		log.ErrorContext(ctx, "notification follower pass failed",
			"error", err.Error(),
			"consequence", "the cursor did not advance for that source; the next pass re-reads the window")
	case n > 0:
		log.InfoContext(ctx, "notifications written", "count", n)
	}
}

// hubPublisher delivers a follower pass to the realtime hub.
//
// It is the only place a notification becomes a stream event, and it carries
// identifiers and a title -- never a body, never a figure. A client that wants
// the notification refetches /v1/me/notifications, which is the same rule the
// rest of the stream follows (PART 109).
type hubPublisher struct{ hub *stream.Hub }

// Notify publishes one notification to its recipient.
func (h hubPublisher) Notify(n notifications.Notification) {
	if h.hub == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"id":            n.ID.String(),
		"kind":          string(n.Kind),
		"severity":      string(n.Severity),
		"title":         n.Title,
		"sandbox":       n.Sandbox,
		"resource_type": n.Ref.Type,
		"resource_id":   n.Ref.ID,
	})
	if err != nil {
		return
	}
	h.hub.Publish(stream.Event{
		Type:       stream.TypeNotification,
		OccurredAt: n.OccurredAt.UTC(),
		ResourceID: n.ID.String(),
		UserID:     n.UserID.String(),
		Data:       payload,
	})
}

// Signal publishes one query invalidation.
func (h hubPublisher) Signal(s notifications.Signal) {
	if h.hub == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{"scope": s.Scope, "ref": s.Ref})
	if err != nil {
		return
	}
	h.hub.Publish(stream.Event{
		Type:       stream.TypeDataChanged,
		OccurredAt: time.Now().UTC(),
		ResourceID: s.Ref,
		UserID:     s.UserID,
		Broadcast:  s.Broadcast,
		Data:       payload,
	})
}

// notificationResume is the durable half of Last-Event-ID resume.
//
// The hub's replay buffer is memory: a redeploy empties it, and a free instance
// redeploys and sleeps. The notifications table does not have that problem, so
// a client whose Last-Event-ID encodes an instant gets everything written since
// that instant, from the table, before the buffer's own replay.
func notificationResume(database *db.DB) stream.Resume {
	return func(ctx context.Context, p security.Principal, since time.Time) ([]stream.Event, bool, error) {
		userID, err := accounts.ParseUserID(p.SubjectID)
		if err != nil {
			// An operator session whose subject is not a customer identity has
			// no notification centre to resume from. That is not an error.
			return nil, false, nil
		}
		missed, truncated, err := notifications.Since(ctx, database, userID, since, notificationResumeLimit)
		if err != nil {
			return nil, false, err
		}
		out := make([]stream.Event, 0, len(missed))
		for _, n := range missed {
			payload, merr := json.Marshal(map[string]any{
				"id":            n.ID.String(),
				"kind":          string(n.Kind),
				"severity":      string(n.Severity),
				"title":         n.Title,
				"sandbox":       n.Sandbox,
				"resource_type": n.Ref.Type,
				"resource_id":   n.Ref.ID,
			})
			if merr != nil {
				continue
			}
			out = append(out, stream.Event{
				// The id encodes the notification's own instant, so a client
				// that disconnects again mid-replay resumes from where it got
				// to rather than from where it started.
				ID:         stream.EventIDAt(n.OccurredAt),
				Type:       stream.TypeNotification,
				OccurredAt: n.OccurredAt.UTC(),
				ResourceID: n.ID.String(),
				UserID:     n.UserID.String(),
				Data:       payload,
			})
		}
		return out, truncated, nil
	}
}
