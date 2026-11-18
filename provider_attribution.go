package main

import "strings"

const (
	codexUsageSQL     = `(lower(trim(provider)) = 'codex' OR lower(executor_type) LIKE '%codex%')`
	apiKeyAuthTypeSQL = `lower(trim(auth_type)) IN ('apikey', 'api_key', 'api-key', 'key')`
	codexAuthIDSQL    = `(lower(trim(auth_id)) LIKE 'codex:apikey:%' OR lower(trim(auth_id)) LIKE 'codex-api-key:%')`
	legacyCodexSQL    = `(lower(trim(auth_type)) NOT IN ('oauth', 'codex') AND lower(trim(auth_id)) NOT LIKE '%.json' AND lower(trim(source)) NOT LIKE '%.json')`
)

func codexAPIKeyUsageSQL(entries []providerConfigEntry) string {
	legacy := []string{`lower(trim(source)) LIKE 'sk-%'`, `lower(trim(source)) LIKE 'bearer sk-%'`}
	for _, entry := range entries {
		if entry.Provider != "Codex" {
			continue
		}
		if entry.AuthIndex != "" {
			legacy = append(legacy, "auth_index = "+sqlQuote(entry.AuthIndex))
		}
		if entry.APIKey != "" {
			legacy = append(legacy, usageKeyMatchSQL("source", entry.APIKey))
		}
	}
	return "(" + codexUsageSQL + " AND (" + apiKeyAuthTypeSQL + " OR " + codexAuthIDSQL + " OR (" +
		legacyCodexSQL + " AND (" + strings.Join(legacy, " OR ") + "))))"
}

func usageKeyMatchSQL(column, key string) string {
	return "(trim(" + column + ") = " + sqlQuote(key) + " OR (lower(substr(trim(" + column +
		"), 1, 7)) = 'bearer ' AND trim(substr(trim(" + column + "), 8)) = " + sqlQuote(key) + "))"
}

func codexProviderNameSQL(entries []providerConfigEntry) string {
	var byID, byIndex, byKey strings.Builder
	indexes, keys := map[string]int{}, map[string]int{}
	for _, entry := range entries {
		if entry.Provider == "Codex" {
			indexes[entry.AuthIndex]++
			keys[entry.APIKey]++
		}
	}
	for _, entry := range entries {
		if entry.Provider != "Codex" || entry.Name == "" {
			continue
		}
		name := sqlQuote(entry.Name)
		if entry.AuthID != "" {
			byID.WriteString("WHEN auth_id = " + sqlQuote(entry.AuthID) + " THEN " + name + "\n")
		}
		if entry.AuthIndex != "" && indexes[entry.AuthIndex] == 1 {
			byIndex.WriteString("WHEN auth_index = " + sqlQuote(entry.AuthIndex) + " THEN " + name + "\n")
		}
		if entry.APIKey != "" && keys[entry.APIKey] == 1 {
			byKey.WriteString("WHEN trim(auth_index) = '' AND " + usageKeyMatchSQL("source", entry.APIKey) + " THEN " + name + "\n")
		}
	}
	return "CASE\n" + byID.String() + "WHEN " + codexAuthIDSQL + " THEN " + sqlQuote(unmatchedCodexProvider) + "\n" +
		byIndex.String() + byKey.String() + "ELSE " + sqlQuote(unmatchedCodexProvider) + " END"
}

func privateUsageIdentitySQL() string {
	return "(" + apiKeyAuthTypeSQL + " OR " + codexAPIKeyUsageSQL(readConfiguredProviderEntries()) + ")"
}

func publicUsageIdentitySQL(column string) string {
	private := "(" + column + " = api_key OR (" + privateUsageIdentitySQL() + " AND " + column + " = source))"
	return "CASE WHEN " + private + " THEN '' ELSE " + column + " END AS " + column
}
