package model

import (
	"crypto/sha256"
	"fmt"
	"net/netip"
	"strings"

	whatwg "github.com/nlnwa/whatwg-url/url"
)

// NotificationDestination validates and normalizes with the same WHATWG URL
// algorithm as reqwest::Url. The identity hashes the normalized URL, including
// its query order and empty-query marker. It never includes a trailing newline.
func NotificationDestination(raw string) (normalized, identity string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 8192 {
		return "", "", fmt.Errorf("Notification webhook URL is empty or exceeds the 8192-byte limit")
	}
	u, err := whatwg.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("Notification webhook URL is not a valid URL")
	}
	if u.Username() != "" || u.Password() != "" {
		return "", "", fmt.Errorf("Notification webhook URL must not contain credentials")
	}
	normalized = u.Href(false)
	if strings.Contains(normalized, "#") {
		return "", "", fmt.Errorf("Notification webhook URL must not contain a fragment")
	}
	if u.Hostname() == "" {
		return "", "", fmt.Errorf("Notification webhook URL must contain a host")
	}
	switch u.Protocol() {
	case "https:":
	case "http:":
		ip, e := netip.ParseAddr(strings.Trim(u.Hostname(), "[]"))
		// Only ::1 qualifies as IPv6 loopback; mapped IPv4 addresses do not.
		if e != nil || !(ip.Is4() && ip.IsLoopback() || ip == netip.IPv6Loopback()) {
			return "", "", fmt.Errorf("Plain HTTP notification webhooks require a loopback IP address")
		}
	default:
		return "", "", fmt.Errorf("Notification webhook URL must use https, or http for a loopback IP")
	}
	return normalized, fmt.Sprintf("%x", sha256.Sum256([]byte(normalized))), nil
}

// DecisionMemoryFingerprint consumes successful `git ls-tree -r` stdout.
// A decision without relevant paths uses the revision itself.
func DecisionMemoryFingerprint(revision string, paths []string, treeOutput string) (string, error) {
	if len(paths) > 40 {
		return "", fmt.Errorf("Decision has too many relevant paths")
	}
	for _, path := range paths {
		if path == "" || strings.HasPrefix(path, "/") {
			return "", fmt.Errorf("Decision paths must be repository-relative files")
		}
		for i, part := range strings.Split(path, "/") {
			if part == ".." || (part == "." && i == 0) {
				return "", fmt.Errorf("Decision paths must be repository-relative files")
			}
		}
	}
	if len(paths) == 0 {
		return revision, nil
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(treeOutput)))), nil
}
