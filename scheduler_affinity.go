package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

const (
	schedulerAffinityTTL         = time.Hour
	schedulerAffinityMaxBindings = 10_000
)

type schedulerAffinityBinding struct {
	AuthID    string
	ExpiresAt time.Time
}

type schedulerAffinityManager struct {
	mu       sync.Mutex
	bindings map[string]schedulerAffinityBinding
}

var globalSchedulerAffinity schedulerAffinityManager

func schedulerAffinityKey(req schedulerPickRequest, provider string) string {
	sessionID := schedulerSessionID(req)
	if sessionID == "" {
		return ""
	}
	provider = strings.ToLower(strings.TrimSpace(firstNonEmptyString(provider, req.Provider)))
	model := strings.ToLower(strings.TrimSpace(req.Model))
	digest := sha256.Sum256([]byte(sessionID))
	return provider + "\x00" + model + "\x00" + hex.EncodeToString(digest[:16])
}

func schedulerSessionID(req schedulerPickRequest) string {
	for _, item := range []struct {
		Prefix string
		Value  string
	}{
		{Prefix: "header:", Value: headerValue(req.Options.Headers, "X-Session-ID")},
		{Prefix: "codex:", Value: firstNonEmptyString(headerValue(req.Options.Headers, "Session-Id"), headerValue(req.Options.Headers, "Session_id"))},
		{Prefix: "clientreq:", Value: headerValue(req.Options.Headers, "X-Client-Request-Id")},
	} {
		if value := strings.TrimSpace(item.Value); value != "" {
			return item.Prefix + value
		}
	}
	return ""
}

func pickSchedulerCandidate(rotationKey, affinityKey string, candidates []schedulerAuthCandidate) schedulerAuthCandidate {
	if chosen, ok := globalSchedulerAffinity.pick(affinityKey, candidates); ok {
		return chosen
	}
	chosen := globalSchedulerRotation.pick(rotationKey, candidates)
	globalSchedulerAffinity.bind(affinityKey, chosen.ID)
	return chosen
}

func (m *schedulerAffinityManager) pick(key string, candidates []schedulerAuthCandidate) (schedulerAuthCandidate, bool) {
	key = strings.TrimSpace(key)
	if key == "" || len(candidates) == 0 {
		return schedulerAuthCandidate{}, false
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	binding, ok := m.bindings[key]
	if !ok {
		return schedulerAuthCandidate{}, false
	}
	if !binding.ExpiresAt.After(now) {
		delete(m.bindings, key)
		return schedulerAuthCandidate{}, false
	}
	for _, candidate := range candidates {
		if candidate.ID == binding.AuthID {
			binding.ExpiresAt = now.Add(schedulerAffinityTTL)
			m.bindings[key] = binding
			return candidate, true
		}
	}
	delete(m.bindings, key)
	return schedulerAuthCandidate{}, false
}

func (m *schedulerAffinityManager) bind(key, authID string) {
	key = strings.TrimSpace(key)
	authID = strings.TrimSpace(authID)
	if key == "" || authID == "" {
		return
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.bindings == nil {
		m.bindings = make(map[string]schedulerAffinityBinding)
	}
	if _, exists := m.bindings[key]; !exists && len(m.bindings) >= schedulerAffinityMaxBindings {
		m.evictOneLocked(now)
	}
	m.bindings[key] = schedulerAffinityBinding{AuthID: authID, ExpiresAt: now.Add(schedulerAffinityTTL)}
}

func (m *schedulerAffinityManager) evictOneLocked(now time.Time) {
	oldestKey := ""
	var oldestExpiry time.Time
	for key, binding := range m.bindings {
		if !binding.ExpiresAt.After(now) {
			delete(m.bindings, key)
			return
		}
		if oldestKey == "" || binding.ExpiresAt.Before(oldestExpiry) {
			oldestKey = key
			oldestExpiry = binding.ExpiresAt
		}
	}
	if oldestKey != "" {
		delete(m.bindings, oldestKey)
	}
}

func (m *schedulerAffinityManager) reset() {
	m.mu.Lock()
	m.bindings = nil
	m.mu.Unlock()
}
