-- +goose Up
-- The notifications table has existed since 00640 with nine kinds and no producer at all
-- (internal/notification has no non-test caller anywhere). The product surface now writes it,
-- in the transaction that caused the state change, so a notification exists if and only if the
-- thing it describes committed.
--
-- Three changes, and nothing else:
--
--   1. The kind CHECK is widened to the product's own vocabulary. The nine original names stay
--      because internal/notification still declares them and its integration test still writes
--      them; removing them would delete a test rather than retire a package. internal/notifications
--      declares the union (AllKinds), so test/integration/enums can pair the CHECK against Go for
--      the first time -- the pairing is what makes the next narrowing safe.
--   2. A `sandbox` flag, because ADR-0023 requires a sandbox outcome to be labelled sandbox
--      everywhere it is stored and shown. There is no environment column here to CHECK against,
--      so the refusal lives in Go: the producer refuses Sandbox=true off a sandbox tier.
--   3. The guard grows to cover the columns added since it was written, plus severity and the
--      resource reference. Only read_at, delivered_at and delivery_error may still change.

ALTER TABLE notifications DROP CONSTRAINT notifications_kind_check;
ALTER TABLE notifications ADD CONSTRAINT notifications_kind_check CHECK (kind IN (
    -- The product vocabulary (internal/notifications).
    'CREDIT_PURCHASE_CAPTURED','CREDIT_PURCHASE_REVERSED',
    'NATIVE_TRADE_FILLED','NATIVE_MARKET_PAUSED',
    'PAYOUT_ACCEPTED','PAYOUT_SETTLED','PAYOUT_FAILED','PAYOUT_NEEDS_REVIEW',
    'VERIFICATION_UPDATED','ACCOUNT_RESTRICTED','SECURITY_NEW_SESSION','AGENT_PAUSED','SYSTEM',
    -- Declared by internal/notification (00640) and written by nothing but its own tests.
    'FUNDING_AVAILABLE','FUNDING_FAILED','FUNDING_REVERSED','TRADE_FILLED','TRADE_FAILED',
    'RISK_LIMIT_HIT','SECURITY_SESSION_EVENT','RECONCILIATION_HOLD'));

ALTER TABLE notifications ADD COLUMN sandbox boolean NOT NULL DEFAULT false;

-- Reading the centre filtered by kind, newest first, is the notification page's only other query
-- shape; without this it is a scan of the user's whole history.
CREATE INDEX notifications_user_kind_idx ON notifications (user_id, kind, created_at DESC, id DESC);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notifications_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'NOTIFICATION_IMMUTABLE: notifications are never deleted by the application' USING ERRCODE = 'LG003';
    END IF;
    IF NEW.user_id IS DISTINCT FROM OLD.user_id OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.title IS DISTINCT FROM OLD.title
       OR NEW.body IS DISTINCT FROM OLD.body OR NEW.data IS DISTINCT FROM OLD.data OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.account_id IS DISTINCT FROM OLD.account_id OR NEW.dedup_key IS DISTINCT FROM OLD.dedup_key
       OR NEW.severity IS DISTINCT FROM OLD.severity OR NEW.sandbox IS DISTINCT FROM OLD.sandbox
       OR NEW.resource_type IS DISTINCT FROM OLD.resource_type OR NEW.resource_id IS DISTINCT FROM OLD.resource_id THEN
        RAISE EXCEPTION 'NOTIFICATION_IMMUTABLE: notification content cannot change' USING ERRCODE = 'LG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 1; -- protected: reverting narrows a CHECK under rows that already satisfy the wider one, and a customer-visible record is retained by policy rather than by rollback
