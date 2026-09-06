package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/errs"
)

// Metadata keys stored on every archived object (POINT_IN_TIME.md §2). S3
// lower-cases user metadata keys, so these are lower-case with hyphens.
const (
	MetaProvider           = "provider"
	MetaEventType          = "event-type"
	MetaSourceEventID      = "source-event-id"
	MetaSchemaVersion      = "schema-version"
	MetaPlatformReceivedAt = "platform-received-at"
	MetaIngestedAt         = "ingested-at"
	MetaSHA256             = "sha256"
	MetaDedupKey           = "dedup-key"
	MetaDataSource         = "data-source"
	MetaRetentionClass     = "retention-class"
)

// Limits protecting the store and callers from unbounded input.
const (
	MaxKeyLength      = 1024
	MaxMetadataKeys   = 32
	MaxMetadataValue  = 1024
	MaxObjectBytes    = 64 << 20 // 64 MiB per raw object
	DefaultListLimit  = 1000
	MaxListLimit      = 100_000
	DefaultContentTyp = "application/octet-stream"
)

// RetentionMode is the Object Lock mode.
type RetentionMode string

// Retention modes. COMPLIANCE cannot be shortened or removed by anyone,
// including the root account; GOVERNANCE can be bypassed with a special
// permission. Audit artifacts use COMPLIANCE.
const (
	RetentionCompliance RetentionMode = "COMPLIANCE"
	RetentionGovernance RetentionMode = "GOVERNANCE"
)

// Valid reports whether the mode is one of the two documented modes.
func (m RetentionMode) Valid() bool {
	return m == RetentionCompliance || m == RetentionGovernance
}

// Retention is an Object Lock retention applied at Put time.
type Retention struct {
	Mode  RetentionMode
	Until time.Time
}

// Validate checks the retention against now.
func (r Retention) Validate(now time.Time) error {
	if !r.Mode.Valid() {
		return fmt.Errorf("archive: unknown retention mode %q", string(r.Mode))
	}
	if r.Until.IsZero() {
		return errors.New("archive: retention until is required")
	}
	if !r.Until.After(now) {
		return errors.New("archive: retention until must be in the future")
	}
	return nil
}

// PutRequest describes one object to store.
type PutRequest struct {
	Bucket      string
	Key         string
	Body        []byte
	ContentType string
	// Metadata is stored as user metadata. Keys must be lower-case
	// [a-z0-9-]; the MetaSHA256 key is always set by the archive and any
	// caller value for it is overwritten.
	Metadata map[string]string
	// Retention, when set, applies an Object Lock retention to the object.
	// The bucket must have Object Lock enabled.
	Retention *Retention
}

// Locator addresses one stored object (a specific version when the bucket
// is versioned, which every Object Lock bucket is).
type Locator struct {
	Bucket    string
	Key       string
	VersionID string
}

// URI renders the locator as s3://bucket/key[?versionId=...].
func (l Locator) URI() string {
	u := "s3://" + l.Bucket + "/" + l.Key
	if l.VersionID != "" {
		u += "?versionId=" + url.QueryEscape(l.VersionID)
	}
	return u
}

// ParseURI parses the form produced by Locator.URI.
func ParseURI(s string) (Locator, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "s3" || u.Host == "" {
		return Locator{}, errs.New(errs.CodeValidationFailed, "archive: uri must be s3://bucket/key")
	}
	key := strings.TrimPrefix(u.Path, "/")
	if key == "" {
		return Locator{}, errs.New(errs.CodeValidationFailed, "archive: uri has no key")
	}
	return Locator{Bucket: u.Host, Key: key, VersionID: u.Query().Get("versionId")}, nil
}

// ObjectRef is what Put returns and what callers persist next to the row
// that references the object.
type ObjectRef struct {
	// URI is Locator.URI() of the stored object (version-specific when the
	// store reported a version id).
	URI       string
	Bucket    string
	Key       string
	VersionID string
	ETag      string
	// SHA256 is the digest of Body, computed by the archive before the
	// bytes left the process and stored as object metadata.
	SHA256   []byte
	Size     int64
	StoredAt time.Time
}

// Locator returns the address of the object.
func (r ObjectRef) Locator() Locator {
	return Locator{Bucket: r.Bucket, Key: r.Key, VersionID: r.VersionID}
}

// HexSHA256 renders the digest as lower-case hex.
func (r ObjectRef) HexSHA256() string { return hex.EncodeToString(r.SHA256) }

// Object is a fetched object.
type Object struct {
	Ref         ObjectRef
	Body        []byte
	ContentType string
	Metadata    map[string]string
}

// HeadInfo describes an object without its body.
type HeadInfo struct {
	Ref          ObjectRef
	ContentType  string
	Metadata     map[string]string
	Retention    *Retention
	LastModified time.Time
}

// ListRequest selects objects by key prefix.
type ListRequest struct {
	Bucket string
	Prefix string
	// Limit bounds the number of entries returned (DefaultListLimit when
	// zero, never more than MaxListLimit).
	Limit int
}

// ListEntry is one listed object.
type ListEntry struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

// Sentinel errors. Implementations wrap them in *errs.Error so callers can
// use errors.Is on the sentinel and errs.CodeOf on the code.
var (
	// ErrNotFound: no such object (or version).
	ErrNotFound = errors.New("archive: object not found")
	// ErrRetentionLocked: the object version is under Object Lock retention
	// and the store refused to delete it.
	ErrRetentionLocked = errors.New("archive: object is retention locked")
	// ErrIntegrity: the fetched bytes do not hash to the sha256 recorded in
	// the object's metadata.
	ErrIntegrity = errors.New("archive: object integrity violation")
	// ErrObjectLockNotEnabled: a retention was requested on a bucket without
	// Object Lock, or the bucket was verified and found unlocked.
	ErrObjectLockNotEnabled = errors.New("archive: bucket does not have object lock enabled")
)

// ObjectArchive is the provider-neutral object store contract.
type ObjectArchive interface {
	// Put stores the object and returns its reference. The SHA-256 of Body
	// is computed here, sent as the transfer checksum and stored as
	// metadata; Put never returns before the store acknowledged the write.
	Put(ctx context.Context, req PutRequest) (ObjectRef, error)
	// Get fetches the object and verifies its bytes against the sha256
	// metadata when present (ErrIntegrity on mismatch).
	Get(ctx context.Context, loc Locator) (Object, error)
	// Head returns metadata and retention without the body.
	Head(ctx context.Context, loc Locator) (HeadInfo, error)
	// List returns objects under a prefix, sorted by key.
	List(ctx context.Context, req ListRequest) ([]ListEntry, error)
	// Delete removes one object version. A version under retention is
	// refused with ErrRetentionLocked.
	Delete(ctx context.Context, loc Locator) error
}

var (
	metaKeyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	bucketRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// SHA256 returns the digest of b.
func SHA256(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// ValidateBucket checks an S3 bucket name.
func ValidateBucket(bucket string) error {
	if !bucketRe.MatchString(bucket) {
		return errs.New(errs.CodeValidationFailed, "archive: invalid bucket name").WithField("bucket", bucket)
	}
	return nil
}

// ValidateKey checks an object key: non-empty, bounded, no leading slash,
// no empty or dot segments, no control characters.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return errs.New(errs.CodeValidationFailed, "archive: key is required")
	case len(key) > MaxKeyLength:
		return errs.New(errs.CodeValidationFailed, "archive: key too long")
	case strings.HasPrefix(key, "/"):
		return errs.New(errs.CodeValidationFailed, "archive: key must not start with /")
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errs.New(errs.CodeValidationFailed, "archive: key has an empty or dot segment")
		}
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return errs.New(errs.CodeValidationFailed, "archive: key contains control characters")
		}
	}
	return nil
}

// Validate checks the request. Metadata values must be printable ASCII
// (they travel as HTTP headers).
func (p PutRequest) Validate(now time.Time) error {
	if err := ValidateBucket(p.Bucket); err != nil {
		return err
	}
	if err := ValidateKey(p.Key); err != nil {
		return err
	}
	if len(p.Body) > MaxObjectBytes {
		return errs.New(errs.CodeValidationFailed, "archive: body exceeds the object size limit")
	}
	if len(p.Metadata) > MaxMetadataKeys {
		return errs.New(errs.CodeValidationFailed, "archive: too many metadata keys")
	}
	for k, v := range p.Metadata {
		if !metaKeyRe.MatchString(k) {
			return errs.New(errs.CodeValidationFailed, "archive: invalid metadata key").WithField("key", k)
		}
		if len(v) > MaxMetadataValue || !printableASCII(v) {
			return errs.New(errs.CodeValidationFailed, "archive: invalid metadata value").WithField("key", k)
		}
	}
	if p.ContentType != "" && !printableASCII(p.ContentType) {
		return errs.New(errs.CodeValidationFailed, "archive: invalid content type")
	}
	if p.Retention != nil {
		if err := p.Retention.Validate(now); err != nil {
			return errs.Wrap(err, errs.CodeValidationFailed, err.Error())
		}
	}
	return nil
}

// Validate checks the locator.
func (l Locator) Validate() error {
	if err := ValidateBucket(l.Bucket); err != nil {
		return err
	}
	if err := ValidateKey(l.Key); err != nil {
		return err
	}
	if !printableASCII(l.VersionID) {
		return errs.New(errs.CodeValidationFailed, "archive: invalid version id")
	}
	return nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// contentTypeOrDefault fills the default content type.
func contentTypeOrDefault(ct string) string {
	if ct == "" {
		return DefaultContentTyp
	}
	return ct
}

// mergeMetadata copies the caller's metadata and stamps the digest.
func mergeMetadata(in map[string]string, sum []byte) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	out[MetaSHA256] = hex.EncodeToString(sum)
	return out
}

// verifyIntegrity compares body against the sha256 metadata when present.
func verifyIntegrity(meta map[string]string, body []byte) ([]byte, error) {
	sum := SHA256(body)
	want, ok := meta[MetaSHA256]
	if !ok || want == "" {
		return sum, nil
	}
	if !strings.EqualFold(want, hex.EncodeToString(sum)) {
		return sum, errs.Wrap(ErrIntegrity, errs.CodeArchiveIntegrityViolation, "archive: object bytes do not match recorded sha256")
	}
	return sum, nil
}

// listLimit normalises a ListRequest limit.
func listLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultListLimit
	case n > MaxListLimit:
		return MaxListLimit
	}
	return n
}
