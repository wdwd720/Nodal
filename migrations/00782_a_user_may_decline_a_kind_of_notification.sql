-- +goose Up
-- Per-kind notification preferences (product goal §36, "avoid notification spam").
--
-- The channel column holds exactly one value, IN_APP, and says so in a CHECK rather than in a
-- comment. No e-mail, SMS or push provider exists anywhere in this codebase -- internal/notification's
-- Provider interface has two implementations, a no-op and a fan-out over nothing -- so a row saying
-- EMAIL would be a promise the deployment cannot keep. When a delivery channel is built it widens
-- this CHECK; until then the schema states the truth.
--
-- A kind absent from this table is ENABLED. That direction is deliberate: a new kind must not be
-- silently muted for every existing user because nobody backfilled a row for it.
--
-- Some kinds are not suppressible at all (security, account restriction, a reversed purchase, a
-- failed payout, SYSTEM). That rule lives in Go, in notifications.Kind.Suppressible, because it is a
-- product judgement about which facts a person may switch off and still be treated as informed --
-- not a schema constraint. A row disabling one of them is accepted and ignored, and the read side
-- reports it as enforced=false so the UI never shows a switch that does nothing.

CREATE TABLE notification_preferences (
    user_id     uuid NOT NULL REFERENCES users(id),
    kind        text NOT NULL,
    channel     text NOT NULL CHECK (channel IN ('IN_APP')),
    enabled     boolean NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, kind, channel)
);
CREATE TRIGGER notification_preferences_updated_at BEFORE UPDATE ON notification_preferences
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The kind CHECK is deliberately NOT repeated here. notifications.kind is the list, and a second
-- copy of it in a second CHECK is exactly the divergence test/integration/enums exists to prevent;
-- a preference row for a kind that no longer exists is inert, not a defect.

GRANT SELECT, INSERT, UPDATE ON notification_preferences TO cp_app;
GRANT SELECT ON notification_preferences TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: a person's stated preference is theirs, and a rollback is not where it is discarded
