//go:build integration

package marketplace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"sync"
	"testing"
	"time"

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
	_, err = f.follow(f.svc, in)
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
	f.validate(v.ID)
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
	// ... and an APPROVED status with no operator approval behind it: the
	// creator cannot even write the transition (MK011), and with anyone
	// else's the status still needs an approval by an operator (MK002).
	approveAround := func(actor string) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO marketplace_version_transitions (id, version_id, from_status, to_status, actor_type, actor_id, reason)
				VALUES ($1,$2,'IN_REVIEW','APPROVED','USER',$3,'self')`, uuid.New(), v.ID, actor); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE marketplace_listing_versions SET status = 'APPROVED' WHERE id = $1`, v.ID)
			return err
		})
	}
	err = approveAround(f.creator.ID)
	require.Error(t, err)
	assert.Equal(t, "MK011", db.SQLState(err))
	err = approveAround(uuid.NewString())
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
	f.validate(v.ID)
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
				VALUES ($1,$2,$3,$4,'ACTIVE','USER',$5)`, uuid.New(), f.follower, v.ID, string(mode), f.followerActor.ID)
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
			out, val := f.validate(v.ID)
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
			require.NotNil(t, stored.ClaimID, "recorded under its build claim")
			if c.reason == sandboxrt.BuildFailureCompile {
				assert.Contains(t, string(stored.BuildLog), "./main.go:", "the author sees what the compiler said")
				assert.NotContains(t, string(stored.BuildLog), os.TempDir(), "and not where the host built it")
			} else {
				assert.Empty(t, stored.BuildLog, "nothing was compiled")
			}
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
	f.validate(fv.ID)
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
		_, err := f.svc.PaperFollow(ctx, tx, FollowRequest{InstallID: in.ID, Trigger: TriggerExplicit, Actor: agent})
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
}

// countingBuilder counts builds and can hold one open until released.
type countingBuilder struct {
	inner   ArtifactBuilder
	mu      sync.Mutex
	builds  int
	started chan struct{}
	release chan struct{}
	fail    error
}

func (b *countingBuilder) Build(ctx context.Context, src sandboxrt.Source) (sandboxrt.Build, error) {
	b.mu.Lock()
	b.builds++
	started, release, fail := b.started, b.release, b.fail
	b.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	if fail != nil {
		return sandboxrt.Build{}, fail
	}
	return b.inner.Build(ctx, src)
}

func (b *countingBuilder) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builds
}

func (f *fixture) withBuilder(b ArtifactBuilder) {
	f.t.Helper()
	cfg := f.config(f.sandbox)
	cfg.Builder = b
	svc, err := NewService(cfg)
	require.NoError(f.t, err)
	f.svc = svc
}

func (f *fixture) submitted(strategy string, src sandboxrt.Source) Version {
	f.t.Helper()
	req := f.versionRequest(strategy)
	if src != nil {
		req.Source = src
	}
	var v Version
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, v, err = f.svc.CreateListing(ctx, tx, ListingRequest{CreatorAccountID: f.creatorAccount, Creator: f.creator, Title: "submitted " + strategy,
			Version: req})
		return err
	}))
	return f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.Submit(ctx, tx, v.ID, f.creator) })
}

// blobSource is review-2 M2's source: 547 bytes that fold into a 64 MiB
// string constant.
const blobSource = `package main

import sdk "nodal.dev/sdk"

const a0 = "0123456789abcdef"
const a1 = a0 + a0 + a0 + a0 + a0 + a0 + a0 + a0
const a2 = a1 + a1 + a1 + a1 + a1 + a1 + a1 + a1
const a3 = a2 + a2 + a2 + a2 + a2 + a2 + a2 + a2
const a4 = a3 + a3 + a3 + a3 + a3 + a3 + a3 + a3
const a5 = a4 + a4 + a4 + a4 + a4 + a4 + a4 + a4
const a6 = a5 + a5 + a5 + a5 + a5 + a5 + a5 + a5
const a7 = a6 + a6 + a6 + a6 + a6 + a6 + a6 + a6

var blob = a7 + a7

func main() {
	sdk.Run(func(s *sdk.Session) error {
		if len(blob) == 0 {
			return nil
		}
		return nil
	})
}
`

// TestIntegration_AnOversizedBuildIsARecordedRejectionNeverAWedgedSubmission
// is review-2 F12 (M2): a build past the builder's output bound is the
// strategy's recorded rejection -- the version moves to REJECTED with the
// bound as its rejection code and its compiler output kept -- never an
// INTERNAL error that leaves it SUBMITTED for every retry to recompile.
func TestIntegration_AnOversizedBuildIsARecordedRejectionNeverAWedgedSubmission(t *testing.T) {
	f := newFixture(t)
	counting := &countingBuilder{inner: testBuilder}
	f.withBuilder(counting)
	v := f.submitted("momentum", sandboxrt.Source{"main.go": []byte(blobSource)})
	out, val, err := f.svc.RunValidation(f.ctx, testDB, v.ID, econ.SystemActor("marketplace.validator"))
	require.NoError(t, err, "a refusal of the strategy is an outcome, not an error")
	assert.Equal(t, StatusRejected, out.Status)
	assert.Contains(t, []string{sandboxrt.BuildFailureOutputLimit, sandboxrt.BuildFailureMemoryLimit}, out.RejectionCode)
	assert.Equal(t, ValidationFailed, val.Outcome)
	assert.Equal(t, out.RejectionCode, val.FailureReason)
	assert.Zero(t, f.count(`SELECT count(*) FROM strategy_artifacts a JOIN strategy_artifact_sources s ON s.artifact_digest = a.artifact_digest
		WHERE s.source_digest = $1`, v.SourceDigest), "nothing of it became an artifact")

	_, _, err = f.svc.RunValidation(f.ctx, testDB, v.ID, econ.SystemActor("marketplace.validator"))
	assert.Equal(t, errs.CodeInvalidStateTransition, errs.CodeOf(err), "a rejected version is not built again")
	assert.Equal(t, 1, counting.count())

	// The transactional form of validation builds nothing at all.
	other := f.submitted("momentum", nil)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.Validate(ctx, tx, other.ID, econ.SystemActor("marketplace.validator"))
		return err
	})
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	assert.Equal(t, 1, counting.count())
	back, err := f.svc.Version(f.ctx, testDB, other.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusSubmitted, back.Status)
}

// TestIntegration_ABuildRunsBetweenTransactionsUnderOneClaim is review-2
// F12's transaction shape: while a build runs, no transaction holds the
// version (another may lock it at once), a second validation is refused
// rather than building again, a build that could not run releases its claim,
// and a claim whose builder vanished lapses so the version is never wedged.
func TestIntegration_ABuildRunsBetweenTransactionsUnderOneClaim(t *testing.T) {
	f := newFixture(t)
	gate := &countingBuilder{inner: testBuilder, started: make(chan struct{}, 1), release: make(chan struct{})}
	f.withBuilder(gate)
	v := f.submitted("momentum", nil)
	done := make(chan error, 1)
	go func() {
		_, _, err := f.svc.RunValidation(f.ctx, testDB, v.ID, econ.SystemActor("validator-1"))
		done <- err
	}()
	<-gate.started
	// The build is running: the version's row is free ...
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT id FROM marketplace_listing_versions WHERE id = $1 FOR UPDATE NOWAIT`, v.ID)
		return err
	}), "no transaction holds the version while it builds")
	// ... and a second validation is refused, not run.
	_, _, err := f.svc.RunValidation(f.ctx, testDB, v.ID, econ.SystemActor("validator-2"))
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err))
	close(gate.release)
	require.NoError(t, <-done)
	back, err := f.svc.Version(f.ctx, testDB, v.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusValidated, back.Status)
	assert.Equal(t, 1, gate.count(), "one build for one version")

	// A build that could not run releases its claim at once.
	broken := &countingBuilder{inner: testBuilder, fail: errs.New(errs.CodeUnsupported, "no toolchain today")}
	f.withBuilder(broken)
	w := f.submitted("momentum", nil)
	_, _, err = f.svc.RunValidation(f.ctx, testDB, w.ID, econ.SystemActor("validator"))
	assert.Equal(t, errs.CodeUnsupported, errs.CodeOf(err))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM marketplace_validation_claim_releases r JOIN marketplace_validation_claims c
		ON c.id = r.claim_id WHERE c.version_id = $1`, w.ID))
	f.withBuilder(testBuilder)
	out, _, err := f.svc.RunValidation(f.ctx, testDB, w.ID, econ.SystemActor("validator"))
	require.NoError(t, err)
	assert.Equal(t, StatusValidated, out.Status, "the version was claimed again and built")

	// A claim whose builder vanished lapses with its lease.
	x := f.submitted("momentum", nil)
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.claimValidation(ctx, tx, x.ID, econ.SystemActor("vanished"))
		return err
	}))
	_, _, err = f.svc.RunValidation(f.ctx, testDB, x.ID, econ.SystemActor("validator"))
	assert.Equal(t, errs.CodeConflict, errs.CodeOf(err), "the lease still holds")
	f.clk.Advance(16 * time.Minute)
	out, _, err = f.svc.RunValidation(f.ctx, testDB, x.ID, econ.SystemActor("validator"))
	require.NoError(t, err)
	assert.Equal(t, StatusValidated, out.Status)
}

// TestIntegration_TheDatabaseHoldsTheBuildClaimAndTheBuildLog goes around
// Go: a validation outside a claim, a second live claim, and a build log that
// is not the one its digest names are refused.
func TestIntegration_TheDatabaseHoldsTheBuildClaimAndTheBuildLog(t *testing.T) {
	f := newFixture(t)
	v := f.submitted("momentum", nil)
	claim := func(at time.Time) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO marketplace_validation_claims (id, version_id, claimed_by_actor_type, claimed_by_actor_id,
				claimed_at, lease_until) VALUES ($1,$2,'SYSTEM','x',$3,$4)`, uuid.New(), v.ID, at, at.Add(time.Minute))
			return err
		})
	}
	now := f.clk.Now()
	require.NoError(t, claim(now))
	err := claim(now.Add(time.Second))
	require.Error(t, err)
	assert.Equal(t, "MK015", db.SQLState(err), "one live claim per version")
	validation := func(claimID any, log []byte, digest any) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO marketplace_version_validations (id, version_id, outcome, failure_reason, scan_codes,
				build_log_digest, toolchain_version, sdk_version, runtime_version, validated_by_actor_type, validated_by_actor_id, validated_at,
				claim_id, build_log) VALUES ($1,$2,'FAILED','COMPILE_FAILED','{}',$3,'go','sdk','rt','SYSTEM','x',now(),$4,$5)`,
				uuid.New(), v.ID, digest, claimID, log)
			return err
		})
	}
	err = validation(nil, []byte{}, nil)
	require.Error(t, err)
	assert.Equal(t, "MK015", db.SQLState(err), "a validation outside a claim")
	var claimID uuid.UUID
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT id FROM marketplace_validation_claims WHERE version_id = $1`, v.ID).Scan(&claimID))
	sum := sha256.Sum256([]byte("what the compiler said"))
	err = validation(claimID, []byte("something else"), sum[:])
	require.Error(t, err)
	assert.Contains(t, err.Error(), "marketplace_version_validations_build_log_is_digested")
	require.NoError(t, validation(claimID, []byte("what the compiler said"), sum[:]))
}

// TestIntegration_AnOperatorCannotReclearTheirOwnVersion is review-2 F14: an
// operator who created a strategy may neither take it into review, approve
// it, nor moderate it -- re-clearing it after another operator flagged it
// included -- in Go and in SQL.
func TestIntegration_AnOperatorCannotReclearTheirOwnVersion(t *testing.T) {
	f := newFixture(t)
	// An operator who owns a customer account publishes from it.
	opAccount, opUser := f.account()
	operatorCreator := econ.Actor{Type: security.ActorOperator, ID: opUser.ID}
	var v Version
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, v, err = f.svc.CreateListing(ctx, tx, ListingRequest{CreatorAccountID: opAccount, Creator: operatorCreator, Title: "the operator's own",
			Version: f.versionRequest("momentum")})
		return err
	}))
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Submit(ctx, tx, v.ID, operatorCreator)
	})
	f.validate(v.ID)
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.StartReview(ctx, tx, v.ID, operatorCreator)
		return err
	})
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err))
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.StartReview(ctx, tx, v.ID, f.operator)
	})
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Approve(ctx, tx, v.ID, f.operator, "MEETS_POLICY", "")
	})
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Moderate(ctx, tx, v.ID, f.operator2, ModerationFlagged, "reported by a customer")
	})
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Moderate(ctx, tx, v.ID, operatorCreator, ModerationCleared, "looks fine to me")
		return err
	})
	require.Error(t, err)
	assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), "the creator cannot re-clear their own flagged version")
	back, err := f.svc.Version(f.ctx, testDB, v.ID)
	require.NoError(t, err)
	assert.Equal(t, ModerationFlagged, back.Moderation)

	// Around Go: the moderation row and the review transition of the creator.
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO marketplace_moderation_transitions (id, version_id, from_state, to_state, actor_type, actor_id, reason)
			VALUES ($1,$2,'FLAGGED','CLEARED','OPERATOR',$3,'self')`, uuid.New(), v.ID, operatorCreator.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE marketplace_listing_versions SET moderation_state = 'CLEARED' WHERE id = $1`, v.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK011", db.SQLState(err))
	w := f.submitted("momentum", nil)
	f.validate(w.ID)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO marketplace_version_transitions (id, version_id, from_status, to_status, actor_type, actor_id, reason)
			VALUES ($1,$2,'VALIDATED','IN_REVIEW','OPERATOR',$3,'self')`, uuid.New(), w.ID, f.creator.ID)
		return err
	})
	require.Error(t, err)
	assert.Equal(t, "MK011", db.SQLState(err), "the creator does not take their version into review")
}
