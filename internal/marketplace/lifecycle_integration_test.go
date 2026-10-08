//go:build integration

package marketplace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
)

func TestIntegration_SQLAndGoAgreeOnTheLifecycle(t *testing.T) {
	requireEnv(t)
	ctx := context.Background()
	for _, from := range AllStatuses() {
		for _, to := range AllStatuses() {
			var legal bool
			require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_marketplace_transition_legal($1,$2)`, from, to).Scan(&legal))
			assert.Equal(t, CanTransition(from, to), legal, "%s -> %s", from, to)
		}
	}
	for _, from := range AllModerationStates() {
		for _, to := range AllModerationStates() {
			var legal bool
			require.NoError(t, testDB.QueryRow(ctx, `SELECT cp_marketplace_moderation_legal($1,$2)`, from, to).Scan(&legal))
			assert.Equal(t, CanModerate(from, to), legal, "%s -> %s", from, to)
		}
	}
}

func TestIntegration_LifecycleFromDraftToDeprecated(t *testing.T) {
	f := newFixture(t)
	l, v := f.approved("momentum")
	assert.Equal(t, 1, v.Number)
	assert.Equal(t, l.ID.String()+"@1", v.StrategyRef())
	assert.Equal(t, 4, f.count(`SELECT count(*) FROM marketplace_version_transitions WHERE version_id = $1`, v.ID),
		"DRAFT->SUBMITTED->VALIDATED->IN_REVIEW->APPROVED: every step is a recorded transition")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM marketplace_moderation_transitions WHERE version_id = $1`, v.ID))
	val, ok, err := f.svc.ValidationOf(f.ctx, testDB, v.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, ValidationPassed, val.Outcome)
	assert.Len(t, val.ArtifactDigest, 32)
	assert.Empty(t, val.ScanCodes)

	// A second version of the listing starts at DRAFT; the first is untouched.
	var v2 Version
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v2, err = f.svc.NewVersion(ctx, tx, l.ID, f.creator, f.versionRequest("momentum"))
		return err
	}))
	assert.Equal(t, 2, v2.Number)
	assert.Equal(t, StatusDraft, v2.Status)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.NewVersion(ctx, tx, l.ID, f.followerActor, f.versionRequest("momentum"))
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "only the creator publishes versions")

	// Skipping a step is refused.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Approve(ctx, tx, v2.ID, f.operator, "MEETS_POLICY", "")
		return err
	})
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))

	in := f.install(v)
	dep := f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Deprecate(ctx, tx, v.ID, f.creator, "superseded")
	})
	assert.Equal(t, StatusDeprecated, dep.Status)
	_, err = f.follow(f.svc, in, f.input(f.clk.Now()))
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "a deprecated version is no longer followed")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.InstallVersion(ctx, tx, InstallRequest{VersionID: v.ID, AccountID: f.creatorAccount, Mode: gates.ModePaper, Actor: f.creator})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "nor installed")
}

func TestIntegration_TheDatabaseRefusesIllegalTransitions(t *testing.T) {
	f := newFixture(t)
	_, v := f.draft("momentum")
	exec := func(stmts ...string) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			for _, s := range stmts {
				if _, err := tx.Exec(ctx, s, v.ID); err != nil {
					return err
				}
			}
			return nil
		})
	}
	row := func(from, to string) string {
		return `INSERT INTO marketplace_version_transitions (id, version_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ('` + uuid.NewString() + `', $1, '` + from + `', '` + to + `', 'SYSTEM', 'bypass', 'bypass')`
	}
	// DRAFT -> APPROVED with a row describing exactly that: still illegal.
	err := exec(row("DRAFT", "APPROVED"), `UPDATE marketplace_listing_versions SET status = 'APPROVED' WHERE id = $1`)
	require.Error(t, err)
	assert.Equal(t, "MK001", db.SQLState(err))
	// A legal edge with no transition row: refused at COMMIT.
	err = exec(`UPDATE marketplace_listing_versions SET status = 'SUBMITTED' WHERE id = $1`)
	require.Error(t, err)
	assert.Equal(t, "AU001", db.SQLState(err))
	// VALIDATED without a passed validation.
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.Submit(ctx, tx, v.ID, f.creator) })
	err = exec(row("SUBMITTED", "VALIDATED"), `UPDATE marketplace_listing_versions SET status = 'VALIDATED' WHERE id = $1`)
	require.Error(t, err)
	assert.Equal(t, "MK003", db.SQLState(err))
	// A version cannot be born anywhere but DRAFT.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_listing_versions (id, listing_id, version, status, moderation_state, description,
			risk_class, supported_instrument_ids, manifest_canonical, manifest_hash, source_digest, source_commit, data_dependencies,
			performance_fee_bps, management_fee_bps, fee_high_water_mark, config, created_by_actor_type, created_by_actor_id)
			SELECT $2, listing_id, 99, 'APPROVED', 'CLEARED', description, risk_class, supported_instrument_ids, manifest_canonical,
			manifest_hash, source_digest, source_commit, data_dependencies, performance_fee_bps, management_fee_bps, fee_high_water_mark,
			config, created_by_actor_type, created_by_actor_id FROM marketplace_listing_versions WHERE id = $1`, v.ID, uuid.New())
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK001", db.SQLState(err))
}

func TestIntegration_CreatorCannotApproveOwnListing(t *testing.T) {
	f := newFixture(t)
	_, v := f.draft("momentum")
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.Submit(ctx, tx, v.ID, f.creator) })
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		out, _, err := f.svc.Validate(ctx, tx, v.ID, econ.SystemActor("validator"))
		return out, err
	})
	// The creator, even holding an operator identity, cannot review.
	creatorAsOperator := econ.Actor{Type: security.ActorOperator, ID: f.creator.ID}
	for _, try := range []func(ctx context.Context, tx pgx.Tx) error{
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.StartReview(ctx, tx, v.ID, creatorAsOperator)
			return err
		},
		func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.StartReview(ctx, tx, v.ID, f.creator)
			return err
		},
	} {
		err := f.inTx(try)
		require.Error(t, err)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	}
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.StartReview(ctx, tx, v.ID, f.operator)
	})
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Approve(ctx, tx, v.ID, creatorAsOperator, "SELF_APPROVAL", "")
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	// Around Go: the database refuses the creator's review row ...
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_reviews (id, version_id, reviewer_actor_type, reviewer_actor_id, decision, reason_code, reviewed_at)
			VALUES ($1,$2,'OPERATOR',$3,'APPROVE','SELF_APPROVAL',now())`, uuid.New(), v.ID, f.creator.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK002", db.SQLState(err))
	// ... and an APPROVED status with no operator approval behind it.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_version_transitions (id, version_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'IN_REVIEW','APPROVED','USER',$3,'self')`, uuid.New(), v.ID, f.creator.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE marketplace_listing_versions SET status = 'APPROVED' WHERE id = $1`, v.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK002", db.SQLState(err))

	// A different operator approves.
	out := f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Approve(ctx, tx, v.ID, f.operator2, "MEETS_POLICY", "")
	})
	assert.Equal(t, StatusApproved, out.Status)
}

func TestIntegration_ConcurrentApprovalsApproveOnce(t *testing.T) {
	f := newFixture(t)
	_, v := f.draft("momentum")
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.Submit(ctx, tx, v.ID, f.creator) })
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		out, _, err := f.svc.Validate(ctx, tx, v.ID, econ.SystemActor("validator"))
		return out, err
	})
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.StartReview(ctx, tx, v.ID, f.operator)
	})
	var wg sync.WaitGroup
	results := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := econ.Actor{Type: security.ActorOperator, ID: uuid.NewString()}
			results <- f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, err := f.svc.Approve(ctx, tx, v.ID, op, "MEETS_POLICY", "")
				return err
			})
		}()
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		if err == nil {
			ok++
			continue
		}
		assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err))
	}
	assert.Equal(t, 1, ok, "exactly one approval wins")
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM marketplace_reviews WHERE version_id = $1`, v.ID))
}

func TestIntegration_ManifestIsImmutable(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	wider := f.manifest()
	wider.AllowedIntentKinds = sandboxrt.ProposableKinds()
	wider.MaxNotionalPerIntent = "100000000.00"
	canonical, err := wider.CanonicalBytes()
	require.NoError(t, err)
	hash, err := wider.Hash()
	require.NoError(t, err)

	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE marketplace_listing_versions SET manifest_canonical = $2, manifest_hash = $3 WHERE id = $1`, v.ID, canonical, hash)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err), "the application role cannot even name the manifest columns")

	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer func() { _ = owner.Close(f.ctx) }()
	_, err = owner.Exec(f.ctx, `UPDATE marketplace_listing_versions SET manifest_canonical = $2, manifest_hash = $3 WHERE id = $1`, v.ID, canonical, hash)
	require.Error(t, err)
	assert.Equal(t, "MK004", db.SQLState(err), "and the owner is stopped by the trigger")
	_, err = owner.Exec(f.ctx, `UPDATE marketplace_listing_versions SET config = '{"order_units":"999999999"}' WHERE id = $1`, v.ID)
	require.Error(t, err)
	assert.Equal(t, "MK004", db.SQLState(err), "the approved configuration is frozen too")

	// A hash that is not the hash of the stored bytes is refused outright.
	_, err = owner.Exec(f.ctx, `INSERT INTO marketplace_listing_versions (id, listing_id, version, status, moderation_state, description,
		risk_class, supported_instrument_ids, manifest_canonical, manifest_hash, source_digest, source_commit, data_dependencies,
		performance_fee_bps, management_fee_bps, fee_high_water_mark, config, created_by_actor_type, created_by_actor_id)
		SELECT $2, listing_id, 77, 'DRAFT', 'UNREVIEWED', description, risk_class, supported_instrument_ids, $3, manifest_hash, source_digest,
		source_commit, data_dependencies, performance_fee_bps, management_fee_bps, fee_high_water_mark, config, created_by_actor_type,
		created_by_actor_id FROM marketplace_listing_versions WHERE id = $1`, v.ID, uuid.New(), canonical)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marketplace_listing_versions_manifest_hash_matches")

	// Adding a source file to a stored version breaks its digest at COMMIT.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_version_sources (id, version_id, path, content) VALUES ($1,$2,'extra.go','package main')`,
			uuid.New(), v.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK005", db.SQLState(err))

	// What a run enforces is the manifest that was reviewed.
	back, err := f.svc.Version(f.ctx, testDB, v.ID)
	require.NoError(t, err)
	assert.Equal(t, f.manifest(), back.Manifest)
	sum := sha256.Sum256(mustCanonical(t, back.Manifest))
	assert.True(t, bytes.Equal(sum[:], back.ManifestHash))
}

func mustCanonical(t *testing.T, m sandboxrt.Manifest) []byte {
	t.Helper()
	b, err := m.CanonicalBytes()
	require.NoError(t, err)
	return b
}

func TestIntegration_InstallIsPaperOnly(t *testing.T) {
	f := newFixture(t)
	_, v := f.approved("momentum")
	for _, mode := range []gates.Mode{gates.ModeSandbox, gates.ModeDevnet, gates.ModeLive} {
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := f.svc.InstallVersion(ctx, tx, InstallRequest{VersionID: v.ID, AccountID: f.follower, Mode: mode, Actor: f.followerActor})
			return err
		})
		require.Error(t, err, mode)
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
		// And the database refuses the row when Go is not the caller.
		err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO marketplace_installs (id, account_id, version_id, mode, status, installed_by_actor_type, installed_by_actor_id)
				VALUES ($1,$2,$3,$4,'ACTIVE','USER','u')`, uuid.New(), f.follower, v.ID, string(mode))
			return err
		})
		require.Error(t, err, mode)
		assert.Contains(t, err.Error(), "marketplace_installs_mode_check")
	}
	in := f.install(v)
	assert.Equal(t, gates.ModePaper, in.Mode)
	again := f.install(v)
	assert.Equal(t, in.ID, again.ID, "installing twice is one install")

	// A version that is not approved cannot be installed, even around Go.
	_, draft := f.draft("momentum")
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_installs (id, account_id, version_id, mode, status, installed_by_actor_type, installed_by_actor_id)
			VALUES ($1,$2,$3,'PAPER','ACTIVE','USER','u')`, uuid.New(), f.follower, draft.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK006", db.SQLState(err))

	removed, err := func() (Install, error) {
		var out Install
		err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = f.svc.RemoveInstall(ctx, tx, in.ID, f.followerActor, "done")
			return err
		})
		return out, err
	}()
	require.NoError(t, err)
	assert.Equal(t, InstallRemoved, removed.Status)
	// REMOVED is final, in SQL too.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_install_transitions (id, install_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'REMOVED','ACTIVE','SYSTEM','x','revive')`, uuid.New(), in.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE marketplace_installs SET status = 'ACTIVE' WHERE id = $1`, in.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK008", db.SQLState(err))
}

func TestIntegration_ValidationRejectsForbiddenAndBrokenSource(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		strategy string
		reason   string
		code     string
	}{
		{"forbidden", sandboxrt.BuildFailureScan, string(sandboxrt.ScanImportForbidden)},
		{"broken", sandboxrt.BuildFailureCompile, ""},
	} {
		t.Run(c.strategy, func(t *testing.T) {
			_, v := f.draft(c.strategy)
			f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.Submit(ctx, tx, v.ID, f.creator) })
			var val Validation
			out := f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
				var out Version
				var err error
				out, val, err = f.svc.Validate(ctx, tx, v.ID, econ.SystemActor("validator"))
				return out, err
			})
			assert.Equal(t, StatusRejected, out.Status)
			assert.Equal(t, c.reason, out.RejectionCode)
			assert.Equal(t, ValidationFailed, val.Outcome)
			if c.code != "" {
				assert.Contains(t, val.ScanCodes, c.code)
			}
			stored, ok, err := f.svc.ValidationOf(f.ctx, testDB, v.ID)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Empty(t, stored.ArtifactDigest, "a refused build is never an artifact")
		})
	}
}

func TestIntegration_ForkProvenance(t *testing.T) {
	f := newFixture(t)
	parentListing, parent := f.approved("momentum")
	forkerAccount, forker := f.account()
	var fork Listing
	var fv Version
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		fork, fv, err = f.svc.Fork(ctx, tx, parent.ID, ForkRequest{CreatorAccountID: forkerAccount, Creator: forker, Title: "Momentum, forked",
			Version: VersionRequest{Description: "a fork", RiskClass: RiskAggressive, SourceCommit: "fork-1", Fees: FeeModel{}}})
		return err
	}))
	require.NotNil(t, fork.ForkedFromVersionID)
	assert.Equal(t, parent.ID, *fork.ForkedFromVersionID)
	assert.Equal(t, StatusDraft, fv.Status, "a fork goes through the whole lifecycle again")
	assert.Equal(t, parent.SourceDigest, fv.SourceDigest, "with no source of its own it starts from the parent's")
	assert.Equal(t, parent.ManifestHash, fv.ManifestHash)

	// A fork of the fork: provenance walks the whole chain.
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.Submit(ctx, tx, fv.ID, forker) })
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		out, _, err := f.svc.Validate(ctx, tx, fv.ID, econ.SystemActor("validator"))
		return out, err
	})
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.StartReview(ctx, tx, fv.ID, f.operator)
	})
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Approve(ctx, tx, fv.ID, f.operator, "MEETS_POLICY", "")
	})
	var grandchild Version
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, grandchild, err = f.svc.Fork(ctx, tx, fv.ID, ForkRequest{CreatorAccountID: f.creatorAccount, Creator: f.creator, Title: "Third generation",
			Version: VersionRequest{Description: "again", RiskClass: RiskModerate, SourceCommit: "fork-2"}})
		return err
	}))
	p, err := f.svc.Provenance(f.ctx, testDB, grandchild.ID)
	require.NoError(t, err)
	require.Len(t, p.ForkedFrom, 2)
	assert.Equal(t, fv.ID, p.ForkedFrom[0].VersionID)
	assert.Equal(t, parent.ID, p.ForkedFrom[1].VersionID)
	assert.Equal(t, parentListing.ID, p.ForkedFrom[1].ListingID)
	assert.Equal(t, "fork-2", p.SourceCommit)

	pp, err := f.svc.Provenance(f.ctx, testDB, parent.ID)
	require.NoError(t, err)
	assert.Len(t, pp.ArtifactDigest, 32)
	assert.Contains(t, pp.ToolchainVersion, "go1.")
	assert.Contains(t, pp.SDKVersion, "nodalsdk/v1@sha256:")
	assert.Equal(t, "wazero/v1.12.0", pp.RuntimeVersion)

	// Only a public version can be forked: in Go, and in SQL.
	_, draft := f.draft("momentum")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.Fork(ctx, tx, draft.ID, ForkRequest{CreatorAccountID: forkerAccount, Creator: forker, Title: "early"})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_listings (id, creator_account_id, creator_actor_type, creator_actor_id, title, forked_from_version_id)
			VALUES ($1,$2,'USER',$3,'sneaky',$4)`, uuid.New(), forkerAccount, forker.ID, draft.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK010", db.SQLState(err))
}

func TestIntegration_AnAgentCannotTouchTheMarketplace(t *testing.T) {
	f := newFixture(t)
	agent := econ.Actor{Type: security.ActorAgent, ID: uuid.NewString()}
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.CreateListing(ctx, tx, ListingRequest{CreatorAccountID: f.creatorAccount, Creator: agent, Title: "agent",
			Version: f.versionRequest("momentum")})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	_, v := f.approved("momentum")
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.InstallVersion(ctx, tx, InstallRequest{VersionID: v.ID, AccountID: f.follower, Mode: gates.ModePaper, Actor: agent})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	in := f.install(v)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.PaperFollow(ctx, tx, FollowRequest{InstallID: in.ID, Input: f.input(f.clk.Now()), Trigger: TriggerExplicit, Actor: agent})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}
