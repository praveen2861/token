package main

import (
	"context"
	"regexp"
	"time"
)

const (
	feePages  = 5
	feeBudget = 12 * time.Second
)

var collectFeeRe = regexp.MustCompile(`Instruction: Collect\w*CreatorFee\w*`)

type feeTx struct {
	BlockTime int64 `json:"blockTime"`
	Meta      struct {
		LogMessages  []string `json:"logMessages"`
		PreBalances  []int64  `json:"preBalances"`
		PostBalances []int64  `json:"postBalances"`
	} `json:"meta"`
	Transaction struct {
		Message struct {
			AccountKeys []struct {
				Pubkey string `json:"pubkey"`
			} `json:"accountKeys"`
		} `json:"message"`
	} `json:"transaction"`
}

// collectedFee returns the SOL a wallet gained in a creator-fee collection, or false if the
// transaction isn't one. It's the balance change, so it's after that transaction's network fee.
func collectedFee(tx *feeTx, wallet string) (float64, bool) {
	found := false
	for _, line := range tx.Meta.LogMessages {
		if collectFeeRe.MatchString(line) {
			found = true
			break
		}
	}
	if !found {
		return 0, false
	}
	for i, key := range tx.Transaction.Message.AccountKeys {
		if key.Pubkey == wallet && i < len(tx.Meta.PreBalances) && i < len(tx.Meta.PostBalances) {
			if gained := tx.Meta.PostBalances[i] - tx.Meta.PreBalances[i]; gained > 0 {
				return float64(gained) / 1e9, true
			}
		}
	}
	return 0, false
}

// feeCheck totals creator fees in the dev's newest transactions. Busy wallets are only partly read.
func feeCheck(ctx context.Context, dev string) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, feeBudget)
	defer cancel()
	start := time.Now()
	total, claims, scanned := 0.0, 0, 0
	var latest, oldest int64
	token, complete := "", false
	for page := 0; page < feePages; page++ {
		options := map[string]any{"limit": 100, "transactionDetails": "full", "encoding": "jsonParsed",
			"maxSupportedTransactionVersion": 1, "sortOrder": "desc", "filters": map[string]any{"status": "succeeded"}}
		if token != "" {
			options["paginationToken"] = token
		}
		var result struct {
			Data            []feeTx `json:"data"`
			PaginationToken string  `json:"paginationToken"`
		}
		if err := rpc(ctx, "getTransactionsForAddress", []any{dev, options}, &result); err != nil {
			if scanned == 0 {
				return map[string]any{"stage": "fees", "error": err.Error()}
			}
			break
		}
		for i := range result.Data {
			tx := &result.Data[i]
			scanned++
			oldest = tx.BlockTime
			if sol, ok := collectedFee(tx, dev); ok {
				total += sol
				claims++
				latest = max(latest, tx.BlockTime)
			}
		}
		token = result.PaginationToken
		if token == "" {
			complete = true
			break
		}
	}
	return map[string]any{"stage": "fees", "sol": total, "claims": claims, "scanned": scanned, "complete": complete,
		"latest": latest, "oldest": oldest, "ms": time.Since(start).Milliseconds()}
}
