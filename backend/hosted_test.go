package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAllowOrigins(t *testing.T) {
	handler := allowOrigins("https://a.test, https://b.test/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for origin, allow := range map[string]string{"https://a.test": "https://a.test", "https://b.test": "https://b.test", "https://evil.test": "", "": ""} {
		req := httptest.NewRequest("GET", "/", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allow {
			t.Errorf("%q: got %q, want %q", origin, got, allow)
		}
	}
}

func TestLookupLimiter(t *testing.T) {
	now := time.Now()
	l := newLookupLimiter(2, 1)
	if ok, _ := l.acquire("a", now); !ok {
		t.Fatal("first lookup refused")
	}
	if ok, _ := l.acquire("b", now); ok {
		t.Fatal("second lookup at once should be refused")
	}
	l.release()
	l.acquire("a", now)
	l.release()
	if ok, _ := l.acquire("a", now); ok {
		t.Fatal("third lookup in a minute should be refused")
	}
	if ok, _ := l.acquire("a", now.Add(time.Minute)); !ok {
		t.Fatal("a new minute should allow lookups")
	}
}

func TestVisitorAddressUsesLastForwarded(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 9.9.9.9")
	if got := visitorAddress(req); got != "9.9.9.9" {
		t.Errorf("got %s", got)
	}
}
