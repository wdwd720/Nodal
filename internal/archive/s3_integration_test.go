//go:build integration

package archive_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
)

// The suite runs against the LOCAL MinIO (docker-compose) or any
// S3-compatible endpoint named by CP_TEST_ARCHIVE_ENDPOINT. Every run writes
// under a unique key prefix so parallel runs never collide. Objects written
// to the audit bucket stay locked for the retention they were given.
func newTestS3(t *testing.T) (*archive.S3, string, string) {
	t.Helper()
	endpoint := os.Getenv("CP_TEST_ARCHIVE_ENDPOINT")
	if endpoint == "" {
		t.Skip("CP_TEST_ARCHIVE_ENDPOINT not set; skipping MinIO integration test")
	}
	cfg := config.ArchiveConfig{
		Endpoint:       endpoint,
		Region:         envOr("CP_TEST_ARCHIVE_REGION", "us-east-1"),
		RawBucket:      envOr("CP_TEST_ARCHIVE_RAW_BUCKET", "raw-events"),
		AuditBucket:    envOr("CP_TEST_ARCHIVE_AUDIT_BUCKET", "audit-evidence"),
		ForcePathStyle: true,
		AccessKeyRef:   config.SecretRef(envOr("CP_TEST_ARCHIVE_ACCESS_KEY", "cp_minio")),
		SecretKeyRef:   config.SecretRef(envOr("CP_TEST_ARCHIVE_SECRET_KEY", "cp_minio_local")),
	}
	s, err := archive.NewS3(context.Background(), cfg, config.NewResolver(config.EnvTest, os.LookupEnv), archive.S3Options{OperationTimeout: 20 * time.Second})
	require.NoError(t, err)
	return s, cfg.RawBucket, cfg.AuditBucket
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type testRunKind struct{}

func uniquePrefix() string { return "test/" + id.New[testRunKind]().String() }

func TestIntegration_S3_PutGetHeadListRoundTrip(t *testing.T) {
	s, raw, _ := newTestS3(t)
	ctx := context.Background()
	prefix := uniquePrefix()
	body := []byte(`{"signature":"abc","amount":"123456789012345678901234567890"}`)
	p := provenance()
	key, err := archive.Layout{Prefix: prefix}.Key(p)
	require.NoError(t, err)
	meta := archive.Layout{}.Metadata(p, archive.SHA256(body))

	ref, err := s.Put(ctx, archive.PutRequest{Bucket: raw, Key: key, Body: body, ContentType: "application/json", Metadata: meta})
	require.NoError(t, err)
	require.Equal(t, archive.SHA256(body), ref.SHA256)
	require.Equal(t, int64(len(body)), ref.Size)
	require.NotEmpty(t, ref.ETag)

	loc, err := archive.ParseURI(ref.URI)
	require.NoError(t, err)
	obj, err := s.Get(ctx, loc)
	require.NoError(t, err)
	require.Equal(t, body, obj.Body)
	require.Equal(t, "application/json", obj.ContentType)
	require.Equal(t, ref.HexSHA256(), obj.Metadata[archive.MetaSHA256], "sha256 metadata round-trips")
	require.Equal(t, "helius", obj.Metadata[archive.MetaProvider])
	require.Equal(t, "5VfYd8sig", obj.Metadata[archive.MetaSourceEventID])
	require.Equal(t, "1", obj.Metadata[archive.MetaSchemaVersion])
	require.Equal(t, fixedReceived.Format(time.RFC3339Nano), obj.Metadata[archive.MetaPlatformReceivedAt])

	head, err := s.Head(ctx, loc)
	require.NoError(t, err)
	require.Equal(t, ref.SHA256, head.Ref.SHA256)
	require.Equal(t, int64(len(body)), head.Ref.Size)
	require.Nil(t, head.Retention)

	list, err := s.List(ctx, archive.ListRequest{Bucket: raw, Prefix: prefix + "/"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, key, list[0].Key)
	require.Equal(t, int64(len(body)), list[0].Size)

	_, err = s.Get(ctx, archive.Locator{Bucket: raw, Key: prefix + "/missing.json"})
	require.ErrorIs(t, err, archive.ErrNotFound)
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
	_, err = s.Head(ctx, archive.Locator{Bucket: raw, Key: prefix + "/missing.json"})
	require.ErrorIs(t, err, archive.ErrNotFound)

	require.NoError(t, s.Delete(ctx, loc), "raw bucket objects can be deleted")
}

func TestIntegration_S3_AuditBucketObjectLockRefusesEarlyDeletion(t *testing.T) {
	s, _, audit := newTestS3(t)
	ctx := context.Background()
	locked, err := s.ObjectLockEnabled(ctx, audit)
	require.NoError(t, err)
	require.True(t, locked, "audit bucket must have object lock (docker-compose minio-init)")

	key := uniquePrefix() + "/audit-object.json"
	until := time.Now().UTC().Add(10 * time.Minute)
	ref, err := s.Put(ctx, archive.PutRequest{
		Bucket: audit, Key: key, Body: []byte(`{"chain":"x"}`), ContentType: "application/json",
		Retention: &archive.Retention{Mode: archive.RetentionCompliance, Until: until},
	})
	require.NoError(t, err)
	require.NotEmpty(t, ref.VersionID, "object lock buckets are versioned")

	head, err := s.Head(ctx, ref.Locator())
	require.NoError(t, err)
	require.NotNil(t, head.Retention)
	require.Equal(t, archive.RetentionCompliance, head.Retention.Mode)
	require.WithinDuration(t, until, head.Retention.Until, 2*time.Second)

	err = s.Delete(ctx, ref.Locator())
	require.Error(t, err, "deleting a locked version must be refused")
	require.ErrorIs(t, err, archive.ErrRetentionLocked)
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	obj, err := s.Get(ctx, ref.Locator())
	require.NoError(t, err, "the locked version is still readable")
	require.Equal(t, ref.SHA256, obj.Ref.SHA256)
}

func TestIntegration_S3_RetentionOnUnlockedBucketIsRejected(t *testing.T) {
	s, raw, _ := newTestS3(t)
	_, err := s.Put(context.Background(), archive.PutRequest{
		Bucket: raw, Key: uniquePrefix() + "/x.json", Body: []byte("x"),
		Retention: &archive.Retention{Mode: archive.RetentionCompliance, Until: time.Now().Add(time.Hour)},
	})
	require.Error(t, err)
}
