package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const defaultModelPriceURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

type modelPriceUpdateState struct {
	Enabled        bool   `json:"enabled"`
	URL            string `json:"url"`
	Path           string `json:"path"`
	IntervalHours  int    `json:"interval_hours"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	LastCheckedAt  string `json:"last_checked_at,omitempty"`
	LastUpdatedAt  string `json:"last_updated_at,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	FileSizeBytes  int64  `json:"file_size_bytes,omitempty"`
	Entries        int    `json:"entries,omitempty"`
	LoadedPrices   int    `json:"loaded_prices,omitempty"`
}

type modelPriceUpdateManager struct {
	mu     sync.Mutex
	cfg    pluginConfig
	cancel context.CancelFunc
	state  modelPriceUpdateState
}

func modelPriceFilePath() string {
	path := strings.TrimSpace(os.Getenv("CPA_MODEL_PRICE_FILE"))
	if path == "" {
		path = "/root/plugins/codex-token-usage/model_prices.json"
	}
	return path
}

func (m *modelPriceUpdateManager) configure(cfg pluginConfig) {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.cfg = cfg
	m.state.Enabled = cfg.ModelPriceAutoUpdateEnabled
	m.state.URL = strings.TrimSpace(cfg.ModelPriceUpdateURL)
	m.state.Path = modelPriceFilePath()
	m.state.IntervalHours = cfg.ModelPriceUpdateIntervalHours
	m.state.TimeoutSeconds = cfg.ModelPriceUpdateTimeoutSeconds
	m.mu.Unlock()
	if !cfg.ModelPriceAutoUpdateEnabled {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.cancel = cancel
	m.mu.Unlock()
	go m.loop(ctx, cfg)
}

func (m *modelPriceUpdateManager) stop() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mu.Unlock()
}

func (m *modelPriceUpdateManager) status() modelPriceUpdateState {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.state
	path := modelPriceFilePath()
	if info, err := os.Stat(path); err == nil {
		state.FileSizeBytes = info.Size()
		if state.LastUpdatedAt == "" {
			state.LastUpdatedAt = info.ModTime().Format(time.RFC3339)
		}
		if state.Entries == 0 || state.LoadedPrices == 0 {
			if raw, err := os.ReadFile(path); err == nil {
				if entries, loaded, err := validateModelPrices(raw); err == nil {
					state.Entries = entries
					state.LoadedPrices = loaded
				}
			}
		}
	}
	return state
}

func (m *modelPriceUpdateManager) ensureFresh() {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	if !cfg.ModelPriceAutoUpdateEnabled {
		return
	}
	if modelPriceFileFresh(modelPriceFilePath(), time.Duration(cfg.ModelPriceUpdateIntervalHours)*time.Hour) {
		return
	}
	go m.update(context.Background(), cfg)
}

func (m *modelPriceUpdateManager) loop(ctx context.Context, cfg pluginConfig) {
	m.update(ctx, cfg)
	ticker := time.NewTicker(time.Duration(cfg.ModelPriceUpdateIntervalHours) * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.update(ctx, cfg)
		}
	}
}

func (m *modelPriceUpdateManager) update(ctx context.Context, cfg pluginConfig) {
	if cfg.ModelPriceUpdateURL == "" {
		cfg.ModelPriceUpdateURL = defaultModelPriceURL
	}
	if modelPriceFileFresh(modelPriceFilePath(), time.Duration(cfg.ModelPriceUpdateIntervalHours)*time.Hour) {
		m.recordPriceUpdateCheck("", 0, 0, false)
		return
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.ModelPriceUpdateTimeoutSeconds)*time.Second)
	defer cancel()
	entries, loaded, size, err := downloadModelPrices(timeoutCtx, cfg.ModelPriceUpdateURL, modelPriceFilePath())
	if err != nil {
		m.recordPriceUpdateCheck(err.Error(), 0, 0, false)
		return
	}
	m.recordPriceUpdateCheck("", entries, loaded, true)
	m.mu.Lock()
	m.state.FileSizeBytes = size
	m.mu.Unlock()
}

func (m *modelPriceUpdateManager) recordPriceUpdateCheck(message string, entries, loaded int, updated bool) {
	now := time.Now().Format(time.RFC3339)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state.LastCheckedAt = now
	m.state.LastError = sanitizeTriggerError(message)
	if updated {
		m.state.LastUpdatedAt = now
		m.state.Entries = entries
		m.state.LoadedPrices = loaded
	}
}

func modelPriceFileFresh(path string, maxAge time.Duration) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 0 {
		return false
	}
	return time.Since(info.ModTime()) < maxAge
}

func downloadModelPrices(ctx context.Context, url, path string) (int, int, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSpace(url), nil)
	if err != nil {
		return 0, 0, 0, err
	}
	req.Header.Set("User-Agent", pluginID+"/"+pluginVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, 0, 0, fmt.Errorf("price update returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return 0, 0, 0, err
	}
	entries, loaded, err := validateModelPrices(raw)
	if err != nil {
		return 0, 0, 0, err
	}
	if loaded == 0 {
		return entries, loaded, 0, fmt.Errorf("price update contained no usable prices")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return entries, loaded, 0, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		return entries, loaded, 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return entries, loaded, 0, err
	}
	return entries, loaded, int64(len(raw)), nil
}

func validateModelPrices(raw []byte) (int, int, error) {
	var entries map[string]map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		return 0, 0, err
	}
	loaded := 0
	for _, entry := range entries {
		if _, ok := modelPriceFromJSON(entry); ok {
			loaded++
		}
	}
	return len(entries), loaded, nil
}
