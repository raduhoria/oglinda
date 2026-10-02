package main

import (
	"context"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestResolveTargetLiterals(t *testing.T) {
	extra := mustPrefixes("8.8.8.0/24")
	cases := []struct {
		in     string
		family int
		ok     bool
	}{
		{"1.1.1.1", 4, true},
		{"2606:4700:4700::1111", 6, true},
		{"::ffff:1.1.1.1", 4, true},
		{"1.1.1.1", 6, false},
		{"127.0.0.1", 4, false},
		{"10.0.0.1", 4, false},
		{"192.168.1.1", 4, false},
		{"172.16.0.1", 4, false},
		{"100.64.0.1", 4, false},
		{"169.254.169.254", 4, false},
		{"0.0.0.0", 4, false},
		{"255.255.255.255", 4, false},
		{"224.0.0.1", 4, false},
		{"198.51.100.7", 4, false},
		{"8.8.8.8", 4, false}, // blocked by config
		{"::1", 6, false},
		{"fe80::1", 6, false},
		{"fe80::1%eth0", 6, false},
		{"fd00::1", 6, false},
		{"2001:db8::1", 6, false},
		{"fec0::1", 6, false},   // deprecated site-local
		{"4000::1", 6, false},   // outside 2000::/3
		{"::1.2.3.4", 6, false}, // IPv4-compatible
		{"192.88.99.1", 4, false},
		{"-c 1000 1.1.1.1", 4, false},
		{"1.1.1.1; rm -rf /", 4, false},
		{"$(id)", 4, false},
		{"--help", 4, false},
		{"localhost", 4, false},
		{"", 4, false},
	}
	for _, c := range cases {
		_, err := resolveTarget(context.Background(), c.in, c.family, extra)
		if (err == nil) != c.ok {
			t.Errorf("resolveTarget(%q, %d) err=%v, want ok=%v", c.in, c.family, err, c.ok)
		}
	}
}

func TestHostnameShape(t *testing.T) {
	good := []string{"example.com", "a.b-c.ro", "xn--80ak6aa92e.com", "example.com."}
	bad := []string{"-x.com", "a..com", "a_b.com", "a.com/x", "a.com -c 9", "x", "1.2.3.4.5"}
	for _, h := range good {
		if !hostnameRegex.MatchString(h) {
			t.Errorf("%q should match", h)
		}
	}
	for _, h := range bad {
		if hostnameRegex.MatchString(h) {
			t.Errorf("%q should not match", h)
		}
	}
}

func TestMaskInternal(t *testing.T) {
	in := " 2  10.162.49.62  0.545 ms\n 5  1.1.1.1  3.1 ms"
	want := " 2  [internal]  0.545 ms\n 5  1.1.1.1  3.1 ms"
	if got := maskInternal(in); got != want {
		t.Errorf("got %q", got)
	}
}

func TestClientIP(t *testing.T) {
	s := &server{cfg: &config{trustedProxy: mustPrefixes("127.0.0.1/32")}}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 8.8.8.8")
	if got := s.clientIP(r); got != netip.MustParseAddr("8.8.8.8") {
		t.Errorf("trusted proxy: got %v", got)
	}

	r.RemoteAddr = "9.9.9.9:5000"
	if got := s.clientIP(r); got != netip.MustParseAddr("9.9.9.9") {
		t.Errorf("untrusted peer must ignore XFF: got %v", got)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(60, 2)
	if !l.acquire("a") {
		t.Fatal("first acquire")
	}
	if l.acquire("a") {
		t.Fatal("concurrent acquire must fail")
	}
	l.release("a")
	if !l.acquire("a") {
		t.Fatal("second acquire")
	}
	l.release("a")
	if l.acquire("a") {
		t.Fatal("bucket should be empty")
	}
	if !l.acquire("b") {
		t.Fatal("other key is independent")
	}
}
