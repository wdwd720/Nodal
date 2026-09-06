package reality_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/reality"
)

func TestRetentionPolicy_MapsEveryClassFromConfig(t *testing.T) {
	t.Parallel()
	cfg := config.RetentionConfig{FinancialRecordDays: 2555, SecurityAuditDays: 2555, RawMarketDataDays: 90, SocialDataDays: 30, ModelIODays: 60, OperationalLogDays: 14}
	p, err := reality.NewRetentionPolicy(cfg)
	require.NoError(t, err)
	want := map[reality.RetentionClass]int{
		reality.RetentionFinancialRecord: 2555, reality.RetentionSecurityAudit: 2555, reality.RetentionRawMarketData: 90,
		reality.RetentionSocialData: 30, reality.RetentionModelIO: 60, reality.RetentionOperationalLog: 14,
	}
	require.Len(t, reality.RetentionClasses(), 6)
	for _, c := range reality.RetentionClasses() {
		d, err := p.Days(c)
		require.NoError(t, err)
		require.Equal(t, want[c], d, string(c))
		until, err := p.Until(c, fixedNow)
		require.NoError(t, err)
		require.Equal(t, fixedNow.AddDate(0, 0, want[c]), until)
		parsed, err := reality.ParseRetentionClass(string(c))
		require.NoError(t, err)
		require.Equal(t, c, parsed)
	}
	require.True(t, reality.RetentionFinancialRecord.LockRequired())
	require.True(t, reality.RetentionSecurityAudit.LockRequired())
	require.False(t, reality.RetentionRawMarketData.LockRequired())
	_, err = reality.ParseRetentionClass("FOREVER")
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = p.Days("FOREVER")
	require.Error(t, err)
}

func TestRetentionPolicy_UnconfiguredClassNeverInventsRetention(t *testing.T) {
	t.Parallel()
	p, err := reality.NewRetentionPolicy(config.RetentionConfig{RawMarketDataDays: 90})
	require.NoError(t, err)
	_, err = p.Until(reality.RetentionSecurityAudit, fixedNow)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	_, err = reality.NewRetentionPolicy(config.RetentionConfig{SocialDataDays: -1})
	require.Error(t, err)
	_, err = reality.RetentionUntil(0, fixedNow)
	require.Error(t, err)
}

func TestBucketFor_ClassesMapToBucketsAndLocks(t *testing.T) {
	t.Parallel()
	a := config.ArchiveConfig{RawBucket: "raw-events", EvidenceBucket: "provider-evidence", AuditBucket: "audit-evidence"}
	for _, tc := range []struct {
		class  reality.RetentionClass
		bucket string
		locked bool
	}{
		{reality.RetentionFinancialRecord, "audit-evidence", true},
		{reality.RetentionSecurityAudit, "audit-evidence", true},
		{reality.RetentionRawMarketData, "raw-events", false},
		{reality.RetentionSocialData, "raw-events", false},
		{reality.RetentionModelIO, "provider-evidence", false},
		{reality.RetentionOperationalLog, "provider-evidence", false},
	} {
		bucket, locked, err := reality.BucketFor(tc.class, a)
		require.NoError(t, err, string(tc.class))
		require.Equal(t, tc.bucket, bucket)
		require.Equal(t, tc.locked, locked)
		r := reality.RetentionFor(tc.class, fixedNow.Add(time.Hour))
		if tc.locked {
			require.NotNil(t, r)
			require.Equal(t, archive.RetentionCompliance, r.Mode)
		} else {
			require.Nil(t, r)
		}
	}
	_, _, err := reality.BucketFor(reality.RetentionSecurityAudit, config.ArchiveConfig{RawBucket: "raw-events"})
	require.Error(t, err, "no audit bucket configured")
	_, _, err = reality.BucketFor("FOREVER", a)
	require.Error(t, err)
}

// PART 122: production cannot set critical audit retention to zero. The
// rule lives in config.Validate; this test proves it fires for a PROD
// config and stays quiet outside production.
func TestRetention_ProductionRefusesZeroAuditRetention(t *testing.T) {
	t.Parallel()
	load := func(days string) *config.Config {
		cfg, err := config.Load(context.Background(), config.LookupFromMap(map[string]string{
			"CP_ENV": "TEST", "CP_RETENTION_SECURITY_AUDIT_DAYS": days, "CP_RETENTION_FINANCIAL_RECORD_DAYS": days,
		}))
		require.NoError(t, err, "TEST accepts zero: the rule is production-only")
		return cfg
	}
	zero := load("0")
	require.NoError(t, zero.Validate())
	prod := zero.Clone()
	prod.Env = config.EnvProd
	err := prod.Validate()
	require.Error(t, err)
	require.True(t, config.HasViolation(err, config.RuleRetentionNonZero), "PROD must refuse zero audit retention: %v", err)
	names := map[string]bool{}
	for _, v := range config.Violations(err) {
		if v.Rule == config.RuleRetentionNonZero {
			names[v.Field] = true
		}
	}
	require.True(t, names["Retention.SecurityAuditDays"])
	require.True(t, names["Retention.FinancialRecordDays"])

	nonZero := load("2555").Clone()
	nonZero.Env = config.EnvProd
	require.False(t, config.HasViolation(nonZero.Validate(), config.RuleRetentionNonZero), "non-zero audit retention passes the rule")

	// And the engine cannot compute a retention for a zero-day class.
	p, err := reality.NewRetentionPolicy(zero.Retention)
	require.NoError(t, err)
	_, err = p.Until(reality.RetentionSecurityAudit, fixedNow)
	require.Error(t, err)
}
