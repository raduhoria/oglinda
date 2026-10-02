package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer(cfg *config) *server {
	return &server{
		cfg:         cfg,
		limit:       newLimiter(float64(cfg.perMinute), cfg.perMinute),
		jobs:        make(semaphore, cfg.maxJobs),
		speedLimit:  newLimiter(6, 6),
		speedSem:    make(semaphore, cfg.speedJobs),
		speedClient: newLimiter(float64(cfg.speedClientMB)/60, cfg.speedClientMB),
		speedTotal:  newLimiter(float64(cfg.speedTotalMB), cfg.speedTotalMB),
	}
}

func get(h http.HandlerFunc, url, remote string) int {
	r := httptest.NewRequest("GET", url, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h(w, r)
	return w.Code
}

func TestSpeedClientBudget(t *testing.T) {
	s := testServer(&config{speedMaxMB: 4, speedJobs: 3, speedClientMB: 6, speedTotalMB: 100})
	a, b := "203.0.113.1:1000", "203.0.113.2:1000"
	if c := get(s.handleSpeedDown, "/speed/down?mb=4", a); c != 200 {
		t.Fatalf("first download: %d", c)
	}
	if c := get(s.handleSpeedDown, "/speed/down?mb=4", a); c != 429 {
		t.Fatalf("over client budget: %d, want 429", c)
	}
	// What is left of the budget is still usable, and other clients are unaffected.
	if c := get(s.handleSpeedDown, "/speed/down?mb=2", a); c != 200 {
		t.Fatalf("within remaining budget: %d", c)
	}
	if c := get(s.handleSpeedDown, "/speed/down?mb=4", b); c != 200 {
		t.Fatalf("other client: %d", c)
	}
}

func TestSpeedTotalBudget(t *testing.T) {
	s := testServer(&config{speedMaxMB: 4, speedJobs: 3, speedClientMB: 100, speedTotalMB: 6})
	if c := get(s.handleSpeedDown, "/speed/down?mb=4", "203.0.113.1:1000"); c != 200 {
		t.Fatalf("first download: %d", c)
	}
	if c := get(s.handleSpeedDown, "/speed/down?mb=4", "203.0.113.2:1000"); c != 503 {
		t.Fatalf("over total budget: %d, want 503", c)
	}
	// A refused transfer must not cost the client anything.
	if !s.speedClient.spend("203.0.113.2", 100) {
		t.Fatal("client budget was charged for a refused transfer")
	}
}

func TestSpeedUploadSize(t *testing.T) {
	s := testServer(&config{speedMaxMB: 1, speedJobs: 3, speedClientMB: 100, speedTotalMB: 100})
	cases := []struct {
		length int64
		body   string
		want   int
	}{
		{-1, "abc", 400},      // no Content-Length (chunked)
		{0, "", 400},          // empty
		{2 << 20, "abc", 400}, // over speedMaxMB
		{3, "abc", 200},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/speed/up", io.NopCloser(strings.NewReader(c.body)))
		r.ContentLength = c.length
		r.RemoteAddr = "203.0.113.1:1000"
		w := httptest.NewRecorder()
		s.handleSpeedUp(w, r)
		if w.Code != c.want {
			t.Errorf("Content-Length %d: %d, want %d", c.length, w.Code, c.want)
		}
	}
}

func TestRunBusyCostsNoToken(t *testing.T) {
	s := testServer(&config{perMinute: 1, maxJobs: 1})
	s.jobs.tryAcquire() // server is busy
	url := "/api/run?tool=ping&family=4&target=example.com"
	if c := get(s.handleRun, url, "203.0.113.1:1000"); c != 503 {
		t.Fatalf("busy: %d, want 503", c)
	}
	if !s.limit.acquire("203.0.113.1") {
		t.Fatal("a busy response cost the client its token")
	}
}
