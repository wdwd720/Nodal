package notifications

import (
	"context"
	"fmt"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// ChannelInApp is the only delivery channel that exists. The CHECK on
// notification_preferences.channel admits this value and no other, so a row
// promising an e-mail cannot be written by anything, including a mistake.
const ChannelInApp = "IN_APP"

// Preference is one person's answer for one kind.
type Preference struct {
	Kind    Kind
	Channel string
	Enabled bool
	// Enforced is false when the kind is not suppressible: the stored answer is
	// kept, and ignored. The read side reports it so a settings page can show
	// the switch as fixed rather than showing one that silently does nothing.
	Enforced bool
}

// LoadPreferences returns one Preference per product kind: the stored answer
// where there is one, ENABLED where there is not.
//
// Absence means enabled on purpose. The alternative -- absence means disabled --
// would mute every new kind for every existing user until somebody remembered
// to backfill a row, and the failure would be silence, which is the one failure
// mode a notification system cannot detect in itself.
func LoadPreferences(ctx context.Context, q db.Querier, userID accounts.UserID) ([]Preference, error) {
	if err := requireSelf(ctx, userID); err != nil {
		return nil, err
	}
	stored := map[Kind]bool{}
	rows, err := q.Query(ctx,
		`SELECT kind, enabled FROM notification_preferences WHERE user_id = $1 AND channel = $2`,
		userID, ChannelInApp)
	if err != nil {
		return nil, fmt.Errorf("notification: load preferences: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k Kind
		var enabled bool
		if err := rows.Scan(&k, &enabled); err != nil {
			return nil, fmt.Errorf("notification: load preferences: %w", err)
		}
		stored[k] = enabled
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notification: load preferences: %w", err)
	}
	out := make([]Preference, 0, len(productKinds))
	for _, k := range productKinds {
		p := Preference{Kind: k, Channel: ChannelInApp, Enabled: true, Enforced: k.Suppressible()}
		if v, ok := stored[k]; ok && k.Suppressible() {
			p.Enabled = v
		}
		out = append(out, p)
	}
	return out, nil
}

// SavePreferences replaces the caller's answers for the kinds named in want.
// Kinds absent from want keep whatever they had; a kind that is not
// suppressible is stored anyway and reported as unenforced, because refusing
// the write would make a settings page that round-trips its own state fail on a
// switch the user never touched.
func SavePreferences(ctx context.Context, q db.Querier, userID accounts.UserID, want map[Kind]bool, now time.Time) error {
	if err := requireSelf(ctx, userID); err != nil {
		return err
	}
	for k := range want {
		if !k.IsProduct() {
			return errs.Newf(errs.CodeValidationFailed, "notification: %q is not a kind a person can hold a preference about", k)
		}
	}
	for k, enabled := range want {
		if _, err := q.Exec(ctx,
			`INSERT INTO notification_preferences (user_id, kind, channel, enabled, updated_at)
			 VALUES ($1,$2,$3,$4,$5)
			 ON CONFLICT (user_id, kind, channel) DO UPDATE SET enabled = EXCLUDED.enabled`,
			userID, k, ChannelInApp, enabled, now.UTC()); err != nil {
			return fmt.Errorf("notification: save preference %s: %w", k, err)
		}
	}
	return nil
}

// suppressed reports whether the person switched this kind off. It is only
// consulted for suppressible kinds, so a stored `false` against a kind that is
// not suppressible can never reach it.
func suppressed(ctx context.Context, q db.Querier, userID accounts.UserID, kind Kind) (bool, error) {
	var enabled bool
	err := q.QueryRow(ctx,
		`SELECT enabled FROM notification_preferences WHERE user_id = $1 AND kind = $2 AND channel = $3`,
		userID, kind, ChannelInApp).Scan(&enabled)
	switch {
	case err == nil:
		return !enabled, nil
	case isNoRows(err):
		return false, nil
	default:
		return false, fmt.Errorf("notification: read preference: %w", err)
	}
}
