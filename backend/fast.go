package main

import (
	"context"
	"errors"
	"sync"
)

// fastPage is one getTransactionsForAddress response: up to 100 full transactions per call.
type fastPage struct {
	Data []struct {
		transaction
		BlockTime int64 `json:"blockTime"`
	} `json:"data"`
	PaginationToken string `json:"paginationToken"`
}

func fastCall(ctx context.Context, dev string, gte, lte uint64, token string) (fastPage, error) {
	options := map[string]any{
		"limit":                          100,
		"transactionDetails":             "full",
		"encoding":                       "jsonParsed",
		"maxSupportedTransactionVersion": 1,
		"sortOrder":                      "asc",
		"filters": map[string]any{
			"status": "succeeded",
			"slot":   map[string]any{"gte": gte, "lte": lte},
		},
	}
	if token != "" {
		options["paginationToken"] = token
	}
	var page fastPage
	err := rpc(ctx, "getTransactionsForAddress", []any{dev, options}, &page)
	return page, err
}

// firstSlot finds the wallet's oldest successful transaction, which bounds the slot range to split.
func firstSlot(ctx context.Context, dev string) (uint64, bool, error) {
	var page struct {
		Data []struct {
			Slot uint64 `json:"slot"`
		} `json:"data"`
	}
	options := map[string]any{"limit": 1, "transactionDetails": "signatures", "sortOrder": "asc", "filters": map[string]any{"status": "succeeded"}}
	if err := rpc(ctx, "getTransactionsForAddress", []any{dev, options}, &page); err != nil {
		return 0, false, err
	}
	if len(page.Data) == 0 {
		return 0, false, nil
	}
	return page.Data[0].Slot, true, nil
}

// scanWalletFast splits the wallet's slot range into shards and pages through each in parallel.
// It reports an error only when the method itself is unavailable, so callers can fall back.
// onProgress, if set, hears about each new mint as it's found and about the running transaction count.
func scanWalletFast(ctx context.Context, dev string, shards int, onProgress func(mint string, scanned int)) (scanResult, error) {
	start, found, err := firstSlot(ctx, dev)
	if err != nil {
		return scanResult{}, err
	}
	if !found {
		return scanResult{done: true}, nil
	}
	var end uint64
	if err := rpc(ctx, "getSlot", []any{}, &end); err != nil {
		return scanResult{}, err
	}
	width := (end-start)/uint64(shards) + 1

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		seen   = map[string]bool{}
		result = scanResult{done: true}
	)
	for i := range shards {
		gte := start + uint64(i)*width
		lte := gte + width - 1
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := ""
			for {
				page, err := fastCall(ctx, dev, gte, lte, token)
				mu.Lock()
				if err != nil {
					result.failed++
					result.done = false
					mu.Unlock()
					return
				}
				for j := range page.Data {
					item := &page.Data[j]
					result.scanned++
					if result.oldest == 0 || item.BlockTime < result.oldest {
						result.oldest = item.BlockTime
					}
					for _, mint := range mintsCreated(&item.transaction, dev) {
						if !seen[mint] {
							seen[mint] = true
							result.mints = append(result.mints, mint)
							if onProgress != nil {
								onProgress(mint, result.scanned)
							}
						}
					}
				}
				if onProgress != nil {
					onProgress("", result.scanned)
				}
				mu.Unlock()
				if page.PaginationToken == "" {
					return
				}
				token = page.PaginationToken
			}
		}()
	}
	wg.Wait()
	if !result.done {
		result.oldest = 0 // shards finish unevenly, so no single date marks where coverage ends
	}
	return result, nil
}

// creatorFast reads the fee payer of the mint's oldest transaction in one call.
func creatorFast(ctx context.Context, mint string) (string, error) {
	var page fastPage
	options := map[string]any{"limit": 1, "transactionDetails": "full", "encoding": "jsonParsed", "maxSupportedTransactionVersion": 1, "sortOrder": "asc"}
	if err := rpc(ctx, "getTransactionsForAddress", []any{mint, options}, &page); err != nil {
		return "", err
	}
	if len(page.Data) == 0 || len(page.Data[0].Transaction.Message.AccountKeys) == 0 {
		return "", errors.New("no transactions for this mint")
	}
	return page.Data[0].Transaction.Message.AccountKeys[0].Pubkey, nil
}
