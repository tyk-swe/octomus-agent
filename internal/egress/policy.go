// Package egress is the gateway every sandbox reaches the internet through. Sandboxes sit on internal networks with
// no route out; the gateway accepts only HTTPS CONNECT tunnels, identifies the sandbox by its proxy credential, and
// opens a tunnel only to an allowlisted host that resolves to a public address.
package egress

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/tyk-swe/octomus-agent/internal/sandbox"
)

// Rule allows one host, or every subdomain of one (*.example.com), on one port.
type Rule struct {
	Host     string `json:"host"`
	Wildcard bool   `json:"wildcard"`
	Port     uint16 `json:"port"`
}

func (r Rule) String() string {
	host := r.Host
	if r.Wildcard {
		host = "*." + host
	}
	if r.Port == 443 {
		return host
	}
	return host + ":" + strconv.Itoa(int(r.Port))
}

func (r Rule) matches(host string, port uint16) bool {
	if port != r.Port {
		return false
	}
	if r.Wildcard {
		return strings.HasSuffix(host, "."+r.Host)
	}
	return host == r.Host
}

// Policy is the deployment's allowlist. Model hosts serve runner sandboxes only; build hosts (package registries and
// the like) serve runner and verification sandboxes. Probes get nothing.
type Policy struct {
	Model []Rule `json:"model"`
	Build []Rule `json:"build"`
}

func (p Policy) Allows(kind, host string, port uint16) bool {
	var rules []Rule
	switch kind {
	case sandbox.KindRunner.String():
		rules = append(append(rules, p.Model...), p.Build...)
	case sandbox.KindVerify.String():
		rules = p.Build
	}
	for _, rule := range rules {
		if rule.matches(host, port) {
			return true
		}
	}
	return false
}

var labelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeHost lowercases a DNS name and refuses anything that is not one, including IP literals: an allowlist
// names services, never addresses.
func NormalizeHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || len(host) > 253 {
		return "", errors.New("host name length")
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return "", errors.New("IP literal")
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("single-label host")
	}
	for _, label := range labels {
		if !labelPattern.MatchString(label) {
			return "", errors.New("invalid host name")
		}
	}
	return host, nil
}

// ParseRules reads a comma- or space-separated allowlist such as "api.openai.com, *.npmjs.org, git.example.com:8443".
func ParseRules(list string) ([]Rule, error) {
	rules := []Rule{}
	for _, entry := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		host, portText, hasPort := strings.Cut(entry, ":")
		port := uint64(443)
		if hasPort {
			var err error
			port, err = strconv.ParseUint(portText, 10, 16)
			if err != nil || port == 0 {
				return nil, fmt.Errorf("Egress rule %q has an invalid port", entry)
			}
		}
		rule := Rule{Port: uint16(port)}
		if trimmed, ok := strings.CutPrefix(host, "*."); ok {
			rule.Wildcard, host = true, trimmed
		}
		normalized, err := NormalizeHost(host)
		if err != nil {
			return nil, fmt.Errorf("Egress rule %q is not a host name: %w", entry, err)
		}
		rule.Host = normalized
		rules = append(rules, rule)
	}
	return rules, nil
}

// globalIPv6 is the only IPv6 range assigned for global unicast. Outside it lie the IPv4-compatible (::/96) and
// translated (::ffff:0:0/96) forms, deprecated site-local addresses and the SRv6 and other special ranges.
var globalIPv6 = netip.MustParsePrefix("2000::/3")

var blockedPrefixes = func() []netip.Prefix {
	prefixes := []netip.Prefix{}
	for _, cidr := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "255.255.255.255/32",
		"64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:2::/48", "2001:10::/28", "2001:20::/28",
		"2001:db8::/32", "2002::/16", "3fff::/20",
	} {
		prefixes = append(prefixes, netip.MustParsePrefix(cidr))
	}
	return prefixes
}()

// PublicAddress reports whether a tunnel may reach addr. Loopback, private, link-local (including cloud metadata),
// carrier-grade NAT, documentation, benchmark and reserved ranges are refused, as are IPv6 addresses outside global
// unicast and IPv6 forms that embed an IPv4 address (IPv4-compatible, SIIT, NAT64, 6to4, Teredo), so no allowlisted
// name can be pointed at the host, the VPS's neighbours or other containers.
func PublicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || !addr.IsGlobalUnicast() ||
		(addr.Is6() && !globalIPv6.Contains(addr)) {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}
