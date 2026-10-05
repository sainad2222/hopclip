package server

import (
	"sync"
	"time"
)

// failureLimiter blocks a key after max failures within window. The window
// starts at the first failure and is not extended by later ones.
type failureLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	entries map[string]*failures
	now     func() time.Time
}

type failures struct {
	count int
	reset time.Time
}

func newFailureLimiter(max int, window time.Duration) *failureLimiter {
	return &failureLimiter{max: max, window: window, entries: map[string]*failures{}, now: time.Now}
}

func (l *failureLimiter) blocked(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return false, 0
	}
	now := l.now()
	if now.After(e.reset) {
		delete(l.entries, key)
		return false, 0
	}
	if e.count >= l.max {
		return true, e.reset.Sub(now)
	}
	return false, 0
}

func (l *failureLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e, ok := l.entries[key]
	if !ok || now.After(e.reset) {
		e = &failures{reset: now.Add(l.window)}
		l.entries[key] = e
	}
	e.count++
}

func (l *failureLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

func (l *failureLimiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, e := range l.entries {
		if now.After(e.reset) {
			delete(l.entries, k)
		}
	}
}
