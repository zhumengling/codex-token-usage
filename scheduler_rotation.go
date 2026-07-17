package main

import (
	"os"
	"sort"
	"strings"
	"sync"
)

type cpaSchedulerStrategy string

const (
	cpaSchedulerRoundRobin cpaSchedulerStrategy = "round-robin"
	cpaSchedulerFillFirst  cpaSchedulerStrategy = "fill-first"
)

// schedulerRotationManager mirrors CPA's built-in round-robin behavior for
// requests the plugin must handle itself after applying bans or protection.
// CPA builds scheduler candidates from a map, so their incoming slice order is
// not a usable rotation order.
type schedulerRotationManager struct {
	mu      sync.Mutex
	cursors map[string]uint64
}

var globalSchedulerRotation schedulerRotationManager

type cpaSchedulerStrategyCache struct {
	mu       sync.Mutex
	path     string
	modTime  int64
	size     int64
	strategy cpaSchedulerStrategy
}

var globalCPASchedulerStrategy cpaSchedulerStrategyCache

func currentCPASchedulerStrategy() cpaSchedulerStrategy {
	path := configuredConfigPath()
	if strings.TrimSpace(path) == "" {
		return cpaSchedulerRoundRobin
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return cpaSchedulerRoundRobin
	}
	modTime := info.ModTime().UnixNano()
	size := info.Size()
	globalCPASchedulerStrategy.mu.Lock()
	if globalCPASchedulerStrategy.path == path && globalCPASchedulerStrategy.modTime == modTime && globalCPASchedulerStrategy.size == size {
		strategy := globalCPASchedulerStrategy.strategy
		globalCPASchedulerStrategy.mu.Unlock()
		return normalizeCPASchedulerStrategy(string(strategy))
	}
	previous := globalCPASchedulerStrategy.strategy
	globalCPASchedulerStrategy.mu.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return cpaSchedulerRoundRobin
	}
	strategy := parseCPASchedulerStrategy(string(raw))
	globalCPASchedulerStrategy.mu.Lock()
	globalCPASchedulerStrategy.path = path
	globalCPASchedulerStrategy.modTime = modTime
	globalCPASchedulerStrategy.size = size
	globalCPASchedulerStrategy.strategy = strategy
	globalCPASchedulerStrategy.mu.Unlock()
	if previous != "" && previous != strategy {
		globalSchedulerRotation.reset()
	}
	return strategy
}

func parseCPASchedulerStrategy(raw string) cpaSchedulerStrategy {
	routingIndent := -1
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		value = strings.Trim(value, `"'`)
		if routingIndent < 0 {
			if key == "routing" {
				routingIndent = indent
				if value != "" {
					return normalizeCPASchedulerStrategy(value)
				}
				continue
			}
			if key == "routing.strategy" || key == "routing_strategy" {
				return normalizeCPASchedulerStrategy(value)
			}
			continue
		}
		if indent <= routingIndent {
			break
		}
		if key == "strategy" {
			return normalizeCPASchedulerStrategy(value)
		}
	}
	return cpaSchedulerRoundRobin
}

func normalizeCPASchedulerStrategy(value string) cpaSchedulerStrategy {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "fill-first", "fill_first", "fillfirst", "fill":
		return cpaSchedulerFillFirst
	case "round-robin", "round_robin", "roundrobin", "round":
		return cpaSchedulerRoundRobin
	default:
		return cpaSchedulerRoundRobin
	}
}

func resetCPASchedulerStrategyCache() {
	globalCPASchedulerStrategy.mu.Lock()
	globalCPASchedulerStrategy.path = ""
	globalCPASchedulerStrategy.modTime = 0
	globalCPASchedulerStrategy.size = 0
	globalCPASchedulerStrategy.strategy = ""
	globalCPASchedulerStrategy.mu.Unlock()
}

func schedulerRotationKey(req schedulerPickRequest, provider string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(req.Provider))
	}
	return provider + "\x00" + strings.ToLower(strings.TrimSpace(req.Model))
}

func (m *schedulerRotationManager) pick(key string, candidates []schedulerAuthCandidate) schedulerAuthCandidate {
	ordered := highestPrioritySchedulerCandidates(candidates)
	if len(ordered) == 0 {
		return schedulerAuthCandidate{}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cursors == nil {
		m.cursors = make(map[string]uint64)
	}
	cursor := m.cursors[key]
	chosen := ordered[cursor%uint64(len(ordered))]
	m.cursors[key] = cursor + 1
	return chosen
}

func pickSchedulerCandidateByStrategy(rotationKey string, strategy cpaSchedulerStrategy, candidates []schedulerAuthCandidate) schedulerAuthCandidate {
	if normalizeCPASchedulerStrategy(string(strategy)) == cpaSchedulerFillFirst {
		ordered := highestPrioritySchedulerCandidates(candidates)
		if len(ordered) == 0 {
			return schedulerAuthCandidate{}
		}
		return ordered[0]
	}
	return globalSchedulerRotation.pick(rotationKey, candidates)
}

func (m *schedulerRotationManager) reset() {
	m.mu.Lock()
	m.cursors = nil
	m.mu.Unlock()
}

func highestPrioritySchedulerCandidates(candidates []schedulerAuthCandidate) []schedulerAuthCandidate {
	if len(candidates) == 0 {
		return nil
	}
	highest := candidates[0].Priority
	for _, candidate := range candidates[1:] {
		if candidate.Priority > highest {
			highest = candidate.Priority
		}
	}
	ordered := make([]schedulerAuthCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Priority == highest {
			ordered = append(ordered, candidate)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].ID < ordered[j].ID
	})
	return ordered
}
