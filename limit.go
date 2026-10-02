package main

import (
	"sync"
	"time"
)

// limiter is a per-key token bucket plus a per-key cap on jobs running at
// once. Idle buckets are dropped only after their spent tokens have fully
// refilled.
type limiter struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	burst     float64
	maxActive int
	buckets   map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
	active int
}

// newLimiter allows one job per key at a time.
func newLimiter(perMinute float64, burst int) *limiter {
	return newLimiterN(perMinute, burst, 1)
}

// newLimiterN allows up to maxActive jobs per key at a time.
func newLimiterN(perMinute float64, burst, maxActive int) *limiter {
	l := &limiter{
		rate:      perMinute / 60,
		burst:     float64(burst),
		maxActive: maxActive,
		buckets:   make(map[string]*bucket),
	}
	go l.gc()
	return l
}

// refill returns key's bucket, topped up for the time since it was last used.
// The caller holds l.mu.
func (l *limiter) refill(key string) *bucket {
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	return b
}

// acquire takes a token and starts a job for key. It fails if the bucket is
// empty or key already has maxActive jobs running. Call release when the job
// ends.
func (l *limiter) acquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	if b.active >= l.maxActive || b.tokens < 1 {
		return false
	}
	b.tokens--
	b.active++
	return true
}

// spend takes n tokens from key's bucket, or none if it holds fewer.
// It is for budgets (e.g. MiB transferred) and ignores running jobs.
func (l *limiter) spend(key string, n float64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	if b.tokens < n {
		return false
	}
	b.tokens -= n
	return true
}

// refund returns n tokens taken by spend.
func (l *limiter) refund(key string, n float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key)
	b.tokens = min(l.burst, b.tokens+n)
}

func (l *limiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok && b.active > 0 {
		b.active--
	}
}

func (l *limiter) gc() {
	for now := range time.Tick(time.Minute) {
		l.collect(now)
	}
}

func (l *limiter) collect(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-10 * time.Minute)
	for k, b := range l.buckets {
		// Removing an exhausted hourly budget after ten minutes would grant
		// a fresh burst on the next request. Evict only when recreating the
		// bucket cannot increase the tokens available to the client.
		refilled := b.tokens + now.Sub(b.last).Seconds()*l.rate
		if b.active == 0 && b.last.Before(cutoff) && refilled >= l.burst {
			delete(l.buckets, k)
		}
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
