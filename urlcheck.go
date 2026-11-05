package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const maxURLLength = 2048

var errBadURL = errors.New("That doesn't look like a link. Paste a full URL.")
var errBlockedURL = errors.New("This address isn't reachable from Roger.")

// Ranges that are not on the public internet but that net/netip's predicates
// don't cover. Everything else is handled by blockedAddr.
var extraBlocked = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64 can embed a private v4
	netip.MustParsePrefix("2001:db8::/32"), // documentation
}

func blockedAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return true // covers loopback, link-local, multicast and unspecified too
	}
	for _, prefix := range extraBlocked {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

type resolver func(ctx context.Context, host string) ([]netip.Addr, error)

func systemResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// checkURL decides whether a user-supplied link may be handed to yt-dlp. The
// server fetches these links from inside your network, so without this anyone
// could point it at a metadata endpoint or a neighbouring container. A host
// that doesn't resolve is refused: libc accepts spellings of 127.0.0.1 such as
// "2130706433" that Go's resolver doesn't, and yt-dlp would use libc.
//
// This checks the first hop only. A public page can still redirect inward, see
// the README for how to contain that at the network level.
func checkURL(ctx context.Context, raw string, allowPrivate bool, resolve resolver) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLength {
		return "", errBadURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", errBadURL
	}
	if allowPrivate {
		return parsed.String(), nil
	}
	if parsed.User != nil {
		return "", errBlockedURL
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := resolve(ctx, parsed.Hostname())
	if err != nil || len(addrs) == 0 {
		return "", errors.New("Couldn't find that website. Check the link and try again.")
	}
	for _, addr := range addrs {
		if blockedAddr(addr) {
			return "", errBlockedURL
		}
	}
	return parsed.String(), nil
}

var platforms = []struct {
	hosts []string
	label string
}{
	{[]string{"youtube.com", "youtu.be"}, "YouTube"},
	{[]string{"x.com", "twitter.com"}, "X / Twitter"},
	{[]string{"instagram.com"}, "Instagram"},
	{[]string{"threads.net", "threads.com"}, "Threads"},
	{[]string{"tiktok.com"}, "TikTok"},
	{[]string{"vimeo.com"}, "Vimeo"},
	{[]string{"twitch.tv"}, "Twitch"},
	{[]string{"reddit.com"}, "Reddit"},
	{[]string{"facebook.com", "fb.watch"}, "Facebook"},
}

func platformLabel(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "Unknown site"
	}
	hostname := strings.ToLower(parsed.Hostname())
	for _, platform := range platforms {
		for _, host := range platform.hosts {
			if hostname == host || strings.HasSuffix(hostname, "."+host) {
				return platform.label
			}
		}
	}
	return strings.TrimPrefix(hostname, "www.")
}
