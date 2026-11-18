package main

import (
	"context"
	"database/sql"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type usageIngestionDiagnostics struct {
	writeFailures     atomic.Uint64
	effectFailures    atomic.Uint64
	lastWriteFailure  atomic.Int64
	lastEffectFailure atomic.Int64
}

type usageIngestionStatus struct {
	WriteFailures       uint64 `json:"write_failures"`
	StateUpdateFailures uint64 `json:"state_update_failures"`
	LastWriteFailureAt  int64  `json:"last_write_failure_at,omitempty"`
	LastStateFailureAt  int64  `json:"last_state_failure_at,omitempty"`
}

func (diagnostics *usageIngestionDiagnostics) status() usageIngestionStatus {
	return usageIngestionStatus{
		WriteFailures: diagnostics.writeFailures.Load(), StateUpdateFailures: diagnostics.effectFailures.Load(),
		LastWriteFailureAt: diagnostics.lastWriteFailure.Load(), LastStateFailureAt: diagnostics.lastEffectFailure.Load(),
	}
}

type providerBudgetConfig struct {
	Budgets map[string]float64 `yaml:"provider_budgets_usd"`
	Invalid bool               `yaml:"-"`
}

var sharedProviderBudgets atomic.Pointer[providerBudgetConfig]

func parseProviderBudgets(raw []byte) providerBudgetConfig {
	var config providerBudgetConfig
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return providerBudgetConfig{Invalid: true}
	}
	for name, amount := range config.Budgets {
		if strings.TrimSpace(name) != name || name == "" || !validProviderBudget(amount) {
			return providerBudgetConfig{Invalid: true}
		}
	}
	return config
}

func validProviderBudget(amount float64) bool {
	return amount > 0 && amount <= 1e9 && !math.IsNaN(amount)
}

type providerReportTotals struct {
	Requests       int64   `json:"requests"`
	Failed         int64   `json:"failed"`
	TotalTokens    int64   `json:"total_tokens"`
	CostUSD        float64 `json:"cost_usd"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
}

type providerReportPoint struct {
	Bucket string `json:"bucket"`
	providerReportTotals
}

type providerReportBudget struct {
	LimitUSD       float64 `json:"limit_usd"`
	CostUSD        float64 `json:"cost_usd"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
	MonthStart     int64   `json:"month_start"`
	MonthEnd       int64   `json:"month_end"`
	Month          string  `json:"month"`
	Status         string  `json:"status"`
}

type providerReport struct {
	Provider   string                `json:"provider"`
	Window     string                `json:"window"`
	Totals     providerReportTotals  `json:"totals"`
	Statuses   map[string]int64      `json:"statuses"`
	Trend      []providerReportPoint `json:"trend"`
	Models     []modelRow            `json:"models"`
	Recent     []recentRow           `json:"recent"`
	ModelPage  int                   `json:"model_page"`
	RecentPage int                   `json:"recent_page"`
	MoreModels bool                  `json:"more_models"`
	MoreRecent bool                  `json:"more_recent"`
	Budget     providerReportBudget  `json:"budget"`
}

const providerReportModelPageSize = 20
const providerReportRecentPageSize = 30

func handleProviderReport(req managementRequest) managementResponse {
	if !strings.EqualFold(req.Method, http.MethodGet) {
		return jsonResponse(http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
	}
	name := firstQuery(req.Query, "provider", "")
	if name == "" || len(name) > 1024 {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "invalid_provider"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, _, err := globalStore.open(ctx)
	if err == nil {
		budget := sharedProviderBudgets.Load()
		if budget == nil {
			budget = &providerBudgetConfig{}
		}
		report, reportErr := queryProviderReport(ctx, db, name, firstQuery(req.Query, "window", "24h"),
			parseInt(firstQuery(req.Query, "model_page", "1"), 1, 1, 1000),
			parseInt(firstQuery(req.Query, "recent_page", "1"), 1, 1, 1000), time.Now(), readConfiguredProviderEntries(), defaultModelPrices(), *budget)
		if reportErr == nil {
			response := jsonResponse(http.StatusOK, report)
			response.Headers["cache-control"] = []string{"no-store"}
			return response
		}
	}
	return jsonResponse(http.StatusServiceUnavailable, map[string]string{"error": "provider_report_unavailable"})
}

func queryProviderReport(ctx context.Context, db *sql.DB, name, window string, modelPage, recentPage int, now time.Time, entries []providerConfigEntry, prices map[string]modelPrice, budget providerBudgetConfig) (providerReport, error) {
	since, window := windowStartAt(window, now)
	expr := cpaProviderSQLWithEntries(entries)
	where := "requested_at >= ? AND requested_at < ? AND " + usageScopeSQLWithEntries("other", entries) + " AND " + expr + " = ?"
	args := []any{since, now.Unix() + 1, name}
	out := providerReport{Provider: name, Window: window, ModelPage: modelPage, RecentPage: recentPage,
		Statuses: map[string]int64{"ok": 0, "401": 0, "402": 0, "403": 0, "429": 0, "5xx": 0, "other": 0},
		Trend:    []providerReportPoint{}, Models: []modelRow{}, Recent: []recentRow{}}
	format := "%Y-%m-%d"
	if window == "24h" || window == "today" {
		format += " %H:00"
	}
	points, modelCosts, err := providerReportAggregates(ctx, db, &out, where, args, format, prices)
	if err != nil {
		return out, err
	}
	out.Trend = points
	models, err := queryModelPage(ctx, db, expr, where, args, providerReportModelPageSize+1, (modelPage-1)*providerReportModelPageSize)
	if err != nil {
		return out, err
	}
	if out.MoreModels = len(models) > providerReportModelPageSize; out.MoreModels {
		models = models[:providerReportModelPageSize]
	}
	for index := range models {
		row := &models[index]
		cost := modelCosts[[2]string{row.Model, row.Alias}]
		row.CostUSD, row.UnpricedTokens = cost.CostUSD, cost.UnpricedTokens
		row.CostAvailable = costAvailable(row.TotalTokens, row.UnpricedTokens)
	}
	out.Models = append(out.Models, models...)
	recent, err := queryRecentPage(ctx, db, expr, where, args, providerReportRecentPageSize+1, (recentPage-1)*providerReportRecentPageSize, prices)
	if err != nil {
		return out, err
	}
	if out.MoreRecent = len(recent) > providerReportRecentPageSize; out.MoreRecent {
		recent = recent[:providerReportRecentPageSize]
	}
	out.Recent = append(out.Recent, recent...)
	out.Budget, err = queryProviderBudget(ctx, db, where, name, now, prices, budget)
	return out, err
}

const providerCostColumns = `COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(cache_read_tokens),0), COALESCE(SUM(cache_creation_tokens),0), COALESCE(SUM(total_tokens),0)`

func providerReportAggregates(ctx context.Context, db *sql.DB, out *providerReport, where string, args []any, format string, prices map[string]modelPrice) ([]providerReportPoint, map[[2]string]costSummary, error) {
	rows, err := db.QueryContext(ctx, `SELECT strftime(?, requested_at, 'unixepoch', 'localtime') AS bucket,
model, alias, provider, service_tier, status_code, failed, COUNT(*), `+providerCostColumns+`
FROM usage_events WHERE `+where+` GROUP BY bucket, model, alias, provider, service_tier, status_code, failed ORDER BY bucket`, append([]any{format}, args...)...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	points := make([]providerReportPoint, 0)
	modelCosts := make(map[[2]string]costSummary)
	for rows.Next() {
		var bucket string
		var event costTokenRow
		var status int
		var failed bool
		var requests int64
		if err := rows.Scan(&bucket, &event.Model, &event.Alias, &event.Provider, &event.ServiceTier, &status, &failed, &requests,
			&event.InputTokens, &event.OutputTokens, &event.CachedTokens, &event.CacheReadTokens, &event.CacheCreationTokens, &event.TotalTokens); err != nil {
			return nil, nil, err
		}
		if len(points) == 0 || points[len(points)-1].Bucket != bucket {
			points = append(points, providerReportPoint{Bucket: bucket})
		}
		category := providerStatusCategory(status, failed)
		out.Statuses[category] += requests
		var cost costSummary
		addCost(&cost, event, prices)
		key := [2]string{event.Model, event.Alias}
		modelCost := modelCosts[key]
		modelCost.CostUSD += cost.CostUSD
		modelCost.UnpricedTokens += cost.UnpricedTokens
		modelCosts[key] = modelCost
		for _, totals := range []*providerReportTotals{&out.Totals, &points[len(points)-1].providerReportTotals} {
			totals.Requests += requests
			if category != "ok" {
				totals.Failed += requests
			}
			totals.TotalTokens += event.TotalTokens
			totals.CostUSD += cost.CostUSD
			totals.UnpricedTokens += cost.UnpricedTokens
		}
	}
	if len(points) > 240 {
		points = points[len(points)-240:]
	}
	return points, modelCosts, rows.Err()
}

func providerStatusCategory(status int, failed bool) string {
	switch status {
	case 401:
		return "401"
	case 402:
		return "402"
	case 403:
		return "403"
	case 429:
		return "429"
	}
	if status >= 500 && status < 600 {
		return "5xx"
	}
	if !failed && (status == 0 || status >= 200 && status < 300) {
		return "ok"
	}
	return "other"
}

func queryProviderBudget(ctx context.Context, db *sql.DB, where, name string, now time.Time, prices map[string]modelPrice, config providerBudgetConfig) (providerReportBudget, error) {
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	budget := providerReportBudget{LimitUSD: config.Budgets[name], MonthStart: month.Unix(), MonthEnd: month.AddDate(0, 1, 0).Unix(), Month: month.Format("2006-01"), Status: "not_configured"}
	if config.Invalid {
		budget.Status = "invalid_config"
		return budget, nil
	}
	if budget.LimitUSD == 0 {
		return budget, nil
	}
	rows, err := queryCostRows(ctx, db, `SELECT model, alias, provider, service_tier, `+providerCostColumns+`
FROM usage_events WHERE `+where+` GROUP BY model, alias, provider, service_tier`, budget.MonthStart, now.Unix()+1, name)
	if err != nil {
		return budget, err
	}
	cost := summarizeCost(rows, prices)
	budget.CostUSD, budget.UnpricedTokens = cost.CostUSD, cost.UnpricedTokens
	budget.Status = "under"
	switch {
	case budget.CostUSD >= budget.LimitUSD:
		budget.Status = "over"
	case budget.CostUSD >= budget.LimitUSD*0.8:
		budget.Status = "warning"
	case budget.UnpricedTokens > 0:
		budget.Status = "incomplete"
	}
	return budget, nil
}
