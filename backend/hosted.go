package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	lookupsPerMinute = 30
	maxLookupsAtOnce = 4
)

// loadDotEnv reads KEY=value lines from a local .env file. Real environment variables win.
func loadDotEnv(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	lines := bufio.NewScanner(file)
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		key, value, found := strings.Cut(line, "=")
		if !found || strings.HasPrefix(line, "#") {
			continue
		}
		key = strings.TrimSpace(key)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}
}

// allowOrigins lets pages on the listed comma-separated origins read the API's responses.
func allowOrigins(origins string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, origin := range strings.Split(origins, ",") {
		allowed[strings.TrimRight(strings.TrimSpace(origin), "/")] = true
	}
	delete(allowed, "")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		next.ServeHTTP(w, r)
	})
}

// lookupLimiter protects the shared API quotas: a cap per visitor per minute and a cap overall.
type lookupLimiter struct {
	mu      sync.Mutex
	window  time.Time
	counts  map[string]int
	running int
	perMin  int
	atOnce  int
}

func newLookupLimiter(perMin, atOnce int) *lookupLimiter {
	return &lookupLimiter{counts: map[string]int{}, perMin: perMin, atOnce: atOnce}
}

func (l *lookupLimiter) acquire(visitor string, now time.Time) (ok bool, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.window) >= time.Minute {
		l.window, l.counts = now, map[string]int{}
	}
	if l.counts[visitor] >= l.perMin {
		return false, "Too many lookups this minute. Try again shortly."
	}
	if l.running >= l.atOnce {
		return false, "The lab is busy with other lookups. Try again in a moment."
	}
	l.counts[visitor]++
	l.running++
	return true, ""
}

func (l *lookupLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running--
}

// visitorAddress trusts only the last forwarded address, the one the host's own proxy added.
func visitorAddress(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	return r.RemoteAddr
}

func (l *lookupLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok, message := l.acquire(visitorAddress(r), time.Now())
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"message": message})
			return
		}
		defer l.release()
		next(w, r)
	}
}
