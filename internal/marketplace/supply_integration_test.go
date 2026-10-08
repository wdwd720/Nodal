//go:build integration

package marketplace

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/econ/econtest"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// approvedWith takes a strategy whose version request mutate adjusts from
// source to an APPROVED, CLEARED version.
func (f *fixture) approvedWith(strategy string, mutate func(*VersionRequest)) Version {
	f.t.Helper()
	req := f.versionRequest(strategy)
	mutate(&req)
	var v Version
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, v, err = f.svc.CreateListing(ctx, tx, ListingRequest{CreatorAccountID: f.creatorAccount, Creator: f.creator,
			Title: "Listing " + strategy + " " + uuid.NewString()[:8], Version: req})
		return err
	}))
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.Submit(ctx, tx, v.ID, f.creator) })
	f.validate(v.ID)
	f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) { return f.svc.StartReview(ctx, tx, v.ID, f.operator) })
	v = f.step(func(ctx context.Context, tx pgx.Tx) (Version, error) {
		return f.svc.Approve(ctx, tx, v.ID, f.operator, "MEETS_POLICY", "looked at it")
	})
	require.Equal(f.t, StatusApproved, v.Status)
	return v
}

// TestIntegration_NoFollowTradesAnInstrumentNamingASupplyControlledAsset
// (migration 00763): a follow's dispatches are paper-venue fills, whose side
// is external and never booked, so a fill of an instrument quoted in a
// platform stable asset would create or destroy units of it (SU002), and the
// venue refuses it -- after a follow had written its sandbox run, intents and
// decisions. The marketplace refuses, before anything is written, an install
// of a version naming such an instrument, and a follow of an install that
// exists anyway.
func TestIntegration_NoFollowTradesAnInstrumentNamingASupplyControlledAsset(t *testing.T) {
	f := newFixture(t)
	var s econtest.Supply
	var quotedInStable string
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if s, err = econtest.RegisterSupply(ctx, tx); err != nil {
			return err
		}
		sym := "MKTS" + uuid.NewString()[:8]
		base, err := assets.NewRepository().Create(ctx, tx, assets.Asset{Chain: assets.SandboxChain, Kind: assets.KindSimulated,
			ValueDomain: valuedomain.Simulated, Symbol: sym, Name: sym + " (sandbox)", Decimals: 6, RiskClass: assets.RiskSettlement,
			Status: assets.StatusActive})
		if err != nil {
			return err
		}
		ins, err := instruments.NewRepository().CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: base.ID, Quote: s.Stable.Asset.ID,
			Settlement: s.Stable.Asset.ID, CanonicalName: sym + "/" + s.Stable.Asset.Symbol, RiskClass: assets.RiskMajor,
			Status: assets.StatusActive, ActiveFrom: f.clk.Now().Add(-time.Hour)})
		quotedInStable = ins.ID.String()
		return err
	}))
	v := f.approvedWith("momentum", func(r *VersionRequest) {
		r.SupportedInstrumentIDs = append(r.SupportedInstrumentIDs, quotedInStable)
	})

	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.InstallVersion(ctx, tx, InstallRequest{VersionID: v.ID, AccountID: f.follower, Mode: gates.ModePaper,
			Actor: f.followerActor})
		return err
	})
	econtest.RequireRefusal(t, err, s.Stable.Asset.ID, econ.ControllerStableIssuer)
	assert.Zero(t, f.count(`SELECT count(*) FROM marketplace_installs WHERE account_id = $1 AND version_id = $2`, f.follower, v.ID),
		"no install was written")

	// An install made around InstallVersion -- before this check, say -- is
	// refused when it is followed.
	iid := id.New[installKind]()
	_, err = testDB.Pool().Exec(f.ctx, `INSERT INTO marketplace_installs (id, account_id, version_id, mode, status,
		installed_by_actor_type, installed_by_actor_id) VALUES ($1,$2,$3,'PAPER','ACTIVE','USER',$4)`,
		iid, f.follower, v.ID, f.followerActor.ID)
	require.NoError(t, err)
	runsBefore := f.count(`SELECT count(*) FROM sandbox_runs`)
	_, err = f.follow(f.svc, Install{ID: iid})
	econtest.RequireRefusal(t, err, s.Stable.Asset.ID, econ.ControllerStableIssuer)
	assert.Zero(t, f.count(`SELECT count(*) FROM marketplace_follow_runs WHERE install_id = $1`, iid), "no follow run was written")
	assert.Zero(t, f.count(`SELECT count(*) FROM economic_intents WHERE account_id = $1`, f.follower), "no intent was written")
	assert.Equal(t, runsBefore, f.count(`SELECT count(*) FROM sandbox_runs`), "the strategy never ran")
}
