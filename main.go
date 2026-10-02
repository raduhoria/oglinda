// oglinda is a small, self-contained looking glass: ping, traceroute and an
// in-browser speed test, with no shell, no dependencies and no privileges.
package main

import (
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type config struct {
	listen        string
	trustedProxy  []netip.Prefix
	brand         string
	brandURL      string
	location      string
	datacenter    string
	testIPv4      string
	testIPv6      string
	pingBin       string
	tracerouteBin string
	maskInternal  bool
	blocked       []netip.Prefix
	perMinute     int
	maxJobs       int
	speedMaxMB    int
	speedJobs     int
	speedClientMB int // per client, per hour
	speedTotalMB  int // all clients, per minute
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func parsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, err
			}
			f = netip.PrefixFrom(a, a.BitLen()).String()
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func loadConfig() (*config, error) {
	c := &config{}
	var trusted, blocked string
	flag.StringVar(&c.listen, "listen", env("LG_LISTEN", "127.0.0.1:8090"), "address to listen on")
	flag.StringVar(&trusted, "trusted-proxy", env("LG_TRUSTED_PROXY", "127.0.0.1,::1"), "proxies whose X-Forwarded-For is trusted")
	flag.StringVar(&c.brand, "brand", env("LG_BRAND", "Looking Glass"), "name shown in the header")
	flag.StringVar(&c.brandURL, "brand-url", env("LG_BRAND_URL", ""), "link for the brand name")
	flag.StringVar(&c.location, "location", env("LG_LOCATION", ""), "city, country")
	flag.StringVar(&c.datacenter, "datacenter", env("LG_DATACENTER", ""), "datacenter description")
	flag.StringVar(&c.testIPv4, "ipv4", env("LG_IPV4", ""), "public IPv4 to advertise for tests")
	flag.StringVar(&c.testIPv6, "ipv6", env("LG_IPV6", ""), "public IPv6 to advertise for tests")
	flag.StringVar(&c.pingBin, "ping", env("LG_PING", "/usr/bin/ping"), "ping binary")
	flag.StringVar(&c.tracerouteBin, "traceroute", env("LG_TRACEROUTE", "/usr/bin/traceroute"), "traceroute binary")
	flag.BoolVar(&c.maskInternal, "mask-internal", env("LG_MASK_INTERNAL", "true") == "true", "hide non-public hops in output")
	flag.StringVar(&blocked, "block", env("LG_BLOCK", ""), "extra prefixes that may not be targeted")
	flag.IntVar(&c.perMinute, "per-minute", envInt("LG_PER_MINUTE", 6), "network tests per client per minute")
	flag.IntVar(&c.maxJobs, "max-jobs", envInt("LG_MAX_JOBS", 4), "concurrent network tests, all clients")
	flag.IntVar(&c.speedMaxMB, "speed-max-mb", envInt("LG_SPEED_MAX_MB", 100), "largest speed test transfer, MiB")
	flag.IntVar(&c.speedJobs, "speed-jobs", envInt("LG_SPEED_JOBS", 3), "concurrent speed transfers, all clients")
	flag.IntVar(&c.speedClientMB, "speed-client-mb", envInt("LG_SPEED_CLIENT_MB", 500), "speed test traffic per client per hour, MiB")
	flag.IntVar(&c.speedTotalMB, "speed-total-mb", envInt("LG_SPEED_TOTAL_MB", 1000), "speed test traffic for all clients per minute, MiB")
	flag.Parse()

	for name, v := range map[string]int{
		"per-minute": c.perMinute, "max-jobs": c.maxJobs, "speed-max-mb": c.speedMaxMB,
		"speed-jobs": c.speedJobs, "speed-client-mb": c.speedClientMB, "speed-total-mb": c.speedTotalMB,
	} {
		if v < 1 {
			return nil, fmt.Errorf("%s must be at least 1", name)
		}
	}
	if c.speedMaxMB > c.speedClientMB || c.speedMaxMB > c.speedTotalMB {
		return nil, fmt.Errorf("speed-max-mb must not exceed speed-client-mb or speed-total-mb")
	}

	var err error
	if c.trustedProxy, err = parsePrefixes(trusted); err != nil {
		return nil, fmt.Errorf("trusted-proxy: %w", err)
	}
	if c.blocked, err = parsePrefixes(blocked); err != nil {
		return nil, fmt.Errorf("block: %w", err)
	}
	for _, bin := range []string{c.pingBin, c.tracerouteBin} {
		if _, err := os.Stat(bin); err != nil {
			return nil, fmt.Errorf("missing tool: %w", err)
		}
	}
	return c, nil
}

type server struct {
	cfg        *config
	limit      *limiter
	jobs       semaphore
	speedLimit *limiter
	speedSem   semaphore
	// MiB budgets for speed test traffic, per client and for all clients.
	speedClient *limiter
	speedTotal  *limiter
}

// clientIP trusts X-Forwarded-For only from a configured proxy, and then only
// its right-most entry: the address the proxy itself saw.
func (s *server) clientIP(r *http.Request) netip.Addr {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	peer := ap.Addr().Unmap()
	for _, p := range s.cfg.trustedProxy {
		if p.Contains(peer) {
			xff := r.Header.Values("X-Forwarded-For")
			if len(xff) == 0 {
				break
			}
			parts := strings.Split(xff[len(xff)-1], ",")
			if a, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
				return a.Unmap()
			}
			break
		}
	}
	return peer
}

// clientKey groups IPv6 clients by /64, since one host usually controls
// the whole prefix.
func clientKey(a netip.Addr) string {
	if a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// GET /api/info
func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	family := 4
	if ip.Is6() {
		family = 6
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"brand":      s.cfg.brand,
		"brandURL":   s.cfg.brandURL,
		"location":   s.cfg.location,
		"datacenter": s.cfg.datacenter,
		"ipv4":       s.cfg.testIPv4,
		"ipv6":       s.cfg.testIPv6,
		"tools":      []string{"ping", "traceroute"},
		"speedMaxMB": s.cfg.speedMaxMB,
		"client":     map[string]any{"ip": ip.String(), "family": family},
	})
}

// GET /api/run?tool=ping&target=1.1.1.1&family=4
// Streams the output as server-sent events: "line" per output line, then a
// single "done" or "error".
func (s *server) handleRun(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := q.Get("tool")
	if _, ok := tools[name]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown_tool"})
		return
	}
	family, _ := strconv.Atoi(q.Get("family"))
	if family != 4 && family != 6 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_family"})
		return
	}
	// Take a global slot and a client token before resolving, so DNS lookups
	// count against both limits. The slot comes first so that a busy server
	// does not cost the client a token.
	if !s.jobs.tryAcquire() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "busy"})
		return
	}
	defer s.jobs.release()
	key := clientKey(s.clientIP(r))
	if !s.limit.acquire(key) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate_limited"})
		return
	}
	defer s.limit.release(key)

	target, err := resolveTarget(r.Context(), q.Get("target"), family, s.cfg.blocked)
	if err != nil {
		code := "bad_target"
		switch {
		case errors.Is(err, errNotPublic):
			code = "not_public"
		case errors.Is(err, errNoAddress):
			code = "no_address"
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
		return
	}

	// The server's ReadTimeout would otherwise cancel the request context, and
	// with it the tool, partway through a long traceroute.
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(90 * time.Second)
	rc.SetReadDeadline(deadline)
	rc.SetWriteDeadline(deadline)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event, data string) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		rc.Flush()
	}

	log.Printf("%s %s %s", name, target, key)
	send("target", target.String())
	err = run(r.Context(), s.cfg, name, family, target, func(line string) { send("line", line) })
	if err != nil && r.Context().Err() == nil {
		// ping exits non-zero when nothing answers; that is a result, not a failure.
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			send("done", "exit "+strconv.Itoa(exit.ExitCode()))
			return
		}
		send("error", "failed")
		return
	}
	send("done", "ok")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func main() {
	log.SetFlags(0)
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	s := &server{
		cfg:         cfg,
		limit:       newLimiter(float64(cfg.perMinute), cfg.perMinute),
		jobs:        make(semaphore, cfg.maxJobs),
		speedLimit:  newLimiter(6, 6),
		speedSem:    make(semaphore, cfg.speedJobs),
		speedClient: newLimiter(float64(cfg.speedClientMB)/60, cfg.speedClientMB),
		speedTotal:  newLimiter(float64(cfg.speedTotalMB), cfg.speedTotalMB),
	}

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /api/run", s.handleRun)
	mux.HandleFunc("GET /speed/ping", s.handleSpeedPing)
	mux.HandleFunc("GET /speed/down", s.handleSpeedDown)
	mux.HandleFunc("POST /speed/up", s.handleSpeedUp)

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		// Bounds reading a request body, including the one the server drains
		// after a handler that ignored it. Uploads extend their own deadline.
		ReadTimeout:    10 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 8 << 10,
	}
	log.Printf("oglinda listening on %s", ln.Addr())
	log.Fatal(srv.Serve(ln))
}
