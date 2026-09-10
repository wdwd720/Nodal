package proof

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/config"
)

// Archive is the write-once object store checkpoint documents are archived
// to (PART 123). Put stores body under key with the given retention (nil
// means the bucket default) and returns the object URI and the sha256 of the
// bytes as stored; Get returns the bytes of the object a previous Put
// returned the URI for. Implementations must refuse to overwrite an existing
// key. The S3/MinIO Object Lock implementation lives in internal/archive and
// is wired by the composition root; this package only depends on the
// interface.
type Archive interface {
	Put(ctx context.Context, key string, body []byte, retention *time.Duration) (uri string, sha256 []byte, err error)
	Get(ctx context.Context, uri string) ([]byte, error)
}

// Archive errors.
var (
	ErrObjectExists   = errors.New("proof: archive object already exists")
	ErrObjectNotFound = errors.New("proof: archive object not found")

	// ErrObjectExistsIdentical is the collision that is not a collision: what
	// is already stored under the key hashes to exactly what the caller is
	// storing. errors.Is(err, ErrObjectExists) is true for it, so a caller
	// that does not care keeps its existing behaviour.
	//
	// It exists because the two cases mean opposite things. Different bytes
	// under a live key is an attempt to rewrite evidence and is worth waking
	// someone for. Identical bytes are a retry of something that already
	// succeeded -- and the caller that matters here is the provider webhook
	// pipeline, where a retry is not an anomaly but the delivery contract:
	// Stripe delivers at least once and retries anything it does not get a 2xx
	// for. Distinguishing them only in the error MESSAGE meant that pipeline
	// answered every genuine retry with 503, which is a request for another
	// one.
	//
	// Put still refuses in both cases. Nothing is ever overwritten. What
	// changes is that a caller can tell which refusal it got, and the URI and
	// digest of what is already stored come back with it, so a caller for whom
	// the replay is expected can use the object that is already there.
	ErrObjectExistsIdentical = fmt.Errorf("%w (identical bytes: a replay of a Put that already succeeded)", ErrObjectExists)
)

// DirArchive is a filesystem Archive for LOCAL, TEST and DEV: objects are
// files under a root directory, written create-only, addressed as file://
// URIs. It cannot enforce retention, which is why NewDirArchive refuses to
// construct one in STAGING/PROD.
type DirArchive struct {
	root string
}

var _ Archive = (*DirArchive)(nil)

// NewDirArchive returns a DirArchive rooted at dir (created if absent).
func NewDirArchive(env config.Environment, dir string) (*DirArchive, error) {
	if env.IsProductionLike() {
		return nil, fmt.Errorf("proof: DirArchive is not permitted in %s; wire the Object Lock archive", env)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("proof: archive dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("proof: archive dir: %w", err)
	}
	return &DirArchive{root: abs}, nil
}

// Root returns the absolute root directory.
func (a *DirArchive) Root() string { return a.root }

func (a *DirArchive) resolve(key string) (string, error) {
	slashed := strings.ReplaceAll(key, `\`, "/")
	for _, seg := range strings.Split(slashed, "/") {
		if seg == ".." {
			return "", fmt.Errorf("proof: archive key %q must not contain \"..\"", key)
		}
	}
	clean := path.Clean("/" + slashed)
	if clean == "/" {
		return "", errors.New("proof: empty archive key")
	}
	full := filepath.Join(a.root, filepath.FromSlash(clean))
	if !strings.HasPrefix(full, a.root+string(filepath.Separator)) {
		return "", fmt.Errorf("proof: archive key %q escapes the archive root", key)
	}
	return full, nil
}

// Put implements Archive. Retention is accepted for interface parity and
// ignored: a filesystem cannot enforce it.
func (a *DirArchive) Put(_ context.Context, key string, body []byte, _ *time.Duration) (string, []byte, error) {
	full, err := a.resolve(key)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return "", nil, fmt.Errorf("proof: archive put: %w", err)
	}
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o440) // #nosec G302 G304 -- resolve rejects ".." segments and asserts the result stays under the archive root; 0440 is deliberate write-once and the parent directory is 0750
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return a.existing(full, key, body)
		}
		return "", nil, fmt.Errorf("proof: archive put: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return "", nil, fmt.Errorf("proof: archive put: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", nil, fmt.Errorf("proof: archive put: %w", err)
	}
	sum := sha256.Sum256(body)
	return fileURI(full), sum[:], nil
}

// fileURI renders a local path as a file:// URI.
func fileURI(full string) string {
	p := filepath.ToSlash(full)
	if !strings.HasPrefix(p, "/") { // Windows drive letters: file:///C:/...
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}

// existing answers a Put that lost to a file already on disk. It reads what is
// there so the caller learns which kind of collision this is, and gets the URI
// and digest of the object that won.
//
// Reading the file is the cost of telling a retry apart from an overwrite:
// O_EXCL fails without ever looking at the bytes, which is why this used to be
// a single undifferentiated refusal.
func (a *DirArchive) existing(full, key string, body []byte) (string, []byte, error) {
	uri := fileURI(full)
	stored, err := os.ReadFile(full) //nolint:gosec // G304: resolve confined full to the archive root
	if err != nil {
		return "", nil, fmt.Errorf("proof: archive put: %s exists and could not be read: %w", key, err)
	}
	have := sha256.Sum256(stored)
	if have == sha256.Sum256(body) {
		return uri, have[:], fmt.Errorf("%w: %s", ErrObjectExistsIdentical, key)
	}
	return uri, have[:], fmt.Errorf("%w: %s (a different object is already stored under this key)", ErrObjectExists, key)
}

// Get implements Archive.
func (a *DirArchive) Get(_ context.Context, uri string) ([]byte, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return nil, fmt.Errorf("proof: archive get: not a file:// URI: %q", uri)
	}
	p := u.Path
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' { // /C:/... on Windows
		p = p[1:]
	}
	full := filepath.Clean(filepath.FromSlash(p))
	if !strings.HasPrefix(full, a.root+string(filepath.Separator)) {
		return nil, fmt.Errorf("proof: archive get: %q is outside the archive root", uri)
	}
	b, err := os.ReadFile(full) //nolint:gosec // G304: confined to the archive root above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrObjectNotFound, uri)
		}
		return nil, fmt.Errorf("proof: archive get: %w", err)
	}
	return b, nil
}
