package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// DefaultPrefix is the top-level key prefix of raw objects.
const DefaultPrefix = "raw"

// dedupPrefixLength bounds the dedup component of an object name.
const dedupPrefixLength = 24

// SegmentPattern is the alphabet of the provider and event-type path
// segments. It is deliberately lower case: object keys are compared verbatim
// by every store, mirror and restore drill, so one canonical casing removes
// a whole class of "same object, two keys" incidents.
const SegmentPattern = `^[a-z0-9][a-z0-9_.-]{0,63}$`

var (
	segmentRe = regexp.MustCompile(SegmentPattern)
	safeRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	extRe     = regexp.MustCompile(`^[a-z0-9]{1,8}$`)
)

// ValidSegment reports whether s is usable as a provider or event-type path
// segment.
func ValidSegment(s string) bool { return segmentRe.MatchString(s) }

// Provenance is everything the key layout and object metadata carry so the
// origin of an object can be reconstructed from the bucket alone (PART 124).
type Provenance struct {
	DataSource    string
	Provider      string
	EventType     string
	SourceEventID string
	DedupKey      string
	SchemaVersion int
	// PlatformReceivedAt is the platform clock at first byte (knowledge-time
	// anchor); it decides the hour partition and the object name.
	PlatformReceivedAt time.Time
	// IngestedAt is when the archive write happened (defaults to
	// PlatformReceivedAt when zero).
	IngestedAt     time.Time
	RetentionClass string
	// Extension is the file extension without the dot ("json" by default).
	Extension string
}

// Validate checks the fields that become path segments.
func (p Provenance) Validate() error {
	fields := map[string]any{}
	if !segmentRe.MatchString(p.Provider) {
		fields["provider"] = "must match ^[a-z0-9][a-z0-9_.-]{0,63}$"
	}
	if !segmentRe.MatchString(p.EventType) {
		fields["event_type"] = "must match ^[a-z0-9][a-z0-9_.-]{0,63}$"
	}
	if p.SchemaVersion < 1 {
		fields["schema_version"] = "must be >= 1"
	}
	if p.PlatformReceivedAt.IsZero() {
		fields["platform_received_at"] = "required"
	}
	if p.DedupKey == "" {
		fields["dedup_key"] = "required"
	}
	if p.Extension != "" && !extRe.MatchString(p.Extension) {
		fields["extension"] = "must match ^[a-z0-9]{1,8}$"
	}
	if len(fields) > 0 {
		return errs.New(errs.CodeValidationFailed, "archive: invalid provenance").WithFields(fields)
	}
	return nil
}

// NormalizeSegment maps an identifier from a foreign namespace onto the
// path alphabet segmentRe accepts, so a caller at a boundary (for example
// the chain adapter, whose event types are JSON-RPC method names like
// "getTransaction") never has to invent its own mapping and never mangles
// the value silently. ASCII upper case folds to lower case and a lower→upper
// case boundary becomes "_", so "getTransaction" becomes "get_transaction".
// Nothing else is rewritten: a value that still does not match after the
// fold is rejected rather than stripped, because an object whose key silently
// dropped part of its identity is no longer self-describing evidence.
func NormalizeSegment(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 && isLowerOrDigit(s[i-1]) {
				b.WriteByte('_')
			}
			b.WriteByte(c - 'A' + 'a')
			continue
		}
		b.WriteByte(c)
	}
	out := b.String()
	if !segmentRe.MatchString(out) {
		return "", errs.New(errs.CodeValidationFailed, "archive: identifier cannot be normalized to a path segment").
			WithFields(map[string]any{"value": s, "normalized": out, "must_match": segmentRe.String()})
	}
	return out, nil
}

func isLowerOrDigit(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// Layout produces keys and metadata in the documented layout
// (POINT_IN_TIME.md §2):
//
//	<prefix>/<provider>/<event_type>/v<schema_version>/YYYY/MM/DD/HH/<platform_received_at_ns>-<dedup_key_prefix>.<ext>
//
// The hour partition and the name derive from platform_received_at, never
// from a provider clock, so listing an hour prefix yields exactly what the
// platform learned in that hour.
type Layout struct {
	// Prefix is the top-level prefix (DefaultPrefix when empty).
	Prefix string
}

func (l Layout) prefix() string {
	if l.Prefix == "" {
		return DefaultPrefix
	}
	return strings.Trim(l.Prefix, "/")
}

// PartitionKey returns provider/event_type/vN/YYYY/MM/DD/HH, the value
// stored in raw_archive_objects.partition_key.
func (l Layout) PartitionKey(p Provenance) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	t := p.PlatformReceivedAt.UTC()
	return fmt.Sprintf("%s/%s/v%d/%04d/%02d/%02d/%02d",
		p.Provider, p.EventType, p.SchemaVersion, t.Year(), int(t.Month()), t.Day(), t.Hour()), nil
}

// Key returns the full object key.
func (l Layout) Key(p Provenance) (string, error) {
	part, err := l.PartitionKey(p)
	if err != nil {
		return "", err
	}
	ext := p.Extension
	if ext == "" {
		ext = "json"
	}
	name := strconv.FormatInt(p.PlatformReceivedAt.UTC().UnixNano(), 10) + "-" + DedupPrefix(p.DedupKey) + "." + ext
	key := l.prefix() + "/" + part + "/" + name
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

// DedupPrefix returns the object-name component of a dedup key: the key's
// first dedupPrefixLength characters when it is path-safe, else the first
// 16 hex characters of its sha256. Deterministic for a given key.
func DedupPrefix(dedupKey string) string {
	if safeRe.MatchString(dedupKey) {
		if len(dedupKey) > dedupPrefixLength {
			return dedupKey[:dedupPrefixLength]
		}
		return dedupKey
	}
	sum := sha256.Sum256([]byte(dedupKey))
	return hex.EncodeToString(sum[:8])
}

// Metadata returns the provenance metadata for an object with digest sum.
// Values are printable ASCII (non-ASCII identifiers are hex-encoded).
func (l Layout) Metadata(p Provenance, sum []byte) map[string]string {
	ingested := p.IngestedAt
	if ingested.IsZero() {
		ingested = p.PlatformReceivedAt
	}
	m := map[string]string{
		MetaProvider:           p.Provider,
		MetaEventType:          p.EventType,
		MetaSchemaVersion:      strconv.Itoa(p.SchemaVersion),
		MetaPlatformReceivedAt: p.PlatformReceivedAt.UTC().Format(time.RFC3339Nano),
		MetaIngestedAt:         ingested.UTC().Format(time.RFC3339Nano),
		MetaDedupKey:           asciiOrHex(p.DedupKey),
		MetaSHA256:             hex.EncodeToString(sum),
	}
	if p.SourceEventID != "" {
		m[MetaSourceEventID] = asciiOrHex(p.SourceEventID)
	}
	if p.DataSource != "" {
		m[MetaDataSource] = asciiOrHex(p.DataSource)
	}
	if p.RetentionClass != "" {
		m[MetaRetentionClass] = p.RetentionClass
	}
	return m
}

// asciiOrHex keeps printable ASCII values verbatim and hex-encodes anything
// else with a "hex:" prefix so the value survives as an HTTP header.
func asciiOrHex(s string) string {
	if printableASCII(s) && len(s) <= MaxMetadataValue {
		return s
	}
	return "hex:" + hex.EncodeToString([]byte(s))
}

// KeyParts is what ParseKey recovers from an object key.
type KeyParts struct {
	Prefix             string
	Provider           string
	EventType          string
	SchemaVersion      int
	Hour               time.Time // start of the hour partition, UTC
	PlatformReceivedAt time.Time
	DedupPrefix        string
	Extension          string
}

// ParseKey inverts Layout.Key. It never panics on any input.
// isPaddedInt reports whether s is exactly n ASCII digits: no sign, no spaces,
// no shorter unpadded form. Used for the date partition, which must be written
// the one way PartitionKey renders it so keys sort in time order.
func isPaddedInt(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func ParseKey(key string) (KeyParts, error) {
	bad := func(why string) (KeyParts, error) {
		return KeyParts{}, errs.New(errs.CodeValidationFailed, "archive: "+why).WithField("key", key)
	}
	if err := ValidateKey(key); err != nil {
		return KeyParts{}, err
	}
	segs := strings.Split(key, "/")
	if len(segs) != 9 {
		return bad("key does not have 9 segments")
	}
	var kp KeyParts
	kp.Prefix, kp.Provider, kp.EventType = segs[0], segs[1], segs[2]
	if !segmentRe.MatchString(kp.Provider) || !segmentRe.MatchString(kp.EventType) {
		return bad("invalid provider or event type segment")
	}
	if !strings.HasPrefix(segs[3], "v") {
		return bad("schema segment must start with v")
	}
	v, err := strconv.Atoi(segs[3][1:])
	if err != nil || v < 1 {
		return bad("invalid schema version")
	}
	kp.SchemaVersion = v
	// The date partition must be written exactly as PartitionKey renders it:
	// YYYY/MM/DD/HH, zero-padded. strconv.Atoi alone would accept "2026/9/5/13"
	// and every other unpadded or sign-prefixed spelling of the same hour, which
	// a fuzz round-trip found: the key parsed, re-rendered as "2026/09/05/13",
	// and no longer contained its own partition.
	//
	// Padding is not cosmetic here. Archive keys are listed and range-scanned by
	// prefix, and that only works because they sort lexicographically in time
	// order. "2026/9/…" sorts AFTER "2026/10/…", so one unpadded key silently
	// falls outside every time-bounded listing that should have found it —
	// including a retention sweep and an audit reconstruction.
	if !isPaddedInt(segs[4], 4) || !isPaddedInt(segs[5], 2) || !isPaddedInt(segs[6], 2) || !isPaddedInt(segs[7], 2) {
		return bad("date partition must be zero-padded YYYY/MM/DD/HH")
	}
	y, _ := strconv.Atoi(segs[4])
	mo, _ := strconv.Atoi(segs[5])
	d, _ := strconv.Atoi(segs[6])
	h, _ := strconv.Atoi(segs[7])
	if mo < 1 || mo > 12 || d < 1 || d > 31 || h > 23 {
		return bad("invalid date partition")
	}
	// A day that does not exist in that month (2026/02/31) must not round-trip
	// into a different date via time.Date's normalisation.
	if t := time.Date(y, time.Month(mo), d, h, 0, 0, 0, time.UTC); t.Day() != d || int(t.Month()) != mo || t.Year() != y {
		return bad("date partition names a day that does not exist")
	}
	kp.Hour = time.Date(y, time.Month(mo), d, h, 0, 0, 0, time.UTC)
	name := segs[8]
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return bad("object name has no extension")
	}
	kp.Extension = name[dot+1:]
	base := name[:dot]
	dash := strings.IndexByte(base, '-')
	if dash <= 0 || dash == len(base)-1 {
		return bad("object name is not <received_ns>-<dedup>")
	}
	ns, err := strconv.ParseInt(base[:dash], 10, 64)
	if err != nil {
		return bad("invalid received timestamp")
	}
	kp.PlatformReceivedAt = time.Unix(0, ns).UTC()
	kp.DedupPrefix = base[dash+1:]
	if kp.PlatformReceivedAt.Truncate(time.Hour) != kp.Hour {
		return bad("object timestamp is outside its hour partition")
	}
	return kp, nil
}
