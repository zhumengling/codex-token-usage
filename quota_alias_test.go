package main

import "testing"

func TestAccountQuotaAliasSetsExcludeSharedWorkspaceIdentity(t *testing.T) {
	accounts := []accountRow{
		{
			AuthIndex:        "shared-workspace-id",
			AuthID:           "shared-workspace-id",
			Source:           "a@example.com",
			Email:            "a@example.com",
			AuthFile:         "codex-a@example.com-k12.json",
			ChatGPTAccountID: "shared-workspace-id",
		},
		{
			AuthIndex:        "shared-workspace-id",
			AuthID:           "shared-workspace-id",
			Source:           "b@example.com",
			Email:            "b@example.com",
			AuthFile:         "codex-b@example.com-k12.json",
			ChatGPTAccountID: "shared-workspace-id",
		},
	}
	sets := accountQuotaAliasSets(accounts)
	if len(sets) != 2 {
		t.Fatalf("sets=%+v", sets)
	}
	for index, aliases := range sets {
		if containsAlias(aliases, "shared-workspace-id") {
			t.Fatalf("account %d retained shared workspace alias: %+v", index, aliases)
		}
	}
	if !containsAlias(sets[0], "codex-a@example.com-k12.json") || containsAlias(sets[0], "a@example.com") {
		t.Fatalf("account A lost unique aliases: %+v", sets[0])
	}
	if !containsAlias(sets[1], "codex-b@example.com-k12.json") || containsAlias(sets[1], "b@example.com") {
		t.Fatalf("account B lost unique aliases: %+v", sets[1])
	}
	index := accountQuotaAliasIndex(accounts)
	if len(index["shared-workspace-id"]) != 0 {
		t.Fatalf("shared workspace alias still maps to accounts: %+v", index["shared-workspace-id"])
	}
}

func TestAccountQuotaAliasSetsKeepUniqueAccountIdentity(t *testing.T) {
	accounts := []accountRow{{
		AuthIndex:        "codex-user@example.com-plus.json",
		AuthID:           "unique-account-id",
		Email:            "user@example.com",
		AuthFile:         "codex-user@example.com-plus.json",
		ChatGPTAccountID: "unique-account-id",
	}}
	sets := accountQuotaAliasSets(accounts)
	if len(sets) != 1 || !containsAlias(sets[0], "unique-account-id") {
		t.Fatalf("unique account identity should be retained: %+v", sets)
	}
}

func containsAlias(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
