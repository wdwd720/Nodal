package archive_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/config"
)

func TestNewS3_ConstructorRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolver := config.NewResolver(config.EnvTest, func(string) (string, bool) { return "", false })
	base := config.ArchiveConfig{Endpoint: "http://127.0.0.1:9100", Region: "us-east-1", RawBucket: "raw-events", ForcePathStyle: true}

	t.Run("region required", func(t *testing.T) {
		t.Parallel()
		cfg := base
		cfg.Region = ""
		_, err := archive.NewS3(ctx, cfg, resolver, archive.S3Options{})
		require.Error(t, err)
	})
	t.Run("static keys must be set together", func(t *testing.T) {
		t.Parallel()
		cfg := base
		cfg.AccessKeyRef = "cp_minio"
		_, err := archive.NewS3(ctx, cfg, resolver, archive.S3Options{})
		require.Error(t, err)
	})
	t.Run("static keys need a resolver", func(t *testing.T) {
		t.Parallel()
		cfg := base
		cfg.AccessKeyRef, cfg.SecretKeyRef = "cp_minio", "cp_minio_local"
		_, err := archive.NewS3(ctx, cfg, nil, archive.S3Options{})
		require.Error(t, err)
	})
	t.Run("unresolvable secret fails closed", func(t *testing.T) {
		t.Parallel()
		cfg := base
		cfg.AccessKeyRef, cfg.SecretKeyRef = "env://CP_TEST_ARCHIVE_MISSING_KEY", "env://CP_TEST_ARCHIVE_MISSING_SECRET"
		_, err := archive.NewS3(ctx, cfg, resolver, archive.S3Options{})
		require.Error(t, err)
	})
	t.Run("static credentials build without dialing", func(t *testing.T) {
		t.Parallel()
		cfg := base
		cfg.AccessKeyRef, cfg.SecretKeyRef = "cp_minio", "cp_minio_local"
		s, err := archive.NewS3(ctx, cfg, resolver, archive.S3Options{})
		require.NoError(t, err)
		require.NotNil(t, s)
	})
	t.Run("iam role mode builds without static keys", func(t *testing.T) {
		t.Parallel()
		cfg := base
		cfg.Endpoint = ""
		s, err := archive.NewS3(ctx, cfg, nil, archive.S3Options{})
		require.NoError(t, err)
		require.NotNil(t, s)
	})
}
