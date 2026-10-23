package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lararium-app/lararium/internal/surface"
)

// createDoorToken creates a bearer token with label in the token store configured by cfg.
// Factored helper shared byte-identically with tokenCmd (spec §1.b, §3).
func createDoorToken(cfg *Config, label string) (string, error) {
	store, err := surface.OpenTokenStore(filepath.Join(cfg.Hearth.Home, "tokens.json"))
	if err != nil {
		return "", err
	}
	return store.Create(label)
}

// validateDeviceName checks that the device name is non-empty, <= 64 printable ASCII chars,
// and has no control characters (spec §1.a).
func validateDeviceName(name string) error {
	if len(name) == 0 {
		return errors.New("device name cannot be empty")
	}
	if len(name) > 64 {
		return errors.New("device name cannot exceed 64 characters")
	}
	for i := range name {
		b := name[i]
		if b < 32 || b > 126 {
			return errors.New("device name must contain only printable ASCII characters and no control characters")
		}
	}
	return nil
}

// defaultHostnameLabel returns the first DNS label of the current machine's hostname.
func defaultHostnameLabel() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return ""
	}
	parts := strings.Split(h, ".")
	label := parts[0]
	if err := validateDeviceName(label); err != nil {
		return ""
	}
	return label
}

// promptDeviceName prompts the operator for the hearth device name (spec §1.a).
func promptDeviceName(errOut io.Writer, readLine func(string) (string, error), defaultName string) (string, error) {
	prompt := "Device name for this hearth"
	if defaultName != "" {
		prompt += fmt.Sprintf(" [%s]", defaultName)
	}
	prompt += ": "

	for {
		raw, err := readLine(prompt)
		if err != nil {
			return "", err
		}
		if raw == "cancel" {
			return "", errors.New("setup canceled by operator")
		}

		// readLine already trims: an empty line accepts the default name.
		name := raw
		if name == "" {
			if defaultName == "" {
				fmt.Fprintln(errOut, "device name cannot be empty")
				continue
			}
			name = defaultName
		}

		if err := validateDeviceName(name); err != nil {
			fmt.Fprintln(errOut, err)
			continue
		}

		return name, nil
	}
}

// runFirstDoorStep executes step 1 of the first-door flow: device name prompt,
// in-process token creation, and single rendering of the ready URL with warning (spec §1).
func runFirstDoorStep(cfg *Config, out, errOut io.Writer, readLine func(string) (string, error), defaultDeviceName string) (string, error) {
	deviceName, err := promptDeviceName(errOut, readLine, defaultDeviceName)
	if err != nil {
		return "", err
	}

	token, err := createDoorToken(cfg, deviceName)
	if err != nil {
		return "", fmt.Errorf("creating first door token: %w", err)
	}

	// Print ONLY the "Open: http://<host>:<port>/#<token>" line and the single warning line (spec §1.c, §3).
	fmt.Fprintf(out, "Open: http://%s/#%s\n", serveHostPort(cfg), token)
	fmt.Fprintln(out, "Write it down now — it is shown only once.")

	return token, nil
}

// validateAllowedHost validates that raw is a bare host[:port] with no scheme,
// no path, and no whitespace (spec §2.b, §3).
func validateAllowedHost(raw string) error {
	if len(raw) == 0 {
		return errors.New("host cannot be empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return errors.New("host cannot contain whitespace")
	}
	if strings.Contains(raw, "/") {
		return errors.New("host cannot contain path or slash")
	}
	if strings.Contains(raw, "://") {
		return errors.New("host cannot contain URL scheme")
	}
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
		return errors.New("host cannot be a URL")
	}

	host := raw
	if strings.Contains(raw, ":") {
		h, p, err := net.SplitHostPort(raw)
		if err == nil {
			portNum, parseErr := strconv.Atoi(p)
			if parseErr != nil || portNum < 1 || portNum > 65535 {
				return errors.New("invalid port in host:port")
			}
			host = h
		} else {
			ip := net.ParseIP(strings.Trim(raw, "[]"))
			if ip == nil {
				return fmt.Errorf("invalid host %q", raw)
			}
			host = strings.Trim(raw, "[]")
		}
	}

	trimmedHost := strings.Trim(host, "[]")
	if ip := net.ParseIP(trimmedHost); ip != nil {
		return nil
	}

	if len(trimmedHost) > 253 {
		return errors.New("hostname exceeds 253 characters")
	}
	for _, part := range strings.Split(trimmedHost, ".") {
		if len(part) == 0 || len(part) > 63 {
			return errors.New("invalid hostname label length")
		}
		for i := range part {
			c := part[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
				return fmt.Errorf("invalid character %q in hostname", c)
			}
		}
		if part[0] == '-' || part[len(part)-1] == '-' {
			return errors.New("hostname label cannot start or end with hyphen")
		}
	}
	return nil
}

// isLoopbackHost checks whether host represents a loopback address (spec §2.b).
func isLoopbackHost(raw string) bool {
	h, _, err := net.SplitHostPort(raw)
	if err != nil {
		h = raw
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" || h == "::1" || strings.HasPrefix(h, "127.") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// appendAllowedHosts appends non-loopback host entries to serve.allowed_hosts in path,
// preserving comments and unrelated YAML via yaml.Node round-trip (spec §2.b).
func appendAllowedHosts(path string, newHosts []string) error {
	bakPath := path + ".bak"
	if _, err := os.Stat(bakPath); os.IsNotExist(err) {
		oldBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading config for backup: %w", err)
		}
		if err := atomicWriteFile(bakPath, oldBytes, 0o600); err != nil {
			return fmt.Errorf("writing backup %q: %w", bakPath, err)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decoding YAML nodes: %w", err)
	}

	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("invalid YAML structure in %s", path)
	}

	root := doc.Content[0]
	var serveNode *yaml.Node
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value == "serve" {
			serveNode = root.Content[i+1]
			break
		}
	}

	if serveNode == nil {
		serveNode = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "serve"},
			serveNode,
		)
	}

	var allowedHostsNode *yaml.Node
	for i := 0; i < len(serveNode.Content); i += 2 {
		if serveNode.Content[i].Value == "allowed_hosts" {
			allowedHostsNode = serveNode.Content[i+1]
			break
		}
	}

	if allowedHostsNode == nil {
		allowedHostsNode = &yaml.Node{Kind: yaml.SequenceNode}
		serveNode.Content = append(serveNode.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "allowed_hosts"},
			allowedHostsNode,
		)
	}

	existing := make(map[string]bool)
	for _, item := range allowedHostsNode.Content {
		existing[strings.ToLower(strings.Trim(item.Value, "[]"))] = true
	}

	appended := false
	for _, h := range newHosts {
		norm := strings.ToLower(strings.Trim(h, "[]"))
		if !existing[norm] {
			existing[norm] = true
			appended = true
			allowedHostsNode.Content = append(allowedHostsNode.Content, &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: h,
				Style: yaml.DoubleQuotedStyle,
			})
		}
	}

	if !appended {
		return nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encoding YAML nodes: %w", err)
	}

	fileMode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		fileMode = info.Mode().Perm()
	}

	// Validate the rewritten YAML BEFORE it can become the live config:
	// stage to a sibling temp file, load-check it, and only then rename.
	tmpPath := path + ".firstdoor.tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("staging updated config: %w", err)
	}
	if _, err := LoadConfig(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rewritten config would fail validation: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing updated config: %w", err)
	}
	if err := os.Chmod(path, fileMode); err != nil {
		return fmt.Errorf("chmod updated config: %w", err)
	}
	return nil
}

// promptRemoteAccess executes step 2 of the first-door flow: remote-access query,
// allowed_hosts appending, and guidance printing (spec §2).
func promptRemoteAccess(cfgPath string, cfg *Config, out, errOut io.Writer, readLine func(string) (string, error)) error {
	ans, err := readLine("Allow access from outside this machine (Tailscale/LAN)? [y/N]: ")
	if err != nil {
		return err
	}

	var confirmedHosts []string
	if strings.EqualFold(strings.TrimSpace(ans), "y") {
		seen := make(map[string]bool)
		for {
			rawHost, err := readLine("Allowed host (e.g. tailnet/LAN IP or hostname, empty to finish): ")
			if err != nil {
				return err
			}
			host := strings.TrimSpace(rawHost)
			if host == "" {
				break
			}

			if err := validateAllowedHost(host); err != nil {
				fmt.Fprintln(errOut, err)
				continue
			}

			if isLoopbackHost(host) {
				continue
			}

			norm := strings.ToLower(strings.Trim(host, "[]"))
			if seen[norm] {
				continue
			}
			seen[norm] = true
			confirmedHosts = append(confirmedHosts, host)
		}

		if len(confirmedHosts) > 0 {
			if err := appendAllowedHosts(cfgPath, confirmedHosts); err != nil {
				return fmt.Errorf("updating allowed_hosts: %w", err)
			}
		}
	}

	hostIP := "<host-ip>"
	if len(confirmedHosts) > 0 {
		h, _, err := net.SplitHostPort(confirmedHosts[0])
		if err != nil {
			h = confirmedHosts[0]
		}
		hostIP = h
	}

	port := "7717"
	if cfg != nil && cfg.Serve.Listen != "" {
		_, p, err := net.SplitHostPort(cfg.Serve.Listen)
		if err == nil && p != "" {
			port = p
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "ports:")
	fmt.Fprintf(out, "  - \"%s:%s:%s\"\n", hostIP, port, port)
	fmt.Fprintln(out, "The wizard never widens the network bind; edit your compose file as shown.")

	return nil
}

// runFirstDoorFlow executes the two interactive steps in order (spec §1, §2).
func runFirstDoorFlow(cfgPath string, cfg *Config, out, errOut io.Writer, readLine func(string) (string, error), defaultDeviceName string) error {
	if _, err := runFirstDoorStep(cfg, out, errOut, readLine, defaultDeviceName); err != nil {
		return err
	}
	return promptRemoteAccess(cfgPath, cfg, out, errOut, readLine)
}
