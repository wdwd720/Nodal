-- +goose Up
-- In-app notifications (PART 193). Delivery through external providers is abstracted in
-- internal/notification; this table is the customer-visible record and is never financial truth.

CREATE TABLE notifications (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id),
    account_id    uuid REFERENCES accounts(id),
    kind          text NOT NULL CHECK (kind IN (
                      'FUNDING_AVAILABLE','FUNDING_FAILED','FUNDING_REVERSED','TRADE_FILLED','TRADE_FAILED',
                      'AGENT_PAUSED','RISK_LIMIT_HIT','SECURITY_SESSION_EVENT','RECONCILIATION_HOLD')),
    severity      text NOT NULL CHECK (severity IN ('INFO','WARN','CRITICAL')),
    title         text NOT NULL,
    body          text NOT NULL,
    resource_type text,
    resource_id   text,
    data          jsonb NOT NULL DEFAULT '{}'::jsonb,
    dedup_key     text,
    correlation_id text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    read_at       timestamptz,
    delivered_at  timestamptz,
    delivery_error text
);
CREATE INDEX notifications_user_idx ON notifications (user_id, created_at DESC, id DESC);
CREATE INDEX notifications_unread_idx ON notifications (user_id) WHERE read_at IS NULL;
CREATE UNIQUE INDEX notifications_dedup_idx ON notifications (user_id, dedup_key) WHERE dedup_key IS NOT NULL;
CREATE INDEX notifications_undelivered_idx ON notifications (created_at) WHERE delivered_at IS NULL AND delivery_error IS NULL;

-- Only read_at / delivered_at / delivery_error may change after insert.
-- +goose StatementBegin
CREATE FUNCTION notifications_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'NOTIFICATION_IMMUTABLE: notifications are never deleted by the application' USING ERRCODE = 'LG003';
    END IF;
    IF NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.title IS DISTINCT FROM OLD.title
       OR NEW.body IS DISTINCT FROM OLD.body OR NEW.data IS DISTINCT FROM OLD.data OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.account_id IS DISTINCT FROM OLD.account_id OR NEW.dedup_key IS DISTINCT FROM OLD.dedup_key THEN
        RAISE EXCEPTION 'NOTIFICATION_IMMUTABLE: notification content cannot change' USING ERRCODE = 'LG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER notifications_guard BEFORE UPDATE OR DELETE ON notifications FOR EACH ROW EXECUTE FUNCTION notifications_guard();

GRANT SELECT, INSERT, UPDATE ON notifications TO cp_app;
GRANT SELECT ON notifications TO cp_readonly, cp_ops;
GRANT DELETE ON notifications TO cp_ops;

-- +goose Down
SELECT 1; -- protected: customer-visible history is retained per retention policy, not by rollback
