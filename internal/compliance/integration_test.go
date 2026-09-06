//go:build integration

package compliance_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/compliance"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

func openDB(t *testing.T) *db.DB {
	t.Helper()
	url := os.Getenv("CP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	d, err := db.Open(context.Background(), db.Config{URL: url, MaxConns: 4, MinConns: 1, AppName: "compliance-test"})
	require.NoError(t, err)
	t.Cleanup(d.Close)
	return d
}

func TestIntegration_Compliance_UpsertAuditsAndRejectsSelfAttestation(t *testing.T) {
	d := openDB(t)
	ctx := context.Background()
	u, err := accounts.NewRepository().CreateUser(ctx, d.Pool(), "https://idp.test", "sub-"+id.New[id.Any]().String(), nil)
	require.NoError(t, err)
	writer := audit.NewWriter()
	repo := compliance.NewRepository(writer)

	_, err = repo.Get(ctx, d.Pool(), u.ID)
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err), "missing profile is NOT_FOUND (eligibility fails closed)")

	now := time.Now().UTC().Truncate(time.Microsecond)
	var p compliance.Profile
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = repo.Upsert(ctx, tx, compliance.Profile{
			UserID: u.ID, IdentityState: compliance.IdentityVerified, AgeVerified: true, JurisdictionCountry: "US", JurisdictionRegion: "CA",
			ResidencyCountry: "US", SanctionsState: compliance.SanctionsClear, Provider: "kyc-provider", ProviderRef: "ver_123",
			PolicyVersion: "kyc-v1", Restrictions: []string{"no_leverage"}, VerifiedAt: &now,
		}, compliance.Change{ActorType: security.ActorSystem, ActorID: "kyc-webhook", Reason: "provider verification completed", CorrelationID: "corr-1"})
		return err
	}))
	assert.Equal(t, compliance.IdentityVerified, p.IdentityState)
	assert.Equal(t, []string{"no_leverage"}, p.Restrictions)

	// Update by an operator; audit trail grows with before/after hashes.
	require.NoError(t, d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
		p.SanctionsState = compliance.SanctionsReview
		_, err := repo.Upsert(ctx, tx, p, compliance.Change{ActorType: security.ActorOperator, ActorID: "op-1", Reason: "manual review opened"})
		return err
	}))
	got, err := repo.Get(ctx, d.Pool(), u.ID)
	require.NoError(t, err)
	assert.Equal(t, compliance.SanctionsReview, got.SanctionsState)
	var n int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'compliance_profile' AND resource_id = $1`, u.ID.String()).Scan(&n))
	assert.Equal(t, 2, n)
	var withBefore int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'compliance_profile' AND resource_id = $1 AND before_hash IS NOT NULL`, u.ID.String()).Scan(&withBefore))
	assert.Equal(t, 1, withBefore, "second write carries the before hash")

	// Customers and agents cannot self-attest.
	for _, actor := range []security.ActorType{security.ActorUser, security.ActorAgent, security.ActorService} {
		err := d.InTx(ctx, db.TxOptions{}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := repo.Upsert(ctx, tx, p, compliance.Change{ActorType: actor, ActorID: "x", Reason: "self"})
			return err
		})
		assert.Equal(t, errs.CodeForbidden, errs.CodeOf(err), string(actor))
	}

	// Validation fails closed on unknown enums and bad country codes.
	bad := p
	bad.IdentityState = "MAYBE"
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()))
	bad = p
	bad.JurisdictionCountry = "usa"
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()))
	bad = p
	bad.VerifiedAt = nil
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(bad.Validate()))
}
