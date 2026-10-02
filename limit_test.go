package main

import (
	"testing"
	"time"
)

func TestHourlyBudgetSurvivesIdleCleanup(t *testing.T) {
	l := newLimiter(500.0/60, 500)
	if !l.spend("client", 500) {
		t.Fatal("initial budget unavailable")
	}
	now := time.Now()
	l.mu.Lock()
	l.buckets["client"].last = now.Add(-11 * time.Minute)
	l.mu.Unlock()
	l.collect(now)
	if l.spend("client", 500) {
		t.Fatal("idle cleanup reset a depleted hourly budget")
	}
	if !l.spend("client", 90) {
		t.Fatal("naturally refilled budget unavailable")
	}
	if l.spend("client", 10) {
		t.Fatal("client received more than eleven minutes of refill")
	}
}

func TestLimiterCleanupKeepsActiveAndUnrefilledBuckets(t *testing.T) {
	now := time.Now()
	l := newLimiter(500.0/60, 500)
	l.mu.Lock()
	l.buckets = map[string]*bucket{
		"depleted": {tokens: 0, last: now.Add(-11 * time.Minute)},
		"refilled": {tokens: 0, last: now.Add(-61 * time.Minute)},
		"partial":  {tokens: 450, last: now.Add(-11 * time.Minute)},
		"busy":     {tokens: 500, last: now.Add(-61 * time.Minute), active: 1},
		"recent":   {tokens: 500, last: now.Add(-time.Minute)},
	}
	l.mu.Unlock()
	l.collect(now)
	l.mu.Lock()
	for _, key := range []string{"depleted", "busy", "recent"} {
		if _, ok := l.buckets[key]; !ok {
			t.Errorf("cleanup removed %s bucket", key)
		}
	}
	for _, key := range []string{"refilled", "partial"} {
		if _, ok := l.buckets[key]; ok {
			t.Errorf("cleanup retained fully refilled %s bucket", key)
		}
	}
	l.mu.Unlock()
	if !l.spend("refilled", 500) {
		t.Fatal("evicted fully refilled budget was not usable")
	}
}
