package archive_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/archive/archivetest"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

func TestMemory_PutGetHeadRoundTripAndHash(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(fixedReceived)
	m := archivetest.New(clk)
	ctx := context.Background()
	body := []byte(`{"hello":"world"}`)
	ref, err := m.Put(ctx, archive.PutRequest{Bucket: "raw-events", Key: "raw/a/b.json", Body: body, ContentType: "application/json", Metadata: map[string]string{archive.MetaProvider: "helius"}})
	require.NoError(t, err)
	require.Equal(t, archive.SHA256(body), ref.SHA256)
	require.Equal(t, "s3://raw-events/raw/a/b.json?versionId=v1", ref.URI)
	require.Equal(t, int64(len(body)), ref.Size)
	require.Equal(t, fixedReceived, ref.StoredAt)

	obj, err := m.Get(ctx, ref.Locator())
	require.NoError(t, err)
	require.Equal(t, body, obj.Body)
	require.Equal(t, "application/json", obj.ContentType)
	require.Equal(t, ref.HexSHA256(), obj.Metadata[archive.MetaSHA256])
	require.Equal(t, "helius", obj.Metadata[archive.MetaProvider])

	head, err := m.Head(ctx, archive.Locator{Bucket: "raw-events", Key: "raw/a/b.json"})
	require.NoError(t, err)
	require.Equal(t, ref.SHA256, head.Ref.SHA256)
	require.Nil(t, head.Retention)

	list, err := m.List(ctx, archive.ListRequest{Bucket: "raw-events", Prefix: "raw/a/"})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "raw/a/b.json", list[0].Key)

	_, err = m.Get(ctx, archive.Locator{Bucket: "raw-events", Key: "missing"})
	require.ErrorIs(t, err, archive.ErrNotFound)
	require.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
}

func TestMemory_CorruptionIsDetectedOnGet(t *testing.T) {
	t.Parallel()
	m := archivetest.New(clock.NewFake(fixedReceived))
	ctx := context.Background()
	ref, err := m.Put(ctx, archive.PutRequest{Bucket: "raw-events", Key: "k", Body: []byte("payload")})
	require.NoError(t, err)
	require.True(t, m.Corrupt(ref.Locator()))
	_, err = m.Get(ctx, ref.Locator())
	require.ErrorIs(t, err, archive.ErrIntegrity)
	require.Equal(t, errs.CodeArchiveIntegrityViolation, errs.CodeOf(err))
}

func TestMemory_RetentionRefusesEarlyDeletion(t *testing.T) {
	t.Parallel()
	clk := clock.NewFake(fixedReceived)
	m := archivetest.New(clk).EnableObjectLock("audit-evidence")
	ctx := context.Background()
	until := fixedReceived.Add(time.Hour)
	ref, err := m.Put(ctx, archive.PutRequest{Bucket: "audit-evidence", Key: "k", Body: []byte("x"), Retention: &archive.Retention{Mode: archive.RetentionCompliance, Until: until}})
	require.NoError(t, err)
	head, err := m.Head(ctx, ref.Locator())
	require.NoError(t, err)
	require.NotNil(t, head.Retention)
	require.Equal(t, until, head.Retention.Until)

	err = m.Delete(ctx, ref.Locator())
	require.ErrorIs(t, err, archive.ErrRetentionLocked)
	require.Equal(t, errs.CodeForbidden, errs.CodeOf(err))

	clk.Advance(2 * time.Hour)
	require.NoError(t, m.Delete(ctx, ref.Locator()))
	_, err = m.Get(ctx, ref.Locator())
	require.ErrorIs(t, err, archive.ErrNotFound)

	_, err = m.Put(ctx, archive.PutRequest{Bucket: "raw-events", Key: "k", Body: []byte("x"), Retention: &archive.Retention{Mode: archive.RetentionCompliance, Until: clk.Now().Add(time.Hour)}})
	require.ErrorIs(t, err, archive.ErrObjectLockNotEnabled)
}

func TestMemory_VersionsAreIndependent(t *testing.T) {
	t.Parallel()
	m := archivetest.New(clock.NewFake(fixedReceived))
	ctx := context.Background()
	r1, err := m.Put(ctx, archive.PutRequest{Bucket: "raw-events", Key: "k", Body: []byte("one")})
	require.NoError(t, err)
	r2, err := m.Put(ctx, archive.PutRequest{Bucket: "raw-events", Key: "k", Body: []byte("two")})
	require.NoError(t, err)
	require.NotEqual(t, r1.VersionID, r2.VersionID)
	o1, err := m.Get(ctx, r1.Locator())
	require.NoError(t, err)
	require.Equal(t, "one", string(o1.Body))
	latest, err := m.Get(ctx, archive.Locator{Bucket: "raw-events", Key: "k"})
	require.NoError(t, err)
	require.Equal(t, "two", string(latest.Body))
	require.Equal(t, 2, m.Len())
}

func TestMemory_FailPutAndCounts(t *testing.T) {
	t.Parallel()
	m := archivetest.New(nil)
	injected := errors.New("boom")
	m.FailPut(injected)
	_, err := m.Put(context.Background(), archive.PutRequest{Bucket: "raw-events", Key: "k", Body: []byte("x")})
	require.ErrorIs(t, err, injected)
	m.FailPut(nil)
	_, err = m.Put(context.Background(), archive.PutRequest{Bucket: "raw-events", Key: "k", Body: []byte("x")})
	require.NoError(t, err)
	puts, _, _ := m.Counts()
	require.Equal(t, 2, puts)
}
