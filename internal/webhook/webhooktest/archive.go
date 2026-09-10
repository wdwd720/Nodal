// Package webhooktest holds test doubles for internal/webhook. It is never
// imported by production wiring.
package webhooktest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
)

// MemoryArchive is an in-memory ArchiveWriter. Set Err to simulate an
// unavailable archive.
//
// It is write-once, like every real archive behind this interface, and that
// was not always true. It used to overwrite whatever was under the key and
// never fail -- which meant the duplicate-delivery tests in this package ran
// against an archive that could not refuse, and could not have caught the
// deployment defect they were written to prevent: the database-backed archive
// refused a provider's own retry, and the pipeline turned that refusal into a
// 503, which asks the provider to retry again.
//
// So the contract this models is the one the pipeline actually depends on: a
// replay of the same bytes under the same key is the delivery arriving twice,
// and returns the reference that already exists; different bytes under a live
// key is an attempt to rewrite evidence, and fails.
type MemoryArchive struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
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
	m.puts++
	if stored, ok := m.objects[key]; ok {
		if !bytes.Equal(stored, body) {
			return "", fmt.Errorf("webhooktest: %s is already stored with different bytes", key)
		}
		return "mem://" + key, nil
	}
	m.objects[key] = append([]byte(nil), body...)
	return "mem://" + key, nil
}

// Puts returns how many times Put was called, including the calls that found
// the object already there. Len counts distinct objects; the difference
// between the two is what a replay looks like.
func (m *MemoryArchive) Puts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.puts
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
