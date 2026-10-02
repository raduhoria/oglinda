package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseWorldPlaces(t *testing.T) {
	got, err := parseWorldPlaces(" București=Bucharest+eyeball-network ; Frankfurt ;; ")
	if err != nil {
		t.Fatal(err)
	}
	want := []worldPlace{{"București", "Bucharest+eyeball-network"}, {"", "Frankfurt"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"x=", "=", strings.Repeat("a", 101), strings.Repeat("a", 41) + "=b"} {
		if _, err := parseWorldPlaces(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if _, err := parseWorldPlaces(strings.Repeat("a;", 21)); err == nil {
		t.Error("more than 20 places should be refused")
	}
}

func f(v float64) *float64 { return &v }

func measurement(rows ...[3]any) gpMeasurement {
	var m gpMeasurement
	m.Status = "finished"
	for _, r := range rows {
		var row struct {
			Probe  gpProbe `json:"probe"`
			Result struct {
				Status string `json:"status"`
				Stats  struct {
					Avg  *float64 `json:"avg"`
					Loss *float64 `json:"loss"`
				} `json:"stats"`
			} `json:"result"`
		}
		row.Probe = gpProbe{City: r[0].(string), Country: "RO", Network: "net-" + r[0].(string)}
		row.Result.Status = "finished"
		if avg, ok := r[1].(float64); ok {
			row.Result.Stats.Avg = f(avg)
		}
		row.Result.Stats.Loss = f(r[2].(float64))
		m.Results = append(m.Results, row)
	}
	return m
}

func TestSummarize(t *testing.T) {
	m := measurement([3]any{"A", 50.0, 0.0}, [3]any{"B", 20.0, 50.0}, [3]any{"C", 30.0, 25.0}, [3]any{"D", nil, 100.0})
	r, err := summarize(worldPlace{Label: "Here"}, m)
	if err != nil {
		t.Fatal(err)
	}
	// Median of 20/30/50 is the probe in C; D had no average and is skipped.
	if r.Label != "Here" || r.City != "C" || r.Avg != 30 || r.Probes != 3 || r.Loss != 25 {
		t.Fatalf("got %+v", r)
	}
	if r, _ := summarize(worldPlace{}, m); r.Label != "C" {
		t.Fatalf("label falls back to the city, got %q", r.Label)
	}
	if _, err := summarize(worldPlace{}, measurement([3]any{"A", nil, 100.0})); err == nil {
		t.Fatal("no answering probe must be an error")
	}
}

// fakeGlobalping answers like the real API: create returns an id, the first
// poll is still in progress, the second is finished. Magic "fail" gets a 422,
// "limit" a 429.
type fakeGlobalping struct {
	mu    sync.Mutex
	polls map[string]int
	posts int
	auth  string
}

func (g *fakeGlobalping) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Method == http.MethodPost {
		g.posts++
		g.auth = r.Header.Get("Authorization")
		var req struct {
			Target    string `json:"target"`
			Locations []struct {
				Magic string `json:"magic"`
			} `json:"locations"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch magic := req.Locations[0].Magic; magic {
		case "fail":
			http.Error(w, "no probes", http.StatusUnprocessableEntity)
		case "limit":
			http.Error(w, "slow down", http.StatusTooManyRequests)
		default:
			json.NewEncoder(w).Encode(map[string]string{"id": "m-" + magic})
		}
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/measurements/")
	g.polls[id]++
	if g.polls[id] == 1 {
		json.NewEncoder(w).Encode(map[string]string{"status": "in-progress"})
		return
	}
	city := strings.TrimPrefix(id, "m-")
	json.NewEncoder(w).Encode(measurement([3]any{city, 10.0, 0.0}, [3]any{city, 12.0, 0.0}))
}

func testWorld(t *testing.T, places string) (*world, *fakeGlobalping) {
	t.Helper()
	g := &fakeGlobalping{polls: map[string]int{}}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	p, err := parseWorldPlaces(places)
	if err != nil {
		t.Fatal(err)
	}
	return newWorld(&config{
		worldPlaces: p, worldTarget: "203.0.113.5", worldInterval: time.Hour,
		globalpingToken: "tok", globalpingAPI: srv.URL, stateDir: t.TempDir(),
	}), g
}

func TestWorldRefreshAndPersist(t *testing.T) {
	w, g := testWorld(t, "One=one;two")
	w.refresh(context.Background())
	snap := w.snapshot()
	if snap == nil || len(snap.Results) != 2 || snap.Results[0].Label != "One" || snap.Results[1].Label != "two" {
		t.Fatalf("snapshot %+v", snap)
	}
	if snap.Results[0].Avg != 12 { // median of two probes is the upper one
		t.Fatalf("avg %v", snap.Results[0].Avg)
	}
	if g.auth != "Bearer tok" {
		t.Fatalf("token not sent: %q", g.auth)
	}

	// A restart within the interval reuses the saved snapshot without asking
	// Globalping again.
	again := &world{places: w.places, target: w.target, interval: w.interval, api: w.api, stateDir: w.stateDir, client: w.client}
	if s := again.load(); s == nil || len(s.Results) != 2 {
		t.Fatal("saved snapshot not loaded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	posts := g.posts
	go func() { again.run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if g.posts != posts {
		t.Fatal("fresh snapshot should not trigger a measurement")
	}

	// A snapshot for another target is ignored.
	other := &world{target: "203.0.113.6", stateDir: w.stateDir}
	if other.load() != nil {
		t.Fatal("snapshot for another target was loaded")
	}
}

func TestWorldFailureKeepsPrevious(t *testing.T) {
	w, _ := testWorld(t, "Keep=keep;Gone=gone")
	w.refresh(context.Background())

	w.places = []worldPlace{{"Keep", "fail"}, {"Gone", "gone"}}
	w.refresh(context.Background())
	snap := w.snapshot()
	if len(snap.Results) != 2 || snap.Results[0].Label != "Keep" || snap.Results[0].City != "keep" {
		t.Fatalf("failed place should keep its previous row: %+v", snap.Results)
	}
}

func TestWorldRateLimitStops(t *testing.T) {
	w, g := testWorld(t, "a;limit;b")
	w.refresh(context.Background())
	if g.posts != 2 {
		t.Fatalf("%d requests after a 429, want the loop to stop at 2", g.posts)
	}
	if snap := w.snapshot(); snap == nil || len(snap.Results) != 1 {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestHandleWorld(t *testing.T) {
	s := testServer(&config{speedJobs: 1})
	w := httptest.NewRecorder()
	s.handleWorld(w, httptest.NewRequest("GET", "/api/world", nil))
	if !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled: %s", w.Body)
	}

	s.world, _ = testWorld(t, "One=one")
	s.world.refresh(context.Background())
	w = httptest.NewRecorder()
	s.handleWorld(w, httptest.NewRequest("GET", "/api/world", nil))
	var out struct {
		Enabled bool          `json:"enabled"`
		Results []worldResult `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !out.Enabled || len(out.Results) != 1 {
		t.Fatalf("enabled: %s", w.Body)
	}
}
