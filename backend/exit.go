package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	wrappedSOL     = "So11111111111111111111111111111111111111112"
	testBuyLamport = 100_000_000 // 0.1 SOL
	minHolderFee   = 5_000_000   // A holder needs some SOL, or a simulated sale fails for the wrong reason.
	exitBudget     = 15 * time.Second
	maxSimWallets  = 3
)

func jupiterQuote(ctx context.Context, from, to string, amount uint64) (json.RawMessage, uint64, error) {
	target := fmt.Sprintf("https://lite-api.jup.ag/swap/v1/quote?inputMint=%s&outputMint=%s&amount=%d&slippageBps=1000", from, to, amount)
	var raw json.RawMessage
	if err := fetch(ctx, "GET", target, nil, &raw); err != nil {
		return nil, 0, err
	}
	var quote struct {
		OutAmount string `json:"outAmount"`
	}
	if err := json.Unmarshal(raw, &quote); err != nil {
		return nil, 0, err
	}
	out, err := strconv.ParseUint(quote.OutAmount, 10, 64)
	return raw, out, err
}

type exitHolder struct {
	wallet string
	amount uint64
}

// exitHolders finds ordinary wallets holding the token with enough SOL to pay a fee.
func exitHolders(ctx context.Context, mint string) ([]exitHolder, error) {
	var largest struct {
		Value []struct {
			Address string `json:"address"`
			Amount  string `json:"amount"`
		} `json:"value"`
	}
	if err := rpc(ctx, "getTokenLargestAccounts", []any{mint}, &largest); err != nil {
		return nil, err
	}
	addresses := []string{}
	for _, account := range largest.Value {
		addresses = append(addresses, account.Address)
	}
	if len(addresses) == 0 {
		return nil, nil
	}
	var accounts struct {
		Value []*struct {
			Data struct {
				Parsed struct {
					Info struct {
						Owner string `json:"owner"`
					} `json:"info"`
				} `json:"parsed"`
			} `json:"data"`
		} `json:"value"`
	}
	if err := rpc(ctx, "getMultipleAccounts", []any{addresses, map[string]any{"encoding": "jsonParsed"}}, &accounts); err != nil {
		return nil, err
	}
	owners, amounts := []string{}, map[string]uint64{}
	for i, account := range accounts.Value {
		if account == nil || i >= len(largest.Value) {
			continue
		}
		owner := account.Data.Parsed.Info.Owner
		held, _ := strconv.ParseUint(largest.Value[i].Amount, 10, 64)
		if held > 0 && amounts[owner] == 0 {
			owners = append(owners, owner)
			amounts[owner] = held
		}
	}
	if len(owners) == 0 {
		return nil, nil
	}
	var ownerAccounts struct {
		Value []*struct {
			Owner    string `json:"owner"`
			Lamports uint64 `json:"lamports"`
		} `json:"value"`
	}
	if err := rpc(ctx, "getMultipleAccounts", []any{owners, map[string]any{"encoding": "base64", "dataSlice": map[string]int{"offset": 0, "length": 0}}}, &ownerAccounts); err != nil {
		return nil, err
	}
	holders := []exitHolder{}
	for i, owner := range owners {
		if i < len(ownerAccounts.Value) && ownerAccounts.Value[i] != nil && ownerAccounts.Value[i].Owner == systemProgram &&
			ownerAccounts.Value[i].Lamports >= minHolderFee && owner != incinerator {
			holders = append(holders, exitHolder{owner, amounts[owner]})
		}
	}
	return holders, nil
}

// simulateSale builds a real sale for a holder and asks the RPC to simulate it. Nothing is signed
// or sent. The result is the simulation's own error, or empty if the sale would have worked.
func simulateSale(ctx context.Context, mint string, holder exitHolder, amount uint64) (string, error) {
	quote, _, err := jupiterQuote(ctx, mint, wrappedSOL, amount)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"quoteResponse": quote, "userPublicKey": holder.wallet, "wrapAndUnwrapSol": true, "dynamicComputeUnitLimit": true})
	var swap struct {
		SwapTransaction string `json:"swapTransaction"`
	}
	if err := fetch(ctx, "POST", "https://lite-api.jup.ag/swap/v1/swap", body, &swap); err != nil {
		return "", err
	}
	var result struct {
		Value struct {
			Err json.RawMessage `json:"err"`
		} `json:"value"`
	}
	options := map[string]any{"encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}
	if err := rpc(ctx, "simulateTransaction", []any{swap.SwapTransaction, options}, &result); err != nil {
		return "", err
	}
	if len(result.Value.Err) == 0 || string(result.Value.Err) == "null" {
		return "", nil
	}
	return string(result.Value.Err), nil
}

// exitVerdict combines what the simulations showed. One failure can be the wallet's own problem,
// so blocked needs at least two failures and no success.
func exitVerdict(passed, failed int) string {
	switch {
	case passed > 0:
		return "sellable"
	case failed >= 2:
		return "blocked"
	}
	return "untested"
}

func exitCheck(ctx context.Context, mint string) map[string]any {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, exitBudget)
	defer cancel()
	event := map[string]any{"stage": "exit"}
	_, bought, err := jupiterQuote(ctx, wrappedSOL, mint, testBuyLamport)
	if err != nil || bought == 0 {
		event["route"] = false
		event["verdict"] = "no route"
		event["ms"] = time.Since(start).Milliseconds()
		return event
	}
	event["route"] = true
	if _, returned, err := jupiterQuote(ctx, mint, wrappedSOL, bought); err == nil && returned > 0 {
		event["roundTripLoss"] = (1 - float64(returned)/testBuyLamport) * 100
	}

	holders, _ := exitHolders(ctx, mint)
	passed, failed := 0, 0
	lastError := ""
	for _, holder := range holders[:min(len(holders), maxSimWallets)] {
		// A small part of the bag, so the test isn't about one huge sale's price impact.
		amount := max(holder.amount/100, 1)
		simErr, err := simulateSale(ctx, mint, holder, amount)
		switch {
		case err != nil:
			continue
		case simErr == "":
			passed++
		default:
			failed++
			lastError = simErr
		}
		if passed > 0 {
			break
		}
	}
	event["simPassed"] = passed
	event["simFailed"] = failed
	event["verdict"] = exitVerdict(passed, failed)
	if lastError != "" {
		event["simError"] = strings.TrimSpace(lastError)
	}
	event["ms"] = time.Since(start).Milliseconds()
	return event
}
