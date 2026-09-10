package proof

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/proof/prooftest"
)

var _ Archive = (*prooftest.MemArchive)(nil)

func TestDirArchive_PutGetWriteOnce(t *testing.T) {
	ctx := context.Background()
	a, err := NewDirArchive(config.EnvTest, filepath.Join(t.TempDir(), "audit"))
	require.NoError(t, err)
	body := []byte(`{"checkpoint":1}`)
	retention := 24 * time.Hour
	uri, sum, err := a.Put(ctx, "audit-checkpoints/000000000001/x.json", body, &retention)
	require.NoError(t, err)
	want := sha256.Sum256(body)
	assert.Equal(t, want[:], sum)
	assert.True(t, strings.HasPrefix(uri, "file:///"), uri)

	got, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, body, got)

	_, _, err = a.Put(ctx, "audit-checkpoints/000000000001/x.json", []byte("other"), nil)
	assert.ErrorIs(t, err, ErrObjectExists, "write-once")
	again, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, body, again)

	_, err = a.Get(ctx, strings.Replace(uri, "x.json", "missing.json", 1))
	assert.ErrorIs(t, err, ErrObjectNotFound)
	_, err = a.Get(ctx, "s3://bucket/key")
	assert.Error(t, err)
	_, err = a.Get(ctx, "file:///"+filepath.ToSlash(filepath.Join(os.TempDir(), "outside.json")))
	assert.Error(t, err, "outside the root")
}

func TestDirArchive_RejectsEscapingKeys(t *testing.T) {
	a, err := NewDirArchive(config.EnvLocal, t.TempDir())
	require.NoError(t, err)
	for _, key := range []string{"", "/", "../escape.json", `..\escape.json`, "a/../../escape.json", "a/b/../c.json"} {
		_, _, err := a.Put(context.Background(), key, []byte("x"), nil)
		assert.Error(t, err, "key %q", key)
	}
	entries, err := os.ReadDir(a.Root())
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was written")
}

func TestDirArchive_RefusedInProductionLikeEnvironments(t *testing.T) {
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		a, err := NewDirArchive(env, t.TempDir())
		assert.Nil(t, a)
		assert.Error(t, err, env)
	}
}

func TestMemArchive_Behaves(t *testing.T) {
	ctx := context.Background()
	a := prooftest.NewMemArchive()
	body := []byte("hello")
	retention := time.Hour
	uri, sum, err := a.Put(ctx, "k/1", body, &retention)
	require.NoError(t, err)
	want := sha256.Sum256(body)
	assert.Equal(t, want[:], sum)
	got, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.Equal(t, body, got)
	_, _, err = a.Put(ctx, "k/1", body, nil)
	assert.Error(t, err, "write-once")
	r, ok := a.Retention(uri)
	require.True(t, ok)
	require.NotNil(t, r)
	assert.Equal(t, time.Hour, *r)
	assert.True(t, a.Tamper(uri, func(b []byte) []byte { b[0] ^= 1; return b }))
	tampered, err := a.Get(ctx, uri)
	require.NoError(t, err)
	assert.NotEqual(t, body, tampered)
	assert.Equal(t, 1, a.Puts())
	assert.Equal(t, []string{uri}, a.URIs())
	a.Delete(uri)
	_, err = a.Get(ctx, uri)
	assert.Error(t, err)
}

// TestArchivesTellARetryFromAnOverwrite.
//
// Refusing to overwrite is the whole point of an evidence archive. But two
// very different things reach that refusal, and collapsing them cost a
// deployment its webhook pipeline: identical bytes under an existing key are a
// retry of a Put that already succeeded, and different bytes are an attempt to
// rewrite evidence. The first is routine -- a provider that delivers at least
// once will produce it -- and the second is worth waking someone for.
//
// Every implementation must distinguish them the same way, because which one a
// caller gets must not depend on which archive the deployment wired.
func TestArchivesTellARetryFromAnOverwrite(t *testing.T) {
	ctx := context.Background()
	body := []byte(`{"delivery":"one"}`)
	other := []byte(`{"delivery":"rewritten"}`)

	t.Run("DirArchive", func(t *testing.T) {
		a, err := NewDirArchive(config.EnvLocal, t.TempDir())
		require.NoError(t, err)

		uri, sum, err := a.Put(ctx, "webhooks/stripe/evt_1/abcd.json", body, nil)
		require.NoError(t, err)

		gotURI, gotSum, err := a.Put(ctx, "webhooks/stripe/evt_1/abcd.json", body, nil)
		assert.ErrorIs(t, err, ErrObjectExistsIdentical, "a replay of the same bytes")
		assert.ErrorIs(t, err, ErrObjectExists, "and it is still a refusal")
		assert.Equal(t, uri, gotURI, "the refusal must name the object that is already there")
		assert.Equal(t, sum, gotSum)

		_, _, err = a.Put(ctx, "webhooks/stripe/evt_1/abcd.json", other, nil)
		assert.ErrorIs(t, err, ErrObjectExists)
		assert.NotErrorIs(t, err, ErrObjectExistsIdentical, "different bytes are not a replay")

		// And through all of that, nothing was overwritten.
		stored, err := a.Get(ctx, uri)
		require.NoError(t, err)
		assert.Equal(t, body, stored)
	})

	t.Run("MemArchive", func(t *testing.T) {
		// The fake has its own sentinels -- it cannot import this package,
		// because this package's tests import it -- so this is what keeps the
		// two sets in step.
		a := prooftest.NewMemArchive()

		uri, sum, err := a.Put(ctx, "k/1", body, nil)
		require.NoError(t, err)

		gotURI, gotSum, err := a.Put(ctx, "k/1", body, nil)
		assert.ErrorIs(t, err, prooftest.ErrObjectExistsIdentical)
		assert.ErrorIs(t, err, prooftest.ErrObjectExists)
		assert.Equal(t, uri, gotURI)
		assert.Equal(t, sum, gotSum)

		_, _, err = a.Put(ctx, "k/1", other, nil)
		assert.ErrorIs(t, err, prooftest.ErrObjectExists)
		assert.NotErrorIs(t, err, prooftest.ErrObjectExistsIdentical)

		stored, err := a.Get(ctx, uri)
		require.NoError(t, err)
		assert.Equal(t, body, stored)
	})
}
