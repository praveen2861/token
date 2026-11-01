package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

const (
	checkBudget   = 12 * time.Second
	systemProgram = "11111111111111111111111111111111"
	incinerator   = "1nc1nerator11111111111111111111111111111111"
)

type safetyFlag struct {
	Name   string `json:"name"`
	Level  string `json:"level"` // ok, caution or danger
	Detail string `json:"detail"`
}

type mintInfo struct {
	MintAuthority   *string `json:"mintAuthority"`
	FreezeAuthority *string `json:"freezeAuthority"`
	Extensions      []struct {
		Extension string         `json:"extension"`
		State     map[string]any `json:"state"`
	} `json:"extensions"`
}

// transferTaxDangerBps is 10%. A smaller tax is still listed, but isn't a red flag.
const transferTaxDangerBps = 1000

// safetyFlags turns a mint account into findings. Empty extension authorities mean it's switched off.
func safetyFlags(info mintInfo, token2022 bool) []safetyFlag {
	flags := []safetyFlag{}
	authority := func(name string, value *string, what string) {
		if value != nil && *value != "" {
			flags = append(flags, safetyFlag{name, "danger", what})
		} else {
			flags = append(flags, safetyFlag{name, "ok", "Revoked"})
		}
	}
	authority("Mint authority", info.MintAuthority, "Active: more tokens can be created")
	authority("Freeze authority", info.FreezeAuthority, "Active: wallets can be frozen")
	if !token2022 {
		return flags
	}
	active := func(state map[string]any, key string) bool {
		text, ok := state[key].(string)
		return ok && text != "" && text != systemProgram
	}
	for _, ext := range info.Extensions {
		switch ext.Extension {
		case "transferFeeConfig", "transferFeeAmount":
			if ext.Extension == "transferFeeAmount" {
				continue
			}
			bps := 0.0
			if newer, ok := ext.State["newerTransferFee"].(map[string]any); ok {
				bps, _ = newer["transferFeeBasisPoints"].(float64)
			}
			if bps > 0 {
				level := "ok"
				if bps >= transferTaxDangerBps {
					level = "danger"
				}
				flags = append(flags, safetyFlag{"Transfer tax", level, fmt.Sprintf("%.2f%% on every transfer", bps/100)})
			}
		case "transferHook":
			if active(ext.State, "programId") {
				flags = append(flags, safetyFlag{"Transfer hook", "danger", "A program runs on every transfer and could block sells"})
			}
		case "permanentDelegate":
			if active(ext.State, "delegate") {
				flags = append(flags, safetyFlag{"Permanent delegate", "danger", "An address can move or burn anyone's tokens"})
			}
		case "nonTransferable":
			flags = append(flags, safetyFlag{"Non-transferable", "danger", "Tokens can't be moved"})
		case "pausable":
			if active(ext.State, "authority") {
				flags = append(flags, safetyFlag{"Pausable", "danger", "Transfers can be paused"})
			}
		case "defaultAccountState":
			if state, _ := ext.State["accountState"].(string); state == "frozen" {
				flags = append(flags, safetyFlag{"Default frozen", "danger", "New accounts start frozen"})
			}
		}
	}
	return flags
}

func safetyCheck(ctx context.Context, mint string) map[string]any {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	report(ctx, "Inspecting the mint account…", 0, 0)
	var out struct {
		Value *struct {
			Owner string `json:"owner"`
			Data  struct {
				Parsed struct {
					Type string   `json:"type"`
					Info mintInfo `json:"info"`
				} `json:"parsed"`
			} `json:"data"`
		} `json:"value"`
	}
	err := rpc(ctx, "getAccountInfo", []any{mint, map[string]any{"encoding": "jsonParsed"}}, &out)
	if err == nil && (out.Value == nil || out.Value.Data.Parsed.Type != "mint") {
		err = errors.New("this address isn't a token mint")
	}
	if err != nil {
		return map[string]any{"stage": "safety", "error": err.Error()}
	}
	token2022 := out.Value.Owner == "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	flags := safetyFlags(out.Value.Data.Parsed.Info, token2022)
	verdict := "No red flags"
	for _, flag := range flags {
		if flag.Level == "danger" {
			verdict = "Red flags"
		}
	}
	return map[string]any{"stage": "safety", "verdict": verdict, "flags": flags, "token2022": token2022, "ms": time.Since(start).Milliseconds()}
}

type holderRow struct {
	Wallet string  `json:"wallet"`
	Pct    float64 `json:"pct"`
}

func holdersVerdict(top10, largest float64) string {
	switch {
	case top10 >= 50 || largest >= 20:
		return "Concentrated"
	case top10 >= 25:
		return "Moderate"
	}
	return "Spread out"
}

// holdersCheck ranks real wallets. Accounts owned by a program, like a bonding curve or pool, and
// burned tokens are left out, so the figures show what people hold.
func holdersCheck(ctx context.Context, mint string, self asset) map[string]any {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	fail := func(err error) map[string]any { return map[string]any{"stage": "holders", "error": err.Error()} }
	report(ctx, "Finding the largest holders…", 0, 3)

	var largest struct {
		Value []struct {
			Address string `json:"address"`
			Amount  string `json:"amount"`
		} `json:"value"`
	}
	var supplyOut struct {
		Value struct {
			Amount string `json:"amount"`
		} `json:"value"`
	}
	if err := rpc(ctx, "getTokenLargestAccounts", []any{mint}, &largest); err != nil {
		return fail(err)
	}
	if err := rpc(ctx, "getTokenSupply", []any{mint}, &supplyOut); err != nil {
		return fail(err)
	}
	supply, _ := strconv.ParseFloat(supplyOut.Value.Amount, 64)
	if supply <= 0 || len(largest.Value) == 0 {
		return fail(errors.New("no holder data for this token"))
	}

	report(ctx, "Separating wallets from pools…", 1, 3)
	addresses := make([]string, len(largest.Value))
	for i, account := range largest.Value {
		addresses[i] = account.Address
	}
	var tokenAccounts struct {
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
	if err := rpc(ctx, "getMultipleAccounts", []any{addresses, map[string]any{"encoding": "jsonParsed"}}, &tokenAccounts); err != nil {
		return fail(err)
	}
	amounts := map[string]float64{}
	owners := []string{}
	for i, account := range tokenAccounts.Value {
		if account == nil || i >= len(largest.Value) {
			continue
		}
		owner := account.Data.Parsed.Info.Owner
		held, _ := strconv.ParseFloat(largest.Value[i].Amount, 64)
		if _, seen := amounts[owner]; !seen {
			owners = append(owners, owner)
		}
		amounts[owner] += held
	}
	var ownerAccounts struct {
		Value []*struct {
			Owner string `json:"owner"`
		} `json:"value"`
	}
	if err := rpc(ctx, "getMultipleAccounts", []any{owners, map[string]any{"encoding": "base64", "dataSlice": map[string]int{"offset": 0, "length": 0}}}, &ownerAccounts); err != nil {
		return fail(err)
	}
	rows := []holderRow{}
	programPct := 0.0
	for i, owner := range owners {
		pct := amounts[owner] / supply * 100
		isWallet := i < len(ownerAccounts.Value) && (ownerAccounts.Value[i] == nil || ownerAccounts.Value[i].Owner == systemProgram)
		if !isWallet || owner == incinerator {
			programPct += pct
			continue
		}
		rows = append(rows, holderRow{owner, pct})
	}
	sort.Slice(rows, func(a, b int) bool { return rows[a].Pct > rows[b].Pct })
	top10, biggest := 0.0, 0.0
	for i, row := range rows {
		if i < 10 {
			top10 += row.Pct
		}
		biggest = max(biggest, row.Pct)
	}

	event := map[string]any{"stage": "holders", "verdict": holdersVerdict(top10, biggest), "top10": top10, "largest": biggest,
		"programPct": programPct, "wallets": rows[:min(len(rows), 10)], "holderCount": self.HolderCount, "dev": self.Dev,
		"ms": time.Since(start).Milliseconds()}
	if self.Dev != "" {
		report(ctx, "Checking the dev's bag…", 2, 3)
		dev := []bundleBuyer{{Wallet: self.Dev}}
		held, known := fillHoldings(ctx, dev, mint, supply)
		event["devKnown"] = known
		event["devPct"] = held
		event["ms"] = time.Since(start).Milliseconds()
	}
	return event
}

type dexPair struct {
	DexID     string `json:"dexId"`
	URL       string `json:"url"`
	Liquidity struct {
		USD float64 `json:"usd"`
	} `json:"liquidity"`
}

func liquidityVerdict(liquidity, mcap float64, onCurve bool) string {
	switch {
	case onCurve:
		return "On bonding curve"
	case liquidity <= 0:
		return "No liquidity found"
	case liquidity < 1000:
		return "Very thin"
	case liquidity < 5000 || (mcap > 0 && liquidity/mcap < 0.05):
		return "Thin"
	}
	return "Healthy"
}

func liquidityCheck(ctx context.Context, mint string, self asset) map[string]any {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, checkBudget)
	defer cancel()
	report(ctx, "Measuring the pools…", 0, 0)
	var pairs []dexPair
	dexErr := fetch(ctx, "GET", "https://api.dexscreener.com/tokens/v1/solana/"+url.PathEscape(mint), nil, &pairs)
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].Liquidity.USD > pairs[b].Liquidity.USD })
	total := 0.0
	for _, pair := range pairs {
		total += pair.Liquidity.USD
	}
	// DexScreener can return only some pools, so a low total there isn't proof of thin liquidity.
	total = max(total, self.Liquidity)
	if dexErr != nil && self.ID == "" {
		return map[string]any{"stage": "liquidity", "error": "Neither DexScreener nor Jupiter responded"}
	}
	onCurve := self.Graduated == "" && self.BondingCurve > 0 && self.BondingCurve < 100
	event := map[string]any{"stage": "liquidity", "verdict": liquidityVerdict(total, self.Mcap, onCurve), "liquidity": total, "mcap": self.Mcap,
		"onCurve": onCurve, "curvePct": self.BondingCurve, "pools": len(pairs), "ms": time.Since(start).Milliseconds()}
	if len(pairs) > 0 {
		event["topDex"] = pairs[0].DexID
		event["topURL"] = pairs[0].URL
	}
	return event
}
