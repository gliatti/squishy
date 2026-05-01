package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// ---- helpers ----

func callHandler(ctx context.Context, method, path string, body any) *mcp.CallToolResult {
	data, err := doRequest(ctx, method, path, body)
	if err != nil {
		return mcp.NewToolResultError(err.Error())
	}
	return mcp.NewToolResultText(formatJSON(data))
}

func requireString(req mcp.CallToolRequest, key string) (string, *mcp.CallToolResult) {
	v := req.GetString(key, "")
	if v == "" {
		return "", mcp.NewToolResultError(fmt.Sprintf("%s is required", key))
	}
	return v, nil
}

// ---- projects ----

func listProjectsHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return callHandler(ctx, "GET", "/api/v1/projects", nil), nil
}

func getProjectHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "project_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "GET", "/api/v1/projects/"+id, nil), nil
}

func createProjectHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, errRes := requireString(req, "name")
	if errRes != nil {
		return errRes, nil
	}
	body := map[string]any{
		"name":        name,
		"description": req.GetString("description", ""),
	}
	return callHandler(ctx, "POST", "/api/v1/projects", body), nil
}

func deleteProjectHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "project_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "DELETE", "/api/v1/projects/"+id, nil), nil
}

func updateProjectHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "project_id")
	if errRes != nil {
		return errRes, nil
	}
	name, errRes := requireString(req, "name")
	if errRes != nil {
		return errRes, nil
	}
	body := map[string]any{
		"name":        name,
		"description": req.GetString("description", ""),
	}
	return callHandler(ctx, "PUT", "/api/v1/projects/"+id, body), nil
}

// ---- instances ----

func listInstancesHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, errRes := requireString(req, "project_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "GET", "/api/v1/projects/"+projectID+"/instances", nil), nil
}

func createInstanceHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, errRes := requireString(req, "project_id")
	if errRes != nil {
		return errRes, nil
	}
	name, errRes := requireString(req, "name")
	if errRes != nil {
		return errRes, nil
	}
	kind, errRes := requireString(req, "kind")
	if errRes != nil {
		return errRes, nil
	}
	host, errRes := requireString(req, "host")
	if errRes != nil {
		return errRes, nil
	}
	database, errRes := requireString(req, "database")
	if errRes != nil {
		return errRes, nil
	}
	username, errRes := requireString(req, "username")
	if errRes != nil {
		return errRes, nil
	}
	strategy, errRes := requireString(req, "target_strategy")
	if errRes != nil {
		return errRes, nil
	}
	tHost, errRes := requireString(req, "target_host")
	if errRes != nil {
		return errRes, nil
	}
	tDB, errRes := requireString(req, "target_database")
	if errRes != nil {
		return errRes, nil
	}
	tUser, errRes := requireString(req, "target_username")
	if errRes != nil {
		return errRes, nil
	}
	body := map[string]any{
		"name":             name,
		"kind":             kind,
		"host":             host,
		"port":             req.GetInt("port", 0),
		"database":         database,
		"username":         username,
		"password":         req.GetString("password", ""),
		"ssl_mode":         req.GetString("ssl_mode", ""),
		"target_strategy":  strategy,
		"target_host":      tHost,
		"target_port":      req.GetInt("target_port", 0),
		"target_database":  tDB,
		"target_username":  tUser,
		"target_password":  req.GetString("target_password", ""),
		"target_ssl_mode":  req.GetString("target_ssl_mode", ""),
		"target_create_db": req.GetBool("target_create_db", false),
	}
	return callHandler(ctx, "POST", "/api/v1/projects/"+projectID+"/instances", body), nil
}

func getInstanceHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "instance_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "GET", "/api/v1/instances/"+id, nil), nil
}

func deleteInstanceHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "instance_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "DELETE", "/api/v1/instances/"+id, nil), nil
}

func updateInstanceHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "instance_id")
	if errRes != nil {
		return errRes, nil
	}
	name, errRes := requireString(req, "name")
	if errRes != nil {
		return errRes, nil
	}
	host, errRes := requireString(req, "host")
	if errRes != nil {
		return errRes, nil
	}
	database, errRes := requireString(req, "database")
	if errRes != nil {
		return errRes, nil
	}
	username, errRes := requireString(req, "username")
	if errRes != nil {
		return errRes, nil
	}
	strategy, errRes := requireString(req, "target_strategy")
	if errRes != nil {
		return errRes, nil
	}
	tHost, errRes := requireString(req, "target_host")
	if errRes != nil {
		return errRes, nil
	}
	tDB, errRes := requireString(req, "target_database")
	if errRes != nil {
		return errRes, nil
	}
	tUser, errRes := requireString(req, "target_username")
	if errRes != nil {
		return errRes, nil
	}
	body := map[string]any{
		"name":             name,
		"host":             host,
		"port":             req.GetInt("port", 0),
		"database":         database,
		"username":         username,
		"password":         req.GetString("password", ""),
		"ssl_mode":         req.GetString("ssl_mode", ""),
		"target_strategy":  strategy,
		"target_host":      tHost,
		"target_port":      req.GetInt("target_port", 0),
		"target_database":  tDB,
		"target_username":  tUser,
		"target_password":  req.GetString("target_password", ""),
		"target_ssl_mode":  req.GetString("target_ssl_mode", ""),
		"target_create_db": req.GetBool("target_create_db", false),
	}
	return callHandler(ctx, "PUT", "/api/v1/instances/"+id, body), nil
}

func testInstanceConnectionHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "instance_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "POST", "/api/v1/instances/"+id+"/test-connection", nil), nil
}

func testInstanceTargetConnectionHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "instance_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "POST", "/api/v1/instances/"+id+"/test-target-connection", nil), nil
}

func rediscoverInstanceHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "instance_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "POST", "/api/v1/instances/"+id+"/rediscover", nil), nil
}

func listMigrationsHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "instance_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "GET", "/api/v1/instances/"+id+"/migrations", nil), nil
}

// ---- migrations ----

func getMigrationHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "migration_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "GET", "/api/v1/migrations/"+id, nil), nil
}

func inspectMigrationHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "migration_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "POST", "/api/v1/migrations/"+id+"/inspect", nil), nil
}

// planMigrationHandler executes the plan and returns a compact, paginated
// summary. The full raw response on a large schema can exceed several MB
// (DDL + thousands of explanations + thousands of type_mappings) which both
// blows up the LLM context and makes the tool useless. This handler exposes:
//
//   - default: counts + ALL warnings + ALL prerequisites + the first 4 KB of
//     ddl_script + an aggregated explanation summary (top reasons by count).
//   - section="ddl" / "ddl_post" / "explanations" / "type_mappings": fetches
//     just that slice using offset/limit. For DDL the slice is char-based;
//     for explanations / type_mappings it's element-based.
func planMigrationHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "migration_id")
	if errRes != nil {
		return errRes, nil
	}
	raw, err := doRequest(ctx, "POST", "/api/v1/migrations/"+id+"/plan", map[string]any{})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var p struct {
		MigrationID    string           `json:"migration_id"`
		Status         string           `json:"status"`
		StmtsParsed    int              `json:"stmts_parsed"`
		DDLScript      string           `json:"ddl_script"`
		DDLPostScript  string           `json:"ddl_post_script"`
		Warnings       []map[string]any `json:"warnings"`
		Prerequisites  []map[string]any `json:"prerequisites"`
		Explanations   []map[string]any `json:"explanations"`
		TypeMappings   []map[string]any `json:"type_mappings"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("decode plan: %v", err)), nil
	}

	section := req.GetString("section", "summary")
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}
	limit := req.GetInt("limit", 0)
	// Optional filters applied client-side. severity / kind / object on
	// warnings + explanations; level on explanations.
	severityFilter := req.GetString("severity", "")
	kindFilter := req.GetString("kind", "")
	objectFilter := req.GetString("object", "")
	levelFilter := req.GetString("level", "")
	reasonFilter := req.GetString("reason_contains", "")

	warnings := filterWarnings(p.Warnings, severityFilter, kindFilter, objectFilter)
	explanations := filterExplanations(p.Explanations, levelFilter, objectFilter, reasonFilter)

	switch section {
	case "ddl":
		return mcp.NewToolResultText(formatJSON(mustJSON(sliceString(p.DDLScript, offset, limit, 8000)))), nil
	case "ddl_post":
		return mcp.NewToolResultText(formatJSON(mustJSON(sliceString(p.DDLPostScript, offset, limit, 8000)))), nil
	case "warnings":
		return mcp.NewToolResultText(formatJSON(mustJSON(sliceArray(toAnySlice(warnings), offset, limit, 200)))), nil
	case "explanations":
		return mcp.NewToolResultText(formatJSON(mustJSON(sliceArray(toAnySlice(explanations), offset, limit, 200)))), nil
	case "type_mappings":
		return mcp.NewToolResultText(formatJSON(mustJSON(sliceArray(toAnySlice(p.TypeMappings), offset, limit, 200)))), nil
	}

	// Default: summary. When warnings/explanations are very long, only
	// surface counts + a small head; the caller can paginate via section=*.
	summary := map[string]any{
		"migration_id":          p.MigrationID,
		"status":                p.Status,
		"stmts_parsed":          p.StmtsParsed,
		"ddl_script_chars":      len(p.DDLScript),
		"ddl_post_script_chars": len(p.DDLPostScript),
		"ddl_script_head":       headOfString(p.DDLScript, 4000),
		"warnings_total":        len(warnings),
		"warnings_head":         headOfArray(toAnySlice(warnings), 50),
		"warnings_by_severity":  groupByField(warnings, "severity"),
		"warnings_by_kind":      groupByField(warnings, "kind"),
		"prerequisites":         p.Prerequisites,
		"explanations_total":    len(explanations),
		"explanations_summary":  summarizeExplanations(explanations),
		"type_mappings_total":   len(p.TypeMappings),
		"type_mappings_summary": summarizeTypeMappings(p.TypeMappings),
		"_pagination_hint":      "section=ddl|ddl_post|warnings|explanations|type_mappings + offset/limit. Filters: severity, kind, object, level, reason_contains.",
	}
	return mcp.NewToolResultText(formatJSON(mustJSON(summary))), nil
}

func filterWarnings(in []map[string]any, severity, kind, object string) []map[string]any {
	if severity == "" && kind == "" && object == "" {
		return in
	}
	out := make([]map[string]any, 0, len(in))
	for _, w := range in {
		if severity != "" {
			if v, _ := w["severity"].(string); v != severity {
				continue
			}
		}
		if kind != "" {
			if v, _ := w["kind"].(string); v != kind {
				continue
			}
		}
		if object != "" {
			if v, _ := w["object"].(string); !strings.Contains(v, object) {
				continue
			}
		}
		out = append(out, w)
	}
	return out
}

func filterExplanations(in []map[string]any, level, object, reasonContains string) []map[string]any {
	if level == "" && object == "" && reasonContains == "" {
		return in
	}
	out := make([]map[string]any, 0, len(in))
	for _, e := range in {
		if level != "" {
			if v, _ := e["level"].(string); v != level {
				continue
			}
		}
		if object != "" {
			if v, _ := e["object"].(string); !strings.Contains(v, object) {
				continue
			}
		}
		if reasonContains != "" {
			if v, _ := e["reason"].(string); !strings.Contains(v, reasonContains) {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

func headOfArray(arr []any, n int) []any {
	if len(arr) <= n {
		return arr
	}
	return arr[:n]
}

func groupByField(rows []map[string]any, field string) map[string]int {
	counts := map[string]int{}
	for _, r := range rows {
		v, _ := r[field].(string)
		if v == "" {
			v = "(unset)"
		}
		counts[v]++
	}
	return counts
}

func getPrerequisitesHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "migration_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "GET", "/api/v1/migrations/"+id+"/prerequisites", nil), nil
}

func ackPrerequisitesHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "migration_id")
	if errRes != nil {
		return errRes, nil
	}
	acked := req.GetStringSlice("acked", nil)
	body := map[string]any{"acked": acked}
	return callHandler(ctx, "POST", "/api/v1/migrations/"+id+"/prerequisites/ack", body), nil
}

func startRunHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "migration_id")
	if errRes != nil {
		return errRes, nil
	}
	body := map[string]any{
		"mode":      req.GetString("mode", "auto"),
		"skip_data": req.GetBool("skip_data", false),
	}
	return callHandler(ctx, "POST", "/api/v1/migrations/"+id+"/runs", body), nil
}

// ---- runs ----

func getRunHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "GET", "/api/v1/runs/"+id, nil), nil
}

// listStepsHandler returns steps for a run with status/kind filtering and
// element-based pagination. Without filters or pagination, large runs can
// produce 90 000+ lines of JSON; default page size is 100 and counts always
// surface so callers can decide whether to keep paging.
func listStepsHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	statusFilter := req.GetString("status", "")
	kindFilter := req.GetString("kind", "")
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}
	limit := req.GetInt("limit", 100)
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	raw, err := doRequest(ctx, "GET", "/api/v1/runs/"+id+"/steps", nil)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var payload struct {
		Steps []map[string]any `json:"steps"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("decode steps: %v", err)), nil
	}
	filtered := make([]map[string]any, 0, len(payload.Steps))
	for _, s := range payload.Steps {
		if statusFilter != "" {
			if v, _ := s["status"].(string); v != statusFilter {
				continue
			}
		}
		if kindFilter != "" {
			if v, _ := s["kind"].(string); v != kindFilter {
				continue
			}
		}
		filtered = append(filtered, s)
	}
	total := len(filtered)
	end := offset + limit
	if offset > total {
		offset = total
	}
	if end > total {
		end = total
	}
	page := filtered[offset:end]
	resp := map[string]any{
		"steps":        page,
		"total":        total,
		"offset":       offset,
		"limit":        limit,
		"next_offset":  nilIfDone(end, total),
	}
	out, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(out)), nil
}

// ---- helpers for plan/list pagination ----

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func toAnySlice(in []map[string]any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func nilIfDone(end, total int) any {
	if end >= total {
		return nil
	}
	return end
}

func headOfString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sliceString(s string, offset, limit, def int) map[string]any {
	if limit <= 0 {
		limit = def
	}
	if offset > len(s) {
		offset = len(s)
	}
	end := offset + limit
	if end > len(s) {
		end = len(s)
	}
	return map[string]any{
		"chunk":       s[offset:end],
		"chunk_chars": end - offset,
		"total_chars": len(s),
		"offset":      offset,
		"limit":       limit,
		"next_offset": nilIfDone(end, len(s)),
	}
}

func sliceArray(arr []any, offset, limit, def int) map[string]any {
	if limit <= 0 {
		limit = def
	}
	total := len(arr)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return map[string]any{
		"items":       arr[offset:end],
		"total":       total,
		"offset":      offset,
		"limit":       limit,
		"next_offset": nilIfDone(end, total),
	}
}

// summarizeExplanations groups explanations by reason and returns the top 30
// by count, plus warn/info totals. Lets the caller see what's repeating
// without dragging the full list (often 1000s of identical rows).
func summarizeExplanations(in []map[string]any) map[string]any {
	type bucket struct {
		count int
		level string
		first map[string]any
	}
	by := map[string]*bucket{}
	warn, info := 0, 0
	for _, e := range in {
		reason, _ := e["reason"].(string)
		level, _ := e["level"].(string)
		if level == "warn" {
			warn++
		} else {
			info++
		}
		b, ok := by[reason]
		if !ok {
			b = &bucket{level: level, first: e}
			by[reason] = b
		}
		b.count++
	}
	type row struct {
		Reason  string         `json:"reason"`
		Level   string         `json:"level"`
		Count   int            `json:"count"`
		Example map[string]any `json:"example"`
	}
	rows := make([]row, 0, len(by))
	for r, b := range by {
		rows = append(rows, row{Reason: r, Level: b.level, Count: b.count, Example: b.first})
	}
	// sort desc by count
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].Count > rows[i].Count {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if len(rows) > 30 {
		rows = rows[:30]
	}
	return map[string]any{
		"warn_total":     warn,
		"info_total":     info,
		"unique_reasons": len(by),
		"top_reasons":    rows,
	}
}

// summarizeTypeMappings groups type mappings by source→target pair and
// returns the top 30 pairs. Useful to see at a glance what type rules fired
// and how often, without reading thousands of identical rows.
func summarizeTypeMappings(in []map[string]any) map[string]any {
	type bucket struct {
		count int
		first map[string]any
	}
	by := map[string]*bucket{}
	for _, m := range in {
		src, _ := m["mysql"].(string)
		pg, _ := m["pg"].(string)
		key := src + " → " + pg
		b, ok := by[key]
		if !ok {
			b = &bucket{first: m}
			by[key] = b
		}
		b.count++
	}
	type row struct {
		Pair    string         `json:"pair"`
		Count   int            `json:"count"`
		Example map[string]any `json:"example"`
	}
	rows := make([]row, 0, len(by))
	for k, b := range by {
		rows = append(rows, row{Pair: k, Count: b.count, Example: b.first})
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].Count > rows[i].Count {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if len(rows) > 30 {
		rows = rows[:30]
	}
	return map[string]any{
		"unique_pairs": len(by),
		"top_pairs":    rows,
	}
}

func listBatchesHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	runID, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	stepID, errRes := requireString(req, "step_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "GET", "/api/v1/runs/"+runID+"/steps/"+stepID+"/batches", nil), nil
}

func playStepHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	runID, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	stepID, errRes := requireString(req, "step_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "POST", "/api/v1/runs/"+runID+"/steps/"+stepID+"/play", nil), nil
}

func replayStepHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	runID, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	stepID, errRes := requireString(req, "step_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "POST", "/api/v1/runs/"+runID+"/steps/"+stepID+"/replay", nil), nil
}

func playLevelHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	runID, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	level := req.GetInt("level", -1)
	if level < 0 {
		return mcp.NewToolResultError("level is required and must be >= 0"), nil
	}
	return callHandler(ctx, "POST", fmt.Sprintf("/api/v1/runs/%s/levels/%d/play", runID, level), nil), nil
}

func replayLevelHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	runID, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	level := req.GetInt("level", -1)
	if level < 0 {
		return mcp.NewToolResultError("level is required and must be >= 0"), nil
	}
	return callHandler(ctx, "POST", fmt.Sprintf("/api/v1/runs/%s/levels/%d/replay", runID, level), nil), nil
}

func cancelRunHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "POST", "/api/v1/runs/"+id+"/cancel", nil), nil
}

func retryRunHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id, errRes := requireString(req, "run_id")
	if errRes != nil {
		return errRes, nil
	}
	return callHandler(ctx, "POST", "/api/v1/runs/"+id+"/retry", nil), nil
}

// ---- dev-loop helpers (no direct shell access from Claude) ----

// runDocker runs a single docker compose command and returns stdout+stderr.
// Used for the dev-loop helpers below; not for arbitrary command execution.
// The CWD is forced to /workspace (the project root mounted into this
// container) so docker compose picks up docker-compose.yml regardless of
// where the Go process was started.
func runDocker(ctx context.Context, args ...string) ([]byte, error) {
	// Force the same compose project name the host uses, otherwise running
	// `docker compose` from /workspace inside the MCP container creates a
	// SECOND, isolated stack (`workspace_*` networks/volumes) and the
	// dev-loop helpers can't see the host's running services. The host
	// project name comes from the directory containing docker-compose.yml
	// — `squishy` — so we pin it explicitly.
	full := append([]string{"compose", "-p", "squishy"}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = "/workspace"
	return cmd.CombinedOutput()
}

func restartApiHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := runDocker(rctx, "restart", "api")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("restart api failed: %v\n%s", err, string(out))), nil
	}
	// Wait briefly for the API to listen again, then probe /health.
	time.Sleep(3 * time.Second)
	logs, _ := runDocker(rctx, "logs", "api", "--since=10s", "--tail=50")
	resp := map[string]any{
		"docker_output": string(out),
		"recent_logs":   string(logs),
	}
	b, _ := json.MarshalIndent(resp, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

func restartMcpHandler(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	// Note: this restarts the MCP server itself, ending the current request
	// in-flight. Useful when MCP-side handler code changes are made.
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := runDocker(rctx, "restart", "squishy-mcp")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("restart squishy-mcp failed: %v\n%s", err, string(out))), nil
	}
	return mcp.NewToolResultText("squishy-mcp restart issued; reconnect once the new instance is listening"), nil
}

func runUnitTestsHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	pkgs := req.GetString("packages", "./internal/dialects/oracle/... ./internal/translate/...")
	pattern := req.GetString("run", "")
	timeoutSec := req.GetInt("timeout_seconds", 180)
	if timeoutSec < 30 {
		timeoutSec = 30
	}
	if timeoutSec > 600 {
		timeoutSec = 600
	}
	rctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer cancel()
	args := []string{"--profile", "test", "run", "--rm", "unit-tests", "go", "test"}
	for _, p := range strings.Fields(pkgs) {
		args = append(args, p)
	}
	args = append(args, "-count=1", fmt.Sprintf("-timeout=%ds", timeoutSec))
	if pattern != "" {
		args = append(args, "-run", pattern)
	}
	if req.GetBool("verbose", false) {
		args = append(args, "-v")
	}
	out, err := runDocker(rctx, args...)
	resp := map[string]any{
		"command":     "docker compose " + strings.Join(args, " "),
		"output":      string(out),
		"exit_failed": err != nil,
	}
	if err != nil {
		resp["error"] = err.Error()
	}
	b, _ := json.MarshalIndent(resp, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

func getStepPayloadHandler(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	stepID, errRes := requireString(req, "step_id")
	if errRes != nil {
		return errRes, nil
	}
	field := req.GetString("field", "")
	maxChars := req.GetInt("max_chars", 4000)
	if maxChars < 200 {
		maxChars = 200
	}
	offset := req.GetInt("offset", 0)
	if offset < 0 {
		offset = 0
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Pull the latest job payload for this step from the app DB. Using
	// docker compose exec keeps credentials local to the stack.
	q := fmt.Sprintf("SELECT payload::text FROM squishy.jobs WHERE step_id='%s' ORDER BY created_at DESC LIMIT 1", stepID)
	out, err := runDocker(rctx, "exec", "-T", "postgres", "psql", "-U", "squishy", "-d", "squishy", "-tAc", q)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("psql query failed: %v\n%s", err, string(out))), nil
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return mcp.NewToolResultText("{}"), nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("decode payload: %v", err)), nil
	}
	if field != "" {
		v, ok := payload[field]
		if !ok {
			return mcp.NewToolResultError(fmt.Sprintf("payload has no field %q (keys: %v)", field, mapKeysOf(payload))), nil
		}
		// Char-paginate string fields; render any other field verbatim.
		if str, ok := v.(string); ok {
			return mcp.NewToolResultText(formatJSON(mustJSON(sliceString(str, offset, maxChars, maxChars)))), nil
		}
		b, _ := json.MarshalIndent(v, "", "  ")
		return mcp.NewToolResultText(string(b)), nil
	}
	// No field: return shape (keys + per-string-field length).
	shape := map[string]any{}
	for k, v := range payload {
		switch x := v.(type) {
		case string:
			shape[k] = map[string]any{"type": "string", "chars": len(x)}
		default:
			shape[k] = map[string]any{"type": fmt.Sprintf("%T", v)}
		}
	}
	resp := map[string]any{"shape": shape, "_hint": "call again with field=<name> [offset, max_chars] to fetch a slice"}
	b, _ := json.MarshalIndent(resp, "", "  ")
	return mcp.NewToolResultText(string(b)), nil
}

func mapKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

