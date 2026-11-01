package main

import "time"

type tradeStats struct {
	BuyVolume         float64 `json:"buyVolume"`
	SellVolume        float64 `json:"sellVolume"`
	BuyOrganicVolume  float64 `json:"buyOrganicVolume"`
	SellOrganicVolume float64 `json:"sellOrganicVolume"`
	NumBuys           int     `json:"numBuys"`
	NumSells          int     `json:"numSells"`
	NumTraders        int     `json:"numTraders"`
}

func volumeVerdict(total, organicShare float64, traders int) string {
	switch {
	case total <= 0:
		return "No recent volume"
	case traders > 0 && total/float64(traders) >= 5000 && organicShare < 25:
		return "Looks washed"
	case organicShare < 10:
		return "Mostly non-organic"
	case organicShare < 40:
		return "Mixed"
	}
	return "Mostly organic"
}

// volumeCheck reads Jupiter's 24-hour stats from the token lookup, so it makes no extra calls.
func volumeCheck(self asset) map[string]any {
	start := time.Now()
	if self.ID == "" {
		return map[string]any{"stage": "volume", "error": "Jupiter has no stats for this token"}
	}
	s := self.Stats24h
	total := s.BuyVolume + s.SellVolume
	organic := s.BuyOrganicVolume + s.SellOrganicVolume
	share := 0.0
	if total > 0 {
		share = min(organic/total*100, 100)
	}
	return map[string]any{"stage": "volume", "verdict": volumeVerdict(total, share, s.NumTraders), "total": total, "buy": s.BuyVolume, "sell": s.SellVolume,
		"organicShare": share, "buys": s.NumBuys, "sells": s.NumSells, "traders": s.NumTraders, "score": self.OrganicScoreLabel, "ms": time.Since(start).Milliseconds()}
}
