package main

import (
	"sync"
	"time"
)

// limiter is a per-key token bucket plus a per-key "one job at a time" lock.
// Buckets are dropped once idle so the map cannot grow without bound.
type limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
	busy   bool
}

func newLimiter(perMinute, burst int) *limiter {
	l := &limiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		buckets: make(map[string]*bucket),
	}
	go l.gc()
	return l
}

// acquire takes a token and marks key busy. It fails if the bucket is empty
// or the key already has a job running. Call release when the job ends.
func (l *limiter) acquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.busy || b.tokens < 1 {
		return false
	}
	b.tokens--
	b.busy = true
	return true
}

func (l *limiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		b.busy = false
	}
}

func (l *limiter) gc() {
	for range time.Tick(time.Minute) {
		l.mu.Lock()
		cutoff := time.Now().Add(-10 * time.Minute)
		for k, b := range l.buckets {
			if !b.busy && b.last.Before(cutoff) {
				delete(l.buckets, k)
			}
		}
		l.mu.Unlock()
	}
}

// semaphore caps total concurrent jobs across all clients.
type semaphore chan struct{}

func (s semaphore) tryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s semaphore) release() { <-s }
