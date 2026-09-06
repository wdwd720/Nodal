package archive_test

import (
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/errs"
)

var fixedReceived = time.Date(2026, 9, 5, 13, 7, 9, 123456789, time.UTC)

func provenance() archive.Provenance {
	return archive.Provenance{
		DataSource: "helius.wallet_events", Provider: "helius", EventType: "wallet_transaction",
		SourceEventID: "5VfYd8sig", DedupKey: "5VfYd8sig/WalletAAA", SchemaVersion: 1,
		PlatformReceivedAt: fixedReceived, RetentionClass: "RAW_MARKET_DATA",
	}
}

func TestLayout_KeyFollowsPart124Layout(t *testing.T) {
	t.Parallel()
	p := provenance()
	key, err := archive.Layout{}.Key(p)
	require.NoError(t, err)
	// The dedup key contains "/", so its name component is the hash prefix.
	want := "raw/helius/wallet_transaction/v1/2026/09/05/13/" + strconv.FormatInt(fixedReceived.UnixNano(), 10) + "-" + archive.DedupPrefix(p.DedupKey) + ".json"
	require.Equal(t, want, key)
	require.NotContains(t, archive.DedupPrefix(p.DedupKey), "/")
	require.Equal(t, 9, len(strings.Split(key, "/")), key)

	p.DedupKey = "5VfYd8sigSafeKey"
	key, err = archive.Layout{}.Key(p)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(key, "-5VfYd8sigSafeKey.json"), "path-safe dedup keys are kept verbatim: %s", key)

	part, err := archive.Layout{}.PartitionKey(provenance())
	require.NoError(t, err)
	require.Equal(t, "helius/wallet_transaction/v1/2026/09/05/13", part)
}

func TestLayout_HourPartitionUsesPlatformClockNotProvider(t *testing.T) {
	t.Parallel()
	p := provenance()
	// A provider clock far in the past must not move the partition.
	p.PlatformReceivedAt = time.Date(2026, 1, 31, 23, 59, 59, 0, time.FixedZone("plus2", 2*3600))
	part, err := archive.Layout{}.PartitionKey(p)
	require.NoError(t, err)
	require.Equal(t, "helius/wallet_transaction/v1/2026/01/31/21", part, "partition is in UTC")
}

func TestLayout_MetadataAllowsProvenanceReconstruction(t *testing.T) {
	t.Parallel()
	p := provenance()
	p.IngestedAt = fixedReceived.Add(3 * time.Millisecond)
	sum := archive.SHA256([]byte(`{"a":1}`))
	m := archive.Layout{}.Metadata(p, sum)
	require.Equal(t, "helius", m[archive.MetaProvider])
	require.Equal(t, "wallet_transaction", m[archive.MetaEventType])
	require.Equal(t, "5VfYd8sig", m[archive.MetaSourceEventID])
	require.Equal(t, "1", m[archive.MetaSchemaVersion])
	require.Equal(t, fixedReceived.Format(time.RFC3339Nano), m[archive.MetaPlatformReceivedAt])
	require.Equal(t, p.IngestedAt.Format(time.RFC3339Nano), m[archive.MetaIngestedAt])
	require.Equal(t, hex.EncodeToString(sum), m[archive.MetaSHA256])
	require.Equal(t, "helius.wallet_events", m[archive.MetaDataSource])
	require.Equal(t, "RAW_MARKET_DATA", m[archive.MetaRetentionClass])
	require.Equal(t, "5VfYd8sig/WalletAAA", m[archive.MetaDedupKey])
	for k, v := range m {
		require.NoError(t, archive.PutRequest{Bucket: "raw-events", Key: "x/y", Metadata: map[string]string{k: v}}.Validate(fixedReceived), k)
	}
}

func TestLayout_NonASCIIIdentifiersAreHexEncodedInMetadata(t *testing.T) {
	t.Parallel()
	p := provenance()
	p.SourceEventID = "évènement\x00"
	m := archive.Layout{}.Metadata(p, archive.SHA256(nil))
	require.True(t, strings.HasPrefix(m[archive.MetaSourceEventID], "hex:"))
	require.NoError(t, archive.PutRequest{Bucket: "raw-events", Key: "x/y", Metadata: m}.Validate(fixedReceived))
}

func TestLayout_RejectsUnsafeSegments(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*archive.Provenance){
		"provider slash":  func(p *archive.Provenance) { p.Provider = "he/lius" },
		"provider upper":  func(p *archive.Provenance) { p.Provider = "Helius" },
		"event dotdot":    func(p *archive.Provenance) { p.EventType = ".." },
		"schema zero":     func(p *archive.Provenance) { p.SchemaVersion = 0 },
		"no received":     func(p *archive.Provenance) { p.PlatformReceivedAt = time.Time{} },
		"no dedup":        func(p *archive.Provenance) { p.DedupKey = "" },
		"bad extension":   func(p *archive.Provenance) { p.Extension = "JSON!" },
		"empty provider":  func(p *archive.Provenance) { p.Provider = "" },
		"event too long":  func(p *archive.Provenance) { p.EventType = strings.Repeat("a", 65) },
		"event traversal": func(p *archive.Provenance) { p.EventType = "../../etc" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := provenance()
			mutate(&p)
			_, err := archive.Layout{}.Key(p)
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestProp_LayoutKeyRoundTripsThroughParseKey(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(rt *rapid.T) {
		p := provenance()
		p.Provider = rapid.StringMatching(`^[a-z0-9][a-z0-9_.-]{0,20}$`).Draw(rt, "provider")
		p.EventType = rapid.StringMatching(`^[a-z0-9][a-z0-9_.-]{0,20}$`).Draw(rt, "event_type")
		p.SchemaVersion = rapid.IntRange(1, 1000).Draw(rt, "schema")
		p.DedupKey = rapid.String().Filter(func(s string) bool { return s != "" }).Draw(rt, "dedup")
		p.PlatformReceivedAt = time.Unix(rapid.Int64Range(0, 4_000_000_000).Draw(rt, "sec"), rapid.Int64Range(0, 999_999_999).Draw(rt, "nsec")).UTC()
		key, err := archive.Layout{Prefix: "raw"}.Key(p)
		require.NoError(rt, err)
		kp, err := archive.ParseKey(key)
		require.NoError(rt, err)
		require.Equal(rt, p.Provider, kp.Provider)
		require.Equal(rt, p.EventType, kp.EventType)
		require.Equal(rt, p.SchemaVersion, kp.SchemaVersion)
		require.True(rt, kp.PlatformReceivedAt.Equal(p.PlatformReceivedAt))
		require.Equal(rt, p.PlatformReceivedAt.Truncate(time.Hour), kp.Hour)
		require.Equal(rt, archive.DedupPrefix(p.DedupKey), kp.DedupPrefix)
		require.Equal(rt, "json", kp.Extension)
	})
}

func FuzzParseKey(f *testing.F) {
	key, _ := archive.Layout{}.Key(provenance())
	f.Add(key)
	f.Add("raw/a/b/v1/2026/13/01/00/1-x.json")
	f.Add("raw/a/b/v1/2026/09/05/13/notanumber-x.json")
	f.Add("")
	f.Add("///")
	f.Fuzz(func(t *testing.T, s string) {
		kp, err := archive.ParseKey(s)
		if err != nil {
			return
		}
		// Anything that parses re-renders to a key with the same partition.
		part, perr := archive.Layout{Prefix: kp.Prefix}.PartitionKey(archive.Provenance{
			Provider: kp.Provider, EventType: kp.EventType, SchemaVersion: kp.SchemaVersion,
			PlatformReceivedAt: kp.PlatformReceivedAt, DedupKey: "k",
		})
		if perr != nil {
			t.Fatalf("parsed key does not re-render: %v", perr)
		}
		if !strings.Contains(s, part) {
			t.Fatalf("partition %q not in key %q", part, s)
		}
	})
}

func TestParseURI_RoundTrip(t *testing.T) {
	t.Parallel()
	loc := archive.Locator{Bucket: "audit-evidence", Key: "raw/a/b/v1/2026/09/05/13/1-x.json", VersionID: "abc def/=+"}
	parsed, err := archive.ParseURI(loc.URI())
	require.NoError(t, err)
	require.Equal(t, loc, parsed)
	for _, bad := range []string{"", "http://x/y", "s3://", "s3://bucket", "s3://bucket/"} {
		_, err := archive.ParseURI(bad)
		require.Error(t, err, bad)
	}
}

func TestPutRequest_Validate(t *testing.T) {
	t.Parallel()
	now := fixedReceived
	ok := archive.PutRequest{Bucket: "raw-events", Key: "a/b.json", Body: []byte("x"), Metadata: map[string]string{"provider": "helius"}}
	require.NoError(t, ok.Validate(now))
	cases := map[string]archive.PutRequest{
		"bad bucket":       {Bucket: "Raw", Key: "a"},
		"empty key":        {Bucket: "raw-events", Key: ""},
		"leading slash":    {Bucket: "raw-events", Key: "/a"},
		"dot segment":      {Bucket: "raw-events", Key: "a/../b"},
		"control in key":   {Bucket: "raw-events", Key: "a\x01b"},
		"meta key upper":   {Bucket: "raw-events", Key: "a", Metadata: map[string]string{"Provider": "x"}},
		"meta value ctrl":  {Bucket: "raw-events", Key: "a", Metadata: map[string]string{"provider": "x\n"}},
		"meta value utf8":  {Bucket: "raw-events", Key: "a", Metadata: map[string]string{"provider": "é"}},
		"retention past":   {Bucket: "raw-events", Key: "a", Retention: &archive.Retention{Mode: archive.RetentionCompliance, Until: now.Add(-time.Second)}},
		"retention mode":   {Bucket: "raw-events", Key: "a", Retention: &archive.Retention{Mode: "FOREVER", Until: now.Add(time.Hour)}},
		"body too large":   {Bucket: "raw-events", Key: "a", Body: make([]byte, archive.MaxObjectBytes+1)},
		"content type bad": {Bucket: "raw-events", Key: "a", ContentType: "text/plain\r\nX: y"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := req.Validate(now)
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

// NormalizeSegment is the one sanctioned way a foreign namespace (JSON-RPC
// method names, provider labels) becomes a key segment. It must be total for
// the identifiers that actually occur, deterministic, and fail closed on
// anything it cannot express — a key that silently dropped part of its
// identity is no longer self-describing evidence.
func TestNormalizeSegment(t *testing.T) {
	t.Parallel()
	ok := map[string]string{
		"getTransaction":           "get_transaction",
		"getSignaturesForAddress":  "get_signatures_for_address",
		"getLatestBlockhash":       "get_latest_blockhash",
		"wallet_event":             "wallet_event",
		"chain.wallet_transaction": "chain.wallet_transaction",
		"helius":                   "helius",
		"HELIUS":                   "helius",
		"getBlock2":                "get_block2",
		"v2Stream":                 "v2_stream",
		"a-b.c_d":                  "a-b.c_d",
	}
	for in, want := range ok {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, err := archive.NormalizeSegment(in)
			require.NoError(t, err)
			require.Equal(t, want, got)
			require.True(t, archive.ValidSegment(got), "the fold always lands in the segment alphabet")
			again, err := archive.NormalizeSegment(got)
			require.NoError(t, err)
			require.Equal(t, got, again, "the fold is idempotent, so re-archiving cannot drift the key")
		})
	}
	for _, bad := range []string{"", "get transaction", "get/transaction", "_leading", "-leading", "évent", "a\x01b", strings.Repeat("a", 65)} {
		t.Run("reject "+bad, func(t *testing.T) {
			t.Parallel()
			_, err := archive.NormalizeSegment(bad)
			require.Error(t, err)
			require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
		})
	}
}

func TestValidSegment_MatchesTheKeyAlphabet(t *testing.T) {
	t.Parallel()
	require.True(t, archive.ValidSegment("helius"))
	require.True(t, archive.ValidSegment("get_transaction"))
	require.False(t, archive.ValidSegment("getTransaction"), "upper case never reaches a key")
	require.False(t, archive.ValidSegment(""))
	require.Regexp(t, `^\^\[a-z0-9\]`, archive.SegmentPattern)
}
