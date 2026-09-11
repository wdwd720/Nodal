package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/security"
)

// Scopes a data-changed signal can name. They are query invalidations, not
// values: a client that receives one refetches the REST resource it names and
// believes that, because the stream is never authoritative (PART 109).
const (
	ScopeBalance  = "balance"
	ScopePosition = "position"
	ScopeMarket   = "market"
	ScopePayout   = "payout"
	ScopeAccount  = "account"
	// ScopeVerification and ScopeEligibility are the two reads a verification
	// decision invalidates. They are separate because they are separate
	// resources: the profile says what Nodal holds, and the eligibility
	// explanation composes it with the payout policy, the gates and the
	// provider, and changes when any of those do.
	ScopeVerification = "verification"
	ScopeEligibility  = "eligibility"
	// ScopeAgent is one agent's view.
	ScopeAgent = "agent"
)

// Signal tells a client that something it may be displaying is now stale.
type Signal struct {
	// UserID is the person the signal is addressed to. Empty with Broadcast
	// set means everyone.
	UserID string
	Scope  string
	Ref    string
	// Broadcast marks a signal about public data -- a market's price -- which
	// every connected client may see because every client may already read it
	// over REST.
	Broadcast bool
}

// Publisher receives what a follower pass produced, AFTER the transaction that
// wrote it committed. It is deliberately not an error-returning interface: a
// realtime hub is a hint, and a hint that fails must not roll back a fact.
type Publisher interface {
	Notify(n Notification)
	Signal(s Signal)
}

// Change is one source row's consequences.
type Change struct {
	At     time.Time
	RowID  string
	Notify []Notification
	Signal []Signal
}

// source is one table the follower reads.
type source struct {
	name string
	read func(ctx context.Context, q db.Querier, at time.Time, rowID string, limit int) ([]Change, error)
}

// Follower turns rows that already exist into notifications.
//
// It reads transition tables and fills -- rows written by domain services in
// their own transactions -- and emits a notification for each one a person
// needs to know about. It writes nothing but notifications and its own cursor,
// and it edits no domain package, which is why it can exist while those
// packages are being changed by somebody else (D-069).
//
// # Why a cursor is not the correctness argument
//
// Every table it follows orders by a timestamp defaulting to now() and a
// UUIDv7 primary key. now() is the transaction's START time, so a transaction
// that began before the cursor passed and committed after it writes a row the
// cursor has already gone past. No ordering fixes that. So the follower reads a
// lap behind its own cursor, and every notification carries a dedup key derived
// from the source row's id: the lap finds late rows, and the unique index on
// (user_id, dedup_key) refuses the ones the lap sees twice. The cursor stops
// the follower rescanning history; it is not what makes the result correct.
//
// # Why the lap is a second read rather than a rewound cursor (F-167)
//
// The pass used to be one read, from `cursor - lap`, bounded by `batch`, whose
// LAST row became the new cursor. That cursor could therefore move BACKWARDS,
// and it did as soon as one source produced `batch` rows inside one `lap`: the
// 200th row counted from two minutes behind the cursor is itself behind the
// cursor, so the next pass re-read the same window, deduplicated all of it, and
// wrote the same instant back. One ordinary burst -- 120 commands a minute is
// the rate limit, and readNativeFills has no state filter -- stalled a source
// for the life of the deployment, silently: the dedup index made the repeated
// re-read produce nothing, so nothing logged and nothing alerted.
//
// So the two jobs the single read was doing are now two reads. The DRAIN reads
// forward from exactly where the last pass stopped and is the only thing that
// moves the cursor, which is therefore monotonic by construction. The LAP
// re-reads the window behind the cursor and moves nothing; it runs only when
// the drain left room in the batch, because a pass that is still draining a
// backlog has not caught up to the instant a late commit could hide behind, and
// re-reads that window on the pass after it catches up.
type Follower struct {
	producer *Producer
	sources  []source
	lap      time.Duration
	batch    int
	log      *slog.Logger
}

// DefaultLap is how far behind its own cursor each pass re-reads. Two minutes
// is longer than any transaction in this system is permitted to hold open --
// the pool's statement timeout is well below it -- so a row cannot commit
// behind a cursor that has moved a full lap past its start time.
const DefaultLap = 2 * time.Minute

// DefaultBatch bounds one source's pass. A backlog drains over several passes
// rather than in one long transaction holding a pool connection.
const DefaultBatch = 200

// NewFollower returns a follower over every source this commit has domain rows
// for.
func NewFollower(producer *Producer) *Follower {
	return &Follower{
		producer: producer,
		lap:      DefaultLap,
		batch:    DefaultBatch,
		log:      slog.Default(),
		sources: []source{
			{name: "credit_funding_transitions", read: readCreditFundings},
			{name: "payout_request_transitions", read: readPayoutRequests},
			{name: "native_market_fills", read: readNativeFills},
			{name: "native_market_transitions", read: readMarketPauses},
			{name: "account_status_transitions", read: readAccountStatus},
			{name: "security_events_login", read: readNewSessions},
			{name: "compliance_profile_transitions", read: readVerificationUpdates},
			{name: "agent_pauses", read: readAgentPauses},
		},
	}
}

// WithLogger attaches the process's logger and returns f. A follower without
// one still runs; it reports a stalled-looking pass to the default logger
// instead of to the one the binary configured.
func (f *Follower) WithLogger(log *slog.Logger) *Follower {
	if log != nil {
		f.log = log
	}
	return f
}

// SourceNames lists the tables the follower reads, for a log line and for the
// inventory.
func (f *Follower) SourceNames() []string {
	out := make([]string, 0, len(f.sources))
	for _, s := range f.sources {
		out = append(out, s.name)
	}
	return out
}

// followerLockKey namespaces the advisory lock each source takes, so two
// instances of this process (or a worker tier added later) never run the same
// source's pass at once. It is a try-lock: the loser skips the pass rather than
// queueing behind it.
const followerLockKey int64 = 0x6e6f7469 // "noti"

// RunOnce runs one pass over every source and returns how many notifications it
// wrote. Each source's pass is one transaction containing both its emits and
// its cursor advance, so the two can never disagree: either the batch and the
// position commit together or neither does.
//
// Publishing happens after the commit and outside it. A hub is memory; a
// failure to publish loses a live update and nothing else, because the client
// reconnects and resumes from the table.
func (f *Follower) RunOnce(ctx context.Context, database *db.DB, pub Publisher) (int, error) {
	total := 0
	var firstErr error
	for _, s := range f.sources {
		n, err := f.runSource(ctx, database, s, pub)
		total += n
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", s.name, err)
		}
	}
	return total, firstErr
}

func (f *Follower) runSource(ctx context.Context, database *db.DB, s source, pub Publisher) (int, error) {
	// The follower runs as the system: it reads rows belonging to every user
	// and writes notifications addressed to them, which no customer principal
	// may do and no operator principal should.
	ctx = security.WithPrincipal(ctx, security.Principal{
		SubjectID: "notification-follower",
		ActorType: security.ActorSystem,
	})

	var published []Notification
	var signals []Signal
	err := database.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var locked bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1, hashtext($2))`,
			followerLockKey, s.name).Scan(&locked); err != nil {
			return fmt.Errorf("claim the pass: %w", err)
		}
		if !locked {
			return nil // somebody else is on this source; the next tick tries again
		}
		at, rowID, err := loadCursor(ctx, tx, s.name)
		if err != nil {
			return err
		}
		// The drain. It starts at the cursor itself -- not a lap behind it --
		// so every row it reads is a row the cursor has not passed, and the
		// last of them is always ahead of where the cursor stands.
		ahead, err := s.read(ctx, tx, at, rowID, f.batch)
		if err != nil {
			return err
		}
		// The lap, which moves nothing. Skipped while a backlog is draining:
		// the drain is not near the head yet, so there is no "just behind the
		// cursor" for a late commit to hide in, and re-reading that window on
		// every pass of a long drain would spend the batch on rows everybody
		// has already been told about.
		var behind []Change
		full := len(ahead) >= f.batch
		if !full && f.lap > 0 {
			lapped, lerr := s.read(ctx, tx, at.Add(-f.lap), "", f.batch)
			if lerr != nil {
				return lerr
			}
			// Anything at or after the cursor is what the drain just read.
			for _, c := range lapped {
				if notAfterCursor(c, at, rowID) {
					behind = append(behind, c)
				}
			}
		}
		if len(ahead) == 0 && len(behind) == 0 {
			return nil
		}
		// The cursor advances to the last row that is not stamped past the
		// horizon. A row beyond it is REPORTED -- it exists and somebody needs
		// to know -- but it does not carry the cursor with it, because a cursor
		// standing in the future silently skips every row written between now
		// and then, which is the same permanent, invisible loss the stall was.
		horizon, err := futureHorizon(ctx, tx, f.lap)
		if err != nil {
			return err
		}
		newAt, newID := at, rowID
		for i := len(ahead) - 1; i >= 0; i-- {
			if !ahead[i].At.After(horizon) {
				newAt, newID = ahead[i].At, ahead[i].RowID
				break
			}
		}
		if err := markPending(ctx, tx, s.name, newAt); err != nil {
			return err
		}
		written := 0
		// The lap first, so the emits run in the order the rows happened.
		for _, c := range append(behind, ahead...) {
			fresh := 0
			for _, n := range c.Notify {
				em, err := f.producer.Emit(ctx, tx, n)
				if err != nil {
					return fmt.Errorf("emit %s for %s: %w", n.Kind, c.RowID, err)
				}
				if em.Created {
					fresh++
					published = append(published, em.Notification)
				}
			}
			written += fresh
			// A signal rides with the fact it accompanies. A row the lap has
			// already told everybody about is not announced a second time:
			// otherwise every pass would republish an invalidation for every
			// row of the last two minutes, and a client would refetch the same
			// resource every fifteen seconds for as long as the window held it.
			if fresh > 0 || len(c.Notify) == 0 {
				signals = append(signals, c.Signal...)
			}
		}
		// A full drain that told nobody anything has the shape the stall had:
		// rows going past the cursor, every one of them already known. It is
		// legitimate after a cursor is wound back by a restore, and it is worth
		// a line either way -- the defect this replaced was invisible precisely
		// because a re-read writes nothing.
		if full && written == 0 {
			f.log.WarnContext(ctx, "a full notification batch produced no notifications",
				"source", s.name, "rows", len(ahead), "cursor_at", newAt.UTC().Format(time.RFC3339Nano),
				"consequence", "the cursor is advancing over rows everybody has already been told about")
		}
		return saveCursor(ctx, tx, s.name, newAt, newID, written)
	})
	if err != nil {
		return 0, err
	}
	if pub != nil {
		for _, n := range published {
			pub.Notify(n)
		}
		for _, sig := range signals {
			pub.Signal(sig)
		}
	}
	return len(published), nil
}

func loadCursor(ctx context.Context, q db.Querier, name string) (time.Time, string, error) {
	var at time.Time
	var rowID string
	err := q.QueryRow(ctx,
		`SELECT last_at, last_id::text FROM notification_follower_cursors WHERE source = $1`, name).Scan(&at, &rowID)
	switch {
	case err == nil:
		return at.UTC(), rowID, nil
	case isNoRows(err):
		// A source with no cursor starts NOW, not at the beginning of history.
		// The alternative would notify every user about every fill and every
		// login since the database was created, the first time this code ran.
		var now time.Time
		if err := q.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
			return time.Time{}, "", fmt.Errorf("start the cursor: %w", err)
		}
		if _, err := q.Exec(ctx,
			`INSERT INTO notification_follower_cursors (source, last_at, last_id)
			 VALUES ($1, $2, '00000000-0000-0000-0000-000000000000')
			 ON CONFLICT (source) DO NOTHING`, name, now); err != nil {
			return time.Time{}, "", fmt.Errorf("start the cursor: %w", err)
		}
		return now.UTC(), "", nil
	default:
		return time.Time{}, "", fmt.Errorf("read the cursor: %w", err)
	}
}

func markPending(ctx context.Context, q db.Querier, name string, at time.Time) error {
	_, err := q.Exec(ctx,
		`UPDATE notification_follower_cursors SET pending_at = $2 WHERE source = $1`, name, at.UTC())
	if err != nil {
		return fmt.Errorf("record the pass in flight: %w", err)
	}
	return nil
}

func saveCursor(ctx context.Context, q db.Querier, name string, at time.Time, rowID string, written int) error {
	_, err := q.Exec(ctx,
		`UPDATE notification_follower_cursors
		    SET last_at = $2, last_id = $3::uuid, pending_at = NULL, emitted = emitted + $4
		  WHERE source = $1`, name, at.UTC(), rowID, written)
	if err != nil {
		return fmt.Errorf("advance the cursor: %w", err)
	}
	return nil
}

// keysetOn builds the predicate every source query shares: $1 is the instant
// the cursor stands at and $2 the row id that broke the tie at that instant, or
// NULL to take the whole instant -- which is what a lap back needs, because a
// lap lands on an instant no row id was recorded for.
func keysetOn(atCol, idCol string) string {
	return fmt.Sprintf(
		`(%[1]s > $1::timestamptz OR ($2::text IS NOT NULL AND %[1]s = $1::timestamptz AND %[2]s::text > $2::text))`,
		atCol, idCol,
	)
}

// futureHorizon is how far ahead of the database's own clock the cursor is
// allowed to stand: half a lap.
//
// `occurred_at` defaults to now() and every writer may set it, so one row
// carrying a wrong clock -- a skewed host, a service passing the wrong
// instant -- would otherwise drag the cursor to that instant and skip every row
// written between now and then, permanently and silently. Half a lap is the
// bound rather than zero because the two clocks are not the same clock: a row
// stamped milliseconds ahead of the database's now is ordinary, and refusing to
// advance past it would stall the cursor on ordinary skew. It is half a lap
// rather than more because the lap re-reads the window BEHIND the cursor, so
// anything a cursor standing half a lap ahead skipped is still inside the
// window the next pass re-reads.
func futureHorizon(ctx context.Context, q db.Querier, lap time.Duration) (time.Time, error) {
	var now time.Time
	if err := q.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return time.Time{}, fmt.Errorf("read the database clock: %w", err)
	}
	return now.UTC().Add(lap / 2), nil
}

// notAfterCursor reports whether a change is at or behind the cursor, in the
// same (instant, row id) order the source queries use. A cursor with no row id
// stands at the whole instant, so a row AT that instant is behind it.
func notAfterCursor(c Change, at time.Time, rowID string) bool {
	switch {
	case c.At.Before(at):
		return true
	case !c.At.Equal(at):
		return false
	case rowID == "":
		return true
	default:
		return c.RowID <= rowID
	}
}

// nullable turns an empty row id into a SQL NULL for the keyset predicate.
func nullable(rowID string) any {
	if rowID == "" {
		return nil
	}
	return rowID
}

func mustJSON(v map[string]any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func accountPtr(a accounts.AccountID) *accounts.AccountID {
	if a.IsZero() {
		return nil
	}
	return &a
}
