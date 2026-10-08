//go:build integration

package delegation

import (
	"context"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
)

// TestIntegration_TheDatabaseRefusesUnlicensedSwapStates: with Go out of
// the way -- as the migration owner, who holds every privilege -- the
// database still refuses every state the delegated signing path depends on
// never existing.
func TestIntegration_TheDatabaseRefusesUnlicensedSwapStates(t *testing.T) {
	f := newFixture(t)
	c := f.customer(100_000_000)
	capability := f.grant(c, nil)
	owner := ownerConn(t)
	ctx := f.ctx

	t.Run("signed without an approved decision over its message (DG005)", func(t *testing.T) {
		sw := f.mustSwap(c, capability.ID, 1, 1_000_000)
		err := rawTx(ctx, owner, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO delegated_swap_transitions (id, swap_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1, $2, 'ADMITTED', 'SIGNED', 'SYSTEM', 'forger', 'bypass')`, uuid.New(), sw.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE delegated_swaps SET status = 'SIGNED', message = '\x01', recent_blockhash = 'x',
				blockhash_height = 1, signed_at = $2, tx_id = $3, signed_tx = '\x01' WHERE id = $1`, sw.ID, f.clk.Now(), uuid.NewString())
			return err
		})
		requireSQLState(t, err, "DG005")
	})

	t.Run("signed under a revoked capability (DG005)", func(t *testing.T) {
		other := f.grant(c, nil)
		sw := f.mustSwap(c, other.ID, 1, 1_000_000)
		_, err := f.revoke(f.owner(c), other.ID)
		require.NoError(t, err)
		err = rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE delegated_swaps SET status = 'SIGNED', message = '\x01', signed_at = $2 WHERE id = $1`,
				sw.ID, f.clk.Now())
			return err
		})
		requireSQLState(t, err, "DG005")
	})

	t.Run("an approval of a plan the swap does not carry (DG003)", func(t *testing.T) {
		needs := f.grant(c, func(r *GrantRequest) { r.RequiresHumanApproval = true })
		sw := f.mustSwap(c, needs.ID, 1, 1_000_000)
		require.Equal(t, SwapAwaitingApproval, sw.Status)
		forged := sha256Of([]byte("another plan"))
		err := rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_swap_approvals (id, swap_id, decision, plan_hash, approver_actor_type,
				approver_actor_id, authenticated_at, decided_at, reason) VALUES ($1, $2, 'APPROVE', $3, 'USER', $4, now(), now(), 'forged')`,
				uuid.New(), sw.ID, forged[:], c.user.String())
			return err
		})
		requireSQLState(t, err, "DG003")
		// Approved without any approval row at all.
		err = rawTx(ctx, owner, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO delegated_swap_transitions (id, swap_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1, $2, 'AWAITING_APPROVAL', 'APPROVED', 'SYSTEM', 'forger', 'bypass')`, uuid.New(), sw.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE delegated_swaps SET status = 'APPROVED' WHERE id = $1`, sw.ID)
			return err
		})
		requireSQLState(t, err, "DG005")
	})

	settled := f.mustSwap(c, capability.ID, 2, 1_000_000)
	res, err := f.svc.ExecuteSwap(ctx, settled.ID)
	require.NoError(t, err)
	settled = res.Swap
	require.Equal(t, SwapSettled, settled.Status)

	t.Run("a second signing decision for one swap (unique)", func(t *testing.T) {
		err := rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_signing_decisions (id, swap_id, account_id, capability_id, capability_hash,
				plan_hash, message_sha256, signer_public_key, verdict, reason_codes, policy_version, policy_hash, inspector_version, decided_at)
				SELECT $1, swap_id, account_id, capability_id, capability_hash, plan_hash, message_sha256, signer_public_key, verdict,
				       reason_codes, policy_version, policy_hash, inspector_version, decided_at
				  FROM delegated_signing_decisions WHERE swap_id = $2`, uuid.New(), settled.ID)
			return err
		})
		requireSQLState(t, err, "23505")
	})

	t.Run("a set-once column changed (DG002), and identity (DG002)", func(t *testing.T) {
		for _, sql := range []string{
			`UPDATE delegated_swaps SET signed_tx = '\x00' WHERE id = $1`,
			`UPDATE delegated_swaps SET tx_id = 'another' WHERE id = $1`,
			`UPDATE delegated_swaps SET journal_transaction_id = NULL WHERE id = $1`,
		} {
			_, err := owner.Exec(ctx, sql, settled.ID)
			requireSQLState(t, err, "DG002")
		}
		for _, sql := range []string{
			`UPDATE delegated_swaps SET amount_out = amount_out + 1 WHERE id = $1`,
			`UPDATE delegated_swaps SET plan_hash = $2 WHERE id = $1`,
		} {
			args := []any{settled.ID}
			if len(sql) > 0 && regexp.MustCompile(`\$2`).MatchString(sql) {
				h := sha256Of([]byte("x"))
				args = append(args, h[:])
			}
			_, err := owner.Exec(ctx, sql, args...)
			requireSQLState(t, err, "DG002")
		}
		// The application role cannot even name an identity column.
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE delegated_swaps SET amount_in = 1 WHERE id = $1`, settled.ID)
			return err
		})
		requireSQLState(t, err, "42501")
	})

	t.Run("an illegal edge (DG001) and an unlicensed one (AU001)", func(t *testing.T) {
		err := rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_swap_transitions (id, swap_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1, $2, 'SETTLED', 'ADMITTED', 'SYSTEM', 'forger', 'reopen')`, uuid.New(), settled.ID)
			return err
		})
		requireSQLState(t, err, "DG001")
		_, err = owner.Exec(ctx, `UPDATE delegated_swaps SET status = 'ADMITTED' WHERE id = $1`, settled.ID)
		requireSQLState(t, err, "DG001")
		pending := f.mustSwap(c, capability.ID, 3, 1_000_000)
		err = rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE delegated_swaps SET status = 'REJECTED', failure_code = 'X' WHERE id = $1`, pending.ID)
			return err
		})
		requireSQLState(t, err, "AU001")
	})

	t.Run("a capability use beyond its limit or outside its life (DG004)", func(t *testing.T) {
		tight := f.grant(c, func(r *GrantRequest) {
			r.MaxSingleTrade, r.MaxDailyNotional, r.MaxNotional = money.USDFromMinor(100), money.USDFromMinor(150), money.USDFromMinor(1_000)
		})
		use := func(notional int64, at time.Time) error {
			return rawTx(ctx, owner, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO delegated_capability_usage (id, capability_id, account_id, agent_id, subject_kind,
					subject_id, action_class, notional_minor, used_at, usage_day) VALUES ($1, $2, $3, $4, 'SWAP', $5, 'ACQUIRE_EXPOSURE', $6, $7, $8::date)`,
					uuid.New(), tight.ID, c.account, c.agentID, uuid.New(), notional, at, at.UTC().Format(time.DateOnly))
				return err
			})
		}
		require.NoError(t, use(100, f.clk.Now()))
		requireSQLState(t, use(101, f.clk.Now()), "DG004")                     // over the single limit
		requireSQLState(t, use(51, f.clk.Now()), "DG004")                      // over the daily limit
		require.NoError(t, use(50, f.clk.Now()))                               // exactly the daily limit
		requireSQLState(t, use(1, tight.Terms.ExpiresAt), "DG004")             // at the expiry
		requireSQLState(t, use(1, tight.GrantedAt.Add(-time.Minute)), "DG004") // before the grant
		withdraw := rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_capability_usage (id, capability_id, account_id, agent_id, subject_kind,
				subject_id, action_class, notional_minor, used_at, usage_day) VALUES ($1, $2, $3, $4, 'SWAP', $5, 'PROPOSE_WITHDRAWAL', 1, $6, $7::date)`,
				uuid.New(), tight.ID, c.account, c.agentID, uuid.New(), f.clk.Now(), f.clk.Now().Format(time.DateOnly))
			return err
		})
		requireSQLState(t, withdraw, "DG003")
	})

	t.Run("a capability born revoked, re-activated or too long-lived", func(t *testing.T) {
		err := rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_capability_transitions (id, capability_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1, $2, 'REVOKED', 'ACTIVE', 'USER', $3, 'un-revoke')`, uuid.New(), capability.ID, c.user.String())
			return err
		})
		requireSQLState(t, err, "DG001")
		forgedHash := sha256Of([]byte(uuid.NewString()))
		err = rawTx(ctx, owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO delegated_capabilities (id, account_id, agent_id, strategy_id, environment, mode,
				instrument_classes, action_classes, max_notional_minor, max_daily_notional_minor, max_single_trade_minor,
				destination_allowlist, chain_allowlist, granted_at, expires_at, nonce, requires_human_approval, content_hash,
				idempotency_key, status, granted_by_actor_type, granted_by_actor_id)
				SELECT $1, account_id, agent_id, strategy_id, environment, mode, instrument_classes, action_classes, max_notional_minor,
				       max_daily_notional_minor, max_single_trade_minor, destination_allowlist, chain_allowlist, granted_at,
				       granted_at + interval '91 days', $2, false, $3, $4, 'ACTIVE', 'USER', granted_by_actor_id
				  FROM delegated_capabilities WHERE id = $5`,
				uuid.New(), "cd"+capability.Nonce[2:], forgedHash[:], "long-"+uuid.NewString(), capability.ID)
			return err
		})
		requireSQLState(t, err, "23514")
	})

	t.Run("a settled swap that is not its SIM_TRADE (DG006)", func(t *testing.T) {
		devnetCap := f.grant(c, func(r *GrantRequest) { r.Mode = gates.ModeDevnet })
		sw := f.mustSwap(c, devnetCap.ID, 1, 1_000_000)
		res, err := f.svc.ExecuteSwap(ctx, sw.ID)
		require.NoError(t, err)
		require.Equal(t, SwapSubmitted, res.Swap.Status)
		var funding string
		require.NoError(t, testDB.QueryRow(ctx, `SELECT id::text FROM journal_transactions WHERE kind = 'SANDBOX_FUNDING'
			ORDER BY posted_at LIMIT 1`).Scan(&funding))
		err = rawTx(ctx, owner, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO delegated_swap_transitions (id, swap_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1, $2, 'SUBMITTED', 'SETTLED', 'SYSTEM', 'forger', 'bypass')`, uuid.New(), sw.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE delegated_swaps SET status = 'SETTLED', journal_transaction_id = $2 WHERE id = $1`, sw.ID, funding)
			return err
		})
		requireSQLState(t, err, "DG006")
	})
}

// TestIntegration_SQLAndGoAgreeOnTransitions: every legality function the
// migrations define is exactly its Go twin.
func TestIntegration_SQLAndGoAgreeOnTransitions(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	legal := func(fn, from, to string) bool {
		var ok bool
		require.NoError(t, testDB.QueryRow(ctx, `SELECT `+fn+`($1, $2)`, from, to).Scan(&ok))
		return ok
	}
	for _, from := range AllSwapStatuses() {
		for _, to := range AllSwapStatuses() {
			assert.Equal(t, CanTransitionSwap(from, to), legal("cp_delegation_swap_transition_legal", string(from), string(to)), "%s -> %s", from, to)
		}
	}
	for _, from := range AllWithdrawalStatuses() {
		for _, to := range AllWithdrawalStatuses() {
			assert.Equal(t, CanTransitionWithdrawal(from, to), legal("cp_delegation_withdrawal_transition_legal", string(from), string(to)), "%s -> %s", from, to)
		}
	}
	for _, from := range AllCapabilityStatuses() {
		for _, to := range AllCapabilityStatuses() {
			assert.Equal(t, CanTransitionCapability(from, to), legal("cp_delegation_capability_transition_legal", string(from), string(to)), "%s -> %s", from, to)
		}
	}
	for _, from := range AllDestinationStatuses() {
		for _, to := range AllDestinationStatuses() {
			assert.Equal(t, CanTransitionDestination(from, to), legal("cp_delegation_destination_transition_legal", string(from), string(to)), "%s -> %s", from, to)
		}
	}
}

var (
	checkLiteral = regexp.MustCompile(`'([^']*)'::text`)
)

// checkValues returns the values an IN-list or array-subset CHECK admits; a
// missing constraint is a failure, never agreement.
func checkValues(t *testing.T, table, constraint string) []string {
	t.Helper()
	var def string
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = $1::regclass AND conname = $2`, table, constraint).Scan(&def), "%s.%s", table, constraint)
	var out []string
	for _, m := range checkLiteral.FindAllStringSubmatch(def, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

func sortedStrs[T ~string](in []T) []string {
	out := strs(in)
	sort.Strings(out)
	return out
}

// TestIntegration_EveryDelegationEnumMatchesItsCheck: each Go list and the
// CHECK constraint holding the same list agree exactly; these are the pairs
// to register in test/integration/enums.
func TestIntegration_EveryDelegationEnumMatchesItsCheck(t *testing.T) {
	requireEnv(t)
	for _, p := range enumPairs() {
		assert.Equal(t, p.values, checkValues(t, p.table, p.constraint), "%s.%s", p.table, p.constraint)
	}
	// Every IN-list CHECK on these tables is one of the pairs above.
	rows, err := testDB.Query(context.Background(), `SELECT conrelid::regclass::text || '.' || conname FROM pg_constraint
		WHERE contype = 'c' AND connamespace = 'public'::regnamespace
		  AND conrelid::regclass::text LIKE 'delegat%'
		  AND (pg_get_constraintdef(oid) LIKE '%= ANY (ARRAY[%' OR pg_get_constraintdef(oid) LIKE '%<@ ARRAY[%')
		ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var found []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		found = append(found, s)
	}
	require.NoError(t, rows.Err())
	var want []string
	for _, p := range enumPairs() {
		want = append(want, p.table+"."+p.constraint)
	}
	sort.Strings(want)
	assert.Equal(t, want, found)
}

type enumPair struct {
	table, constraint string
	values            []string
}

func enumPairs() []enumPair {
	return []enumPair{
		{"delegated_capabilities", "delegated_capabilities_environment_check", sortedStrs(Environments())},
		{"delegated_capabilities", "delegated_capabilities_mode_check", sortedStrs(SupportedModes())},
		{"delegated_capabilities", "delegated_capabilities_status_check", sortedStrs(AllCapabilityStatuses())},
		{"delegated_capabilities", "delegated_capabilities_revoked_by_actor_type_check", sortedStrs(RevokerActorTypes())},
		{"delegated_capabilities", "delegated_capabilities_instrument_classes", sortedStrs(AllInstrumentClasses())},
		{"delegated_capabilities", "delegated_capabilities_action_classes", sortedStrs(AllActionClasses())},
		{"delegated_capabilities", "delegated_capabilities_chains", sortedStrs(Chains())},
		{"delegated_signer_keys", "delegated_signer_keys_chain_id_check", sortedStrs(Chains())},
		{"delegation_venues", "delegation_venues_chain_id_check", sortedStrs(Chains())},
		{"delegation_venues", "delegation_venues_created_by_actor_type_check", sortedStrs(VenueCreatorActorTypes())},
		{"delegated_capability_usage", "delegated_capability_usage_subject_kind_check", sortedStrs(AllSubjectKinds())},
		{"delegated_capability_usage", "delegated_capability_usage_action_class_check", sortedStrs(AllActionClasses())},
		{"delegated_swaps", "delegated_swaps_mode_check", sortedStrs(SupportedModes())},
		{"delegated_swaps", "delegated_swaps_action_class_check", sortedStrs(SwapActionClasses())},
		{"delegated_swaps", "delegated_swaps_chain_id_check", sortedStrs(Chains())},
		{"delegated_swaps", "delegated_swaps_status_check", sortedStrs(AllSwapStatuses())},
		{"delegated_swap_approvals", "delegated_swap_approvals_decision_check", sortedStrs(AllDecisions())},
		{"delegated_signing_decisions", "delegated_signing_decisions_verdict_check", sortedStrs(AllVerdicts())},
		{"delegated_withdrawal_destinations", "delegated_withdrawal_destinations_chain_id_check", sortedStrs(Chains())},
		{"delegated_withdrawal_destinations", "delegated_withdrawal_destinations_status_check", sortedStrs(AllDestinationStatuses())},
		{"delegated_withdrawal_destinations", "delegated_withdrawal_destinations_removed_by_actor_type_check", sortedStrs(RevokerActorTypes())},
		{"delegated_withdrawals", "delegated_withdrawals_environment_check", sortedStrs(Environments())},
		{"delegated_withdrawals", "delegated_withdrawals_mode_check", sortedStrs(SupportedModes())},
		{"delegated_withdrawals", "delegated_withdrawals_chain_id_check", sortedStrs(Chains())},
		{"delegated_withdrawals", "delegated_withdrawals_evaluation_outcome_check", sortedStrs(StoredEvaluationOutcomes())},
		{"delegated_withdrawals", "delegated_withdrawals_status_check", sortedStrs(AllWithdrawalStatuses())},
		// Rules rather than lists, each with its Go statement of the rule.
		{"delegated_withdrawals", "delegated_withdrawals_auto_only_in_test",
			sortedStrs(append(strs(TestEnvironments()), string(OutcomeAutoApprove)))},
		{"delegated_swaps", "delegated_swaps_signed_has_tx", sortedStrs(SwapStatusesBeforeSigning())},
		{"delegated_swaps", "delegated_swaps_failure_code", sortedStrs(SwapStatusesWithFailureCode())},
		{"delegated_withdrawals", "delegated_withdrawals_signed_has_tx", sortedStrs(WithdrawalStatusesBeforeSigning())},
		{"delegated_withdrawals", "delegated_withdrawals_failure_code", sortedStrs(WithdrawalStatusesWithFailureCode())},
		{"delegated_withdrawal_approvals", "delegated_withdrawal_approvals_decision_check", sortedStrs(AllDecisions())},
		{"delegated_withdrawal_approvals", "delegated_withdrawal_approvals_approver_actor_type_check", sortedStrs(WithdrawalApproverActorTypes())},
	}
}
