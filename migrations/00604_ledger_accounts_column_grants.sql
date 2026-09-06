-- +goose Up
-- The application role may open/close ledger accounts but must never change an
-- account's asset, owner, code, normal side, or negative-balance policy
-- (column-level privilege, PART 19 / PART 101).
REVOKE UPDATE ON ledger_accounts FROM cp_app;
GRANT UPDATE (status) ON ledger_accounts TO cp_app;

-- +goose Down
SELECT 1; -- protected: privilege hardening on ledger tables is never rolled back
