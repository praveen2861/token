package main

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const lookupBudget = 3 * time.Second

var domainRe = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)

// Sites where anyone gets a page: the domain's age says nothing about this token's project.
var sharedHosts = map[string]bool{
	"vercel.app": true, "netlify.app": true, "github.io": true, "pages.dev": true, "web.app": true,
	"carrd.co": true, "linktr.ee": true, "x.com": true, "twitter.com": true, "t.me": true,
	"pump.fun": true, "medium.com": true, "notion.site": true, "wixsite.com": true, "framer.website": true,
}

// domainCandidates turns a website link into registrable domains to try, shortest first.
func domainCandidates(link string) (candidates []string, shared bool) {
	parsed, err := url.Parse(link)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, false
	}
	host := strings.ToLower(strings.TrimPrefix(parsed.Hostname(), "www."))
	if !domainRe.MatchString(host) {
		return nil, false
	}
	labels := strings.Split(host, ".")
	for size := 2; size <= min(3, len(labels)); size++ {
		candidates = append(candidates, strings.Join(labels[len(labels)-size:], "."))
	}
	return candidates, sharedHosts[candidates[0]]
}

// websiteAge reads a domain's registration date from RDAP, the public registration lookup.
func websiteAge(ctx context.Context, link string) map[string]any {
	candidates, shared := domainCandidates(link)
	if len(candidates) == 0 {
		return nil
	}
	info := map[string]any{"domain": candidates[0], "shared": shared}
	if shared {
		return info
	}
	ctx, cancel := context.WithTimeout(ctx, lookupBudget)
	defer cancel()
	for _, domain := range candidates {
		var out struct {
			Events []struct {
				Action string `json:"eventAction"`
				Date   string `json:"eventDate"`
			} `json:"events"`
		}
		if fetch(ctx, "GET", "https://rdap.org/domain/"+url.PathEscape(domain), nil, &out) != nil {
			continue
		}
		for _, event := range out.Events {
			if event.Action == "registration" {
				info["domain"] = domain
				info["created"] = event.Date
				return info
			}
		}
	}
	return info
}

// previousNames asks a public archive for a handle's old names. known is false if it couldn't be
// reached. Empty results are common, so they don't mean the name never changed.
func previousNames(ctx context.Context, handle string) (names []string, known bool) {
	ctx, cancel := context.WithTimeout(ctx, lookupBudget)
	defer cancel()
	var out struct {
		Accounts []struct {
			ScreenNames map[string]any `json:"screen_names"`
		} `json:"accounts"`
	}
	if fetch(ctx, "GET", "https://api.memory.lol/v1/tw/"+url.PathEscape(handle), nil, &out) != nil {
		return nil, false
	}
	seen := map[string]bool{strings.ToLower(handle): true}
	for _, account := range out.Accounts {
		for name := range account.ScreenNames {
			if !seen[strings.ToLower(name)] {
				seen[strings.ToLower(name)] = true
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names, true
}

// addAccountDetails adds name history and, if missing, the account's creation date and followers.
func addAccountDetails(ctx context.Context, stats map[string]any) {
	handle, _ := stats["handle"].(string)
	if handle == "" {
		return
	}
	var wg sync.WaitGroup
	var names []string
	var known bool
	var profile struct {
		Followers *int64 `json:"followers_count"`
		Created   string `json:"created_at"`
	}
	var profileErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		names, known = previousNames(ctx, handle)
	}()
	go func() {
		defer wg.Done()
		profileCtx, cancel := context.WithTimeout(ctx, lookupBudget)
		defer cancel()
		profileErr = fetch(profileCtx, "GET", "https://api.vxtwitter.com/"+url.PathEscape(handle), nil, &profile)
	}()
	wg.Wait()
	stats["previousNames"] = names
	stats["historyKnown"] = known
	if profileErr != nil {
		return
	}
	if _, has := stats["created"]; !has && profile.Created != "" {
		stats["created"] = profile.Created
	}
	if current, _ := stats["followers"].(*int64); current == nil && profile.Followers != nil {
		stats["followers"] = profile.Followers
	}
}
