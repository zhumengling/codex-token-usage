package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	cliProxyDataDirName = ".cli-proxy-api"
)

var (
	userHomeDir       = os.UserHomeDir
	currentWorkingDir = os.Getwd
)

func currentUserHomeDir() (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve current user home directory: %w", err)
	}
	home = strings.TrimSpace(home)
	if home == "" {
		return "", fmt.Errorf("resolve current user home directory: empty path")
	}
	return filepath.Clean(home), nil
}

func cliProxyDataDir() (string, error) {
	home, err := currentUserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, cliProxyDataDirName), nil
}

func pluginDataDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CPA_TOKEN_USAGE_DIR")); dir != "" {
		return filepath.Clean(dir), nil
	}
	base, err := cliProxyDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "plugins", pluginID), nil
}

func usageDBPath() (string, error) {
	dir, err := pluginDataDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create plugin data directory %q: %w", dir, err)
	}
	return filepath.Join(dir, "usage.db"), nil
}

func modelPriceFilePath() string {
	if path := strings.TrimSpace(os.Getenv("CPA_MODEL_PRICE_FILE")); path != "" {
		return filepath.Clean(path)
	}
	dir, err := pluginDataDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "model_prices.json")
}

func configuredConfigPath() string {
	return configuredConfigPathForArgs(os.Args[1:])
}

func configuredConfigPathForArgs(args []string) string {
	if path := strings.TrimSpace(os.Getenv("CPA_CONFIG_PATH")); path != "" {
		return filepath.Clean(path)
	}
	if path := strings.TrimSpace(os.Getenv("CPA_CONFIG_FILE")); path != "" {
		return filepath.Clean(path)
	}
	if path := commandLineConfigPath(args); path != "" {
		return path
	}
	if workingDir, err := currentWorkingDir(); err == nil {
		workingPath := filepath.Join(workingDir, "config.yaml")
		if existingConfigFile(workingPath) {
			return workingPath
		}
	}
	dataDir, err := cliProxyDataDir()
	if err != nil {
		return ""
	}
	canonicalPath := filepath.Join(dataDir, "config.yaml")
	if existingConfigFile(canonicalPath) {
		return canonicalPath
	}

	home, err := currentUserHomeDir()
	if err != nil {
		return ""
	}
	legacyHomePath := filepath.Join(home, "config.yaml")
	if existingConfigFile(legacyHomePath) {
		return legacyHomePath
	}
	return canonicalPath
}

func commandLineConfigPath(args []string) string {
	for i, arg := range args {
		arg = strings.TrimSpace(arg)
		if arg == "--" {
			break
		}
		switch {
		case arg == "-config" || arg == "--config":
			if i+1 < len(args) {
				if path := strings.TrimSpace(args[i+1]); path != "" {
					return filepath.Clean(path)
				}
			}
		case strings.HasPrefix(arg, "-config="):
			if path := strings.TrimSpace(strings.TrimPrefix(arg, "-config=")); path != "" {
				return filepath.Clean(path)
			}
		case strings.HasPrefix(arg, "--config="):
			if path := strings.TrimSpace(strings.TrimPrefix(arg, "--config=")); path != "" {
				return filepath.Clean(path)
			}
		}
	}
	return ""
}

func existingConfigFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
