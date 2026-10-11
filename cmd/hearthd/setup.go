package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/lararium-app/lararium/internal/keystore"
)

// DiscoveredServer represents an OpenAI-compatible server found during local discovery.
type DiscoveredServer struct {
	BaseURL string
	Models  []string
}

// MenuItemKind identifies the canonical menu row type.
type MenuItemKind int

const (
	KindLocal MenuItemKind = iota
	KindPlaceholder
	KindActive
	KindAzureFoundry
	KindCustom
	KindKeepCurrent
	KindDiscovered = KindLocal
)

// MenuItem represents a single numbered choice in the setup wizard menu.
type MenuItem struct {
	Kind          MenuItemKind
	Title         string
	Name          string
	BaseURL       string
	APIKeyEnv     string
	AllowEmptyKey bool
}

// Endpoint URLs verified live 2026-10-11 (GET /models → 200/401 as expected); rows marked placeholder lack a supported auth flow yet.
var canonicalMenuItems = []MenuItem{
	{
		Kind:  KindLocal,
		Title: "Local models (run open models on this machine — no account or API key)",
		Name:  "local",
	},
	{
		Kind:  KindPlaceholder,
		Title: "Nous Portal (Everything your agent needs, 300+ models with bundled tool use)",
	},
	{
		Kind:      KindActive,
		Title:     "Fireworks AI (OpenAI-compatible direct model API)",
		Name:      "fireworks",
		BaseURL:   "https://api.fireworks.ai/inference/v1",
		APIKeyEnv: "FIREWORKS_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "OpenRouter (Pay-per-use API aggregator)",
		Name:      "openrouter",
		BaseURL:   "https://openrouter.ai/api/v1",
		APIKeyEnv: "OPENROUTER_API_KEY",
	},
	{
		Kind:  KindPlaceholder,
		Title: "Mixture of Agents (named presets; aggregator acts after reference models)",
	},
	{
		Kind:      KindActive,
		Title:     "NovitaAI (Cloud: Model API, Agent Sandbox, GPU Cloud)",
		Name:      "novita",
		BaseURL:   "https://api.novita.ai/openai/v1",
		APIKeyEnv: "NOVITA_API_KEY",
	},
	{
		Kind:          KindActive,
		Title:         "LM Studio (Local desktop app with built-in model server)",
		Name:          "lmstudio",
		BaseURL:       "http://127.0.0.1:1234/v1",
		APIKeyEnv:     "LMSTUDIO_API_KEY",
		AllowEmptyKey: true,
	},
	{
		Kind:      KindActive,
		Title:     "Anthropic (Claude models via API key)",
		Name:      "anthropic",
		BaseURL:   "https://api.anthropic.com/v1",
		APIKeyEnv: "ANTHROPIC_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "OpenAI (direct OpenAI API)",
		Name:      "openai",
		BaseURL:   "https://api.openai.com/v1",
		APIKeyEnv: "OPENAI_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Qwen (Qwen Cloud / DashScope API key)",
		Name:      "qwen",
		BaseURL:   "https://dashscope.aliyuncs.com/compatible-mode/v1",
		APIKeyEnv: "DASHSCOPE_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "xAI Grok (direct API)",
		Name:      "xai",
		BaseURL:   "https://api.x.ai/v1",
		APIKeyEnv: "XAI_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Xiaomi MiMo (MiMo-V2.5 and V2 models: pro, omni, flash)",
		Name:      "mimo",
		BaseURL:   "https://api.xiaomimimo.com/v1",
		APIKeyEnv: "MIMO_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Tencent Hunyuan (TokenHub)",
		Name:      "tencent_tokenhub",
		BaseURL:   "https://tokenhub.tencentmaas.com/v1",
		APIKeyEnv: "TENCENT_TOKENHUB_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "NVIDIA NIM (Nemotron models via build.nvidia.com or local NIM)",
		Name:      "nvidia",
		BaseURL:   "https://integrate.api.nvidia.com/v1",
		APIKeyEnv: "NVIDIA_API_KEY",
	},
	{
		Kind:  KindPlaceholder,
		Title: "GitHub Copilot",
	},
	{
		Kind:      KindActive,
		Title:     "Hugging Face Inference Providers",
		Name:      "huggingface",
		BaseURL:   "https://router.huggingface.co/v1",
		APIKeyEnv: "HF_TOKEN",
	},
	{
		Kind:      KindActive,
		Title:     "Google AI Studio (Gemini API)",
		Name:      "gemini",
		BaseURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
		APIKeyEnv: "GEMINI_API_KEY",
	},
	{
		Kind:  KindPlaceholder,
		Title: "Google Vertex AI (Gemini via GCP; OAuth2/ADC)",
	},
	{
		Kind:      KindActive,
		Title:     "DeepSeek (V3, R1, coder, direct API)",
		Name:      "deepseek",
		BaseURL:   "https://api.deepseek.com/v1",
		APIKeyEnv: "DEEPSEEK_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Z.AI / GLM (Zhipu direct API)",
		Name:      "zai",
		BaseURL:   "https://api.z.ai/api/paas/v4",
		APIKeyEnv: "ZAI_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Kimi / Moonshot (Moonshot global endpoint)",
		Name:      "moonshot",
		BaseURL:   "https://api.moonshot.ai/v1",
		APIKeyEnv: "MOONSHOT_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "StepFun Step Plan (Agent / coding models via Step Plan API)",
		Name:      "stepfun",
		BaseURL:   "https://api.stepfun.com/v1",
		APIKeyEnv: "STEPFUN_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "MiniMax (Global endpoint)",
		Name:      "minimax",
		BaseURL:   "https://api.minimax.io/v1",
		APIKeyEnv: "MINIMAX_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Ollama Cloud (Cloud-hosted open models, ollama.com)",
		Name:      "ollama",
		BaseURL:   "https://ollama.com/v1",
		APIKeyEnv: "OLLAMA_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Arcee AI (Trinity models, direct API)",
		Name:      "arcee",
		BaseURL:   "https://api.arcee.ai/api/v1",
		APIKeyEnv: "ARCEE_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "GMI Cloud (Multi-model direct API)",
		Name:      "gmi",
		BaseURL:   "https://api.gmi-serving.com/v1",
		APIKeyEnv: "GMI_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Kilo Code (Kilo Gateway API)",
		Name:      "kilo",
		BaseURL:   "https://api.kilo.ai/api/gateway",
		APIKeyEnv: "KILO_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "OpenCode Zen (pay-as-you-go)",
		Name:      "opencode",
		BaseURL:   "https://opencode.ai/zen/v1",
		APIKeyEnv: "OPENCODE_API_KEY",
	},
	{
		Kind:  KindPlaceholder,
		Title: "AWS Bedrock (IAM/CLI auth)",
	},
	{
		Kind:      KindAzureFoundry,
		Title:     "Azure Foundry (OpenAI-style endpoint, your Azure AI deployment)",
		Name:      "azure_foundry",
		APIKeyEnv: "AZURE_FOUNDRY_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Vercel AI Gateway (Multi-model aggregator)",
		Name:      "vercel_ai_gateway",
		BaseURL:   "https://ai-gateway.vercel.sh/v1",
		APIKeyEnv: "VERCEL_AI_GATEWAY_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Actual Computer (hosted inference via api.actual.inc)",
		Name:      "actual",
		BaseURL:   "https://api.actual.inc/v1",
		APIKeyEnv: "ACTUAL_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "DeepInfra (100+ open models, pay-per-use)",
		Name:      "deepinfra",
		BaseURL:   "https://api.deepinfra.com/v1/openai",
		APIKeyEnv: "DEEPINFRA_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Upstage (Solar API)",
		Name:      "upstage",
		BaseURL:   "https://api.upstage.ai/v1/solar",
		APIKeyEnv: "UPSTAGE_API_KEY",
	},
	{
		Kind:      KindActive,
		Title:     "Nebius Token Factory (OpenAI-compatible inference)",
		Name:      "nebius",
		BaseURL:   "https://api.tokenfactory.nebius.com/v1",
		APIKeyEnv: "NEBIUS_API_KEY",
	},
	{
		Kind:  KindCustom,
		Title: "Custom endpoint (enter URL manually)",
	},
	{
		Kind:  KindKeepCurrent,
		Title: "Keep current config and exit",
	},
}

// CanonicalMenuItems builds the single source of truth provider list for setup.
func CanonicalMenuItems(discovered ...[]DiscoveredServer) []MenuItem {
	items := make([]MenuItem, len(canonicalMenuItems))
	copy(items, canonicalMenuItems)
	return items
}

// RenderMenu prints the numbered choices to out.
func RenderMenu(out io.Writer, items []MenuItem) {
	for i, item := range items {
		fmt.Fprintf(out, "  %d) %s\n", i+1, item.Title)
	}
}

var defaultDiscoveryRoots = []string{
	"http://127.0.0.1:8080/v1",
	"http://127.0.0.1:11434/v1",
	"http://127.0.0.1:1234/v1",
	"http://127.0.0.1:8000/v1",
}

func getDiscoveryRoots() []string {
	roots := append([]string{}, defaultDiscoveryRoots...)
	if os.Getenv("LARARIUM_DOCKER") == "1" {
		roots = append(roots,
			"http://host.docker.internal:8080/v1",
			"http://host.docker.internal:11434/v1",
			"http://host.docker.internal:1234/v1",
			"http://host.docker.internal:8000/v1",
		)
	}
	return roots
}

func probeRoot(ctx context.Context, client *http.Client, root string) *DiscoveredServer {
	u := strings.TrimRight(root, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil
	}
	var models []string
	for _, d := range body.Data {
		id := strings.TrimSpace(d.ID)
		if id != "" {
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		return nil
	}
	return &DiscoveredServer{
		BaseURL: root,
		Models:  models,
	}
}

// DiscoverLocalServers probes the specified roots with the given timeout (1.5s per spec).
func DiscoverLocalServers(roots []string, timeout time.Duration) []DiscoveredServer {
	client := &http.Client{}
	var discovered []DiscoveredServer
	for _, root := range roots {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		s := probeRoot(ctx, client, root)
		cancel()
		if s != nil {
			discovered = append(discovered, *s)
		}
	}
	return discovered
}

// fetchModels issues GET baseURL/models (with optional Bearer key) to discover model IDs.
func fetchModels(ctx context.Context, baseURL, key string) ([]string, error) {
	u := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from /models", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	var models []string
	for _, d := range body.Data {
		id := strings.TrimSpace(d.ID)
		if id != "" {
			models = append(models, id)
		}
	}
	return models, nil
}

// SetupOptions bundles dependencies and configuration for RunSetup.
type SetupOptions struct {
	ConfigPath       string
	In               io.Reader
	Out              io.Writer
	Err              io.Writer
	IsTTY            bool
	DiscoveryRoots   []string
	DiscoveryTimeout time.Duration
	FetchTimeout     time.Duration
	Getenv           func(string) string
	BaseOverrides    map[string]string
}

// RunSetup runs the hearthd setup wizard according to the specification.
func RunSetup(opts SetupOptions) error {
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.Err
	if errOut == nil {
		errOut = os.Stderr
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	roots := opts.DiscoveryRoots
	if roots == nil {
		roots = getDiscoveryRoots()
	}
	discoveryTimeout := opts.DiscoveryTimeout
	if discoveryTimeout == 0 {
		discoveryTimeout = 1500 * time.Millisecond
	}
	fetchTimeout := opts.FetchTimeout
	if fetchTimeout == 0 {
		fetchTimeout = 10 * time.Second
	}

	cfgPath := opts.ConfigPath
	if cfgPath == "" {
		cfgPath = "lararium.yaml"
		if e := getenv("LARARIUM_CONFIG"); e != "" {
			cfgPath = e
		} else if _, err := os.Stat("lararium.yaml"); os.IsNotExist(err) {
			cfgPath = "/etc/lararium/lararium.yaml"
		}
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("cannot load existing config %q: %w", cfgPath, err)
	}

	// Non-TTY stdin: print current config state + env-var instructions, exit 0, touch NO files.
	if !opts.IsTTY {
		printNonTTYState(out, cfgPath, cfg)
		return nil
	}

	bufReader := bufio.NewReader(in)
	readLine := func(prompt string) (string, error) {
		if prompt != "" {
			fmt.Fprint(out, prompt)
		}
		line, err := bufReader.ReadString('\n')
		if err != nil && len(line) == 0 {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	readSecret := func(prompt string) (string, error) {
		if prompt != "" {
			fmt.Fprint(out, prompt)
		}
		if f, ok := in.(*os.File); ok && opts.IsTTY && term.IsTerminal(int(f.Fd())) {
			b, err := term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(out)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(b)), nil
		}
		line, err := bufReader.ReadString('\n')
		if err != nil && len(line) == 0 {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	// MENU loop
	selectedItem, exitOnly, err := selectMenuProvider(out, readLine)
	if err != nil || exitOnly {
		return err
	}

	// Option 1: Local models
	if selectedItem.Kind == KindLocal {
		return runLocalModels(cfgPath, roots, discoveryTimeout, fetchTimeout, out, errOut, readLine)
	}

	providerName, baseURL, apiKeyEnv, isCustom, err := resolveProviderDetails(selectedItem, readLine, errOut, opts.BaseOverrides)
	if err != nil {
		return err
	}

	// KEY PROMPT
	key, storedInKeystore, envShadowed, err := promptProviderKey(selectedItem, isCustom, baseURL, providerName, apiKeyEnv, cfg.Hearth.Home, readSecret, errOut, getenv)
	if err != nil {
		return err
	}

	// MODEL PICK
	defaultModel, compactModel, err := promptModels(nil, baseURL, key, fetchTimeout, readLine, out, errOut)
	if err != nil {
		return err
	}

	// WRITE
	if key == "" && (selectedItem.AllowEmptyKey || (isCustom && routerIsLoopback(baseURL))) {
		apiKeyEnv = ""
	}
	if err := updateYAML(cfgPath, providerName, baseURL, apiKeyEnv, defaultModel, compactModel); err != nil {
		return err
	}

	// DONE LINE
	printDoneSummary(out, providerName, baseURL, apiKeyEnv, defaultModel, storedInKeystore, envShadowed)
	touchSetupDone()
	return nil
}

func runLocalModels(cfgPath string, roots []string, discoveryTimeout, fetchTimeout time.Duration, out, errOut io.Writer, readLine func(string) (string, error)) error {
	fmt.Fprintln(out, "Scanning this machine…")
	discovered := DiscoverLocalServers(roots, discoveryTimeout)

	var baseURL string
	var models []string

	if len(discovered) > 0 {
		fmt.Fprintln(out, "\nDiscovered local servers:")
		for i, d := range discovered {
			fmt.Fprintf(out, "  %d) %s (%d models)\n", i+1, d.BaseURL, len(d.Models))
		}
		var chosen DiscoveredServer
		for {
			cStr, err := readLine("Choice [1]: ")
			if err != nil {
				return err
			}
			idx := 1
			if cStr != "" {
				var parseErr error
				idx, parseErr = strconv.Atoi(cStr)
				if parseErr != nil || idx < 1 || idx > len(discovered) {
					fmt.Fprintf(out, "Invalid choice %q, please select 1..%d\n", cStr, len(discovered))
					continue
				}
			}
			chosen = discovered[idx-1]
			break
		}
		baseURL = chosen.BaseURL
		models = chosen.Models
	} else {
		fmt.Fprintln(out, "no local servers found")
		for {
			rawURL, rErr := readLine("Base URL (e.g. http://localhost:8080/v1): ")
			if rErr != nil {
				return rErr
			}
			u, parseErr := url.Parse(rawURL)
			if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				fmt.Fprintln(errOut, "invalid base URL: must have http or https scheme and host")
				continue
			}
			baseURL = strings.TrimRight(rawURL, "/")
			break
		}
	}

	providerName := "local"
	apiKeyEnv := ""

	defaultModel, compactModel, err := promptModels(models, baseURL, "", fetchTimeout, readLine, out, errOut)
	if err != nil {
		return err
	}

	if err := updateYAML(cfgPath, providerName, baseURL, apiKeyEnv, defaultModel, compactModel); err != nil {
		return err
	}

	printDoneSummary(out, providerName, baseURL, apiKeyEnv, defaultModel, false, false)
	touchSetupDone()
	return nil
}

func printNonTTYState(out io.Writer, cfgPath string, cfg *Config) {
	fmt.Fprintln(out, "Current configuration state:")
	fmt.Fprintf(out, "  Config file: %s\n", cfgPath)
	fmt.Fprintf(out, "  Hearth home: %s\n", cfg.Hearth.Home)
	fmt.Fprintln(out, "  Providers:")
	for _, p := range cfg.Providers {
		fmt.Fprintf(out, "    - name: %s, base_url: %s, api_key_env: %s\n", p.Name, p.BaseURL, p.APIKeyEnv)
	}
	fmt.Fprintf(out, "  Default chain: %v\n", cfg.Models.Default)
	fmt.Fprintf(out, "  Compact chain: %v\n", cfg.Models.Compact)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "To configure via environment variables, export your provider key (e.g. OPENROUTER_API_KEY) and set providers/models in lararium.yaml.")
	fmt.Fprintln(out, "To run the interactive setup wizard, run hearthd in a terminal with a TTY: hearthd setup")
}

func selectMenuProvider(out io.Writer, readLine func(string) (string, error), discovered ...[]DiscoveredServer) (MenuItem, bool, error) {
	menuItems := CanonicalMenuItems(discovered...)
	for {
		fmt.Fprintln(out, "\nSelect a provider:")
		RenderMenu(out, menuItems)

		choiceStr, err := readLine("Choice [1]: ")
		if err != nil {
			return MenuItem{}, false, err
		}
		choice := 1
		if choiceStr != "" {
			var parseErr error
			choice, parseErr = strconv.Atoi(choiceStr)
			if parseErr != nil || choice < 1 || choice > len(menuItems) {
				fmt.Fprintf(out, "Invalid choice %q, please select 1..%d\n", choiceStr, len(menuItems))
				continue
			}
		}
		item := menuItems[choice-1]

		if item.Kind == KindPlaceholder {
			fmt.Fprintln(out, "not yet available — pick OpenRouter, Custom endpoint, or see docs")
			continue
		}
		if item.Kind == KindKeepCurrent {
			fmt.Fprintln(out, "Keeping current config. Exiting.")
			return item, true, nil
		}
		return item, false, nil
	}
}

func resolveProviderDetails(item MenuItem, readLine func(string) (string, error), errOut io.Writer, baseOverrides ...map[string]string) (name, baseURL, apiKeyEnv string, isCustom bool, err error) {
	var overrides map[string]string
	if len(baseOverrides) > 0 {
		overrides = baseOverrides[0]
	}

	switch item.Kind {
	case KindLocal:
		return "local", item.BaseURL, "", false, nil
	case KindActive:
		baseURL = item.BaseURL
		if overrides != nil {
			if o, ok := overrides[item.Name]; ok && o != "" {
				baseURL = o
			} else if o, ok := overrides[item.Title]; ok && o != "" {
				baseURL = o
			}
		}
		return item.Name, baseURL, item.APIKeyEnv, false, nil
	case KindAzureFoundry:
		baseURL = item.BaseURL
		if overrides != nil {
			if o, ok := overrides[item.Name]; ok && o != "" {
				baseURL = o
			}
		}
		if baseURL == "" {
			for {
				rawURL, rErr := readLine("Base URL (e.g. http://localhost:8080/v1): ")
				if rErr != nil {
					return "", "", "", false, rErr
				}
				u, parseErr := url.Parse(rawURL)
				if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					fmt.Fprintln(errOut, "invalid base URL: must have http or https scheme and host")
					continue
				}
				baseURL = strings.TrimRight(rawURL, "/")
				break
			}
		}
		return item.Name, baseURL, item.APIKeyEnv, false, nil
	case KindCustom:
		for {
			n, rErr := readLine("Provider name: ")
			if rErr != nil {
				return "", "", "", false, rErr
			}
			if !keystore.ValidName(n) {
				fmt.Fprintln(errOut, "invalid provider name")
				continue
			}
			name = n
			break
		}
		for {
			rawURL, rErr := readLine("Base URL (e.g. http://localhost:8080/v1): ")
			if rErr != nil {
				return "", "", "", false, rErr
			}
			u, parseErr := url.Parse(rawURL)
			if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				fmt.Fprintln(errOut, "invalid base URL: must have http or https scheme and host")
				continue
			}
			baseURL = strings.TrimRight(rawURL, "/")
			break
		}
		apiKeyEnv = strings.ToUpper(name) + "_API_KEY"
		return name, baseURL, apiKeyEnv, true, nil
	case KindPlaceholder, KindKeepCurrent:
		return "", "", "", false, fmt.Errorf("unexpected menu choice: %v", item.Kind)
	default:
		return "", "", "", false, fmt.Errorf("unknown menu choice: %v", item.Kind)
	}
}

func promptProviderKey(item MenuItem, isCustom bool, baseURL, providerName, apiKeyEnv, hearthHome string, readSecret func(string) (string, error), errOut io.Writer, getenv func(string) string) (key string, storedInKeystore, envShadowed bool, err error) {
	if item.Kind == KindLocal {
		return "", false, false, nil
	}

	allowEmptyKey := item.AllowEmptyKey || (isCustom && routerIsLoopback(baseURL))
	for {
		k, rErr := readSecret("API key: ")
		if rErr != nil {
			return "", false, false, rErr
		}
		if k == "" && !allowEmptyKey {
			fmt.Fprintln(errOut, "empty key")
			continue
		}
		key = k
		break
	}

	if key != "" {
		envVal := strings.TrimSpace(getenv(apiKeyEnv))
		if envVal != "" {
			fmt.Fprintf(errOut, "provider %q: keys.json entry shadowed by api_key_env — the web/CLI editor updates the shadowed copy\n", providerName)
			envShadowed = true
			key = envVal // effective key per K2 precedence
		} else {
			store := keystore.New(hearthHome)
			if sErr := store.Set(providerName, key); sErr != nil {
				return "", false, false, fmt.Errorf("storing key: %w", sErr)
			}
			storedInKeystore = true
		}
	}
	return key, storedInKeystore, envShadowed, nil
}

func promptModels(cachedModels []string, baseURL, key string, fetchTimeout time.Duration, readLine func(string) (string, error), out, errOut io.Writer) (defaultModel, compactModel string, err error) {
	var models []string
	if len(cachedModels) > 0 {
		models = cachedModels
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		found, fErr := fetchModels(ctx, baseURL, key)
		cancel()
		if fErr == nil && len(found) > 0 {
			models = found
		}
	}

	if len(models) > 0 {
		fmt.Fprintln(out, "\nAvailable models:")
		for i, m := range models {
			fmt.Fprintf(out, "  %d) %s\n", i+1, m)
		}
		mChoice, rErr := readLine("Select model [1]: ")
		if rErr != nil {
			return "", "", rErr
		}
		if mChoice == "" {
			defaultModel = models[0]
		} else if idx, aErr := strconv.Atoi(mChoice); aErr == nil && idx >= 1 && idx <= len(models) {
			defaultModel = models[idx-1]
		} else {
			defaultModel = mChoice
		}
	} else {
		for {
			m, rErr := readLine("Enter model ID: ")
			if rErr != nil {
				return "", "", rErr
			}
			if m != "" {
				defaultModel = m
				break
			}
			fmt.Fprintln(errOut, "model ID cannot be empty")
		}
	}

	compactModel = defaultModel
	compactAnswer, cErr := readLine("Same model for compact chain? [Y/n]: ")
	if cErr != nil {
		return "", "", cErr
	}
	compactAnswer = strings.ToLower(strings.TrimSpace(compactAnswer))
	if compactAnswer == "n" || compactAnswer == "no" {
		if len(models) > 0 {
			mChoice, rErr := readLine("Select compact model [1]: ")
			if rErr != nil {
				return "", "", rErr
			}
			if mChoice == "" {
				compactModel = models[0]
			} else if idx, aErr := strconv.Atoi(mChoice); aErr == nil && idx >= 1 && idx <= len(models) {
				compactModel = models[idx-1]
			} else {
				compactModel = mChoice
			}
		} else {
			for {
				m, rErr := readLine("Enter compact model ID: ")
				if rErr != nil {
					return "", "", rErr
				}
				if m != "" {
					compactModel = m
					break
				}
				fmt.Fprintln(errOut, "model ID cannot be empty")
			}
		}
	}
	return defaultModel, compactModel, nil
}

func printDoneSummary(out io.Writer, providerName, baseURL, apiKeyEnv, defaultModel string, storedInKeystore, envShadowed bool) {
	fmt.Fprintf(out, "\nConfigured provider %q (%s) with model %q\n", providerName, baseURL, defaultModel)
	switch {
	case storedInKeystore:
		fmt.Fprintf(out, "stored in keys.json (0600) under name %s\n", providerName)
	case envShadowed:
		fmt.Fprintf(out, "key provided by environment variable %s\n", apiKeyEnv)
	default:
		fmt.Fprintln(out, "no key required for local provider")
	}
	fmt.Fprintln(out, "daemon not restarted — restart hearthd to apply")
}

func touchSetupDone() {
	if dir := "/etc/lararium"; isDirWritable(dir) {
		f, err := os.OpenFile(filepath.Join(dir, ".setup-done"), os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
		}
	}
}

func isDirWritable(dir string) bool {
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

func routerIsLoopback(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "host.docker.internal" {
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// updateYAML updates lararium.yaml in place with node-level comment preservation,
// backed up to <path>.bak once, and validated via LoadConfig.
func updateYAML(path, providerName, baseURL, apiKeyEnv, defaultModel, compactModel string) error {
	bakPath := path + ".bak"
	if _, err := os.Stat(bakPath); os.IsNotExist(err) {
		oldBytes, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading config for backup: %w", err)
		}
		if err := atomicWriteFile(bakPath, oldBytes, 0o644); err != nil {
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
	var modelsNode *yaml.Node
	var providersNode *yaml.Node

	for i := 0; i < len(root.Content); i += 2 {
		k := root.Content[i].Value
		v := root.Content[i+1]
		switch k {
		case "models":
			modelsNode = v
		case "providers":
			providersNode = v
		}
	}

	// Update models default and compact chains
	if modelsNode == nil {
		modelsNode = &yaml.Node{Kind: yaml.MappingNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "models"},
			modelsNode,
		)
	}
	updateModelChain(modelsNode, "default", providerName+"/"+defaultModel)
	updateModelChain(modelsNode, "compact", providerName+"/"+compactModel)

	// Update providers list entry
	provFields := []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "name"},
		{Kind: yaml.ScalarNode, Value: providerName},
		{Kind: yaml.ScalarNode, Value: "base_url"},
		{Kind: yaml.ScalarNode, Value: baseURL},
	}
	if apiKeyEnv != "" {
		provFields = append(provFields,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "api_key_env"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: apiKeyEnv},
		)
	}
	newProvEntry := &yaml.Node{
		Kind:    yaml.MappingNode,
		Content: provFields,
	}

	if providersNode == nil {
		providersNode = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "providers"},
			providersNode,
		)
	}
	providersNode.Content = []*yaml.Node{newProvEntry}

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

	if err := atomicWriteFile(path, buf.Bytes(), fileMode); err != nil {
		return fmt.Errorf("writing updated config: %w", err)
	}

	// Validate rewritten config passes LoadConfig
	if _, err := LoadConfig(path); err != nil {
		return fmt.Errorf("rewritten config failed validation: %w", err)
	}
	return nil
}

func updateModelChain(modelsNode *yaml.Node, key, ref string) {
	for i := 0; i < len(modelsNode.Content); i += 2 {
		k := modelsNode.Content[i].Value
		v := modelsNode.Content[i+1]
		if k == key {
			v.Kind = yaml.SequenceNode
			v.Content = []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: ref},
			}
			return
		}
	}
	modelsNode.Content = append(modelsNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{
			Kind: yaml.SequenceNode,
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Value: ref},
			},
		},
	)
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func setupCmd(cfgPath string, args []string) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	cfgFlag := fs.String("config", cfgPath, "path to lararium.yaml")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: hearthd setup [--config <path>]\n\nInteractive AI provider and model setup wizard.\n")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	targetCfg := *cfgFlag
	if targetCfg == "" || targetCfg == "lararium.yaml" {
		if e := os.Getenv("LARARIUM_CONFIG"); e != "" {
			targetCfg = e
		} else if _, err := os.Stat("lararium.yaml"); os.IsNotExist(err) {
			targetCfg = "/etc/lararium/lararium.yaml"
		}
	}

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	opts := SetupOptions{
		ConfigPath: targetCfg,
		In:         os.Stdin,
		Out:        os.Stdout,
		Err:        os.Stderr,
		IsTTY:      isTTY,
	}
	if err := RunSetup(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
