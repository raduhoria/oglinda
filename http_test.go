package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func requestTestConfig() *config {
	return &config{
		trustedProxy:  []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")},
		pingBin:       "/bin/true",
		tracerouteBin: "/bin/true",
		perMinute:     1,
		maxJobs:       1,
		speedMaxMB:    1,
		speedJobs:     1,
		speedClientMB: 1,
		speedTotalMB:  1,
	}
}

func assertNoAdmission(t *testing.T, s *server) {
	t.Helper()
	if len(s.jobs) != 0 || len(s.speedSem) != 0 {
		t.Fatal("rejected request holds a job slot")
	}
	for name, l := range map[string]*limiter{
		"network": s.limit, "speed requests": s.speedLimit,
		"client traffic": s.speedClient, "global traffic": s.speedTotal,
	} {
		l.mu.Lock()
		n := len(l.buckets)
		l.mu.Unlock()
		if n != 0 {
			t.Errorf("rejected request touched %s admission", name)
		}
	}
}

// A real connection is necessary: ResponseRecorder does not reproduce Go's
// implicit draining of an unread HTTP/1 request body when writing a response.
func TestIncompleteReadBodiesRejectedImmediately(t *testing.T) {
	cases := []struct {
		method, path, framing string
	}{
		{"GET", "/api/run?tool=ping&family=4&target=1.1.1.1", "Content-Length: 1"},
		{"GET", "/api/run?tool=ping&family=4&target=1.1.1.1", "Transfer-Encoding: chunked"},
		{"HEAD", "/api/run?tool=ping&family=4&target=1.1.1.1", "Content-Length: 1"},
		{"HEAD", "/speed/down?mb=1", "Transfer-Encoding: chunked"},
		{"GET", "/speed/down?mb=1", "Content-Length: 1"},
		{"GET", "/api/info", "Content-Length: 1"},
		{"GET", "/", "Transfer-Encoding: chunked"},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path+tc.framing, func(t *testing.T) {
			s := testServer(requestTestConfig())
			ts := httptest.NewUnstartedServer(s.handler())
			ts.Config.ReadTimeout = 10 * time.Second
			ts.Start()
			defer ts.Close()
			conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: lg.example\r\n%s\r\n\r\n", tc.method, tc.path, tc.framing)
			// Intentionally send none of the declared body or chunk framing.
			response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: tc.method})
			if err != nil {
				t.Fatalf("request was not rejected before reading its body: %v", err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest || !response.Close {
				t.Fatalf("response = %d, close = %v; want 400 and close", response.StatusCode, response.Close)
			}
			if _, err := io.ReadAll(response.Body); err != nil {
				t.Fatalf("rejection response blocked: %v", err)
			}
			assertNoAdmission(t, s)
		})
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name, origin, site, host, remote, forwardedHost string
		protos                                          []string
		tls                                             bool
		want                                            bool
	}{
		{name: "command line without browser headers", want: true},
		{name: "same origin metadata", site: "same-origin", want: true},
		{name: "direct navigation", site: "none", want: true},
		{name: "same site is a different origin", site: "same-site"},
		{name: "cross site", site: "cross-site"},
		{name: "unknown metadata", site: "unknown"},
		{name: "matching HTTP origin", origin: "http://lg.example", want: true},
		{name: "case insensitive host", origin: "http://LG.EXAMPLE", want: true},
		{name: "matching host and port", host: "lg.example:8090", origin: "http://lg.example:8090", want: true},
		{name: "different port", host: "lg.example:8090", origin: "http://lg.example:8091"},
		{name: "different host", origin: "http://evil.example"},
		{name: "subdomain", origin: "http://sub.lg.example"},
		{name: "host suffix", origin: "http://lg.example.evil.example"},
		{name: "opaque origin", origin: "null"},
		{name: "multiple serialized origins", origin: "http://lg.example http://evil.example"},
		{name: "different scheme", origin: "https://lg.example"},
		{name: "non HTTP scheme", origin: "ftp://lg.example"},
		{name: "origin with userinfo", origin: "http://user@lg.example"},
		{name: "origin with path", origin: "http://lg.example/"},
		{name: "origin with query", origin: "http://lg.example?x=1"},
		{name: "origin with fragment", origin: "http://lg.example#fragment"},
		{name: "same origin metadata cannot override bad origin", origin: "http://evil.example", site: "same-origin"},
		{name: "same site with matching origin", origin: "http://lg.example", site: "same-site"},
		{name: "HTTPS over TLS", origin: "https://lg.example", tls: true, want: true},
		{name: "HTTP over TLS is cross origin", origin: "http://lg.example", tls: true},
		{name: "HTTPS through trusted proxy", origin: "https://lg.example", remote: "127.0.0.1:5000", protos: []string{"https"}, want: true},
		{name: "HTTP through trusted proxy", origin: "http://lg.example", remote: "127.0.0.1:5000", protos: []string{"http"}, want: true},
		{name: "proxy public scheme differs", origin: "http://lg.example", remote: "127.0.0.1:5000", protos: []string{"https"}},
		{name: "proxy without public scheme", origin: "https://lg.example", remote: "127.0.0.1:5000"},
		{name: "forged proxy scheme from untrusted peer", origin: "https://lg.example", protos: []string{"https"}},
		{name: "untrusted proxy scheme ignored", origin: "http://lg.example", protos: []string{"https"}, want: true},
		{name: "forwarded host cannot change origin", origin: "https://evil.example", remote: "127.0.0.1:5000", protos: []string{"https"}, forwardedHost: "evil.example"},
		{name: "duplicate proxy schemes", origin: "https://lg.example", remote: "127.0.0.1:5000", protos: []string{"https", "https"}},
		{name: "comma separated proxy schemes", origin: "https://lg.example", remote: "127.0.0.1:5000", protos: []string{"https,http"}},
		{name: "invalid proxy scheme", origin: "https://lg.example", remote: "127.0.0.1:5000", protos: []string{"ftp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(requestTestConfig())
			r := httptest.NewRequest("GET", "http://lg.example/api/run", nil)
			r.RemoteAddr = "203.0.113.1:5000"
			if tc.remote != "" {
				r.RemoteAddr = tc.remote
			}
			if tc.host != "" {
				r.Host = tc.host
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.site != "" {
				r.Header.Set("Sec-Fetch-Site", tc.site)
			}
			for _, proto := range tc.protos {
				r.Header.Add("X-Forwarded-Proto", proto)
			}
			if tc.forwardedHost != "" {
				r.Header.Set("X-Forwarded-Host", tc.forwardedHost)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := s.sameOrigin(r); got != tc.want {
				t.Fatalf("sameOrigin = %v, want %v", got, tc.want)
			}
		})
	}
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		for _, values := range [][]string{{""}, {"same-origin", "same-origin"}} {
			s := testServer(requestTestConfig())
			r := httptest.NewRequest("GET", "http://lg.example/api/run", nil)
			r.Header[header] = values
			if s.sameOrigin(r) {
				t.Errorf("accepted %s values %q", header, values)
			}
		}
	}
}

func TestCrossOriginRequestsDoNotConsumeAdmission(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/run?tool=ping&family=4&target=1.1.1.1"},
		{"GET", "/speed/down?mb=1"},
		{"HEAD", "/speed/down?mb=1"},
		{"POST", "/speed/up"},
	} {
		for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
			t.Run(tc.method+tc.path+header, func(t *testing.T) {
				s := testServer(requestTestConfig())
				var body io.Reader
				if tc.method == "POST" {
					body = strings.NewReader("x")
				}
				r := httptest.NewRequest(tc.method, "http://lg.example"+tc.path, body)
				r.RemoteAddr = "203.0.113.1:5000"
				if header == "Origin" {
					r.Header.Set(header, "http://evil.example")
				} else {
					r.Header.Set(header, "cross-site")
				}
				w := httptest.NewRecorder()
				s.handler().ServeHTTP(w, r)
				if w.Code != http.StatusForbidden {
					t.Fatalf("cross-origin response = %d, want 403", w.Code)
				}
				assertNoAdmission(t, s)
				// The same client can still use its complete budget legitimately.
				r.Header.Del(header)
				w = httptest.NewRecorder()
				s.handler().ServeHTTP(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("subsequent legitimate response = %d, want 200", w.Code)
				}
			})
		}
	}
}

func TestCrossOriginIncompleteUploadRejectedImmediately(t *testing.T) {
	s := testServer(requestTestConfig())
	ts := httptest.NewUnstartedServer(s.handler())
	ts.Config.ReadTimeout = 10 * time.Second
	ts.Start()
	defer ts.Close()
	conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprint(conn, "POST /speed/up HTTP/1.1\r\nHost: lg.example\r\nOrigin: http://evil.example\r\nContent-Length: 1\r\n\r\n")
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "POST"})
	if err != nil {
		t.Fatalf("cross-origin rejection blocked reading the upload: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusForbidden || !response.Close {
		t.Fatalf("response = %d, close = %v; want 403 and close", response.StatusCode, response.Close)
	}
	assertNoAdmission(t, s)
}

func TestSameOriginRequestsThroughTLSProxy(t *testing.T) {
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/run?tool=ping&family=4&target=1.1.1.1"},
		{"GET", "/speed/down?mb=1"},
		{"POST", "/speed/up"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			s := testServer(requestTestConfig())
			var body io.Reader
			if tc.method == "POST" {
				body = strings.NewReader("x")
			}
			r := httptest.NewRequest(tc.method, "http://lg.example"+tc.path, body)
			r.RemoteAddr = "127.0.0.1:5000"
			r.Header.Set("Origin", "https://lg.example")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			s.handler().ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("same-origin proxied response = %d, want 200", w.Code)
			}
		})
	}
}

func TestHeadRunStartsNoWork(t *testing.T) {
	s := testServer(requestTestConfig())
	r := httptest.NewRequest("HEAD", "/api/run?tool=ping&family=4&target=1.1.1.1", nil)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
		t.Fatalf("HEAD response = %d, Allow = %q; want 405, GET", w.Code, w.Header().Get("Allow"))
	}
	assertNoAdmission(t, s)
}
