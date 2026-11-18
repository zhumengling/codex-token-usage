package main

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const unmatchedCodexProvider = "Unidentified Codex endpoint"

type providerConfigEntry struct {
	Name      string
	Provider  string
	APIKey    string
	AuthID    string
	AuthIndex string
}

type providerConfigItem struct {
	Name           string                  `yaml:"name"`
	Prefix         string                  `yaml:"prefix"`
	BaseURL        string                  `yaml:"base-url"`
	APIKey         string                  `yaml:"api-key"`
	ProxyURL       string                  `yaml:"proxy-url"`
	Headers        map[string]string       `yaml:"headers"`
	ConnectionPool *providerConnectionPool `yaml:"connection-pool"`
	Disabled       bool                    `yaml:"disabled"`
}

type providerConnectionPool struct {
	Enabled             *bool   `yaml:"enabled"`
	IdleConnTimeout     *string `yaml:"idle-conn-timeout"`
	MaxIdleConnsPerHost *int    `yaml:"max-idle-conns-per-host"`
}

type providerConfigInventory struct {
	entries  []providerConfigEntry
	status   string
	revision string
	authDir  string
}

var providerInventoryCache struct {
	sync.Mutex
	path    string
	info    os.FileInfo
	current providerConfigInventory
}

func readConfiguredProviderEntries() []providerConfigEntry {
	return readProviderInventory().entries
}

func readConfiguredProviderNames() []string {
	entries := readConfiguredProviderEntries()
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

func readProviderInventory() providerConfigInventory {
	path := configuredConfigPath()
	info, err := os.Stat(path)
	if path == "" || os.IsNotExist(err) {
		return providerConfigInventory{status: "missing", revision: "missing"}
	}
	if err != nil || info.IsDir() {
		return providerConfigInventory{status: "unreadable", revision: "unreadable"}
	}
	providerInventoryCache.Lock()
	defer providerInventoryCache.Unlock()
	if previous := providerInventoryCache.info; previous != nil && providerInventoryCache.path == path &&
		os.SameFile(info, previous) && info.ModTime().Equal(previous.ModTime()) && info.Size() == previous.Size() {
		return providerInventoryCache.current
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return providerConfigInventory{status: "unreadable", revision: "unreadable"}
	}
	result := parseProviderInventory(raw)
	revision := sha256.Sum256(raw)
	result.revision = fmt.Sprintf("%x", revision[:8])
	providerInventoryCache.path, providerInventoryCache.info, providerInventoryCache.current = path, info, result
	return result
}

func parseProviderInventory(raw []byte) providerConfigInventory {
	var config struct {
		AuthDir       string `yaml:"auth-dir"`
		LegacyAuthDir string `yaml:"auth_dir"`
		OAuth         struct {
			AuthDir *string `yaml:"auth-dir"`
		} `yaml:"oauth"`
		Fields map[string]yaml.Node `yaml:",inline"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return providerConfigInventory{status: "invalid"}
	}
	result := providerConfigInventory{status: "ok", authDir: firstNonEmptyString(config.AuthDir, config.LegacyAuthDir)}
	if config.OAuth.AuthDir != nil {
		result.authDir = strings.TrimSpace(*config.OAuth.AuthDir)
	}
	blocks := config.Fields
	var v8 map[string]yaml.Node
	if keys := blocks["api-keys"]; keys.Kind == yaml.MappingNode || keys.Kind == yaml.AliasNode {
		if err := keys.Decode(&v8); err != nil {
			result.status = "partial"
		}
	}
	groups := []struct{ legacy, current, provider string }{
		{"openai-compatibility", "openai-compatibility", "OpenAI"}, {"openai-compatible", "", "OpenAI"},
		{"claude-api-key", "claude", "Claude"}, {"anthropic-api-key", "", "Claude"},
		{"gemini-api-key", "gemini", "Gemini"}, {"antigravity-api-key", "", "Antigravity"},
		{"antigravity-oauth", "", "Antigravity"}, {"anthropic-oauth", "", "Claude"},
		{"vertex-api-key", "vertex", "Vertex"}, {"interactions-api-key", "interactions", "Gemini Interactions"},
		{"meta-api-key", "meta", "Meta"}, {"codex-api-key", "codex", "Codex"},
	}
	seen := map[string]bool{normalizeAccountAlias(unmatchedCodexProvider): true}
	authIDs := map[string]int{}
	for _, group := range groups {
		var items []providerConfigItem
		var err error
		if block, ok := v8[group.current]; group.current != "" && ok {
			items, err = expandProviderGroups(block, group.provider == "OpenAI")
		} else if block, ok := blocks[group.legacy]; ok {
			err = block.Decode(&items)
		}
		if err != nil {
			result.status = "partial"
			continue
		}
		for _, item := range items {
			item.BaseURL = strings.TrimSpace(item.BaseURL)
			item.APIKey = strings.TrimSpace(item.APIKey)
			if item.Disabled || (group.provider == "Codex" && item.BaseURL == "") {
				continue
			}
			entry := providerConfigEntry{Provider: group.provider, APIKey: item.APIKey}
			name := providerConfigName(group.provider, item)
			if group.provider == "Codex" {
				entry.AuthID, entry.AuthIndex = codexConfigCredential(item, authIDs)
				for number := 2; seen[normalizeAccountAlias(name)]; number++ {
					name = fmt.Sprintf("%s #%d", providerConfigName(group.provider, item), number)
				}
			}
			entry.Name = name
			key := normalizeAccountAlias(name)
			if !seen[key] {
				seen[key] = true
				result.entries = append(result.entries, entry)
			}
		}
	}
	return result
}

func expandProviderGroups(block yaml.Node, openAICompatible bool) ([]providerConfigItem, error) {
	var groups []struct {
		providerConfigItem `yaml:",inline"`
		Keys               []struct {
			APIKey         string                  `yaml:"api-key"`
			ProxyURL       *string                 `yaml:"proxy-url"`
			Prefix         *string                 `yaml:"prefix"`
			Headers        map[string]string       `yaml:"headers"`
			ConnectionPool *providerConnectionPool `yaml:"connection-pool"`
		} `yaml:"keys"`
	}
	if err := block.Decode(&groups); err != nil {
		return nil, err
	}
	var items []providerConfigItem
	for _, group := range groups {
		if openAICompatible {
			items = append(items, group.providerConfigItem)
			continue
		}
		for _, key := range group.Keys {
			item := group.providerConfigItem
			item.APIKey = key.APIKey
			if key.ProxyURL != nil {
				item.ProxyURL = *key.ProxyURL
			}
			if key.Prefix != nil {
				item.Prefix = *key.Prefix
			}
			if key.Headers != nil {
				item.Headers = key.Headers
			}
			if key.ConnectionPool != nil {
				pool := *key.ConnectionPool
				if upstream := item.ConnectionPool; upstream != nil {
					if pool.Enabled == nil {
						pool.Enabled = upstream.Enabled
					}
					if pool.IdleConnTimeout == nil {
						pool.IdleConnTimeout = upstream.IdleConnTimeout
					}
					if pool.MaxIdleConnsPerHost == nil {
						pool.MaxIdleConnsPerHost = upstream.MaxIdleConnsPerHost
					}
				}
				item.ConnectionPool = &pool
			}
			items = append(items, item)
		}
	}
	return items, nil
}

func providerConfigName(provider string, item providerConfigItem) string {
	if name := strings.TrimSpace(item.Name); name != "" && name != item.APIKey {
		if provider == "Codex" {
			return provider + " · " + safeExportLabel(name)
		}
		return safeExportLabel(name)
	}
	if parsed, err := url.Parse(item.BaseURL); err == nil && parsed.Host != "" {
		return provider + " · " + parsed.Host
	}
	return provider
}

func codexConfigCredential(item providerConfigItem, counts map[string]int) (string, string) {
	const kind = "codex:apikey"
	var headers strings.Builder
	clean := make(map[string]string, len(item.Headers))
	for key, value := range item.Headers {
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key != "" && value != "" {
			clean[key] = value
		}
	}
	names := make([]string, 0, len(clean))
	for key := range clean {
		names = append(names, key)
	}
	sort.Strings(names)
	for _, key := range names {
		headers.WriteString(key + "\x00" + clean[key] + "\x00")
	}
	prefix := strings.Trim(strings.TrimSpace(item.Prefix), "/")
	if strings.Contains(prefix, "/") {
		prefix = ""
	}
	parts := []string{item.APIKey, item.BaseURL, item.ProxyURL, prefix, headers.String()}
	pool := item.ConnectionPool.dedupID()
	if pool != "" {
		parts = append(parts, pool)
	}
	identity := sha256.New()
	_, _ = identity.Write([]byte(kind))
	for _, part := range parts {
		_, _ = identity.Write([]byte("\x00" + strings.TrimSpace(part)))
	}
	baseID := fmt.Sprintf("%s:%x", kind, identity.Sum(nil)[:6])
	count := counts[baseID]
	counts[baseID]++
	fullID := baseID
	if count > 0 {
		fullID = fmt.Sprintf("%s-%d", baseID, count)
	}
	indexSeed := "id:" + fullID
	if item.APIKey != "" {
		indexSeed = "codex-api-key:" + item.BaseURL + "+" + item.APIKey
		if pool != "" {
			indexSeed += "\x00" + pool
		}
	}
	index := sha256.Sum256([]byte(indexSeed))
	return fullID, fmt.Sprintf("%x", index[:8])
}

func (pool *providerConnectionPool) dedupID() string {
	if pool == nil {
		return ""
	}
	var enabled, timeout, maxIdle string
	if pool.Enabled != nil {
		enabled = strconv.FormatBool(*pool.Enabled)
	}
	if pool.MaxIdleConnsPerHost != nil {
		maxIdle = strconv.Itoa(*pool.MaxIdleConnsPerHost)
	}
	if pool.IdleConnTimeout != nil {
		timeout = strings.TrimSpace(*pool.IdleConnTimeout)
	}
	return "pool:" + enabled + "\x00" + timeout + "\x00" + maxIdle
}
