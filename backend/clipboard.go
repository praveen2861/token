package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

var (
	wholeAddressRe = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{32,44}$`)
	linkRe         = regexp.MustCompile(`^https?://\S*?([1-9A-HJ-NP-Za-km-z]{32,44})\S*$`)
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// isSolanaKey checks the text decodes to exactly 32 bytes, which ordinary words and most other
// strings of this length don't.
func isSolanaKey(text string) bool {
	var decoded []byte
	for _, c := range text {
		carry := strings.IndexRune(base58Alphabet, c)
		if carry < 0 {
			return false
		}
		for i := range decoded {
			carry += int(decoded[i]) * 58
			decoded[i] = byte(carry)
			carry >>= 8
		}
		for carry > 0 {
			decoded = append(decoded, byte(carry))
			carry >>= 8
		}
	}
	for _, c := range text {
		if c != '1' {
			break
		}
		decoded = append(decoded, 0)
	}
	return len(decoded) == 32
}

// addressFromClipboard only accepts text that is one address or one link holding one, so copying
// a paragraph that mentions an address doesn't start a lookup.
func addressFromClipboard(text string) string {
	text = strings.TrimSpace(text)
	if wholeAddressRe.MatchString(text) && isSolanaKey(text) {
		return text
	}
	if m := linkRe.FindStringSubmatch(text); m != nil && isSolanaKey(m[1]) {
		return m[1]
	}
	return ""
}

// clipboardHub passes copied addresses to every open page.
type clipboardHub struct {
	mu   sync.Mutex
	subs map[chan string]bool
}

func (h *clipboardHub) publish(address string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub <- address:
		default:
		}
	}
}

func (h *clipboardHub) handle(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher.Flush()
	sub := make(chan string, 4)
	h.mu.Lock()
	h.subs[sub] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, sub)
		h.mu.Unlock()
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case address := <-sub:
			fmt.Fprintf(w, "data: %s\n\n", address)
			flusher.Flush()
		}
	}
}
