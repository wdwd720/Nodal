package archivetest

import (
	"context"
	"encoding/hex"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/errs"
)

type stored struct {
	body        []byte
	contentType string
	metadata    map[string]string
	retention   *archive.Retention
	etag        string
	storedAt    time.Time
	deleted     bool
}

// Memory is an in-memory archive.ObjectArchive. Every Put creates a new
// version ("v1", "v2", …) of its key; Locators without a version address the
// latest live version.
type Memory struct {
	clk clock.Clock

	mu       sync.Mutex
	objects  map[string]map[string][]*stored // bucket → key → versions (index+1 = version number)
	locked   map[string]bool                 // buckets with object lock enabled
	failPut  error
	puts     int
	gets     int
	deletes  int
	corrupts int
}

var _ archive.ObjectArchive = (*Memory)(nil)

// New returns an empty archive. Buckets are created on first use; call
// EnableObjectLock for buckets that should accept retention.
func New(clk clock.Clock) *Memory {
	if clk == nil {
		clk = clock.System()
	}
	return &Memory{clk: clk, objects: map[string]map[string][]*stored{}, locked: map[string]bool{}}
}

// EnableObjectLock marks bucket as Object Lock enabled.
func (m *Memory) EnableObjectLock(bucket string) *Memory {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.locked[bucket] = true
	return m
}

// FailPut makes every Put return err until cleared with nil.
func (m *Memory) FailPut(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failPut = err
}

// Counts returns how many Put, Get and Delete calls were made.
func (m *Memory) Counts() (puts, gets, deletes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts, m.gets, m.deletes
}

// Len returns the number of live object versions.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, keys := range m.objects {
		for _, versions := range keys {
			for _, v := range versions {
				if !v.deleted {
					n++
				}
			}
		}
	}
	return n
}

// Corrupt flips the first byte of the stored bytes of loc without touching
// its metadata, so the next Get fails the integrity check. It reports
// whether the object existed.
func (m *Memory) Corrupt(loc archive.Locator) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.lookup(loc)
	if s == nil {
		return false
	}
	if len(s.body) == 0 {
		s.body = []byte{0xff}
	} else {
		s.body = append([]byte(nil), s.body...)
		s.body[0] ^= 0xff
	}
	m.corrupts++
	return true
}

// Put implements archive.ObjectArchive.
func (m *Memory) Put(ctx context.Context, req archive.PutRequest) (archive.ObjectRef, error) {
	if err := ctx.Err(); err != nil {
		return archive.ObjectRef{}, err
	}
	now := m.clk.Now()
	if err := req.Validate(now); err != nil {
		return archive.ObjectRef{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	if m.failPut != nil {
		return archive.ObjectRef{}, m.failPut
	}
	if req.Retention != nil && !m.locked[req.Bucket] {
		return archive.ObjectRef{}, errs.Wrap(archive.ErrObjectLockNotEnabled, errs.CodeValidationFailed, "archivetest: bucket has no object lock")
	}
	sum := archive.SHA256(req.Body)
	meta := make(map[string]string, len(req.Metadata)+1)
	for k, v := range req.Metadata {
		meta[k] = v
	}
	meta[archive.MetaSHA256] = hex.EncodeToString(sum)
	keys := m.objects[req.Bucket]
	if keys == nil {
		keys = map[string][]*stored{}
		m.objects[req.Bucket] = keys
	}
	s := &stored{
		body: append([]byte(nil), req.Body...), contentType: contentType(req.ContentType), metadata: meta,
		etag: hex.EncodeToString(sum[:16]), storedAt: now,
	}
	if req.Retention != nil {
		r := *req.Retention
		s.retention = &r
	}
	keys[req.Key] = append(keys[req.Key], s)
	ref := archive.ObjectRef{
		Bucket: req.Bucket, Key: req.Key, VersionID: "v" + strconv.Itoa(len(keys[req.Key])),
		ETag: s.etag, SHA256: sum, Size: int64(len(req.Body)), StoredAt: now,
	}
	ref.URI = ref.Locator().URI()
	return ref, nil
}

// lookup finds the addressed version (latest live when unversioned). Must be
// called with mu held.
func (m *Memory) lookup(loc archive.Locator) *stored {
	versions := m.objects[loc.Bucket][loc.Key]
	if loc.VersionID == "" {
		for i := len(versions) - 1; i >= 0; i-- {
			if !versions[i].deleted {
				return versions[i]
			}
		}
		return nil
	}
	n, err := strconv.Atoi(trimV(loc.VersionID))
	if err != nil || n < 1 || n > len(versions) || versions[n-1].deleted {
		return nil
	}
	return versions[n-1]
}

func trimV(s string) string {
	if len(s) > 0 && s[0] == 'v' {
		return s[1:]
	}
	return s
}

func (m *Memory) refOf(loc archive.Locator, s *stored) archive.ObjectRef {
	version := loc.VersionID
	if version == "" {
		versions := m.objects[loc.Bucket][loc.Key]
		for i := len(versions) - 1; i >= 0; i-- {
			if versions[i] == s {
				version = "v" + strconv.Itoa(i+1)
				break
			}
		}
	}
	sum, _ := hex.DecodeString(s.metadata[archive.MetaSHA256])
	ref := archive.ObjectRef{Bucket: loc.Bucket, Key: loc.Key, VersionID: version, ETag: s.etag, SHA256: sum, Size: int64(len(s.body)), StoredAt: s.storedAt}
	ref.URI = ref.Locator().URI()
	return ref
}

// Get implements archive.ObjectArchive.
func (m *Memory) Get(ctx context.Context, loc archive.Locator) (archive.Object, error) {
	if err := ctx.Err(); err != nil {
		return archive.Object{}, err
	}
	if err := loc.Validate(); err != nil {
		return archive.Object{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gets++
	s := m.lookup(loc)
	if s == nil {
		return archive.Object{}, errs.Wrap(archive.ErrNotFound, errs.CodeNotFound, "archivetest: object not found")
	}
	body := append([]byte(nil), s.body...)
	if hex.EncodeToString(archive.SHA256(body)) != s.metadata[archive.MetaSHA256] {
		return archive.Object{}, errs.Wrap(archive.ErrIntegrity, errs.CodeArchiveIntegrityViolation, "archivetest: object bytes do not match recorded sha256")
	}
	return archive.Object{Ref: m.refOf(loc, s), Body: body, ContentType: s.contentType, Metadata: cloneMap(s.metadata)}, nil
}

// Head implements archive.ObjectArchive.
func (m *Memory) Head(ctx context.Context, loc archive.Locator) (archive.HeadInfo, error) {
	if err := ctx.Err(); err != nil {
		return archive.HeadInfo{}, err
	}
	if err := loc.Validate(); err != nil {
		return archive.HeadInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.lookup(loc)
	if s == nil {
		return archive.HeadInfo{}, errs.Wrap(archive.ErrNotFound, errs.CodeNotFound, "archivetest: object not found")
	}
	info := archive.HeadInfo{Ref: m.refOf(loc, s), ContentType: s.contentType, Metadata: cloneMap(s.metadata), LastModified: s.storedAt}
	if s.retention != nil {
		r := *s.retention
		info.Retention = &r
	}
	return info, nil
}

// List implements archive.ObjectArchive.
func (m *Memory) List(ctx context.Context, req archive.ListRequest) ([]archive.ListEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := archive.ValidateBucket(req.Bucket); err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 {
		limit = archive.DefaultListLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []archive.ListEntry
	for key, versions := range m.objects[req.Bucket] {
		if len(req.Prefix) > 0 && (len(key) < len(req.Prefix) || key[:len(req.Prefix)] != req.Prefix) {
			continue
		}
		for i := len(versions) - 1; i >= 0; i-- {
			if !versions[i].deleted {
				out = append(out, archive.ListEntry{Key: key, Size: int64(len(versions[i].body)), ETag: versions[i].etag, LastModified: versions[i].storedAt})
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Delete implements archive.ObjectArchive. A version under retention is
// refused until its date has passed on the archive's clock.
func (m *Memory) Delete(ctx context.Context, loc archive.Locator) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := loc.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes++
	s := m.lookup(loc)
	if s == nil {
		return errs.Wrap(archive.ErrNotFound, errs.CodeNotFound, "archivetest: object not found")
	}
	if s.retention != nil && m.clk.Now().Before(s.retention.Until) {
		return errs.Wrap(archive.ErrRetentionLocked, errs.CodeForbidden, "archivetest: version is under retention")
	}
	s.deleted = true
	return nil
}

func contentType(ct string) string {
	if ct == "" {
		return archive.DefaultContentTyp
	}
	return ct
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
