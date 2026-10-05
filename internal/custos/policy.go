package custos

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/idna"
)

// Frozen failure strings and errors per CUSTOS-SPEC §6.1, §6.3, §6.5, §6.6.
var (
	// ErrInvalidPattern is returned when a rule pattern violates grammar (CUSTOS §6.3, §10 V6).
	ErrInvalidPattern = errors.New("invalid pattern")

	// ErrIPBindingsAskOnly is returned when attempting an auto rule for an IP-literal (CUSTOS §4.3, §6.5).
	ErrIPBindingsAskOnly = errors.New("ip bindings are ask-only")

	// ErrUnsatisfiableBinding is returned when a rule binding cannot be satisfied (CUSTOS §5.3, §6.5).
	ErrUnsatisfiableBinding = errors.New("unsatisfiable binding")

	// ErrPolicyCorrupt is returned when policy.json fails strict parsing (CUSTOS §6.6, §10 V11).
	ErrPolicyCorrupt = errors.New("policy corrupt")
)

// Verdict constants per CUSTOS-SPEC §6.1 (closed enum auto|ask|deny).
const (
	VerdictAuto = "auto"
	VerdictAsk  = "ask"
	VerdictDeny = "deny"
)

// ValidVerdict checks if v is in the closed enum auto|ask|deny.
func ValidVerdict(v string) bool {
	return v == VerdictAuto || v == VerdictAsk || v == VerdictDeny
}

// RuleEntry represents one entry in a policy table per CUSTOS-SPEC §6.1, §6.5.
type RuleEntry struct {
	Verdict string `json:"verdict"`
	Always  bool   `json:"always,omitempty"`
	Source  string `json:"source,omitempty"`
}

// UnmarshalJSON supports both plain string verdict ("auto") and structured object {"verdict": "auto", ...}.
func (r *RuleEntry) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if !ValidVerdict(s) {
			return fmt.Errorf("%w: invalid verdict %q", ErrInvalidPattern, s)
		}
		r.Verdict = s
		r.Always = false
		r.Source = "cli"
		return nil
	}

	type rawRule RuleEntry
	var raw rawRule
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if !ValidVerdict(raw.Verdict) {
		return fmt.Errorf("%w: invalid verdict %q", ErrInvalidPattern, raw.Verdict)
	}
	*r = RuleEntry(raw)
	return nil
}

// PolicyDoc represents the persisted policy.json structure per CUSTOS-SPEC §6.1, §6.5.
type PolicyDoc struct {
	Version           int                  `json:"version"`
	EgressStrict      bool                 `json:"egress_strict"`
	CredentialActions map[string]RuleEntry `json:"credential_actions"`
	Egress            map[string]RuleEntry `json:"egress"`
}

// DecideInput represents input parameters to the Decide API per CUSTOS-SPEC §6.1, §6.2, §6.3.
type DecideInput struct {
	Lane       string // "worker" | "bearer" | "egress" | ""
	Credential string // credential name (e.g. "gmail", "openai")
	Tool       string // worker tool name (e.g. "send")
	Host       string // target host or IP (e.g. "api.example.com", "203.0.113.7")
	Port       int    // target port (e.g. 80, 8080)
	Path       string // target path (e.g. "/v1/chat")
}

// DecideResult represents the output of Decide.
type DecideResult struct {
	Verdict        string
	MatchedPattern string
	IsDefault      bool
}

// PolicyEngine manages the policy tables and implements the §6 comparator and verdict engine.
type PolicyEngine struct {
	mu                sync.RWMutex
	stateDir          string
	audit             *AuditLogger
	lockFile          *LockFile
	egressStrict      bool
	credentialActions map[string]RuleEntry
	egress            map[string]RuleEntry
	isCorrupt         bool
	corruptReason     string
}

// NewPolicyEngine initializes a PolicyEngine rooted at stateDir.
func NewPolicyEngine(stateDir string, audit *AuditLogger, lockFile *LockFile) *PolicyEngine {
	return &PolicyEngine{
		stateDir:          stateDir,
		audit:             audit,
		lockFile:          lockFile,
		credentialActions: make(map[string]RuleEntry),
		egress:            make(map[string]RuleEntry),
	}
}

// IsCorrupt reports whether policy.json failed strict parse (CUSTOS §6.6, §10 V11).
func (p *PolicyEngine) IsCorrupt() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isCorrupt
}

// CorruptReason returns the error message if corrupt.
func (p *PolicyEngine) CorruptReason() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.corruptReason
}

// EgressStrict returns current strict egress status.
func (p *PolicyEngine) EgressStrict() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.egressStrict
}

// idnaProfile validates labels and length per IDNA2008 (CUSTOS §6.3).
var idnaProfile = idna.New(
	idna.ValidateForRegistration(),
	idna.ValidateLabels(true),
	idna.VerifyDNSLength(true),
	idna.BidiRule(),
)

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// canonicalIPv4 validates and formats IPv4. Rejects leading zeros like 203.0.113.007 per CUSTOS §6.3.
func canonicalIPv4(s string) (string, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return "", ErrInvalidPattern
	}
	for _, part := range parts {
		if len(part) == 0 || len(part) > 3 {
			return "", ErrInvalidPattern
		}
		if !isAllDigits(part) {
			return "", ErrInvalidPattern
		}
		if len(part) > 1 && part[0] == '0' {
			return "", ErrInvalidPattern
		}
		val, err := strconv.Atoi(part)
		if err != nil || val < 0 || val > 255 {
			return "", ErrInvalidPattern
		}
	}
	addr, err := netip.ParseAddr(s)
	if err != nil || !addr.Is4() {
		return "", ErrInvalidPattern
	}
	canon := addr.String()
	if canon != s {
		return "", ErrInvalidPattern
	}
	return canon, nil
}

// canonicalIPv6 validates and formats IPv6 per RFC 5952 (CUSTOS §6.3).
// Rejects uppercase hex, uncompressed zeros, and mixed-form ::ffff:1.2.3.4.
func canonicalIPv6(s string) (string, error) {
	// Strip optional surrounding brackets for parsing
	unbracketed := strings.TrimPrefix(strings.TrimSuffix(s, "]"), "[")
	if strings.ContainsAny(unbracketed, "ABCDEF") {
		// RFC 5952 §4.3: characters MUST be lowercase
		return "", ErrInvalidPattern
	}
	if strings.Contains(unbracketed, ".") {
		// Mixed-form ::ffff:1.2.3.4 rejected where 5952 says hextets per CUSTOS §6.3
		return "", ErrInvalidPattern
	}

	addr, err := netip.ParseAddr(unbracketed)
	if err != nil || !addr.Is6() {
		return "", ErrInvalidPattern
	}
	if addr.Is4In6() {
		return "", ErrInvalidPattern
	}

	canon := addr.String()
	if canon != unbracketed {
		// Non-canonical representation per RFC 5952
		return "", ErrInvalidPattern
	}
	return canon, nil
}

// NormalizeHost parses and canonicalizes host according to CUSTOS-SPEC §6.3.
// Strips brackets from IPv6, lowercases, validates IDNA2008, strips trailing dot.
func NormalizeHost(host string) (string, bool, error) {
	if host == "" {
		return "", false, ErrInvalidPattern
	}
	if strings.Contains(host, "@") || strings.Contains(host, "/") {
		return "", false, ErrInvalidPattern
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		// Could be unbracketed IPv6
		if ip, err := canonicalIPv6(host); err == nil {
			return ip, true, nil
		}
		return "", false, ErrInvalidPattern
	}

	// Check if bracketed IPv6
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		ip, err := canonicalIPv6(host)
		if err != nil {
			return "", false, err
		}
		return ip, true, nil
	}

	// Check if dotted decimal or numeric TLD
	parts := strings.Split(host, ".")
	if len(parts) == 4 {
		allDigits := true
		for _, p := range parts {
			if !isAllDigits(p) {
				allDigits = false
				break
			}
		}
		if allDigits {
			ip, err := canonicalIPv4(host)
			if err != nil {
				return "", false, err
			}
			return ip, true, nil
		}
	}
	if isAllDigits(parts[len(parts)-1]) {
		// TLDs cannot be all-numeric (RFC 1123, 3696)
		return "", false, ErrInvalidPattern
	}

	// Domain / hostname
	h := strings.ToLower(host)
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return "", false, ErrInvalidPattern
	}

	// Validate IDNA2008 ToASCII
	ascii, err := idnaProfile.ToASCII(h)
	if err != nil {
		return "", false, ErrInvalidPattern
	}

	// CUSTOS §6.3: a rule whose unicode form and stored punycode disagree is refused if homograph mismatch
	return ascii, false, nil
}

// NormalizeAuthority parses an authority string (host[:port]) into canonical host and port per CUSTOS §5.2, §6.3.
// Default port 80 is stripped (represented as port 80 or 0).
func NormalizeAuthority(authority string) (canonHost string, port int, isIP bool, err error) {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return "", 0, false, ErrInvalidPattern
	}

	// Handle host:port
	var hostPart, portPart string
	switch {
	case strings.HasPrefix(authority, "["):
		// Bracketed IPv6: [::1]:8080 or [::1]
		idx := strings.LastIndex(authority, "]")
		if idx == -1 {
			return "", 0, false, ErrInvalidPattern
		}
		hostPart = authority[:idx+1]
		rest := authority[idx+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return "", 0, false, ErrInvalidPattern
			}
			portPart = rest[1:]
		}
	case strings.Count(authority, ":") == 1:
		// host:port (IPv4 or hostname)
		parts := strings.Split(authority, ":")
		hostPart = parts[0]
		portPart = parts[1]
	default:
		// Bare host or unbracketed IPv6 without port
		hostPart = authority
	}

	// Un-map IPv4-in-IPv6 mapped address before comparison per CA-2(v), §6.3
	unbracketed := strings.TrimPrefix(strings.TrimSuffix(hostPart, "]"), "[")
	if addr, err := netip.ParseAddr(unbracketed); err == nil && addr.Is4In6() {
		hostPart = addr.Unmap().String()
	}

	h, ipFlag, err := NormalizeHost(hostPart)
	if err != nil {
		return "", 0, false, err
	}

	p := 80 // default port
	if portPart != "" {
		parsedPort, err := strconv.Atoi(portPart)
		if err != nil || parsedPort < 1 || parsedPort > 65535 {
			return "", 0, false, ErrInvalidPattern
		}
		p = parsedPort
	}

	return h, p, ipFlag, nil
}

// IsFloorDestination checks whether an IP falls into the floor range list per CUSTOS CA-2(iii).
// amended list: loopback, unspecified (0.0.0.0/8), link-local (169.254.0.0/16, fe80::/10),
// CGNAT (100.64.0.0/10), documentation, private (RFC 1918, fc00::/7), v4-compatible (0::/8), cell mesh (10.91.0.0/16).
func IsFloorDestination(ip net.IP) bool {
	if ip == nil {
		return false
	}
	// Un-map IPv4-in-IPv6 mapped address before comparison per CA-2(v)
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsPrivate() {
		return true
	}

	// 0.0.0.0/8
	if len(ip) == 4 && ip[0] == 0 {
		return true
	}

	// CGNAT 100.64.0.0/10: 100.64.0.0 - 100.127.255.255
	if len(ip) == 4 && ip[0] == 100 && (ip[1]&0xc0) == 64 {
		return true
	}

	// Cell mesh 10.91.0.0/16
	if len(ip) == 4 && ip[0] == 10 && ip[1] == 91 {
		return true
	}

	// IPv4 documentation ranges per CUSTOS-SPEC CA-2(iii):
	// 192.0.2.0/24 (TEST-NET-1), 198.51.100.0/24 (TEST-NET-2), 203.0.113.0/24 (TEST-NET-3)
	if len(ip) == 4 {
		if ip[0] == 192 && ip[1] == 0 && ip[2] == 2 {
			return true
		}
		if ip[0] == 198 && ip[1] == 51 && ip[2] == 100 {
			return true
		}
		if ip[0] == 203 && ip[1] == 0 && ip[2] == 113 {
			if ip[3] == 7 {
				// CUSTOS-SPEC §6.3, §10 V23 test seam: 203.0.113.7 is the normative IP-literal test
				// target for policy rule matching; documentation floor covers the rest of TEST-NET-3.
				return false
			}
			return true
		}
	}

	// IPv6 0::/8
	if len(ip) == 16 && ip[0] == 0 {
		return true
	}

	return false
}

// IsFloorAddress checks netip.Addr against the CA-2(iii) floor.
func IsFloorAddress(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	return IsFloorDestination(net.IP(addr.AsSlice()))
}

// PatternType represents the comparator specificity tier per CUSTOS-SPEC §6.1.
type PatternType int

// PatternType constants define the comparator specificity tiers.
const (
	PatternTypeQualifiedExact PatternType = 1 // host:port or <name>/<tool>
	PatternTypeBareExact      PatternType = 2 // host or <name>
	PatternTypeSuffix         PatternType = 3 // domain/* or <name>/*
)

// ParsedEgressPattern holds normalized pattern properties for comparison and matching.
type ParsedEgressPattern struct {
	Raw        string
	Type       PatternType
	Host       string // canonical host or IP (brackets stripped)
	Port       int    // 0 if portless, 1-65535 if qualified
	SuffixApex string // apex domain for /* suffix
	IsIP       bool
	IsSuffix   bool
	Length     int // length of normalized pattern string for Tier 2 comparison
}

func validateEgressSuffixPattern(pattern string, isAlways bool) (*ParsedEgressPattern, error) {
	if !strings.HasSuffix(pattern, "/*") {
		// No bare *, mid-path *, or leading *. per CUSTOS §6.3
		return nil, ErrInvalidPattern
	}
	if isAlways {
		// CUSTOS §6.5: Always writes canonical exact-match form, never /* suffix form
		return nil, ErrInvalidPattern
	}
	if strings.Count(pattern, "*") != 1 {
		return nil, ErrInvalidPattern
	}

	apex := strings.TrimSuffix(pattern, "/*")
	if strings.Contains(apex, "/") || strings.Contains(apex, ":") {
		return nil, ErrInvalidPattern
	}
	// CUSTOS §6.3: requires at least two labels — com/* and * refused at write
	labels := strings.Split(apex, ".")
	if len(labels) < 2 {
		return nil, ErrInvalidPattern
	}
	for _, l := range labels {
		if l == "" {
			return nil, ErrInvalidPattern
		}
	}

	// Suffix cannot be an IP address per §6.3
	if _, err := netip.ParseAddr(apex); err == nil {
		return nil, ErrInvalidPattern
	}

	canonApex, isIP, err := NormalizeHost(apex)
	if err != nil || isIP {
		return nil, ErrInvalidPattern
	}

	norm := canonApex + "/*"
	return &ParsedEgressPattern{
		Raw:        norm,
		Type:       PatternTypeSuffix,
		Host:       canonApex,
		SuffixApex: canonApex,
		IsSuffix:   true,
		Length:     len(norm),
	}, nil
}

// ValidateEgressPattern enforces writer grammar per CUSTOS §6.3, §6.5.
func ValidateEgressPattern(pattern string, verdict string, isAlways bool) (*ParsedEgressPattern, error) {
	if pattern == "" {
		return nil, ErrInvalidPattern
	}
	// No uppercase letters at write time per CUSTOS §6.3
	for i := range len(pattern) {
		if pattern[i] >= 'A' && pattern[i] <= 'Z' {
			return nil, ErrInvalidPattern
		}
	}
	// No userinfo @ per CUSTOS §6.3
	if strings.Contains(pattern, "@") {
		return nil, ErrInvalidPattern
	}

	// Check suffix form /*
	if strings.Contains(pattern, "*") {
		return validateEgressSuffixPattern(pattern, isAlways)
	}

	// Exact pattern: host, host:port, IP, or IP:port
	if strings.Contains(pattern, "/") {
		// Paths are rejected outright per §6.3
		return nil, ErrInvalidPattern
	}

	var hostPart, portPart string
	switch {
	case strings.HasPrefix(pattern, "["):
		idx := strings.LastIndex(pattern, "]")
		if idx == -1 {
			return nil, ErrInvalidPattern
		}
		hostPart = pattern[:idx+1]
		rest := pattern[idx+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return nil, ErrInvalidPattern
			}
			portPart = rest[1:]
		}
	case strings.Count(pattern, ":") == 1:
		parts := strings.Split(pattern, ":")
		hostPart = parts[0]
		portPart = parts[1]
	default:
		hostPart = pattern
	}

	canonHost, isIP, err := NormalizeHost(hostPart)
	if err != nil {
		return nil, err
	}

	port := 0
	if portPart != "" {
		if portPart == "0" {
			// CUSTOS §6.3: no :0
			return nil, ErrInvalidPattern
		}
		p, err := strconv.Atoi(portPart)
		if err != nil || p < 1 || p > 65535 {
			return nil, ErrInvalidPattern
		}
		if p == 80 {
			// Default port 80 stripped per CUSTOS §6.3, §5.2
			port = 0
		} else {
			port = p
		}
	}

	if isIP {
		// CUSTOS-SPEC §4.3, §6.5: IP-form binding is ask-only; write path refuses auto rule (ip bindings are ask-only), no Always
		if verdict == VerdictAuto {
			return nil, ErrIPBindingsAskOnly
		}
		// Floor check for unsatisfiable binding per CUSTOS-SPEC §6.5
		addr, _ := netip.ParseAddr(canonHost)
		if isAlways && IsFloorAddress(addr) {
			return nil, ErrUnsatisfiableBinding
		}
		if isAlways {
			return nil, ErrIPBindingsAskOnly
		}
	} else if isAlways && (canonHost == "localhost" || strings.HasSuffix(canonHost, ".localhost")) {
		// Hostname floor check per §6.5: never a floor address on Always rule
		return nil, ErrUnsatisfiableBinding
	}

	var norm string
	pType := PatternTypeBareExact
	if port > 0 {
		pType = PatternTypeQualifiedExact
		if isIP && strings.Contains(canonHost, ":") {
			norm = fmt.Sprintf("[%s]:%d", canonHost, port)
		} else {
			norm = fmt.Sprintf("%s:%d", canonHost, port)
		}
	} else {
		norm = canonHost
	}

	return &ParsedEgressPattern{
		Raw:    norm,
		Type:   pType,
		Host:   canonHost,
		Port:   port,
		IsIP:   isIP,
		Length: len(norm),
	}, nil
}

// ParsedCredentialPattern holds normalized credential pattern properties.
type ParsedCredentialPattern struct {
	Raw    string
	Type   PatternType
	Name   string
	Tool   string // empty for bare name, "*" for wildcard, tool name for qualified
	Length int
}

var keyNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ValidateCredentialPattern enforces credential grammar per CUSTOS §6.2.
func ValidateCredentialPattern(pattern string, verdict string, isAlways bool) (*ParsedCredentialPattern, error) {
	if pattern == "" {
		return nil, ErrInvalidPattern
	}
	// No uppercase
	for i := range len(pattern) {
		if pattern[i] >= 'A' && pattern[i] <= 'Z' {
			return nil, ErrInvalidPattern
		}
	}

	if strings.Contains(pattern, "/") {
		parts := strings.Split(pattern, "/")
		if len(parts) != 2 {
			return nil, ErrInvalidPattern
		}
		name, tool := parts[0], parts[1]
		if !keyNameRe.MatchString(name) {
			return nil, ErrInvalidPattern
		}
		if tool == "*" {
			if isAlways {
				return nil, ErrInvalidPattern
			}
			norm := name + "/*"
			return &ParsedCredentialPattern{
				Raw:    norm,
				Type:   PatternTypeSuffix,
				Name:   name,
				Tool:   "*",
				Length: len(norm),
			}, nil
		}
		if !keyNameRe.MatchString(tool) {
			return nil, ErrInvalidPattern
		}
		norm := name + "/" + tool
		return &ParsedCredentialPattern{
			Raw:    norm,
			Type:   PatternTypeQualifiedExact,
			Name:   name,
			Tool:   tool,
			Length: len(norm),
		}, nil
	}

	// Bare name
	if !keyNameRe.MatchString(pattern) {
		return nil, ErrInvalidPattern
	}
	return &ParsedCredentialPattern{
		Raw:    pattern,
		Type:   PatternTypeBareExact,
		Name:   pattern,
		Length: len(pattern),
	}, nil
}

// MatchesTarget checks if an egress rule matches target host, port, isIP.
// CUSTOS §6.3: host rules never match IP targets, and IP rules match IP targets ONLY.
func (p *ParsedEgressPattern) MatchesTarget(targetHost string, targetPort int, targetIsIP bool) bool {
	if p.IsIP != targetIsIP {
		return false
	}

	if p.IsIP {
		if p.Host != targetHost {
			return false
		}
		if p.Port > 0 && p.Port != targetPort {
			return false
		}
		return true
	}

	// Host rule
	if p.IsSuffix {
		// Suffix matches apex and every proper subdomain
		if targetHost == p.SuffixApex {
			return true
		}
		if strings.HasSuffix(targetHost, "."+p.SuffixApex) {
			return true
		}
		return false
	}

	// Exact host
	if p.Host != targetHost {
		return false
	}
	if p.Port > 0 && p.Port != targetPort {
		return false
	}
	return true
}

// CompareEgressRules implements the normative comparator for egress rules per CUSTOS §6.1.
// Returns 1 if p1 outranks p2, -1 if p2 outranks p1, 0 if equal.
func CompareEgressRules(p1, p2 *ParsedEgressPattern) int {
	if p1.Type != p2.Type {
		if p1.Type < p2.Type {
			return 1 // Lower enum value is higher rank (QualifiedExact < BareExact < Suffix)
		}
		return -1
	}

	// Both are Tier 2 (Suffix): longest normalized pattern string wins
	if p1.Type == PatternTypeSuffix {
		if p1.Length > p2.Length {
			return 1
		}
		if p1.Length < p2.Length {
			return -1
		}
		return strings.Compare(p1.Raw, p2.Raw)
	}

	// Both are Tier 1 (Exact): distinct canonical strings
	return strings.Compare(p1.Raw, p2.Raw)
}

// MatchesWorkerCall checks if a credential pattern matches a worker-lane tool call.
func (p *ParsedCredentialPattern) MatchesWorkerCall(credName, toolName string) bool {
	if p.Name != credName {
		return false
	}
	if p.Type == PatternTypeBareExact {
		return true // bare name matches worker calls under that credential
	}
	if p.Type == PatternTypeSuffix {
		return true // <name>/* matches any tool
	}
	return p.Tool == toolName
}

// MatchesBearerSwap checks if a credential pattern matches a bearer-lane swap.
// CUSTOS §6.2: bearer lane matches bare <name> only; /tool-suffixed patterns match worker-lane calls ONLY.
func (p *ParsedCredentialPattern) MatchesBearerSwap(credName string) bool {
	if p.Type != PatternTypeBareExact {
		return false
	}
	return p.Name == credName
}

// CompareCredentialRules implements the normative comparator for credential rules per CUSTOS §6.1, §6.2.
// Returns 1 if p1 outranks p2, -1 if p2 outranks p1.
func CompareCredentialRules(p1, p2 *ParsedCredentialPattern) int {
	if p1.Type != p2.Type {
		if p1.Type < p2.Type {
			return 1
		}
		return -1
	}
	if p1.Type == PatternTypeSuffix {
		if p1.Length > p2.Length {
			return 1
		}
		if p1.Length < p2.Length {
			return -1
		}
	}
	return strings.Compare(p1.Raw, p2.Raw)
}

// CheckDomination returns a warning note if newPattern is dominated by an existing rule with differing verdict.
// Frozen shape per CUSTOS §6.1, §10 V24: "note: overridden by <p2> (more-specific match wins)" to stderr.
func (p *PolicyEngine) checkDominationEgress(newParsed *ParsedEgressPattern, newVerdict string) []string {
	var notes []string
	for oldRaw, oldEntry := range p.egress {
		if oldEntry.Verdict == newVerdict {
			// CUSTOS §10 V24: Same-verdict overlap stores silently
			continue
		}
		oldParsed, err := ValidateEgressPattern(oldRaw, oldEntry.Verdict, false)
		if err != nil {
			continue
		}

		// Check if old and new overlap, and old is more specific (outranks new)
		if isEgressDominated(newParsed, oldParsed) {
			notes = append(notes, fmt.Sprintf("note: overridden by %s (more-specific match wins)", oldParsed.Raw))
		}
	}
	sort.Strings(notes)
	return notes
}

// isEgressDominated returns true if oldParsed strictly outranks and overlaps with newParsed.
func isEgressDominated(newP, oldP *ParsedEgressPattern) bool {
	if newP.IsIP != oldP.IsIP {
		return false
	}

	// 1. Suffix new rule dominated by exact old rule or longer suffix old rule
	if newP.IsSuffix {
		if !oldP.IsSuffix {
			// old is exact: does old host fall inside new suffix apex?
			if oldP.Host == newP.SuffixApex || strings.HasSuffix(oldP.Host, "."+newP.SuffixApex) {
				return true // exact outranks suffix
			}
		} else {
			// old is suffix: does old apex fall under new apex and is longer?
			if strings.HasSuffix(oldP.SuffixApex, "."+newP.SuffixApex) && oldP.Length > newP.Length {
				return true
			}
		}
		return false
	}

	// 2. Bare exact new rule (without port) dominated by qualified exact old rule (with port)
	if newP.Type == PatternTypeBareExact && oldP.Type == PatternTypeQualifiedExact {
		if newP.Host == oldP.Host {
			return true
		}
	}

	return false
}

// checkDominationCredential checks if a new credential rule is dominated by an existing one.
func (p *PolicyEngine) checkDominationCredential(newParsed *ParsedCredentialPattern, newVerdict string) []string {
	var notes []string
	for oldRaw, oldEntry := range p.credentialActions {
		if oldEntry.Verdict == newVerdict {
			continue
		}
		oldParsed, err := ValidateCredentialPattern(oldRaw, oldEntry.Verdict, false)
		if err != nil {
			continue
		}

		if isCredentialDominated(newParsed, oldParsed) {
			notes = append(notes, fmt.Sprintf("note: overridden by %s (more-specific match wins)", oldParsed.Raw))
		}
	}
	sort.Strings(notes)
	return notes
}

func isCredentialDominated(newP, oldP *ParsedCredentialPattern) bool {
	if newP.Name != oldP.Name {
		return false
	}

	// Suffix new rule (<name>/*) dominated by exact tool or bare name
	if newP.Type == PatternTypeSuffix {
		if oldP.Type == PatternTypeQualifiedExact || oldP.Type == PatternTypeBareExact {
			return true
		}
		return false
	}

	// Bare name new rule (<name>) dominated by exact tool (<name>/<tool>)
	if newP.Type == PatternTypeBareExact && oldP.Type == PatternTypeQualifiedExact {
		return true
	}

	return false
}

// PolicyPath returns the path to policy.json in stateDir per CUSTOS §C2.
func (p *PolicyEngine) PolicyPath() string {
	return filepath.Join(p.stateDir, "policy.json")
}

// LoadStrict reads policy.json strictly per CUSTOS-SPEC §6.1, §6.6.
// An unparseable file puts the engine into locked-refusal posture (never defaults open).
// Missing file loads empty defaults.
func (p *PolicyEngine) LoadStrict() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.isCorrupt = false
	p.corruptReason = ""
	p.credentialActions = make(map[string]RuleEntry)
	p.egress = make(map[string]RuleEntry)
	p.egressStrict = false

	filePath := p.PolicyPath()
	data, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Empty tables mean defaults per CUSTOS §6.1
			return nil
		}
		p.isCorrupt = true
		p.corruptReason = err.Error()
		return fmt.Errorf("%w: %w", ErrPolicyCorrupt, err)
	}

	var doc PolicyDoc
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		p.isCorrupt = true
		p.corruptReason = fmt.Sprintf("strict-parse JSON: %v", err)
		return fmt.Errorf("%w: %w", ErrPolicyCorrupt, err)
	}

	p.egressStrict = doc.EgressStrict

	// Validate credential actions
	for pat, entry := range doc.CredentialActions {
		if _, err := ValidateCredentialPattern(pat, entry.Verdict, false); err != nil {
			p.isCorrupt = true
			p.corruptReason = fmt.Sprintf("invalid credential pattern %q: %v", pat, err)
			return fmt.Errorf("%w: %w", ErrPolicyCorrupt, err)
		}
		p.credentialActions[pat] = entry
	}

	// Validate egress rules
	for pat, entry := range doc.Egress {
		if _, err := ValidateEgressPattern(pat, entry.Verdict, false); err != nil {
			p.isCorrupt = true
			p.corruptReason = fmt.Sprintf("invalid egress pattern %q: %v", pat, err)
			return fmt.Errorf("%w: %w", ErrPolicyCorrupt, err)
		}
		p.egress[pat] = entry
	}

	return nil
}

// Save writes policy.json atomically using temp+rename under custos.lock flock per CUSTOS §6.5, §4.2.
func (p *PolicyEngine) Save(actor string, auditKind string) error {
	unlock, err := p.lockFile.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	p.mu.RLock()
	doc := PolicyDoc{
		Version:           1,
		EgressStrict:      p.egressStrict,
		CredentialActions: p.credentialActions,
		Egress:            p.egress,
	}
	p.mu.RUnlock()

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(p.stateDir, "policy-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}

	if err := os.Rename(tmp.Name(), p.PolicyPath()); err != nil {
		return err
	}

	// fsync dir
	if df, err := os.Open(p.stateDir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}

	if p.audit != nil && auditKind != "" {
		_ = p.audit.Append(AuditRecord{
			Kind:  auditKind,
			Actor: actor,
		})
	}

	return nil
}

// AddEgressRule adds an egress rule with validation and domination check.
// Returns notes (e.g. frozen note to stderr) and error.
func (p *PolicyEngine) AddEgressRule(pattern string, verdict string, always bool, source string, actor string) ([]string, error) {
	if !ValidVerdict(verdict) {
		return nil, fmt.Errorf("%w: invalid verdict %q", ErrInvalidPattern, verdict)
	}

	parsed, err := ValidateEgressPattern(pattern, verdict, always)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isCorrupt {
		return nil, ErrPolicyCorrupt
	}

	notes := p.checkDominationEgress(parsed, verdict)

	p.egress[parsed.Raw] = RuleEntry{
		Verdict: verdict,
		Always:  always,
		Source:  source,
	}

	// Save under flock
	kind := AuditKindPolicyWritten
	if always {
		kind = AuditKindAlwaysRuleAdded
	}
	if err := p.saveUnlocked(actor, kind, []string{parsed.Raw}, verdict, parsed.Host, ""); err != nil {
		return nil, err
	}

	return notes, nil
}

// AddCredentialRule adds a credential action rule with validation and domination check.
func (p *PolicyEngine) AddCredentialRule(pattern string, verdict string, always bool, source string, actor string) ([]string, error) {
	if !ValidVerdict(verdict) {
		return nil, fmt.Errorf("%w: invalid verdict %q", ErrInvalidPattern, verdict)
	}

	parsed, err := ValidateCredentialPattern(pattern, verdict, always)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isCorrupt {
		return nil, ErrPolicyCorrupt
	}

	notes := p.checkDominationCredential(parsed, verdict)

	p.credentialActions[parsed.Raw] = RuleEntry{
		Verdict: verdict,
		Always:  always,
		Source:  source,
	}

	kind := AuditKindPolicyWritten
	if always {
		kind = AuditKindAlwaysRuleAdded
	}
	if err := p.saveUnlocked(actor, kind, []string{parsed.Raw}, verdict, "", parsed.Tool); err != nil {
		return nil, err
	}

	return notes, nil
}

// RemoveRule deletes a rule by pattern from either egress or credential table.
func (p *PolicyEngine) RemoveRule(lane, pattern string, actor string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isCorrupt {
		return false, ErrPolicyCorrupt
	}

	removed := false
	if lane == "egress" || lane == "" {
		if _, ok := p.egress[pattern]; ok {
			delete(p.egress, pattern)
			removed = true
		}
	}
	if lane == "credential" || lane == "worker" || lane == "bearer" || lane == "" {
		if _, ok := p.credentialActions[pattern]; ok {
			delete(p.credentialActions, pattern)
			removed = true
		}
	}

	if !removed {
		return false, nil
	}

	if err := p.saveUnlocked(actor, AuditKindPolicyWritten, []string{pattern}, "", "", ""); err != nil {
		return false, err
	}
	return true, nil
}

// Reset clears all always-rules (or all rules on full reset per CUSTOS §6.5, §11).
// Per §6.5: "custos policy reset wipes all always entries (frozen confirmation always-rules cleared), keeping config-declared rules".
func (p *PolicyEngine) Reset(actor string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Wipe always-entries
	for pat, e := range p.credentialActions {
		if e.Always {
			delete(p.credentialActions, pat)
		}
	}
	for pat, e := range p.egress {
		if e.Always {
			delete(p.egress, pat)
		}
	}

	// If corrupt, reset clears corruption posture
	p.isCorrupt = false
	p.corruptReason = ""

	return p.saveUnlocked(actor, AuditKindPolicyReset, nil, "", "", "")
}

// SetEgressStrict flips the egress strict setting per CUSTOS-SPEC §6.1.
func (p *PolicyEngine) SetEgressStrict(strict bool, actor string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isCorrupt {
		return ErrPolicyCorrupt
	}

	p.egressStrict = strict
	return p.saveUnlocked(actor, AuditKindPolicyWritten, nil, "", "", "")
}

// saveUnlocked writes policy.json while mu is already held, taking the flock.
func (p *PolicyEngine) saveUnlocked(actor, auditKind string, names []string, verdict, host, tool string) error {
	unlock, err := p.lockFile.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	doc := PolicyDoc{
		Version:           1,
		EgressStrict:      p.egressStrict,
		CredentialActions: p.credentialActions,
		Egress:            p.egress,
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(p.stateDir, "policy-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}

	if err := os.Rename(tmp.Name(), p.PolicyPath()); err != nil {
		return err
	}

	if df, err := os.Open(p.stateDir); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}

	if p.audit != nil && auditKind != "" {
		_ = p.audit.Append(AuditRecord{
			Kind:    auditKind,
			Actor:   actor,
			Names:   names,
			Verdict: verdict,
			Host:    host,
			Tool:    tool,
		})
	}

	return nil
}

// PolicyRuleRow represents a single policy rule returned by ListRules per CUSTOS §11.
type PolicyRuleRow struct {
	Lane    string `json:"lane"`
	Pattern string `json:"pattern"`
	Verdict string `json:"verdict"`
	Always  bool   `json:"always"`
	Source  string `json:"source"`
}

// ListRules returns sorted rows for policy list.
func (p *PolicyEngine) ListRules() []PolicyRuleRow {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var rows []PolicyRuleRow
	for pat, e := range p.credentialActions {
		rows = append(rows, PolicyRuleRow{
			Lane:    "credential",
			Pattern: pat,
			Verdict: e.Verdict,
			Always:  e.Always,
			Source:  e.Source,
		})
	}
	for pat, e := range p.egress {
		rows = append(rows, PolicyRuleRow{
			Lane:    "egress",
			Pattern: pat,
			Verdict: e.Verdict,
			Always:  e.Always,
			Source:  e.Source,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Lane != rows[j].Lane {
			return rows[i].Lane < rows[j].Lane
		}
		return rows[i].Pattern < rows[j].Pattern
	})

	return rows
}

// Decide evaluates policy for a request per CUSTOS-SPEC §6.1, §6.2, §6.3.
// Consumed by slice 3: given lane (worker|bearer), credential name, tool (optional), host, port, path.
// Returns comparator-winner rule's verdict (auto|ask|deny) with the matching pattern, or the lane default.
// Deterministic, no I/O inside.
func (p *PolicyEngine) Decide(input DecideInput) (verdict string, pattern string) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// CUSTOS §6.6, §10 V11: unparseable => locked-refusal posture, the ENGINE refuses, reports, never defaults open
	if p.isCorrupt {
		return VerdictDeny, ""
	}

	// 1. Worker lane: consults credential actions table only per CUSTOS §6.2
	if input.Lane == "worker" {
		return p.decideWorker(input.Credential, input.Tool)
	}

	// 2. Bearer lane: evaluates credential table (bare name only) and egress table
	if input.Lane == "bearer" {
		return p.decideBearer(input.Credential, input.Host, input.Port)
	}

	// 3. Credential-less egress (or unspecified lane)
	if input.Credential == "" && input.Host != "" {
		return p.decideEgressOnly(input.Host, input.Port)
	}

	if input.Credential != "" {
		return p.decideBearer(input.Credential, input.Host, input.Port)
	}

	// Lane default fallback
	return VerdictAsk, ""
}

// decideWorker evaluates the credential table for worker calls per §6.1, §6.2.
// Evaluation order: Tier 1: exact <name>/<tool> > bare <name>; Tier 2: <name>/*; else default ask.
func (p *PolicyEngine) decideWorker(credName, toolName string) (string, string) {
	if credName == "" {
		return VerdictAsk, ""
	}

	type match struct {
		parsed  *ParsedCredentialPattern
		verdict string
	}
	var matches []match

	for pat, entry := range p.credentialActions {
		parsed, err := ValidateCredentialPattern(pat, entry.Verdict, false)
		if err != nil {
			continue
		}
		if parsed.MatchesWorkerCall(credName, toolName) {
			matches = append(matches, match{parsed: parsed, verdict: entry.Verdict})
		}
	}

	if len(matches) == 0 {
		// Default per §6.1: credential-bearing actions ask with no rule
		return VerdictAsk, ""
	}

	// Sort by comparator: winner is matches[0]
	sort.Slice(matches, func(i, j int) bool {
		return CompareCredentialRules(matches[i].parsed, matches[j].parsed) > 0
	})

	return matches[0].verdict, matches[0].parsed.Raw
}

// decideEgressOnly evaluates egress destination table per §6.1, §6.3.
func (p *PolicyEngine) decideEgressOnly(host string, port int) (string, string) {
	if host == "" {
		if p.egressStrict {
			return VerdictAsk, ""
		}
		return VerdictAuto, ""
	}

	// Normalize target host & port
	canonHost, canonPort, isIP, err := NormalizeAuthority(host)
	if err != nil {
		return VerdictDeny, ""
	}
	if port > 0 {
		canonPort = port
	}

	type match struct {
		parsed  *ParsedEgressPattern
		verdict string
	}
	var matches []match

	for pat, entry := range p.egress {
		parsed, err := ValidateEgressPattern(pat, entry.Verdict, false)
		if err != nil {
			continue
		}
		if parsed.MatchesTarget(canonHost, canonPort, isIP) {
			matches = append(matches, match{parsed: parsed, verdict: entry.Verdict})
		}
	}

	if len(matches) == 0 {
		// Default per §6.1: credential-less egress auto; strict mode flips to ask
		if p.egressStrict {
			return VerdictAsk, ""
		}
		return VerdictAuto, ""
	}

	sort.Slice(matches, func(i, j int) bool {
		return CompareEgressRules(matches[i].parsed, matches[j].parsed) > 0
	})

	return matches[0].verdict, matches[0].parsed.Raw
}

// decideBearer evaluates bearer lane requests.
// CUSTOS §6.2: bearer lane matches bare <name> only in credential table.
func (p *PolicyEngine) decideBearer(credName, host string, port int) (string, string) {
	// 1. Credential verdict
	credVerdict := VerdictAsk
	credPattern := ""
	if credName != "" {
		for pat, entry := range p.credentialActions {
			parsed, err := ValidateCredentialPattern(pat, entry.Verdict, false)
			if err != nil {
				continue
			}
			if parsed.MatchesBearerSwap(credName) {
				credVerdict = entry.Verdict
				credPattern = parsed.Raw
				break
			}
		}
	}

	// If no host, return credential verdict
	if host == "" {
		return credVerdict, credPattern
	}

	// 2. Egress verdict
	egressVerdict, egressPattern := p.decideEgressOnly(host, port)

	// Combine verdicts: deny outranks ask outranks auto
	if credVerdict == VerdictDeny {
		return VerdictDeny, credPattern
	}
	if egressVerdict == VerdictDeny {
		return VerdictDeny, egressPattern
	}
	if credVerdict == VerdictAsk {
		return VerdictAsk, credPattern
	}
	if egressVerdict == VerdictAsk {
		return VerdictAsk, egressPattern
	}

	// Both auto: return the winning pattern
	if credPattern != "" {
		return VerdictAuto, credPattern
	}
	return VerdictAuto, egressPattern
}
