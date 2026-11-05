package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

var (
	// linkLocalV4Net is 169.254.0.0/16 (includes 169.254.169.254 cloud metadata).
	_, linkLocalV4Net, _ = net.ParseCIDR("169.254.0.0/16")
	// cgnatNet is 100.64.0.0/10 (RFC 6598 carrier-grade NAT).
	_, cgnatNet, _ = net.ParseCIDR("100.64.0.0/10")
	// zeroV4Net is 0.0.0.0/8.
	_, zeroV4Net, _ = net.ParseCIDR("0.0.0.0/8")
)

// Resolver resolves hostnames to IP addresses.
type Resolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}

// ResolverFunc is a function adapter implementing Resolver.
type ResolverFunc func(ctx context.Context, network, host string) ([]net.IP, error)

// LookupIP implements the Resolver interface.
func (f ResolverFunc) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	return f(ctx, network, host)
}

// IsBlockedIP checks if an IP is loopback, RFC1918, link-local, multicast,
// unspecified, or in a reserved private range.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}

	// Canonicalize IPv4-mapped IPv6 addresses (e.g. ::ffff:127.0.0.1).
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}

	if ip.IsLoopback() {
		return true
	}
	if ip.IsPrivate() {
		return true
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsMulticast() {
		return true
	}
	if ip.IsUnspecified() {
		return true
	}
	// Tunnel prefixes that can embed IPv4 targets: 6to4 (2002::/16) and
	// Teredo (2001::/32). Both are effectively unused on the modern
	// internet, so blocking them is free defense against SSRF via
	// encapsulation (e.g. 2002:7f00:0001:: wrapping 127.0.0.1).
	if len(ip) == net.IPv6len {
		if ip[0] == 0x20 && ip[1] == 0x02 {
			return true // 2002::/16
		}
		if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3] == 0x00 {
			return true // 2001::/32
		}
	}

	// Explicit CIDR checks for IPv4 ranges
	if len(ip) == net.IPv4len {
		if linkLocalV4Net != nil && linkLocalV4Net.Contains(ip) {
			return true
		}
		if cgnatNet != nil && cgnatNet.Contains(ip) {
			return true
		}
		if zeroV4Net != nil && zeroV4Net.Contains(ip) {
			return true
		}
		// 255.255.255.255 broadcast
		if ip.Equal(net.IPv4bcast) {
			return true
		}
	}

	return false
}

// ValidateURL validates that a URL is http/https and does not target private or blocked IPs.
func ValidateURL(ctx context.Context, rawURL string) (*url.URL, []net.IP, error) {
	return ValidateURLWithResolver(ctx, rawURL, net.DefaultResolver, "")
}

// ValidateURLWithResolver validates that a URL is http/https and its host resolves to safe IPs.
func ValidateURLWithResolver(ctx context.Context, rawURL string, resolver Resolver, allowedTestHost string) (*url.URL, []net.IP, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid URL: %w", err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, nil, fmt.Errorf("unsupported URL scheme %q: only http and https are allowed", parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return nil, nil, errors.New("empty host in URL")
	}

	// Allow specific test server address in tests
	if allowedTestHost != "" && (parsed.Host == allowedTestHost || host == allowedTestHost) {
		return parsed, nil, nil
	}

	if ip := net.ParseIP(host); ip != nil {
		if IsBlockedIP(ip) {
			return nil, nil, fmt.Errorf("ssrf: blocked destination IP %s", ip)
		}
		return parsed, []net.IP{ip}, nil
	}

	if resolver == nil {
		resolver = net.DefaultResolver
	}

	ips, err := resolver.LookupIP(ctx, "ip", host)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, nil, fmt.Errorf("resolve host %q: no IP addresses found", host)
	}

	for _, ip := range ips {
		if IsBlockedIP(ip) {
			return nil, nil, fmt.Errorf("ssrf: host %q resolves to blocked IP %s", host, ip)
		}
	}

	return parsed, ips, nil
}

// NewSafeDialContext returns a DialContext function that validates the destination host/IP
// and connects directly to the validated IP to prevent DNS-rebinding TOCTOU attacks.
func NewSafeDialContext(resolver Resolver, allowedTestHost string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	var dialer net.Dialer
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split host port: %w", err)
		}

		if allowedTestHost != "" && (addr == allowedTestHost || host == allowedTestHost) {
			return dialer.DialContext(ctx, network, addr)
		}

		if resolver == nil {
			resolver = net.DefaultResolver
		}

		var targetIP net.IP
		if ip := net.ParseIP(host); ip != nil {
			if IsBlockedIP(ip) {
				return nil, fmt.Errorf("ssrf: blocked destination IP %s", ip)
			}
			targetIP = ip
		} else {
			ips, err := resolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolve host %q: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP resolved for host %q", host)
			}
			for _, ip := range ips {
				if IsBlockedIP(ip) {
					return nil, fmt.Errorf("ssrf: host %q resolves to blocked IP %s", host, ip)
				}
			}
			targetIP = ips[0]
		}

		dialAddr := net.JoinHostPort(targetIP.String(), port)
		return dialer.DialContext(ctx, network, dialAddr)
	}
}
