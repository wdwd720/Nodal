// Package webhooktest holds test doubles for internal/webhook. It is never
// imported by production wiring.
package webhooktest

import (
	"context"
	"errors"
	"sync"
)

// MemoryArchive is an in-memory ArchiveWriter. Set Err to simulate an
// unavailable archive.
type MemoryArchive struct {
	mu      sync.Mutex
	objects map[string][]byte
	Err     error
}

// NewMemoryArchive returns an empty archive.
func NewMemoryArchive() *MemoryArchive { return &MemoryArchive{objects: map[string][]byte{}} }

// Put stores body under key and returns "mem://<key>".
func (m *MemoryArchive) Put(_ context.Context, key, _ string, body []byte) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	if key == "" {
		return "", errors.New("webhooktest: empty key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), body...)
	return "mem://" + key, nil
}

// Get returns a stored object.
func (m *MemoryArchive) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	return b, ok
}

// Len returns the number of stored objects.
func (m *MemoryArchive) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}
