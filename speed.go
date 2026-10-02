package main

import (
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"os"
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Connection", "close")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mb, err := strconv.Atoi(r.URL.Query().Get("mb"))
	if err != nil || mb < 1 || mb > s.cfg.speedMaxMB {
		http.Error(w, "bad size", http.StatusBadRequest)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(mb<<20))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	release, ok := s.admitSpeed(w, r, mb)
	if !ok {
		return
	}
	defer release()

	rc := http.NewResponseController(w)
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; i < mb; i++ {
		rc.SetWriteDeadline(deadline)
		if _, err := w.Write(payload); err != nil {
			return
		}
	}
}

// POST /speed/up — reads and discards the body, up to the size cap. The size
// must be declared up front so it can be charged to the traffic budgets.
func (s *server) handleSpeedUp(w http.ResponseWriter, r *http.Request) {
	s.handleSpeedUpload(w, r, 60*time.Second, 5*time.Second)
}

func (s *server) handleSpeedUpload(w http.ResponseWriter, r *http.Request, totalTimeout, progressTimeout time.Duration) {
	// If the upload is refused, close the connection instead of letting the
	// server drain a body we did not want.
	w.Header().Set("Connection", "close")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	size := r.ContentLength
	if size < 1 || size > int64(s.cfg.speedMaxMB)<<20 {
		http.Error(w, "bad size", http.StatusBadRequest)
		return
	}
	release, ok := s.admitSpeed(w, r, int((size+1<<20-1)>>20))
	if !ok {
		return
	}
	defer release()

	noStore(w)
	r.Body = http.MaxBytesReader(w, r.Body, size)
	n, err := readUpload(w, r.Body, size, totalTimeout, progressTimeout)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			http.Error(w, "upload timed out", http.StatusRequestTimeout)
		} else {
			http.Error(w, "upload aborted", http.StatusBadRequest)
		}
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strconv.FormatInt(n, 10)))
}

// Each block must arrive before its fixed deadline. A trickle of single bytes
// cannot keep extending a block's wait, and all blocks share an absolute cap.
func readUpload(w http.ResponseWriter, body io.Reader, size int64, totalTimeout, progressTimeout time.Duration) (int64, error) {
	rc := http.NewResponseController(w)
	totalDeadline := time.Now().Add(totalTimeout)
	var buf [64 << 10]byte
	var n int64
	for n < size {
		deadline := time.Now().Add(progressTimeout)
		if totalDeadline.Before(deadline) {
			deadline = totalDeadline
		}
		rc.SetReadDeadline(deadline)
		count := min(int64(len(buf)), size-n)
		read, err := io.ReadFull(body, buf[:int(count)])
		n += int64(read)
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// admitSpeed admits a transfer of mb MiB: a global slot, a per-client turn,
// and mb from both the client's and the global traffic budget. The budgets
// are charged up front and not refunded if the transfer stops early.
func (s *server) admitSpeed(w http.ResponseWriter, r *http.Request, mb int) (func(), bool) {
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
	cost := float64(mb)
	if !s.speedTotal.spend("", cost) {
		s.speedLimit.release(key)
		s.speedSem.release()
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return nil, false
	}
	if !s.speedClient.spend(key, cost) {
		s.speedTotal.refund("", cost)
		s.speedLimit.release(key)
		s.speedSem.release()
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return nil, false
	}
	return func() {
		s.speedLimit.release(key)
		s.speedSem.release()
	}, true
}
