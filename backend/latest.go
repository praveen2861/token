package main

import (
	"context"
	"time"
)

const (
	latestLimit = 10
	shortBudget = 1500 * time.Millisecond
)

// scanLatest reads the wallet newest-first and stops at the first limit tokens other than skip,
// so a busy wallet costs a few calls instead of its whole history. done means it hit the limit
// or the end of the history, rather than running out of time.
func scanLatest(ctx context.Context, dev, skip string, limit int, onProgress func(mint string, scanned int)) (scanResult, error) {
	result := scanResult{}
	seen := map[string]bool{}
	token := ""
	for {
		options := map[string]any{"limit": 100, "transactionDetails": "full", "encoding": "jsonParsed",
			"maxSupportedTransactionVersion": 1, "sortOrder": "desc", "filters": map[string]any{"status": "succeeded"}}
		if token != "" {
			options["paginationToken"] = token
		}
		var page fastPage
		if err := rpc(ctx, "getTransactionsForAddress", []any{dev, options}, &page); err != nil {
			if result.scanned == 0 && ctx.Err() == nil {
				return result, err
			}
			result.failed++
			return result, nil
		}
		for i := range page.Data {
			item := &page.Data[i]
			result.scanned++
			for _, mint := range mintsCreated(&item.transaction, dev) {
				if seen[mint] || mint == skip {
					continue
				}
				seen[mint] = true
				result.mints = append(result.mints, mint)
				onProgress(mint, result.scanned)
				if len(result.mints) == limit {
					result.done = true
					return result, nil
				}
			}
		}
		onProgress("", result.scanned)
		token = page.PaginationToken
		if token == "" {
			result.done = true
			return result, nil
		}
	}
}

// selfAsset looks up the pasted token, falling back to Jupiter's other host when the first is
// rate limiting, so one busy endpoint doesn't lose the name and dev.
func selfAsset(ctx context.Context, mint string) asset {
	find := func(target string, budgetCtx context.Context) (asset, bool) {
		var out []asset
		if fetch(budgetCtx, "GET", target+mint, nil, &out) != nil {
			return asset{}, false
		}
		for _, a := range out {
			if a.ID == mint {
				return a, true
			}
		}
		return asset{}, false
	}
	first, cancelFirst := context.WithTimeout(ctx, shortBudget)
	defer cancelFirst()
	if a, ok := find(jupiter+"/v1/assets/search?query=", first); ok {
		return a
	}
	second, cancelSecond := context.WithTimeout(ctx, shortBudget)
	defer cancelSecond()
	a, _ := find("https://lite-api.jup.ag/tokens/v2/search?query=", second)
	return a
}
