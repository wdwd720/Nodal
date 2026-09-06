package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

// source is one embedded migration file.
type source struct {
	Version int64
	Name    string // file name, e.g. 00002_outbox_inbox.sql
	SHA256  string // hex digest of the file bytes
}

// parseVersion extracts the numeric prefix of a goose-style file name
// (NNNNN_name.sql). ok is false for anything that is not a migration file.
func parseVersion(name string) (version int64, ok bool) {
	base := path.Base(name)
	if !strings.HasSuffix(base, ".sql") {
		return 0, false
	}
	idx := strings.IndexByte(base, '_')
	if idx <= 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(base[:idx], 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// loadSources hashes every migration in fsys, sorted by version. Duplicate
// versions are an error: goose would refuse them too, but we want the message
// before any database is touched.
func loadSources(fsys fs.FS) ([]source, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded migrations: %w", err)
	}
	seen := map[int64]string{}
	var out []source
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		v, ok := parseVersion(e.Name())
		if !ok {
			continue
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrate: duplicate version %d in %s and %s", v, prev, e.Name())
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", e.Name(), err)
		}
		out = append(out, source{Version: v, Name: e.Name(), SHA256: digest(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// digest returns the lower-case hex SHA-256 of b, normalising CRLF to LF so a
// checkout on Windows produces the same checksum as one on Linux.
func digest(b []byte) string {
	normalised := strings.ReplaceAll(string(b), "\r\n", "\n")
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}

func indexByVersion(srcs []source) map[int64]source {
	m := make(map[int64]source, len(srcs))
	for _, s := range srcs {
		m[s.Version] = s
	}
	return m
}
