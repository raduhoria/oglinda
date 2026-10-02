package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testServer(cfg *config) *server {
	return newServer(cfg)
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

func TestSpeedHeadDoesNotUseCapacityOrBudgets(t *testing.T) {
	s := testServer(&config{speedMaxMB: 2, speedJobs: 1, speedClientMB: 2, speedTotalMB: 2})
	s.speedSem.tryAcquire()
	defer s.speedSem.release()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /speed/down", s.handleSpeedDown)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	for range 3 {
		r, err := ts.Client().Head(ts.URL + "/speed/down?mb=2")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil || r.StatusCode != http.StatusOK || len(body) != 0 || r.ContentLength != 2<<20 {
			t.Fatalf("HEAD: status=%d, length=%d, body=%d, error=%v", r.StatusCode, r.ContentLength, len(body), err)
		}
		if r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("HEAD metadata: %v", r.Header)
		}
	}
	for _, l := range []*limiter{s.speedLimit, s.speedClient, s.speedTotal} {
		l.mu.Lock()
		count := len(l.buckets)
		l.mu.Unlock()
		if count != 0 {
			t.Fatalf("HEAD created %d limiter buckets", count)
		}
	}
	if len(s.speedSem) != 1 {
		t.Fatal("HEAD changed the occupied speed slot")
	}
}

func TestSpeedTransferMethods(t *testing.T) {
	s := testServer(&config{speedMaxMB: 1, speedJobs: 1, speedClientMB: 2, speedTotalMB: 2})
	for _, c := range []struct {
		handler http.HandlerFunc
		method  string
		path    string
		allow   string
	}{
		{s.handleSpeedDown, "POST", "/speed/down?mb=1", "GET, HEAD"},
		{s.handleSpeedUp, "GET", "/speed/up", "POST"},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader("x"))
		w := httptest.NewRecorder()
		c.handler(w, r)
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != c.allow {
			t.Fatalf("%s %s: status=%d, Allow=%q", c.method, c.path, w.Code, w.Header().Get("Allow"))
		}
	}
}

func TestSpeedTransfers(t *testing.T) {
	s := testServer(&config{speedMaxMB: 1, speedJobs: 1, speedClientMB: 2, speedTotalMB: 2})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /speed/down", s.handleSpeedDown)
	mux.HandleFunc("POST /speed/up", s.handleSpeedUp)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	r, err := ts.Client().Get(ts.URL + "/speed/down?mb=1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || r.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("download: status=%d, bytes=%d, error=%v", r.StatusCode, len(body), err)
	}
	r, err = ts.Client().Post(ts.URL+"/speed/up", "application/octet-stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || r.StatusCode != http.StatusOK || string(body) != "1048576" {
		t.Fatalf("upload: status=%d, body=%q, error=%v", r.StatusCode, body, err)
	}
}

type uploadReadSignal struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (b *uploadReadSignal) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	return b.ReadCloser.Read(p)
}

func TestSpeedUploadTimeoutReleasesSlot(t *testing.T) {
	for _, c := range []struct {
		name     string
		size     int64
		total    time.Duration
		progress time.Duration
		trickle  bool
	}{
		{"first byte", 1, 2 * time.Second, 100 * time.Millisecond, false},
		{"trickled block", 64 << 10, 2 * time.Second, 120 * time.Millisecond, true},
		{"absolute cap", 1, 100 * time.Millisecond, 2 * time.Second, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := testServer(&config{speedMaxMB: 1, speedJobs: 1, speedClientMB: 10, speedTotalMB: 10})
			started := make(chan struct{})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/stall" {
					r.Body = &uploadReadSignal{ReadCloser: r.Body, started: started}
				}
				s.handleSpeedUpload(w, r, c.total, c.progress)
			}))
			defer ts.Close()
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(ts.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			began := time.Now()
			if _, err := fmt.Fprintf(conn, "POST /stall HTTP/1.1\r\nHost: localhost\r\nContent-Length: %d\r\n\r\n", c.size); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("upload did not start reading")
			}
			if len(s.speedSem) != 1 {
				t.Fatal("stalled upload did not acquire the speed slot")
			}
			stop := make(chan struct{})
			var writers sync.WaitGroup
			if c.trickle {
				writers.Add(1)
				go func() {
					defer writers.Done()
					ticker := time.NewTicker(c.progress / 4)
					defer ticker.Stop()
					for {
						select {
						case <-stop:
							return
						case <-ticker.C:
							if _, err := conn.Write([]byte{'x'}); err != nil {
								return
							}
						}
					}
				}()
			}
			defer func() { close(stop); conn.Close(); writers.Wait() }()
			r, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil || r.StatusCode != http.StatusRequestTimeout || !r.Close {
				t.Fatalf("timed out upload: status=%d, close=%v, error=%v", r.StatusCode, r.Close, err)
			}
			if elapsed := time.Since(began); elapsed >= time.Second {
				t.Fatalf("upload ignored its short deadline: %s", elapsed)
			}
			if len(s.speedSem) != 0 {
				t.Fatal("timed out upload kept its speed slot")
			}
			// The same client must be able to upload again after the timeout.
			r, err = ts.Client().Post(ts.URL+"/complete", "text/plain", strings.NewReader("x"))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil || r.StatusCode != http.StatusOK || string(body) != "1" {
				t.Fatalf("subsequent upload: status=%d, body=%q, error=%v", r.StatusCode, body, err)
			}
		})
	}
}

func TestSpeedUploadRefusalDoesNotDrainBody(t *testing.T) {
	s := testServer(&config{speedMaxMB: 1, speedJobs: 1, speedClientMB: 2, speedTotalMB: 2})
	s.speedSem.tryAcquire()
	defer s.speedSem.release()
	ts := httptest.NewServer(http.HandlerFunc(s.handleSpeedUp))
	defer ts.Close()
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(ts.URL, "http://"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "POST /speed/up HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || r.StatusCode != http.StatusServiceUnavailable || !r.Close {
		t.Fatalf("refused upload: status=%d, close=%v, error=%v", r.StatusCode, r.Close, err)
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

func TestSpeedParallelStreams(t *testing.T) {
	s := testServer(&config{speedMaxMB: 1, speedJobs: 8, speedStreams: 4, speedClientMB: 100, speedTotalMB: 100})
	admit := func(remote string) (func(), int) {
		r := httptest.NewRequest("GET", "/speed/down?mb=1", nil)
		r.RemoteAddr = remote
		w := httptest.NewRecorder()
		release, ok := s.admitSpeed(w, r, 1)
		if !ok {
			return nil, w.Code
		}
		return release, 200
	}

	var releases []func()
	for i := 0; i < 4; i++ {
		release, code := admit("203.0.113.1:1000")
		if code != 200 {
			t.Fatalf("stream %d: %d", i+1, code)
		}
		releases = append(releases, release)
	}
	if _, code := admit("203.0.113.1:1000"); code != 429 {
		t.Fatalf("fifth stream: %d, want 429", code)
	}
	// Another client still gets in while the first holds four slots.
	other, code := admit("203.0.113.2:1000")
	if code != 200 {
		t.Fatalf("other client: %d", code)
	}
	other()

	releases[0]()
	if release, code := admit("203.0.113.1:1000"); code != 200 {
		t.Fatalf("after release: %d", code)
	} else {
		release()
	}
	for _, release := range releases[1:] {
		release()
	}
	if len(s.speedSem) != 0 {
		t.Fatalf("%d slots still held", len(s.speedSem))
	}
}

func TestSpeedStreamsShareGlobalSlots(t *testing.T) {
	s := testServer(&config{speedMaxMB: 1, speedJobs: 2, speedStreams: 2, speedClientMB: 100, speedTotalMB: 100})
	for i, remote := range []string{"203.0.113.1:1000", "203.0.113.1:1000"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if _, ok := s.admitSpeed(httptest.NewRecorder(), r, 1); !ok {
			t.Fatalf("stream %d refused", i+1)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.2:1000"
	w := httptest.NewRecorder()
	if _, ok := s.admitSpeed(w, r, 1); ok || w.Code != 503 {
		t.Fatalf("global slots full: ok=%v code=%d, want 503", ok, w.Code)
	}
}
