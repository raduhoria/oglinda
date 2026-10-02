package main

import (
	"crypto/rand"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Random, incompressible payload generated once at startup and streamed in a
// loop, so a download test costs no disk and almost no CPU.
var payload = func() []byte {
	b := make([]byte, 1<<20)
	rand.Read(b)
	return b
}()

func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// GET /speed/ping — smallest possible response, used by the browser to
// measure round-trip time.
func (s *server) handleSpeedPing(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.WriteHeader(http.StatusNoContent)
}

// GET /speed/down?mb=N — streams N MiB of random bytes.
func (s *server) handleSpeedDown(w http.ResponseWriter, r *http.Request) {
	mb, err := strconv.Atoi(r.URL.Query().Get("mb"))
	if err != nil || mb < 1 || mb > s.cfg.speedMaxMB {
		http.Error(w, "bad size", http.StatusBadRequest)
		return
	}
	release, ok := s.admitSpeed(w, r)
	if !ok {
		return
	}
	defer release()

	noStore(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(mb<<20))
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; i < mb; i++ {
		rc.SetWriteDeadline(deadline)
		if _, err := w.Write(payload); err != nil {
			return
		}
	}
}

// POST /speed/up — reads and discards the body, up to the size cap.
func (s *server) handleSpeedUp(w http.ResponseWriter, r *http.Request) {
	release, ok := s.admitSpeed(w, r)
	if !ok {
		return
	}
	defer release()

	noStore(w)
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.speedMaxMB)<<20)
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(60 * time.Second))
	n, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		http.Error(w, "upload aborted", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strconv.FormatInt(n, 10)))
}

func (s *server) admitSpeed(w http.ResponseWriter, r *http.Request) (func(), bool) {
	key := clientKey(s.clientIP(r))
	if !s.speedSem.tryAcquire() {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return nil, false
	}
	if !s.speedLimit.acquire(key) {
		s.speedSem.release()
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return nil, false
	}
	return func() {
		s.speedLimit.release(key)
		s.speedSem.release()
	}, true
}
