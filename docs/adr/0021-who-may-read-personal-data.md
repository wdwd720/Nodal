# ADR-0021 — Who may read personal data

Status: **Accepted** (2026-09-10)

Supersedes nothing. Resolves F-47. Constrains any future table that holds
personal data and any future grant to `cp_readonly` or `cp_ops`.

## Context

Migration 00010 created `identity_pii` with `email_encrypted`,
`legal_name_encrypted`, `dob_encrypted` and `key_version`, and granted
`cp_readonly` and `cp_ops` SELECT on a named list of tables that deliberately
left `identity_pii` and `sessions` out. The role bootstrap's
`ALTER DEFAULT PRIVILEGES ... GRANT SELECT ON TABLES TO cp_readonly, cp_ops`
granted both anyway and silently won. `privileges_test.go` asserted the
bootstrap's side as the contract.

F-47 recorded the contradiction and refused to resolve it, correctly: whether
SELECT on `identity_pii` is an exposure depends on whether the columns hold
ciphertext, and for a year nothing in the repository wrote them. The
encryption was DESIGNED. A decision taken then would have been a preference.

## Decision

1. **Personal data is encrypted in the application, or it is not stored.**
   `internal/pii` seals each column with AES-256-GCM under a versioned keyring
   the deployment supplies as one SecretRef (`CP_PII_KEYRING`). The additional
   authenticated data binds every ciphertext to the table, the column, the
   user id and the key version, so whoever can write the table cannot
   rearrange it. The key never touches the database. STAGING and PROD refuse
   to start without a keyring; LOCAL and TEST without one store nothing and
   say so.

2. **`internal/pii/store.go` is the only writer of `identity_pii`.**
   `test/security` asserts it. A write anywhere else is a path for plaintext
   into the column.

3. **A role with no use for personal data cannot read it.** Migration 00754
   revokes SELECT on `identity_pii` from `cp_readonly` and `cp_ops`, and
   SELECT on `sessions` from both; `cp_ops` is granted `SELECT (expires_at)`
   on `sessions`, which is what its retention `DELETE` filters by, and nothing
   else there. This is 00010's list, in effect at last.

4. **The bootstrap's blanket default stays.** It is what makes every new table
   readable by the two roles without a migration remembering to say so. The
   tables withheld on purpose are a named list in
   `test/integration/migrations/privileges_test.go`, asserted in the strong
   direction, so the list cannot grow silently. A new table holding personal
   data joins that list and gets its own REVOKE.

5. **No KMS on the launch tier.** A KMS key is a fixed monthly cost. The
   paid tier wraps the same keyring in an envelope; the row format does not
   change.

## Why derived and not chosen

With the columns ciphertext under a key the database never holds, a SELECT
grant to a role with no use for the table is not a capability, it is an
exposure deferred until a key leaks. `cp_readonly` is analytics and support
reads, and PART 121 says personal data does not travel there. `cp_ops` runs
retention and never touches `identity_pii`; its one use of `sessions` needs
one column. Nothing in the architecture gives either role a reason to read
what 00754 withholds, and F-47's own constraint — `cp_ops` must still be able
to purge sessions — is satisfied by the column grant rather than traded away.

## Consequences

- `NODAL_PII_KEYRING` is a dashboard secret the deployment must supply.
- Rotation is: add a key, raise `active`, deploy, `Store.Reseal` each row,
  remove the old key once no row names its version. A ring missing a version
  a row names is an error on read, never an empty record.
- `legal_name` and `dob` have a sealed home and no source until an
  identity-verification provider is selected (B-06).
- Checking what `cp_ops` did with `sessions` found that nothing purged them on
  any tier (F-133). The purge runs from `cmd/api`'s retention loop now.

## Evidence

`internal/pii` unit and integration tests; `cmd/api/pii_test.go`;
`internal/identity` `TestIntegration_Login_StoresTheVerifiedEmailEncrypted`
and `TestIntegration_ExpiredSessionsArePurgedAsOps`; `test/security`
`TestPII_OnlyTheEncryptingStoreWritesPersonalData`;
`test/integration/migrations` `TestIntegration_ApplicationRolePrivileges`.
