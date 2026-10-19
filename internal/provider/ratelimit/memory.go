package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Memory is an in-process Store for tests.
type Memory struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]memoryEntry
	// Err, when set, is returned by every call to simulate an unavailable store.
	Err error
}

type memoryEntry struct {
	count   int64
	expires time.Time
}

func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = time.Now
	}
	return &Memory{now: now, entries: map[string]memoryEntry{}}
}

func (m *Memory) live(key string) (memoryEntry, bool) {
	entry, ok := m.entries[key]
	if ok && !m.now().Before(entry.expires) {
		delete(m.entries, key)
		return memoryEntry{}, false
	}
	return entry, ok
}

func (m *Memory) Hit(_ context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return 0, 0, m.Err
	}
	entry, ok := m.live(key)
	if !ok {
		entry = memoryEntry{expires: m.now().Add(window)}
	}
	entry.count++
	m.entries[key] = entry
	return entry.count, entry.expires.Sub(m.now()), nil
}

func (m *Memory) Count(_ context.Context, key string) (int64, time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return 0, 0, m.Err
	}
	entry, ok := m.live(key)
	if !ok {
		return 0, 0, nil
	}
	return entry.count, entry.expires.Sub(m.now()), nil
}

func (m *Memory) Reset(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	delete(m.entries, key)
	return nil
}
