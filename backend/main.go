// devath: paste a Solana mint, get the dev wallet, their other tokens and each token's ATH.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	jupiter = "https://datapi.jup.ag"
)

var (
	rpcURL    string
	client    = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 128, ForceAttemptHTTP2: true}}
	addressRe = regexp.MustCompile(`[1-9A-HJ-NP-Za-km-z]{32,44}`)
)

type asset struct {
	ID                string     `json:"id"`
	Name              string     `json:"name"`
	Twitter           string     `json:"twitter"`
	Website           string     `json:"website"`
	Telegram          string     `json:"telegram"`
	Symbol            string     `json:"symbol"`
	Dev               string     `json:"dev"`
	Launchpad         string     `json:"launchpad"`
	CreatedAt         string     `json:"createdAt"`
	Mcap              float64    `json:"mcap"`
	Graduated         string     `json:"graduatedPool"`
	HolderCount       int        `json:"holderCount"`
	Stats24h          tradeStats `json:"stats24h"`
	OrganicScoreLabel string     `json:"organicScoreLabel"`
	Liquidity         float64    `json:"liquidity"`
	BondingCurve      float64    `json:"bondingCurve"`
	FDV               float64    `json:"fdv"`
	Audit             struct {
		DevMints      int `json:"devMints"`
		DevMigrations int `json:"devMigrations"`
	} `json:"audit"`
}

type signature struct {
	Signature string          `json:"signature"`
	BlockTime int64           `json:"blockTime"`
	Err       json.RawMessage `json:"err"`
}

type instruction struct {
	Parsed json.RawMessage `json:"parsed"`
}

type transaction struct {
	Meta struct {
		InnerInstructions []struct {
			Instructions []instruction `json:"instructions"`
		} `json:"innerInstructions"`
	} `json:"meta"`
	Transaction struct {
		Message struct {
			AccountKeys []struct {
				Pubkey string `json:"pubkey"`
			} `json:"accountKeys"`
			Instructions []instruction `json:"instructions"`
		} `json:"message"`
	} `json:"transaction"`
}

// fetch retries rate limits and transient failures until ctx expires.
func fetch(ctx context.Context, method, target string, body []byte, out any) error {
	for attempt := 0; ; attempt++ {
		if err := waitForGate(ctx, target); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "devath/1.0")
		resp, err := client.Do(req)
		if err == nil {
			data, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && readErr == nil {
				return json.Unmarshal(data, out)
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				if seconds, parseErr := strconv.Atoi(resp.Header.Get("Retry-After")); parseErr == nil {
					pauseUntil(target, time.Now().Add(time.Duration(seconds)*time.Second))
				}
			}
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return fmt.Errorf("HTTP %d", resp.StatusCode)
			}
		}
		if ctx.Err() == nil {
			reportRetry(ctx, "Source is busy, retrying…")
		}
		wait := time.Duration(250<<min(attempt, 4))*time.Millisecond + time.Duration(rand.Int64N(100))*time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// gates holds the time each host asked us to wait until, so one 429 pauses every worker for that host.
var (
	gateMu sync.Mutex
	gates  = map[string]time.Time{}
)

func gateKey(target string) string {
	if parsed, err := url.Parse(target); err == nil {
		return parsed.Host
	}
	return target
}

func pauseUntil(target string, until time.Time) {
	gateMu.Lock()
	defer gateMu.Unlock()
	if until.After(gates[gateKey(target)]) {
		gates[gateKey(target)] = until
	}
}

func waitForGate(ctx context.Context, target string) error {
	gateMu.Lock()
	until := gates[gateKey(target)]
	gateMu.Unlock()
	if wait := time.Until(until); wait > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return ctx.Err()
}

func rpc(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := fetch(ctx, http.MethodPost, rpcURL, body, &envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return errors.New(envelope.Error.Message)
	}
	return json.Unmarshal(envelope.Result, out)
}

func assets(ctx context.Context, mints []string) ([]asset, error) {
	var out []asset
	err := fetch(ctx, http.MethodGet, jupiter+"/v1/assets/search?query="+url.QueryEscape(strings.Join(mints, ",")), nil, &out)
	return out, err
}

// athCache keeps ATHs for a minute, so re-checking a token doesn't use up Jupiter's rate limit.
const athCacheTTL = time.Minute

type athEntry struct {
	value float64
	at    time.Time
}

var athCache sync.Map

// ath returns the highest market cap across the token's daily candles.
func ath(ctx context.Context, mint string) (float64, error) {
	if cached, ok := athCache.Load(mint); ok {
		if entry := cached.(athEntry); time.Since(entry.at) < athCacheTTL {
			return entry.value, nil
		}
	}
	var out struct {
		Candles []struct {
			High float64 `json:"high"`
		} `json:"candles"`
	}
	target := fmt.Sprintf("%s/v2/charts/%s?interval=1_DAY&to=%d&candles=1000&type=mcap&quote=usd", jupiter, mint, time.Now().UnixMilli())
	if err := fetch(ctx, http.MethodGet, target, nil, &out); err != nil {
		return 0, err
	}
	best := 0.0
	for _, c := range out.Candles {
		best = max(best, c.High)
	}
	athCache.Store(mint, athEntry{best, time.Now()})
	return best, nil
}

func signatures(ctx context.Context, address, before string) ([]signature, error) {
	options := map[string]any{"limit": 1000}
	if before != "" {
		options["before"] = before
	}
	var out []signature
	err := rpc(ctx, "getSignaturesForAddress", []any{address, options}, &out)
	return out, err
}

func getTransaction(ctx context.Context, sig string) (*transaction, error) {
	var tx *transaction
	err := rpc(ctx, "getTransaction", []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 1}}, &tx)
	if err == nil && tx == nil {
		err = errors.New("transaction not found")
	}
	return tx, err
}

// creatorFromChain is the fallback when Jupiter has no dev: the fee payer of the mint's oldest transaction.
func creatorFromChain(ctx context.Context, mint string) (string, error) {
	if payer, err := creatorFast(ctx, mint); err == nil {
		return payer, nil
	}
	var oldest, before string
	for page := 0; page < 20; page++ {
		sigs, err := signatures(ctx, mint, before)
		if err != nil {
			return "", err
		}
		if len(sigs) == 0 {
			break
		}
		oldest, before = sigs[len(sigs)-1].Signature, sigs[len(sigs)-1].Signature
		if len(sigs) < 1000 {
			tx, err := getTransaction(ctx, oldest)
			if err != nil {
				return "", err
			}
			return tx.Transaction.Message.AccountKeys[0].Pubkey, nil
		}
	}
	return "", errors.New("mint history too long to reach its first transaction")
}

func mintsCreated(tx *transaction, dev string) []string {
	message := tx.Transaction.Message
	if len(message.AccountKeys) == 0 || message.AccountKeys[0].Pubkey != dev {
		return nil
	}
	instructions := message.Instructions
	for _, inner := range tx.Meta.InnerInstructions {
		instructions = append(instructions, inner.Instructions...)
	}
	var mints []string
	for _, ins := range instructions {
		var parsed struct {
			Type string `json:"type"`
			Info struct {
				Mint string `json:"mint"`
			} `json:"info"`
		}
		if json.Unmarshal(ins.Parsed, &parsed) == nil && strings.HasPrefix(parsed.Type, "initializeMint") {
			mints = append(mints, parsed.Info.Mint)
		}
	}
	return mints
}

type scanResult struct {
	mints   []string
	scanned int
	failed  int
	oldest  int64
	done    bool
}

// scanWallet walks the dev's history newest-first until it ends or the budget runs out.
func scanWallet(ctx context.Context, dev string, workers int) scanResult {
	jobs := make(chan signature)
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		seen   = map[string]bool{}
		result scanResult
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sig := range jobs {
				tx, err := getTransaction(ctx, sig.Signature)
				mu.Lock()
				if err != nil {
					result.failed++
				} else {
					result.scanned++
					if result.oldest == 0 || sig.BlockTime < result.oldest {
						result.oldest = sig.BlockTime
					}
					for _, mint := range mintsCreated(tx, dev) {
						if !seen[mint] {
							seen[mint] = true
							result.mints = append(result.mints, mint)
						}
					}
				}
				mu.Unlock()
			}
		}()
	}
	before := ""
feed:
	for {
		sigs, err := signatures(ctx, dev, before)
		if err != nil {
			break
		}
		if len(sigs) == 0 {
			result.done = true
			break
		}
		for _, sig := range sigs {
			if len(sig.Err) > 0 && string(sig.Err) != "null" {
				continue
			}
			select {
			case jobs <- sig:
			case <-ctx.Done():
				break feed
			}
		}
		before = sigs[len(sigs)-1].Signature
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil || result.failed > 0 {
		result.done = false
	}
	return result
}

type row struct {
	mint, symbol, created string
	ath, now              float64
	missing, unchecked    bool
}

func enrich(ctx context.Context, mints []string) []row {
	rows := make([]row, len(mints))
	byMint := map[string]asset{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for start := 0; start < len(mints); start += 100 {
		batch := mints[start:min(start+100, len(mints))]
		wg.Add(1)
		go func() {
			defer wg.Done()
			found, err := assets(ctx, batch)
			if err != nil {
				return
			}
			mu.Lock()
			for _, a := range found {
				byMint[a.ID] = a
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	// Most likely high ATH first, so a cut-off run still checks the tokens that matter.
	ordered := append([]string(nil), mints...)
	sort.SliceStable(ordered, func(a, b int) bool {
		x, y := byMint[ordered[a]], byMint[ordered[b]]
		if (x.Graduated != "") != (y.Graduated != "") {
			return x.Graduated != ""
		}
		return x.Mcap > y.Mcap
	})
	limit := make(chan struct{}, 16)
	for i, mint := range ordered {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			if ctx.Err() != nil {
				a := byMint[mint]
				rows[i] = row{mint: mint, symbol: a.Symbol, created: a.CreatedAt, now: a.Mcap, missing: true, unchecked: true}
				return
			}
			a := byMint[mint]
			high, err := ath(ctx, mint)
			rows[i] = row{mint: mint, symbol: a.Symbol, created: a.CreatedAt, ath: high, now: a.Mcap, missing: err != nil || high == 0, unchecked: err != nil}
		}()
	}
	wg.Wait()
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].ath > rows[b].ath })
	return rows
}

func countUnchecked(rows []row) int {
	count := 0
	for _, r := range rows {
		if r.unchecked {
			count++
		}
	}
	return count
}

func countATH(rows []row, mint string) int {
	count := 0
	for _, r := range rows {
		if r.mint != mint && !r.missing {
			count++
		}
	}
	return count
}

func usd(v float64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("$%.2fB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("$%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("$%.1fK", v/1e3)
	}
	return fmt.Sprintf("$%.0f", v)
}

var errSlowMode = errors.New("slow mode selected")

func lookup(mint string, budget, athBudget time.Duration, workers, shards, top int, benchOut bool) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var self asset
	jupCtx, cancelJup := context.WithTimeout(ctx, 3*time.Second)
	defer cancelJup()
	if found, err := assets(jupCtx, []string{mint}); err == nil {
		for _, a := range found {
			if a.ID == mint {
				self = a
			}
		}
	}
	dev, source := self.Dev, "Jupiter"
	if dev == "" {
		var err error
		dev, err = creatorFromChain(ctx, mint)
		source = "first transaction's fee payer"
		if err != nil {
			fmt.Printf("Could not find the creator: %v\n", err)
			return
		}
	}
	fmt.Printf("\nToken  %s %s\nDev    %s (%s)\n", self.Symbol, mint, dev, source)
	if self.ID != "" && self.Audit.DevMints > 0 {
		fmt.Printf("Record %d tokens created, %d graduated (Jupiter) [%dms]\n", self.Audit.DevMints, self.Audit.DevMigrations, time.Since(start).Milliseconds())
	}

	scanCtx, cancelScan := context.WithTimeout(context.Background(), budget)
	defer cancelScan()
	scanStart := time.Now()
	var scan scanResult
	var fastErr error = errSlowMode
	if shards > 0 {
		scan, fastErr = scanWalletFast(scanCtx, dev, shards, nil)
	}
	if fastErr != nil {
		scan = scanWallet(scanCtx, dev, workers)
	}
	scanTime := time.Since(scanStart)

	found := scan.mints
	hasSelf := false
	for _, m := range found {
		hasSelf = hasSelf || m == mint
	}
	if !hasSelf {
		found = append(found, mint)
	}
	richCtx, cancelRich := context.WithTimeout(context.Background(), athBudget)
	defer cancelRich()
	richStart := time.Now()
	rows := enrich(richCtx, found)
	if benchOut {
		fmt.Fprintf(os.Stderr, "BENCH,%s,shards=%d,scan_ms=%d,ath_ms=%d,total_ms=%d,mints=%d,txs=%d,failed=%d,complete=%v,ath_unchecked=%d\n", mint, shards, scanTime.Milliseconds(), time.Since(richStart).Milliseconds(), time.Since(start).Milliseconds(), len(scan.mints), scan.scanned, scan.failed, scan.done, countUnchecked(rows))
	}

	fmt.Printf("\n%-4s %-12s %-10s %-10s %-11s %s\n", "#", "SYMBOL", "ATH MCAP", "NOW", "CREATED", "MINT")
	for i, r := range rows {
		if i == top {
			fmt.Printf("... %d more with lower ATH (use -top to show them)\n", len(rows)-top)
			break
		}
		high := usd(r.ath)
		if r.missing {
			high = "n/a"
		}
		tag, created := "", r.created
		if r.mint == mint {
			tag = "  <- pasted"
		}
		if len(created) >= 10 {
			created = created[:10]
		}
		fmt.Printf("%-4d %-12.12s %-10s %-10s %-11s %s%s\n", i+1, r.symbol, high, usd(r.now), created, r.mint, tag)
	}

	bestFound := false
	for _, r := range rows {
		if r.mint != mint && !r.missing {
			label := "Dev's best ATH"
			if !scan.done {
				label += " so far (scan incomplete)"
			}
			fmt.Printf("\n%s: %s on %s (%s), from %d other tokens with an ATH\n", label, usd(r.ath), r.symbol, r.mint, countATH(rows, mint))
			bestFound = true
			break
		}
	}
	if !bestFound {
		if scan.done {
			fmt.Println("\nDev's best ATH: no other tokens with an ATH")
		} else {
			fmt.Println("\nDev's best ATH: UNKNOWN, the scan was cut off before finding other tokens with an ATH")
		}
	}

	if unchecked := countUnchecked(rows); unchecked > 0 {
		fmt.Printf("WARNING: %d of %d ATHs could not be fetched before the time limit, so the best ATH may be higher (raise -ath-t)\n", unchecked, len(rows))
	}

	coverage := "complete wallet history"
	if !scan.done {
		coverage = "PARTIAL: older tokens may be missing"
		if scan.oldest > 0 {
			coverage += ", reached back to " + time.Unix(scan.oldest, 0).UTC().Format("2006-01-02 15:04 UTC")
		}
	}
	fmt.Printf("\n%d tokens found, %d transactions scanned, %d failed. %s. %.2fs\n", len(scan.mints), scan.scanned, scan.failed, coverage, time.Since(start).Seconds())
}

func main() {
	budget := flag.Duration("t", 20*time.Second, "time budget for scanning the dev's wallet")
	workers := flag.Int("w", 0, "parallel RPC requests (default 8 on Helius, 64 with RPC_URL)")
	top := flag.Int("top", 25, "rows to print")
	shards := flag.Int("shards", 4, "parallel slot ranges for the fast scan; 0 uses the old one-by-one scan")
	athBudget := flag.Duration("ath-t", 180*time.Second, "time budget for fetching ATHs")
	benchOut := flag.Bool("bench", false, "print stage timings to stderr")
	web := flag.Bool("web", false, "run the browser interface instead of the terminal prompt")
	port := flag.Int("port", 8787, "port for the browser interface")
	noOpen := flag.Bool("no-open", false, "don't open the browser automatically")
	flag.Parse()

	loadDotEnv(".env")
	key := os.Getenv("HELIUS_API_KEY")
	if key != "" {
		rpcURL = "https://mainnet.helius-rpc.com/?api-key=" + url.QueryEscape(key)
	}
	if custom := os.Getenv("RPC_URL"); custom != "" {
		rpcURL = custom
		if *workers == 0 {
			*workers = 64
		}
	} else if *workers == 0 {
		*workers = 8
	}
	if rpcURL == "" {
		fmt.Println("Missing API key: set HELIUS_API_KEY or RPC_URL, or add it to a .env file (see .env.example).")
		os.Exit(1)
	}

	if *web {
		if err := serve(*port, *budget, *athBudget, *workers, *shards, !*noOpen); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		return
	}

	if flag.NArg() > 0 {
		for _, arg := range flag.Args() {
			if !addressRe.MatchString(arg) {
				fmt.Printf("Not a Solana address: %s\n", arg)
				continue
			}
			lookup(arg, *budget, *athBudget, *workers, *shards, *top, *benchOut)
		}
		return
	}
	fmt.Print("Paste Solana contract: ")
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		if mint := addressRe.FindString(in.Text()); mint != "" {
			lookup(mint, *budget, *athBudget, *workers, *shards, *top, *benchOut)
		}
		fmt.Print("\nPaste Solana contract: ")
	}
}
