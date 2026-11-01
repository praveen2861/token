package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const socialsBudget = 8 * time.Second

var (
	xStatusRe  = regexp.MustCompile(`(?i)(?:x|twitter)\.com/(?:i/web/|([A-Za-z0-9_]{1,15})/)status(?:es)?/(\d+)`)
	xProfileRe = regexp.MustCompile(`(?i)(?:x|twitter)\.com/([A-Za-z0-9_]{1,15})/?(?:[?#].*)?$`)
)

// xLink classifies an X link as a status or a profile; anything else, like a community, is left unread.
func xLink(link string) (kind, handle, id string) {
	if m := xStatusRe.FindStringSubmatch(link); m != nil {
		return "status", m[1], m[2]
	}
	if m := xProfileRe.FindStringSubmatch(link); m != nil && !strings.EqualFold(m[1], "i") {
		return "profile", m[1], ""
	}
	if strings.Contains(strings.ToLower(link), "x.com/") || strings.Contains(strings.ToLower(link), "twitter.com/") {
		return "other", "", ""
	}
	return "", "", ""
}

type xAuthor struct {
	ScreenName string `json:"screen_name"`
	Followers  *int64 `json:"followers"`
}

func xStats(ctx context.Context, link string) map[string]any {
	kind, handle, id := xLink(link)
	stats := map[string]any{"kind": kind}
	switch kind {
	case "status":
		var out struct {
			Tweet struct {
				Views     *int64  `json:"views"`
				Likes     *int64  `json:"likes"`
				Retweets  *int64  `json:"retweets"`
				CreatedAt string  `json:"created_at"`
				Author    xAuthor `json:"author"`
			} `json:"tweet"`
		}
		if err := fetch(ctx, "GET", "https://api.fxtwitter.com/status/"+id, nil, &out); err != nil {
			stats["error"] = "Couldn't read this post: " + err.Error()
			return stats
		}
		stats["handle"] = out.Tweet.Author.ScreenName
		stats["followers"] = out.Tweet.Author.Followers
		stats["views"] = out.Tweet.Views
		stats["likes"] = out.Tweet.Likes
		stats["retweets"] = out.Tweet.Retweets
		stats["posted"] = out.Tweet.CreatedAt
	case "profile":
		var out struct {
			ScreenName string `json:"screen_name"`
			Followers  *int64 `json:"followers_count"`
			Created    string `json:"created_at"`
			FetchedOn  int64  `json:"fetched_on"`
		}
		stats["handle"] = handle
		if err := fetch(ctx, "GET", "https://api.vxtwitter.com/"+handle, nil, &out); err != nil {
			stats["error"] = "Couldn't read this profile: " + err.Error()
			return stats
		}
		stats["followers"] = out.Followers
		stats["created"] = out.Created
		stats["fetchedOn"] = out.FetchedOn
	}
	return stats
}

// publicHTTPS keeps token-supplied metadata URLs from reaching this machine or its local network.
func publicHTTPS(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	return net.ParseIP(host) == nil
}

// tokenLinks reads links from the token's on-chain record and the metadata file it points to.
func tokenLinks(ctx context.Context, mint string) (map[string]string, bool) {
	var out struct {
		Content struct {
			JSONURI string         `json:"json_uri"`
			Links   map[string]any `json:"links"`
		} `json:"content"`
	}
	links := map[string]string{}
	if rpc(ctx, "getAsset", map[string]any{"id": mint}, &out) != nil {
		return links, false
	}
	add := func(fields map[string]any) {
		for key, name := range map[string]string{"twitter": "twitter", "x": "twitter", "telegram": "telegram", "tg": "telegram", "website": "website", "web": "website"} {
			if text, ok := fields[key].(string); ok && text != "" && links[name] == "" {
				links[name] = text
			}
		}
	}
	add(out.Content.Links)
	complete := true
	if publicHTTPS(out.Content.JSONURI) {
		var file map[string]any
		if err := fetchOnce(ctx, out.Content.JSONURI, &file); err != nil {
			complete = false
		} else {
			add(file)
			if extensions, ok := file["extensions"].(map[string]any); ok {
				add(extensions)
			}
		}
	}
	return links, complete
}

// fetchOnce is for untrusted hosts: no retries and a size cap.
func fetchOnce(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "devath/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func socialsCheck(ctx context.Context, mint string, self asset) map[string]any {
	start := time.Now()
	// Separate budgets, so a slow metadata host can't use up the time for the X lookup.
	report(ctx, "Reading the token's metadata…", 0, 2)
	linkCtx, cancelLinks := context.WithTimeout(ctx, socialsBudget*3/4)
	defer cancelLinks()
	links, linksComplete := tokenLinks(linkCtx, mint)
	ctx, cancel := context.WithTimeout(ctx, socialsBudget)
	defer cancel()

	// Jupiter's fields win where both have a value.
	for key, value := range map[string]string{"twitter": self.Twitter, "website": self.Website, "telegram": self.Telegram} {
		if value != "" {
			links[key] = value
		}
	}
	event := map[string]any{"stage": "socials", "twitter": links["twitter"], "website": links["website"], "telegram": links["telegram"], "linksComplete": linksComplete}

	var wg sync.WaitGroup
	if link := links["twitter"]; link != "" {
		report(ctx, "Reading X stats…", 1, 2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			event["x"] = xStats(ctx, link)
		}()
	}
	wg.Wait()
	event["ms"] = fmt.Sprint(time.Since(start).Milliseconds())
	return event
}
