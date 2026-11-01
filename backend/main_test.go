package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

const sampleTx = `{"meta":{"innerInstructions":[{"instructions":[
 {"parsed":{"type":"initializeMint2","info":{"mint":"MINTA"}}},
 {"parsed":{"type":"transfer","info":{"mint":"NOPE"}}}]}]},
 "transaction":{"message":{"accountKeys":[{"pubkey":"DEV"}],"instructions":[
 {"parsed":"memo text"},{"parsed":{"type":"initializeMint","info":{"mint":"MINTB"}}}]}}}`

func TestMintsCreated(t *testing.T) {
	var tx transaction
	if err := json.Unmarshal([]byte(sampleTx), &tx); err != nil {
		t.Fatal(err)
	}
	got := mintsCreated(&tx, "DEV")
	if len(got) != 2 || got[0] != "MINTB" || got[1] != "MINTA" {
		t.Fatalf("got %v", got)
	}
	if other := mintsCreated(&tx, "SOMEONE_ELSE"); other != nil {
		t.Fatalf("a mint paid for by another wallet must not count: %v", other)
	}
}

const sampleBundle = `[
 {"slot":100,"meta":{"preTokenBalances":[],"postTokenBalances":[
   {"mint":"M","owner":"CURVE","uiTokenAmount":{"amount":"900"}},{"mint":"M","owner":"DEV","uiTokenAmount":{"amount":"100"}}]},
  "transaction":{"message":{"accountKeys":[{"pubkey":"DEV","signer":true},{"pubkey":"CURVE","signer":false}]}}},
 {"slot":100,"meta":{"preTokenBalances":[{"mint":"M","owner":"CURVE","uiTokenAmount":{"amount":"900"}}],"postTokenBalances":[
   {"mint":"M","owner":"CURVE","uiTokenAmount":{"amount":"850"}},{"mint":"M","owner":"A","uiTokenAmount":{"amount":"50"}}]},
  "transaction":{"message":{"accountKeys":[{"pubkey":"A","signer":true},{"pubkey":"CURVE","signer":false}]}}},
 {"slot":105,"meta":{"preTokenBalances":[{"mint":"M","owner":"CURVE","uiTokenAmount":{"amount":"850"}}],"postTokenBalances":[
   {"mint":"M","owner":"CURVE","uiTokenAmount":{"amount":"830"}},{"mint":"M","owner":"B","uiTokenAmount":{"amount":"20"}}]},
  "transaction":{"message":{"accountKeys":[{"pubkey":"B","signer":true},{"pubkey":"CURVE","signer":false}]}}},
 {"slot":106,"meta":{"preTokenBalances":[{"mint":"M","owner":"A","uiTokenAmount":{"amount":"50"}},{"mint":"M","owner":"CURVE","uiTokenAmount":{"amount":"830"}}],"postTokenBalances":[
   {"mint":"M","owner":"A","uiTokenAmount":{"amount":"0"}},{"mint":"M","owner":"CURVE","uiTokenAmount":{"amount":"880"}}]},
  "transaction":{"message":{"accountKeys":[{"pubkey":"A","signer":true},{"pubkey":"CURVE","signer":false}]}}},
 {"slot":200,"meta":{"preTokenBalances":[],"postTokenBalances":[{"mint":"M","owner":"LATE","uiTokenAmount":{"amount":"500"}}]},
  "transaction":{"message":{"accountKeys":[{"pubkey":"LATE","signer":true}]}}}]`

func TestSummarize(t *testing.T) {
	var txs []bundleTx
	if err := json.Unmarshal([]byte(sampleBundle), &txs); err != nil {
		t.Fatal(err)
	}
	r := summarize(txs, "M", 1000)
	if r.Creator != "DEV" || r.DevPct != 10 {
		t.Errorf("creator %s dev %v", r.Creator, r.DevPct)
	}
	if len(r.SameBlock) != 1 || r.SameBlock[0].Wallet != "A" || r.SameBlockPct != 5 {
		t.Errorf("same block %+v %v", r.SameBlock, r.SameBlockPct)
	}
	if r.EarlyWallets != 1 || r.EarlyPct != 2 {
		t.Errorf("early %d %v", r.EarlyWallets, r.EarlyPct)
	}
	if r.Verdict != "Possibly bundled" {
		t.Errorf("verdict %s", r.Verdict)
	}
}

func TestXLink(t *testing.T) {
	cases := []struct{ link, kind, handle, id string }{
		{"https://x.com/dumbmoneypf/status/2116239256184033653", "status", "dumbmoneypf", "2116239256184033653"},
		{"https://twitter.com/abc/status/123?s=20", "status", "abc", "123"},
		{"https://x.com/i/web/status/99", "status", "", "99"},
		{"https://x.com/deskhandai", "profile", "deskhandai", ""},
		{"https://x.com/i/communities/123", "other", "", ""},
		{"https://example.com/foo", "", "", ""},
	}
	for _, c := range cases {
		kind, handle, id := xLink(c.link)
		if kind != c.kind || handle != c.handle || id != c.id {
			t.Errorf("%s: got %s %s %s", c.link, kind, handle, id)
		}
	}
}

func TestAddressFromClipboard(t *testing.T) {
	const mint = "FYUyPWNLQtF4nNQ8nSHwpmwQ4j41UqAUz8gWEeWWpump"
	cases := map[string]string{
		mint:                                   mint,
		"  " + mint + "\r\n":                   mint,
		"https://pump.fun/coin/" + mint:        mint,
		"hello":                                "",
		"token " + mint + " looks good":        "",
		"thisIsNotARealAddressButLongEnough12": "",
		"":                                     "",
	}
	for text, want := range cases {
		if got := addressFromClipboard(text); got != want {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
}

func TestSummarizeSnipers(t *testing.T) {
	var txs []bundleTx
	if err := json.Unmarshal([]byte(sampleBundle), &txs); err != nil {
		t.Fatal(err)
	}
	r := summarizeSnipers(txs, "M", 1000)
	if r.Wallets != 1 || r.Snipers[0].Wallet != "B" || r.BoughtPct != 2 {
		t.Errorf("got %+v", r)
	}
	if v := sniperVerdict(r); v != "A few snipers" {
		t.Errorf("verdict %s", v)
	}
}

func TestSafetyFlags(t *testing.T) {
	authority := "SOMEONE"
	flags := safetyFlags(mintInfo{MintAuthority: &authority}, false)
	if flags[0].Level != "danger" || flags[1].Level != "ok" {
		t.Errorf("got %+v", flags)
	}
	var info mintInfo
	raw := `{"extensions":[{"extension":"transferHook","state":{"programId":null}},{"extension":"transferFeeConfig","state":{"newerTransferFee":{"transferFeeBasisPoints":500}}},{"extension":"permanentDelegate","state":{"delegate":"X"}}]}`
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		t.Fatal(err)
	}
	flags = safetyFlags(info, true)
	if len(flags) != 4 || flags[2].Name != "Transfer tax" || flags[3].Name != "Permanent delegate" {
		t.Errorf("got %+v", flags)
	}
}

func TestTransferTaxLevel(t *testing.T) {
	for bps, want := range map[float64]string{999: "ok", 1000: "danger", 2500: "danger"} {
		info := mintInfo{}
		raw := fmt.Sprintf(`{"extensions":[{"extension":"transferFeeConfig","state":{"newerTransferFee":{"transferFeeBasisPoints":%v}}}]}`, bps)
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			t.Fatal(err)
		}
		flags := safetyFlags(info, true)
		if got := flags[len(flags)-1]; got.Name != "Transfer tax" || got.Level != want {
			t.Errorf("%v bps: got %+v", bps, got)
		}
	}
}

func TestVerdicts(t *testing.T) {
	if holdersVerdict(60, 5) != "Concentrated" || holdersVerdict(10, 25) != "Concentrated" || holdersVerdict(30, 5) != "Moderate" || holdersVerdict(10, 2) != "Spread out" {
		t.Error("holders verdict")
	}
	if liquidityVerdict(100, 0, true) != "On bonding curve" || liquidityVerdict(500, 0, false) != "Very thin" || liquidityVerdict(50000, 2000000, false) != "Thin" || liquidityVerdict(50000, 100000, false) != "Healthy" {
		t.Error("liquidity verdict")
	}
}

func TestUSD(t *testing.T) {
	for v, want := range map[float64]string{76: "$76", 3900: "$3.9K", 4.88e9: "$4.88B", 1.5e6: "$1.50M"} {
		if got := usd(v); got != want {
			t.Errorf("usd(%v) = %s, want %s", v, got, want)
		}
	}
}

func BenchmarkMintsCreated(b *testing.B) {
	var tx transaction
	if err := json.Unmarshal([]byte(sampleTx), &tx); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		mintsCreated(&tx, "DEV")
	}
}
