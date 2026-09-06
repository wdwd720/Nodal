package proof

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/config"
)

func sampleDocument() Document {
	return Document{
		Seq:        7,
		PrevRoot:   testDigest("prev"),
		MerkleRoot: testDigest("root"),
		StreamsCovered: map[string]StreamRange{
			"admin":        {FromSeq: 1, ToSeq: 10, LastContentHash: testDigest("admin-10")},
			"account:b":    {FromSeq: 3, ToSeq: 5, LastContentHash: testDigest("b-5")},
			"account:a":    {FromSeq: 1, ToSeq: 1, LastContentHash: testDigest("a-1")},
			"agent:zed":    {FromSeq: 11, ToSeq: 12, LastContentHash: testDigest("zed-12")},
			"account:a:x2": {FromSeq: 1, ToSeq: 2, LastContentHash: testDigest("ax2-2")},
		},
		LeafCount:    18,
		BuildVersion: "abc123",
	}
}

func TestDocument_DigestIsCanonical(t *testing.T) {
	d := sampleDocument()
	b, err := d.Canonical()
	require.NoError(t, err)
	// Keys are the column names, sorted, with no whitespace and base64 bytes.
	assert.True(t, strings.HasPrefix(string(b), `{"build_version":"abc123","leaf_count":18,"merkle_root":"`), string(b))
	assert.Contains(t, string(b), `"streams_covered":{"account:a":{"from_seq":1,"last_content_hash":"`)
	assert.NotContains(t, string(b), " ")

	d1, err := d.Digest()
	require.NoError(t, err)
	// Rebuilding the same document with a differently ordered map yields the same digest.
	d2 := sampleDocument()
	rebuilt := map[string]StreamRange{}
	for _, s := range []string{"agent:zed", "account:a", "admin", "account:a:x2", "account:b"} {
		rebuilt[s] = d2.StreamsCovered[s]
	}
	d2.StreamsCovered = rebuilt
	got, err := d2.Digest()
	require.NoError(t, err)
	assert.Equal(t, d1, got)
	assert.Len(t, d1, HashSize)

	// Every signed field moves the digest.
	mutate := map[string]func(*Document){
		"seq":            func(x *Document) { x.Seq++ },
		"prev_root":      func(x *Document) { x.PrevRoot = testDigest("x") },
		"prev_root nil":  func(x *Document) { x.PrevRoot = nil },
		"merkle_root":    func(x *Document) { x.MerkleRoot = testDigest("x") },
		"leaf_count":     func(x *Document) { x.LeafCount++ },
		"build_version":  func(x *Document) { x.BuildVersion = "other" },
		"range to_seq":   func(x *Document) { r := x.StreamsCovered["admin"]; r.ToSeq++; x.StreamsCovered["admin"] = r },
		"range from_seq": func(x *Document) { r := x.StreamsCovered["admin"]; r.FromSeq++; x.StreamsCovered["admin"] = r },
		"range last hash": func(x *Document) {
			r := x.StreamsCovered["admin"]
			r.LastContentHash = testDigest("x")
			x.StreamsCovered["admin"] = r
		},
		"extra stream":   func(x *Document) { x.StreamsCovered["system"] = StreamRange{1, 1, testDigest("s")} },
		"dropped stream": func(x *Document) { delete(x.StreamsCovered, "account:b") },
	}
	for name, fn := range mutate {
		x := sampleDocument()
		fn(&x)
		got, err := x.Digest()
		require.NoError(t, err, name)
		assert.NotEqual(t, d1, got, name)
	}
}

func TestDocument_StreamsAreBytewiseSorted(t *testing.T) {
	d := sampleDocument()
	assert.Equal(t, []string{"account:a", "account:a:x2", "account:b", "admin", "agent:zed"}, d.Streams())
}

func TestDocument_RoundTripsThroughJSONB(t *testing.T) {
	// streams_covered is stored as jsonb (which re-orders keys) and read back
	// through encoding/json: the digest must survive the trip.
	d := sampleDocument()
	raw, err := audit.CanonicalJSON(d.StreamsCovered)
	require.NoError(t, err)
	var back map[string]StreamRange
	require.NoError(t, json.Unmarshal(raw, &back))
	d2 := d
	d2.StreamsCovered = back
	want, err := d.Digest()
	require.NoError(t, err)
	got, err := d2.Digest()
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, d.StreamsCovered["admin"].LastContentHash, back["admin"].LastContentHash)
}

func TestArchivedCheckpoint_ShapeAndReservedField(t *testing.T) {
	cp := Checkpoint{ID: NewCheckpointID(), Document: sampleDocument(), Signature: []byte{1, 2, 3}, SigningKeyID: "k", SignatureAlgorithm: AlgorithmECDSASHA256, Signer: SignerLocalTest, CreatedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	prev := NewCheckpointID()
	cp.PrevCheckpointID = &prev
	digest, err := cp.Digest()
	require.NoError(t, err)
	arch := cp.Archived(digest)
	assert.Nil(t, arch.AttestationRef, "reserved for PART 233; nil in V1")
	body, err := audit.CanonicalJSON(arch)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"attestation_ref":null`)
	assert.Contains(t, string(body), `"prev_checkpoint_id":"`+prev.String()+`"`)
	var back ArchivedCheckpoint
	require.NoError(t, json.Unmarshal(body, &back))
	assert.Equal(t, cp.ID, back.ID)
	require.NotNil(t, back.PrevCheckpointID)
	assert.Equal(t, prev, *back.PrevCheckpointID)
	assert.Equal(t, digest, back.Digest)
	got, err := back.Checkpoint.Digest()
	require.NoError(t, err)
	assert.Equal(t, digest, got)
	assert.Equal(t, cp.CreatedAt, back.CreatedAt)
}

func TestObjectKey_EmbedsSeqAndID(t *testing.T) {
	cp := Checkpoint{ID: NewCheckpointID(), Document: Document{Seq: 42}}
	key := ObjectKey("", cp)
	assert.Equal(t, "audit-checkpoints/000000000042/"+cp.ID.String()+".json", key)
	assert.Equal(t, "custom/000000000042/"+cp.ID.String()+".json", ObjectKey("custom", cp))
	other := cp
	other.ID = NewCheckpointID()
	assert.NotEqual(t, key, ObjectKey("", other), "a retried run never reuses an orphan's key")
}

func TestCheckpointerOptions_Defaults(t *testing.T) {
	o := CheckpointerOptions{}.withDefaults()
	assert.Equal(t, DefaultMaxLeaves, o.MaxLeaves)
	assert.Equal(t, config.BuildVersion, o.BuildVersion)
	assert.Equal(t, DefaultKeyPrefix, o.KeyPrefix)
	assert.Equal(t, DefaultCandidateMargin, o.CandidateMargin)
	assert.Nil(t, o.Retention)
	custom := CheckpointerOptions{MaxLeaves: 5, BuildVersion: "b", KeyPrefix: "p", CandidateMargin: time.Minute}.withDefaults()
	assert.Equal(t, CheckpointerOptions{MaxLeaves: 5, BuildVersion: "b", KeyPrefix: "p", CandidateMargin: time.Minute}, custom)
}

func TestConstructors_RequireDependencies(t *testing.T) {
	_, err := NewCheckpointer(nil, nil, nil, nil, CheckpointerOptions{})
	assert.Error(t, err)
	_, err = NewVerifier(nil, nil, nil, VerifierOptions{})
	assert.Error(t, err)
	_, err = NewBundler(nil, "")
	assert.Error(t, err)
}

func TestFailure_String(t *testing.T) {
	seq, cseq := int64(3), int64(2)
	f := Failure{Kind: FailCheckpointRoot, Stream: "admin", StreamSeq: &seq, CheckpointSeq: &cseq, CheckpointID: "cp", ArchiveURI: "mem://x", Reason: "boom"}
	assert.Equal(t, "checkpoint_root stream=admin stream_seq=3 checkpoint_seq=2 checkpoint_id=cp archive_uri=mem://x: boom", f.String())
	assert.Equal(t, "stream_chain: r", Failure{Kind: FailStreamChain, Reason: "r"}.String())
}
