package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

//go:embed dashboard.html
var dashboardHTML []byte

func managementRegistration() any {
	return map[string]any{
		"Routes": []map[string]string{
			{"Method": "GET", "Path": "/plugins/anyrouter-retry/runtime"},
			{"Method": "GET", "Path": "/plugins/anyrouter-retry/requests"},
			{"Method": "GET", "Path": "/plugins/anyrouter-retry/trace"},
		},
		"Resources": []map[string]string{
			{"Path": "/dashboard", "Menu": "AnyRouter 会话请求", "Description": "按会话查看请求、凭据尝试顺序与最终结果"},
		},
	}
}
func managementResponse(status int, value any) any {
	body, _ := json.Marshal(value)
	return map[string]any{"StatusCode": status, "Headers": http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, "Body": body}
}
func runtimeState() any {
	count := 0
	active.Range(func(_, _ any) bool { count++; return true })
	c := currentConfig.Load()
	accept := c != nil && c.Enabled && c.AcceptNew
	retention := 0
	if c != nil {
		retention = c.TraceRetention
	}
	return map[string]any{"version": pluginVersion, "accept_new_requests": accept, "active_requests_current_instance": count, "physical_unload_preserves_streams": false, "trace_storage_errors_current_instance": traceStorageErrors.Load(), "trace_retention_days": retention, "instance": traceInstance, "host_run": traceHostRun}
}
func handleManagement(raw []byte) (any, error) {
	var req struct {
		Method  string
		Path    string
		Headers http.Header
		Query   url.Values
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	resourcePrefix := "/v0/resource/plugins/anyrouter-retry"
	if req.Path == resourcePrefix+"/dashboard" {
		return map[string]any{"StatusCode": 200, "Headers": http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'"}}, "Body": dashboardHTML}, nil
	}
	path := strings.TrimPrefix(req.Path, "/v0/management/plugins/anyrouter-retry/")
	if !strings.HasPrefix(req.Path, "/v0/management/plugins/anyrouter-retry/") {
		return managementResponse(404, map[string]string{"error": "unknown path"}), nil
	}
	if req.Method != "GET" {
		return managementResponse(405, map[string]string{"error": "method not allowed"}), nil
	}
	if path == "runtime" {
		return managementResponse(200, runtimeState()), nil
	}
	c := currentConfig.Load()
	if c == nil {
		return managementResponse(503, map[string]string{"error": "plugin not configured"}), nil
	}
	all, err := traceList(c)
	if err != nil {
		return managementResponse(503, map[string]string{"error": "无法读取请求记录；请检查 trace_dir 与存储状态"}), nil
	}
	labels := readSessionLabels(c)
	for i := range all {
		enrichTrace(&all[i], labels)
	}
	limit := queryInt(req.Query, "limit", 200, 1, 1000)
	offset := queryInt(req.Query, "offset", 0, 0, 1000000)
	if path == "requests" {
		q := strings.ToLower(strings.TrimSpace(req.Query.Get("q")))
		session := req.Query.Get("session")
		phase := req.Query.Get("phase")
		filtered := make([]requestTrace, 0)
		for _, t := range all {
			if session != "" && t.SessionID != session {
				continue
			}
			if phase != "" && phase != "all" && t.Phase != phase {
				continue
			}
			if q != "" {
				hay := t.SessionTitle + " " + t.Project + " " + t.ThreadTitle + " " + t.SessionID + " " + t.ThreadID + " " + t.TurnID + " " + t.AgentName + " " + t.ID + " " + t.Model
				for _, g := range t.Credentials {
					hay += " " + g.Name
				}
				if !strings.Contains(strings.ToLower(hay), q) {
					continue
				}
			}
			filtered = append(filtered, t)
		}
		total := len(filtered)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		return managementResponse(200, map[string]any{"requests": filtered[offset:end], "total": total, "offset": offset, "limit": limit, "server_time": time.Now().UnixMilli(), "runtime": runtimeState()}), nil
	}
	if path == "trace" {
		for _, t := range all {
			if t.ID == req.Query.Get("id") {
				attempts, total, e := traceAttempts(t, offset, limit)
				if e != nil {
					return managementResponse(503, map[string]string{"error": "尝试明细读取失败"}), nil
				}
				return managementResponse(200, map[string]any{"request": t, "attempts": attempts, "total": total, "offset": offset, "limit": limit, "server_time": time.Now().UnixMilli()}), nil
			}
		}
		return managementResponse(404, map[string]string{"error": "请求记录不存在或已超过保留期限"}), nil
	}
	return managementResponse(404, map[string]string{"error": "unknown path"}), nil
}
func queryInt(q url.Values, key string, fallback, low, high int) int {
	n, e := strconv.Atoi(q.Get(key))
	if e != nil {
		return fallback
	}
	if n < low {
		return low
	}
	if n > high {
		return high
	}
	return n
}
