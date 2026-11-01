package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// maxSniperLookups limits the balance checks per token. The largest buyers are checked first.
const maxSniperLookups = 25

type sniperReport struct {
	Verdict   string        `json:"verdict"`
	Snipers   []bundleBuyer `json:"snipers"`
	Wallets   int           `json:"wallets"`
	BoughtPct float64       `json:"boughtPct"`
	HeldPct   float64       `json:"heldPct"`
	HeldKnown bool          `json:"heldKnown"`
	Checked   int           `json:"checked"`
	Complete  bool          `json:"complete"`
	Ms        int64         `json:"ms"`
}

// summarizeSnipers finds wallets that first bought within earlySlots after the launch block.
// Launch-block buyers belong to the bundle check, and the dev isn't a sniper of their own token.
func summarizeSnipers(txs []bundleTx, mint string, supply float64) sniperReport {
	report := sniperReport{Snipers: []bundleBuyer{}}
	if len(txs) == 0 || supply <= 0 {
		return report
	}
	launch := txs[0].Slot
	creator := ""
	if keys := txs[0].Transaction.Message.AccountKeys; len(keys) > 0 {
		creator = keys[0].Pubkey
	}
	inLaunch, bought := map[string]bool{creator: true}, map[string]float64{}
	for i := range txs {
		tx := &txs[i]
		if tx.Slot > launch+earlySlots {
			break
		}
		for wallet, change := range tradesIn(tx, mint) {
			if change <= 0 {
				continue
			}
			if tx.Slot == launch {
				inLaunch[wallet] = true
			} else if !inLaunch[wallet] {
				bought[wallet] += change
			}
		}
	}
	for wallet, tokens := range bought {
		pct := tokens / supply * 100
		report.Snipers = append(report.Snipers, bundleBuyer{Wallet: wallet, Pct: pct})
		report.BoughtPct += pct
	}
	sort.Slice(report.Snipers, func(a, b int) bool { return report.Snipers[a].Pct > report.Snipers[b].Pct })
	report.Wallets = len(report.Snipers)
	return report
}

func sniperVerdict(r sniperReport) string {
	switch {
	case r.Wallets == 0:
		return "No snipers"
	case r.HeldPct >= 10 || r.BoughtPct >= 20:
		return "Heavily sniped"
	case r.Wallets >= 5:
		return "Sniped"
	}
	return "A few snipers"
}

func sniperCheck(ctx context.Context, mint string, launch func() (launchData, error)) (sniperReport, error) {
	start := time.Now()
	report(ctx, "Scoping the first 10 slots…", 0, 2)
	data, err := launch()
	if err != nil {
		return sniperReport{}, err
	}
	result := summarizeSnipers(data.txs, mint, data.supply)
	result.Complete = data.complete
	result.Checked = min(result.Wallets, maxSniperLookups)
	report(ctx, fmt.Sprintf("Checking what %d snipers still hold…", result.Checked), 1, 2)
	checkCtx, cancel := context.WithTimeout(ctx, bundleBudget)
	defer cancel()
	result.HeldPct, result.HeldKnown = fillHoldings(checkCtx, result.Snipers[:result.Checked], mint, data.supply)
	result.Verdict = sniperVerdict(result)
	result.Ms = time.Since(start).Milliseconds()
	return result, nil
}

func sniperEvent(result sniperReport, err error) map[string]any {
	if err != nil {
		return map[string]any{"stage": "sniper", "error": err.Error()}
	}
	raw, _ := json.Marshal(result)
	var event map[string]any
	json.Unmarshal(raw, &event)
	event["stage"] = "sniper"
	return event
}
