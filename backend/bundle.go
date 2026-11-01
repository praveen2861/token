package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"
)

// earlySlots is how far past the launch block a buy still counts as early.
const (
	earlySlots   = 10
	bundlePages  = 5
	bundleBudget = 15 * time.Second
)

type tokenBalance struct {
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	UITokenAmount struct {
		Amount string `json:"amount"`
	} `json:"uiTokenAmount"`
}

type bundleTx struct {
	Slot uint64 `json:"slot"`
	Meta struct {
		Pre  []tokenBalance `json:"preTokenBalances"`
		Post []tokenBalance `json:"postTokenBalances"`
	} `json:"meta"`
	Transaction struct {
		Message struct {
			AccountKeys []struct {
				Pubkey string `json:"pubkey"`
				Signer bool   `json:"signer"`
			} `json:"accountKeys"`
		} `json:"message"`
	} `json:"transaction"`
}

type bundleBuyer struct {
	Wallet  string  `json:"wallet"`
	Pct     float64 `json:"pct"`
	HeldPct float64 `json:"heldPct"`
}

type bundleReport struct {
	Verdict      string        `json:"verdict"`
	Creator      string        `json:"creator"`
	DevPct       float64       `json:"devPct"`
	SameBlock    []bundleBuyer `json:"sameBlock"`
	SameBlockPct float64       `json:"sameBlockPct"`
	HeldPct      float64       `json:"heldPct"`
	HeldKnown    bool          `json:"heldKnown"`
	EarlyWallets int           `json:"earlyWallets"`
	EarlyPct     float64       `json:"earlyPct"`
	Complete     bool          `json:"complete"`
	Ms           int64         `json:"ms"`
}

func amount(b tokenBalance) float64 {
	v, _ := strconv.ParseFloat(b.UITokenAmount.Amount, 64)
	return v
}

// tradesIn returns each wallet's token change in a transaction. Only signers count, which leaves
// out the bonding curve and pool accounts.
func tradesIn(tx *bundleTx, mint string) map[string]float64 {
	deltas := map[string]float64{}
	for _, b := range tx.Meta.Post {
		if b.Mint == mint {
			deltas[b.Owner] += amount(b)
		}
	}
	for _, b := range tx.Meta.Pre {
		if b.Mint == mint {
			deltas[b.Owner] -= amount(b)
		}
	}
	trades := map[string]float64{}
	for _, key := range tx.Transaction.Message.AccountKeys {
		if key.Signer && deltas[key.Pubkey] != 0 {
			trades[key.Pubkey] = deltas[key.Pubkey]
		}
	}
	return trades
}

// fillHoldings fills in what each wallet holds right now, in parallel, and returns the total.
func fillHoldings(ctx context.Context, buyers []bundleBuyer, mint string, supply float64) (float64, bool) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	limit := make(chan struct{}, 8)
	total, known := 0.0, true
	for i := range buyers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			var out struct {
				Value []struct {
					Account struct {
						Data struct {
							Parsed struct {
								Info struct {
									TokenAmount struct {
										Amount string `json:"amount"`
									} `json:"tokenAmount"`
								} `json:"info"`
							} `json:"parsed"`
						} `json:"data"`
					} `json:"account"`
				} `json:"value"`
			}
			err := rpc(ctx, "getTokenAccountsByOwner", []any{buyers[i].Wallet, map[string]any{"mint": mint}, map[string]any{"encoding": "jsonParsed"}}, &out)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				known = false
				return
			}
			for _, account := range out.Value {
				held, _ := strconv.ParseFloat(account.Account.Data.Parsed.Info.TokenAmount.Amount, 64)
				buyers[i].HeldPct += held / supply * 100
				total += held / supply * 100
			}
		}()
	}
	wg.Wait()
	return total, known
}

// summarize turns a mint's oldest transactions into a report. supply is in raw token units.
func summarize(txs []bundleTx, mint string, supply float64) bundleReport {
	report := bundleReport{SameBlock: []bundleBuyer{}}
	if len(txs) == 0 || supply <= 0 {
		return report
	}
	launch := txs[0].Slot
	if keys := txs[0].Transaction.Message.AccountKeys; len(keys) > 0 {
		report.Creator = keys[0].Pubkey
	}
	sameBlock, early := map[string]float64{}, map[string]float64{}
	for i := range txs {
		tx := &txs[i]
		if tx.Slot > launch+earlySlots {
			break
		}
		for wallet, change := range tradesIn(tx, mint) {
			switch {
			case wallet == report.Creator:
				if tx.Slot == launch && change > 0 {
					report.DevPct += change / supply * 100
				}
			case tx.Slot == launch:
				if change > 0 {
					sameBlock[wallet] += change
				}
			default:
				if _, launchBuyer := sameBlock[wallet]; !launchBuyer {
					early[wallet] += change
				}
			}
		}
	}
	for wallet, bought := range sameBlock {
		pct := bought / supply * 100
		report.SameBlock = append(report.SameBlock, bundleBuyer{Wallet: wallet, Pct: pct})
		report.SameBlockPct += pct
	}
	sort.Slice(report.SameBlock, func(a, b int) bool { return report.SameBlock[a].Pct > report.SameBlock[b].Pct })
	// Early figures are net of sells, since these tokens often change hands within seconds.
	for _, net := range early {
		if net > 0 {
			report.EarlyPct += net / supply * 100
			report.EarlyWallets++
		}
	}
	switch count := len(report.SameBlock); {
	case count >= 3 || report.SameBlockPct >= 10:
		report.Verdict = "Likely bundled"
	case count > 0:
		report.Verdict = "Possibly bundled"
	default:
		report.Verdict = "No launch-block buyers"
	}
	return report
}

// reportProgress is report under a name the local report variable below doesn't hide.
var reportProgress = report

// launchData is a token's oldest transactions, shared so the bundle and sniper checks make one fetch.
type launchData struct {
	txs      []bundleTx
	supply   float64
	complete bool
}

// newLaunchFetcher returns a function that fetches once, however many checks call it.
func newLaunchFetcher(ctx context.Context, mint string) func() (launchData, error) {
	return sync.OnceValues(func() (launchData, error) {
		ctx, cancel := context.WithTimeout(ctx, bundleBudget)
		defer cancel()
		supplyDone := make(chan float64, 1)
		go func() {
			var out struct {
				Value struct {
					Amount string `json:"amount"`
				} `json:"value"`
			}
			var supply float64
			if rpc(ctx, "getTokenSupply", []any{mint}, &out) == nil {
				supply, _ = strconv.ParseFloat(out.Value.Amount, 64)
			}
			supplyDone <- supply
		}()

		data := launchData{}
		token := ""
		for page := 0; page < bundlePages; page++ {
			options := map[string]any{"limit": 100, "transactionDetails": "full", "encoding": "jsonParsed",
				"maxSupportedTransactionVersion": 1, "sortOrder": "asc", "filters": map[string]any{"status": "succeeded"}}
			if token != "" {
				options["paginationToken"] = token
			}
			var result struct {
				Data            []bundleTx `json:"data"`
				PaginationToken string     `json:"paginationToken"`
			}
			if err := rpc(ctx, "getTransactionsForAddress", []any{mint, options}, &result); err != nil {
				return data, err
			}
			data.txs = append(data.txs, result.Data...)
			token = result.PaginationToken
			if token == "" || (len(data.txs) > 0 && data.txs[len(data.txs)-1].Slot > data.txs[0].Slot+earlySlots) {
				data.complete = true
				break
			}
		}
		data.supply = <-supplyDone
		if len(data.txs) == 0 {
			return data, errors.New("no transactions found for this token")
		}
		if data.supply <= 0 {
			return data, errors.New("could not read the token supply")
		}
		return data, nil
	})
}

func bundleCheck(ctx context.Context, mint string, launch func() (launchData, error)) (bundleReport, error) {
	start := time.Now()
	report(ctx, "Reading the launch transactions…", 0, 2)
	data, err := launch()
	if err != nil {
		return bundleReport{}, err
	}
	report := summarize(data.txs, mint, data.supply)
	reportProgress(ctx, fmt.Sprintf("Checking what %d launch wallets hold now…", len(report.SameBlock)), 1, 2)
	checkCtx, cancel := context.WithTimeout(ctx, bundleBudget)
	defer cancel()
	report.HeldPct, report.HeldKnown = fillHoldings(checkCtx, report.SameBlock, mint, data.supply)
	report.Complete = data.complete
	report.Ms = time.Since(start).Milliseconds()
	return report, nil
}

// bundleEvent is the page's streaming event for a finished bundle check.
func bundleEvent(report bundleReport, err error) map[string]any {
	if err != nil {
		return map[string]any{"stage": "bundle", "error": err.Error()}
	}
	raw, _ := json.Marshal(report)
	var event map[string]any
	json.Unmarshal(raw, &event)
	event["stage"] = "bundle"
	return event
}
