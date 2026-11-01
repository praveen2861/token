package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

type tokenJSON struct {
	Mint      string  `json:"mint"`
	Symbol    string  `json:"symbol"`
	Created   string  `json:"created"`
	ATH       float64 `json:"ath"`
	Now       float64 `json:"now"`
	Missing   bool    `json:"missing"`
	Unchecked bool    `json:"unchecked"`
	Pasted    bool    `json:"pasted"`
}

// handleLookup streams one JSON object per line so the page can show each stage as it finishes.
func handleLookup(budget, athBudget time.Duration, workers, shards int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		flusher, _ := w.(http.Flusher)
		var sendMu sync.Mutex
		send := func(event map[string]any) {
			sendMu.Lock()
			defer sendMu.Unlock()
			json.NewEncoder(w).Encode(event)
			if flusher != nil {
				flusher.Flush()
			}
		}

		mint := addressRe.FindString(r.URL.Query().Get("ca"))
		if mint == "" {
			send(map[string]any{"stage": "error", "message": "That doesn't look like a Solana contract address."})
			return
		}
		start := time.Now()
		ctx := r.Context()

		// The bundle check runs alongside everything below and reports as soon as it finishes.
		var bundleWG sync.WaitGroup
		bundleWG.Add(1)
		go func() {
			defer bundleWG.Done()
			send(safetyCheck(withAgent(ctx, "safety", send), mint))
		}()

		// Bundle and sniper checks start together and share one fetch of the launch transactions.
		launch := newLaunchFetcher(withAgent(ctx, "bundle", send), mint)
		bundleWG.Add(2)
		go func() {
			defer bundleWG.Done()
			send(bundleEvent(bundleCheck(withAgent(ctx, "bundle", send), mint, launch)))
		}()
		go func() {
			defer bundleWG.Done()
			send(sniperEvent(sniperCheck(withAgent(ctx, "sniper", send), mint, launch)))
		}()
		defer bundleWG.Wait()

		self := selfAsset(ctx, mint)

		bundleWG.Add(1)
		go func() {
			defer bundleWG.Done()
			event, err := sameNameCheck(withAgent(ctx, "name", send), self)
			if err != nil {
				event = map[string]any{"stage": "samename", "error": err.Error()}
			}
			send(event)
		}()

		bundleWG.Add(1)
		go func() {
			defer bundleWG.Done()
			send(socialsCheck(withAgent(ctx, "scout", send), mint, self))
		}()
		bundleWG.Add(2)
		go func() {
			defer bundleWG.Done()
			send(holdersCheck(withAgent(ctx, "holders", send), mint, self))
		}()
		go func() {
			defer bundleWG.Done()
			send(liquidityCheck(withAgent(ctx, "liquidity", send), mint, self))
		}()

		dev, source := self.Dev, "Jupiter"
		if dev == "" {
			devCtx, cancelDev := context.WithTimeout(ctx, 10*time.Second)
			defer cancelDev()
			var err error
			dev, err = creatorFromChain(devCtx, mint)
			source = "first transaction's fee payer"
			if err != nil {
				send(map[string]any{"stage": "error", "message": "Could not find the creator: " + err.Error()})
				return
			}
		}
		send(map[string]any{"stage": "dev", "mint": mint, "symbol": self.Symbol, "dev": dev, "source": source,
			"devMints": self.Audit.DevMints, "devMigrations": self.Audit.DevMigrations, "ms": time.Since(start).Milliseconds()})

		devCtx := withAgent(ctx, "dev", send)
		scanCtx, cancelScan := context.WithTimeout(devCtx, budget)
		defer cancelScan()
		richCtx, cancelRich := context.WithTimeout(devCtx, budget+athBudget)
		defer cancelRich()

		// Tokens and ATHs go to the page while the scan runs, so a busy wallet shows results straight away.
		var (
			liveMu   sync.Mutex
			liveWG   sync.WaitGroup
			pending  = []string{mint}
			seen     = map[string]bool{mint: true}
			scanned  int
			allRows  []row
			finished = make(chan struct{})
		)
		addMint := func(m string) {
			if m != "" && !seen[m] {
				seen[m] = true
				pending = append(pending, m)
			}
		}
		liveWG.Add(2)
		go func() {
			defer liveWG.Done()
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			last := -1
			for {
				select {
				case <-finished:
					return
				case <-ticker.C:
					liveMu.Lock()
					count, tokens := scanned, len(seen)-1
					liveMu.Unlock()
					if count != last {
						last = count
						send(map[string]any{"stage": "progress", "scanned": count, "found": tokens})
					}
				}
			}
		}()
		go func() {
			defer liveWG.Done()
			for {
				scanOver := false
				select {
				case <-finished:
					scanOver = true
				default:
				}
				liveMu.Lock()
				batch := pending[:min(len(pending), 20)]
				pending = pending[len(batch):]
				liveMu.Unlock()
				if len(batch) == 0 {
					if scanOver {
						return
					}
					time.Sleep(200 * time.Millisecond)
					continue
				}
				rows := enrich(richCtx, batch)
				tokens := make([]tokenJSON, len(rows))
				for i, r := range rows {
					tokens[i] = tokenJSON{r.mint, r.symbol, r.created, r.ath, r.now, r.missing, r.unchecked, r.mint == mint}
				}
				liveMu.Lock()
				allRows = append(allRows, rows...)
				liveMu.Unlock()
				send(map[string]any{"stage": "tokens", "tokens": tokens})
			}
		}()

		scan, err := scanLatest(scanCtx, dev, mint, latestLimit, func(m string, count int) {
			liveMu.Lock()
			defer liveMu.Unlock()
			scanned = count
			addMint(m)
		})
		if err != nil {
			scan = scanWallet(scanCtx, dev, workers)
			if len(scan.mints) > latestLimit {
				scan.mints = scan.mints[:latestLimit]
			}
		}
		liveMu.Lock()
		for _, m := range scan.mints {
			addMint(m)
		}
		liveMu.Unlock()
		close(finished)
		send(map[string]any{"stage": "scan", "found": len(scan.mints), "scanned": scan.scanned, "failed": scan.failed,
			"complete": scan.done, "ms": time.Since(start).Milliseconds()})
		liveWG.Wait()

		send(map[string]any{"stage": "done", "unchecked": countUnchecked(allRows), "complete": scan.done,
			"scanned": scan.scanned, "failed": scan.failed, "ms": time.Since(start).Milliseconds()})
	}
}

// serve listens on localhost only, unless a host sets PORT. Then it listens publicly, with lookups
// rate limited to protect the API quota.
func serve(port int, budget, athBudget time.Duration, workers, shards int, open bool) error {
	hostPort := os.Getenv("PORT")
	hosted := hostPort != ""

	app := http.NewServeMux()
	lookup := handleLookup(budget, athBudget, workers, shards)
	if hosted {
		lookup = newLookupLimiter(lookupsPerMinute, maxLookupsAtOnce).wrap(lookup)
	}
	app.HandleFunc("GET /api/lookup", lookup)
	app.HandleFunc("GET /api/config", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"hosted": hosted})
	})
	if !hosted {
		// Locally the backend also serves the page; when hosted, Vercel does.
		app.Handle("GET /", http.FileServer(http.Dir("../frontend")))
		hub := &clipboardHub{subs: map[chan string]bool{}}
		app.HandleFunc("GET /api/clipboard", hub.handle)
		go watchClipboard(hub.publish)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.Handle("/", app)

	address := fmt.Sprintf("127.0.0.1:%d", port)
	if hosted {
		address = ":" + hostPort
	}
	// Listen before opening the browser, otherwise the page can load too early and be refused.
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("could not start on %s, is it already running? %w", address, err)
	}
	link := "http://" + address
	fmt.Println("Open", link, "(keep this window open, Ctrl+C to stop)")
	if open && !hosted {
		exec.CommandContext(context.Background(), "rundll32", "url.dll,FileProtocolHandler", link).Start()
	}
	server := &http.Server{Handler: allowOrigins(os.Getenv("ALLOWED_ORIGINS"), mux), ReadHeaderTimeout: 10 * time.Second}
	return server.Serve(listener)
}
