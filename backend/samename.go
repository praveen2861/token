package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	sameNameLimit  = 10
	sameNameBudget = 60 * time.Second
)

// sameNameCheck finds other tokens with the pasted token's exact name or symbol, ignoring case.
// Jupiter's search caps its results, so the total is what it returned, not every copy on chain.
func sameNameCheck(ctx context.Context, self asset) (map[string]any, error) {
	start := time.Now()
	if self.ID == "" {
		return nil, errors.New("Jupiter has no name for this token")
	}
	ctx, cancel := context.WithTimeout(ctx, sameNameBudget)
	defer cancel()
	report(ctx, "Searching for "+self.Name+"…", 0, 0)

	var mu sync.Mutex
	var wg sync.WaitGroup
	matches := map[string]asset{}
	failed := 0
	for _, query := range []string{self.Name, self.Symbol} {
		if query == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, err := assets(ctx, []string{query})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed++
				return
			}
			for _, a := range found {
				same := strings.EqualFold(a.Name, self.Name) || strings.EqualFold(a.Symbol, self.Symbol)
				if same && a.ID != self.ID {
					matches[a.ID] = a
				}
			}
		}()
	}
	wg.Wait()
	if failed == 2 {
		return nil, errors.New("Jupiter search failed")
	}

	list := make([]asset, 0, len(matches))
	for _, a := range matches {
		list = append(list, a)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].CreatedAt > list[b].CreatedAt })
	total := len(list)
	mints := make([]string, 0, sameNameLimit)
	for _, a := range list[:min(total, sameNameLimit)] {
		mints = append(mints, a.ID)
	}
	tokens := []tokenJSON{}
	for start := 0; start < len(mints); start += 10 {
		report(ctx, fmt.Sprintf("Checking ATHs: %d of %d", start, len(mints)), start, len(mints))
		for _, r := range enrich(ctx, mints[start:min(start+10, len(mints))]) {
			tokens = append(tokens, tokenJSON{Mint: r.mint, Symbol: r.symbol, Created: r.created, ATH: r.ath, Now: r.now, Missing: r.missing, Unchecked: r.unchecked})
		}
	}
	sort.SliceStable(tokens, func(a, b int) bool { return tokens[a].Created > tokens[b].Created })
	return map[string]any{"stage": "samename", "name": self.Name, "symbol": self.Symbol, "total": total, "tokens": tokens,
		"partial": failed > 0, "ms": time.Since(start).Milliseconds()}, nil
}
