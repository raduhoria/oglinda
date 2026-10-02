package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// World latency: the server periodically asks Globalping (https://globalping.io)
// to ping it from probes around the world, and caches the result. Visitors only
// ever read the cache, so they cannot spend the Globalping quota, and the page
// itself never talks to a third party.

const (
	globalpingAPI   = "https://api.globalping.io/v1"
	probesPerPlace  = 3
	pingPackets     = 4
	maxAPIResponse  = 1 << 20
	stateFileName   = "world.json"
	measureDeadline = 60 * time.Second
)

// worldPlace is one configured location: a label shown on the page and a
// Globalping "magic" location filter, e.g. "București=Bucharest+eyeball-network".
type worldPlace struct {
	Label string
	Magic string
}

func parseWorldPlaces(s string) ([]worldPlace, error) {
	var out []worldPlace
	for _, f := range strings.Split(s, ";") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		label, magic, ok := strings.Cut(f, "=")
		if !ok {
			label, magic = "", f
		}
		label, magic = strings.TrimSpace(label), strings.TrimSpace(magic)
		if magic == "" || len(magic) > 100 || len(label) > 40 {
			return nil, fmt.Errorf("bad location %q", f)
		}
		out = append(out, worldPlace{Label: label, Magic: magic})
	}
	if len(out) > 20 {
		return nil, errors.New("at most 20 locations")
	}
	return out, nil
}

type worldResult struct {
	Label   string  `json:"label"`
	City    string  `json:"city"`
	Country string  `json:"country"`
	Network string  `json:"network"`
	Avg     float64 `json:"avg"`  // ms, median of the probes' averages
	Loss    float64 `json:"loss"` // percent, mean over the probes
	Probes  int     `json:"probes"`
}

type worldSnapshot struct {
	Target     string        `json:"target"`
	MeasuredAt time.Time     `json:"measuredAt"`
	Results    []worldResult `json:"results"`
}

type world struct {
	places   []worldPlace
	target   string
	interval time.Duration
	token    string
	api      string
	stateDir string
	client   *http.Client

	mu   sync.RWMutex
	snap *worldSnapshot
}

func newWorld(cfg *config) *world {
	return &world{
		places:   cfg.worldPlaces,
		target:   cfg.worldTarget,
		interval: cfg.worldInterval,
		token:    cfg.globalpingToken,
		api:      cfg.globalpingAPI,
		stateDir: cfg.stateDir,
		client:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (w *world) enabled() bool { return w != nil && len(w.places) > 0 && w.target != "" }

func (w *world) snapshot() *worldSnapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.snap
}

// run measures now unless a saved snapshot is still fresh, then every interval.
func (w *world) run(ctx context.Context) {
	next := time.Duration(0)
	if snap := w.load(); snap != nil {
		w.mu.Lock()
		w.snap = snap
		w.mu.Unlock()
		if age := time.Since(snap.MeasuredAt); age >= 0 && age < w.interval {
			next = w.interval - age
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
		w.refresh(ctx)
		next = w.interval
	}
}

// refresh measures every place and swaps in the new snapshot. Places that
// fail keep their previous result, so one bad probe does not blank the table.
func (w *world) refresh(ctx context.Context) {
	prev := map[string]worldResult{}
	if old := w.snapshot(); old != nil && old.Target == w.target {
		for _, r := range old.Results {
			prev[r.Label] = r
		}
	}
	snap := &worldSnapshot{Target: w.target, MeasuredAt: time.Now().UTC()}
	failed := 0
	for _, p := range w.places {
		r, err := w.measure(ctx, p)
		if err != nil {
			failed++
			log.Printf("world: %s: %v", p.Magic, err)
			if old, ok := prev[p.Label]; ok && p.Label != "" {
				snap.Results = append(snap.Results, old)
			}
			if errors.Is(err, errRateLimited) || ctx.Err() != nil {
				break
			}
			continue
		}
		snap.Results = append(snap.Results, r)
	}
	if len(snap.Results) == 0 {
		return // keep whatever we had
	}
	w.mu.Lock()
	w.snap = snap
	w.mu.Unlock()
	w.save(snap)
	log.Printf("world: measured %d places, %d failed", len(w.places), failed)
}

func labelFor(p worldPlace, city string) string {
	if p.Label != "" {
		return p.Label
	}
	return city
}

var errRateLimited = errors.New("globalping rate limit reached")

type gpProbe struct {
	City    string `json:"city"`
	Country string `json:"country"`
	Network string `json:"network"`
}

type gpMeasurement struct {
	Status  string `json:"status"`
	Results []struct {
		Probe  gpProbe `json:"probe"`
		Result struct {
			Status string `json:"status"`
			Stats  struct {
				Avg  *float64 `json:"avg"`
				Loss *float64 `json:"loss"`
			} `json:"stats"`
		} `json:"result"`
	} `json:"results"`
}

func (w *world) measure(ctx context.Context, p worldPlace) (worldResult, error) {
	ctx, cancel := context.WithTimeout(ctx, measureDeadline)
	defer cancel()

	body, _ := json.Marshal(map[string]any{
		"type":               "ping",
		"target":             w.target,
		"locations":          []map[string]any{{"magic": p.Magic, "limit": probesPerPlace}},
		"measurementOptions": map[string]any{"packets": pingPackets},
	})
	var created struct {
		ID string `json:"id"`
	}
	if err := w.call(ctx, http.MethodPost, w.api+"/measurements", body, &created); err != nil {
		return worldResult{}, err
	}
	if created.ID == "" || strings.ContainsAny(created.ID, "/?#") {
		return worldResult{}, errors.New("bad measurement id")
	}

	for {
		select {
		case <-ctx.Done():
			return worldResult{}, ctx.Err()
		case <-time.After(time.Second):
		}
		var m gpMeasurement
		if err := w.call(ctx, http.MethodGet, w.api+"/measurements/"+created.ID, nil, &m); err != nil {
			return worldResult{}, err
		}
		if m.Status == "in-progress" {
			continue
		}
		return summarize(p, m)
	}
}

func (w *world) call(ctx context.Context, method, url string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "oglinda (+https://github.com/raduhoria/oglinda)")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return errRateLimited
	case resp.StatusCode >= 300:
		return fmt.Errorf("globalping: HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}

// summarize reduces a place's probes to one row: the median of their average
// round-trip times, which keeps a single odd probe from setting the number.
func summarize(p worldPlace, m gpMeasurement) (worldResult, error) {
	type probe struct {
		gpProbe
		avg, loss float64
	}
	var ok []probe
	for _, r := range m.Results {
		s := r.Result.Stats
		if r.Result.Status != "finished" || s.Avg == nil || s.Loss == nil || *s.Avg < 0 || *s.Avg > 10000 {
			continue
		}
		ok = append(ok, probe{r.Probe, *s.Avg, min(max(*s.Loss, 0), 100)})
	}
	if len(ok) == 0 {
		return worldResult{}, errors.New("no probe answered")
	}
	slices.SortFunc(ok, func(a, b probe) int {
		switch {
		case a.avg < b.avg:
			return -1
		case a.avg > b.avg:
			return 1
		}
		return 0
	})
	mid := ok[len(ok)/2]
	loss := 0.0
	for _, pr := range ok {
		loss += pr.loss
	}
	return worldResult{
		Label:   labelFor(p, clip(mid.City, 40)),
		City:    clip(mid.City, 40),
		Country: clip(mid.Country, 2),
		Network: clip(mid.Network, 40),
		Avg:     mid.avg,
		Loss:    loss / float64(len(ok)),
		Probes:  len(ok),
	}, nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (w *world) statePath() string {
	if w.stateDir == "" {
		return ""
	}
	return filepath.Join(w.stateDir, stateFileName)
}

func (w *world) load() *worldSnapshot {
	path := w.statePath()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var snap worldSnapshot
	if json.Unmarshal(data, &snap) != nil || snap.Target != w.target {
		return nil
	}
	return &snap
}

func (w *world) save(snap *worldSnapshot) {
	path := w.statePath()
	if path == "" {
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("world: save: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("world: save: %v", err)
	}
}

// GET /api/world
func (s *server) handleWorld(w http.ResponseWriter, r *http.Request) {
	if !s.world.enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	out := map[string]any{
		"enabled":         true,
		"intervalMinutes": int(s.world.interval / time.Minute),
		"source":          map[string]string{"name": "Globalping", "url": "https://globalping.io"},
	}
	if snap := s.world.snapshot(); snap != nil {
		out["target"] = snap.Target
		out["measuredAt"] = snap.MeasuredAt
		out["results"] = snap.Results
	} else {
		out["results"] = []worldResult{}
	}
	writeJSON(w, http.StatusOK, out)
}
