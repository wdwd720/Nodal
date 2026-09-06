//go:build integration && chaos

package chaos

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func newChaosArchive(t *testing.T, timeout time.Duration) (*archive.S3, string) {
	t.Helper()
	endpoint := os.Getenv("CP_TEST_ARCHIVE_ENDPOINT")
	if endpoint == "" {
		t.Skip("CP_TEST_ARCHIVE_ENDPOINT not set; skipping archive chaos test")
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
	s, err := archive.NewS3(context.Background(), cfg, config.NewResolver(config.EnvTest, os.LookupEnv),
		archive.S3Options{OperationTimeout: timeout})
	require.NoError(t, err)
	return s, cfg.RawBucket
}

// storeEvidence is the shape every caller of the archive has: write the
// object, and only if the store confirms it, record the reference next to the
// row that depends on it. The chaos question is whether a refused or stalled
// write can ever be recorded as stored.
type storeEvidence struct {
	arch      *archive.S3
	bucket    string
	swallow   bool // negative control
	recorded  []string
	lastError error
}

func (s *storeEvidence) put(ctx context.Context, key string, body []byte) (stored bool) {
	ref, err := s.arch.Put(ctx, archive.PutRequest{
		Bucket: s.bucket, Key: key, Body: body, ContentType: "application/json",
	})
	if err != nil {
		s.lastError = err
		if s.swallow {
			// NEGATIVE CONTROL: the caller decides an archive failure is not
			// worth failing the operation over, and records the reference
			// anyway. Evidence is now claimed to exist where none does.
			s.recorded = append(s.recorded, key)
			return true
		}
		return false
	}
	s.recorded = append(s.recorded, ref.URI)
	return true
}

// TestChaos_ArchiveWriteRefusedIsNeverRecordedAsStored asserts "no silent
// divergence" for evidence: when the object store cannot accept a write, the
// system must report a failure, and nothing anywhere may claim the object
// exists.
//
// This matters more than it looks. Archived objects are the evidence half of
// the audit chain: a checkpoint, a provider payload, a signed transaction. A
// reference recorded for an object that was never written is worse than no
// reference at all, because verification will later report tampering — or
// worse, an operator will believe the evidence exists at the moment they most
// need it and discover otherwise during an incident.
//
// The fault is the container paused: the TCP connection stays open and never
// answers, which is the case a naive client handles worst.
func TestChaos_ArchiveWriteRefusedIsNeverRecordedAsStored(t *testing.T) {
	requireHealthyStack(t, MinIOContainer())
	// A short operation timeout so a stalled store fails inside the test
	// rather than on the 30s default.
	arch, bucket := newChaosArchive(t, 5*time.Second)
	ctx := context.Background()
	prefix := "chaos/" + chaosToken() + "/"

	w := &storeEvidence{arch: arch, bucket: bucket, swallow: chaosBreak(t, "archive_failure_swallowed")}

	// Precondition: a healthy write works, so a later failure is the fault
	// and not a misconfiguration.
	healthyKey := prefix + "healthy.json"
	require.True(t, w.put(ctx, healthyKey, []byte(`{"evidence":"before"}`)),
		"the archive refused a write before any fault was injected: %v", w.lastError)
	require.Len(t, w.recorded, 1)

	// --- the fault -----------------------------------------------------------
	f := newFault(t, MinIOContainer())
	f.Pause()

	refusedKey := prefix + "during-outage.json"
	done := make(chan bool, 1)
	go func() { done <- w.put(ctx, refusedKey, []byte(`{"evidence":"during"}`)) }()

	var stored bool
	select {
	case stored = <-done:
	case <-time.After(2 * time.Minute):
		f.Unpause()
		t.Fatal("the archive write never returned while the store was paused; " +
			"a stalled object store must not block its caller indefinitely")
	}

	// THE INVARIANT.
	assert.False(t, stored,
		"a write into a paused object store was reported as stored; a reference now points at an object that does not exist")
	assert.Len(t, w.recorded, 1, "an evidence reference was recorded for an object that was never written")
	if !w.swallow {
		require.Error(t, w.lastError, "the refusal must surface as an error, not as silence")
		assert.NotEqual(t, errs.CodeInternal, errs.CodeOf(w.lastError),
			"an unreachable object store must be reported as a provider problem an operator can act on, "+
				"not as an opaque internal error: %v", w.lastError)
		t.Logf("chaos: archive refused the write with %v (code %s)", w.lastError, errs.CodeOf(w.lastError))
	}

	// --- recovery ------------------------------------------------------------
	f.Unpause()
	require.NoError(t, waitHealthy(MinIOContainer(), 3*time.Minute))

	// The object must genuinely not be there. This is the assertion that
	// distinguishes "reported a failure" from "actually failed": a store that
	// errors on the response but landed the object is its own kind of lie.
	_, err := arch.Head(ctx, archive.Locator{Bucket: bucket, Key: refusedKey})
	assert.Error(t, err, "the object the archive said it had NOT stored is present in the bucket")

	// And the write succeeds once the store returns, so the failure was the
	// outage and not a poisoned client.
	assert.True(t, w.put(ctx, refusedKey, []byte(`{"evidence":"after"}`)),
		"the archive never recovered after the store returned: %v", w.lastError)
	got, err := arch.Get(ctx, archive.Locator{Bucket: bucket, Key: refusedKey})
	require.NoError(t, err)
	assert.Equal(t, `{"evidence":"after"}`, string(got.Body))
}
