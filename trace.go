package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Each request owns its files. Retired and current libraries can write to the
// same directory without sharing Go globals or rewriting each other's traces.
type attemptTrace struct {
	HostOpenMS       int64 `json:"host_open_ms"`
	HostOpenedAt     int64 `json:"host_opened_at,omitempty"`
	FirstHostFrameAt int64 `json:"first_host_frame_at,omitempty"`
	FirstForwardedAt int64 `json:"first_forwarded_at,omitempty"`
	ParseMicros      int64 `json:"parse_micros"`
	EmitMicros       int64 `json:"emit_micros"`

	Number         int    `json:"number"`
	CredentialID   string `json:"credential_id"`
	CredentialName string `json:"credential_name"`
	StartedAt      int64  `json:"started_at"`
	EndedAt        int64  `json:"ended_at,omitempty"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	Outcome        string `json:"outcome"`
	ErrorCode      string `json:"error_code,omitempty"`
	Reason         string `json:"reason,omitempty"`
	UpstreamTrace  string `json:"upstream_trace,omitempty"`
}
type credentialTotals struct {
	Name      string `json:"name"`
	Attempts  int    `json:"attempts"`
	Completed int    `json:"completed"`
	Failed    int    `json:"failed"`
}
type requestTrace struct {
	SessionTitle     string                       `json:"session_title,omitempty"`
	Project          string                       `json:"project,omitempty"`
	ThreadTitle      string                       `json:"thread_title,omitempty"`
	ID               string                       `json:"id"`
	Version          string                       `json:"version"`
	Instance         string                       `json:"instance"`
	HostRun          string                       `json:"host_run"`
	SessionID        string                       `json:"session_id"`
	ThreadID         string                       `json:"thread_id,omitempty"`
	ParentThreadID   string                       `json:"parent_thread_id,omitempty"`
	TurnID           string                       `json:"turn_id,omitempty"`
	AgentName        string                       `json:"agent_name,omitempty"`
	IdentitySource   string                       `json:"identity_source"`
	Model            string                       `json:"model"`
	StartedAt        int64                        `json:"started_at"`
	UpdatedAt        int64                        `json:"updated_at"`
	EndedAt          int64                        `json:"ended_at,omitempty"`
	FirstOutputAt    int64                        `json:"first_output_at,omitempty"`
	Phase            string                       `json:"phase"`
	Attempts         int                          `json:"attempts"`
	Retries          int                          `json:"retries"`
	LastAttempt      *attemptTrace                `json:"last_attempt,omitempty"`
	LastFailure      *attemptTrace                `json:"last_failure,omitempty"`
	Credentials      map[string]*credentialTotals `json:"credentials"`
	FinalCode        string                       `json:"final_code,omitempty"`
	FinalReason      string                       `json:"final_reason,omitempty"`
	DetailsTruncated bool                         `json:"details_truncated"`
	RecordedAttempts int                          `json:"recorded_attempts"`
	StorageErrors    int                          `json:"storage_errors"`
	path             string
	journalBytes     int64
}

var traceStorageErrors atomic.Uint64
var traceInstance = randomID()
var traceHostRun = hostRunID()
var nextTraceCleanup atomic.Int64

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
func hostRunID() string {
	// PID alone is reused after restarts. Linux /proc includes process start ticks.
	raw, err := os.ReadFile("/proc/self/stat")
	if err == nil {
		if i := strings.LastIndex(string(raw), ")"); i >= 0 {
			f := strings.Fields(string(raw[i+1:]))
			if len(f) > 19 {
				return fmt.Sprintf("%d:%s", os.Getpid(), f[19])
			}
		}
	}
	return fmt.Sprintf("%d:%s", os.Getpid(), randomID())
}
func safeIdentity(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return ""
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return ""
		}
	}
	return s
}
func headerValue(h http.Header, name string) string {
	for k, vs := range h {
		if strings.EqualFold(k, name) && len(vs) > 0 {
			return safeIdentity(vs[0])
		}
	}
	return ""
}
func newRequestTrace(c *Config, req executorRequest) *requestTrace {
	now := time.Now()
	id := randomID()
	t := &requestTrace{ID: id, Version: pluginVersion, Instance: traceInstance, HostRun: traceHostRun, Model: safeIdentity(req.Model), StartedAt: now.UnixMilli(), Phase: "accepted", IdentitySource: "missing", Credentials: map[string]*credentialTotals{}}
	var meta map[string]any
	// Only decode the small identity header, never traverse message/tool histories.
	for k, vs := range req.Headers {
		if strings.EqualFold(k, "X-Codex-Turn-Metadata") && len(vs) > 0 && len(vs[0]) <= 16384 {
			_ = json.Unmarshal([]byte(vs[0]), &meta)
		}
	}
	get := func(k string) string { s, _ := meta[k].(string); return safeIdentity(s) }
	t.SessionID = headerValue(req.Headers, "Session-Id")
	if t.SessionID == "" {
		t.SessionID = headerValue(req.Headers, "Session_id")
	}
	if t.SessionID != "" {
		t.IdentitySource = "Session-Id"
	} else if t.SessionID = get("session_id"); t.SessionID != "" {
		t.IdentitySource = "X-Codex-Turn-Metadata"
	}
	t.ThreadID = headerValue(req.Headers, "Thread-Id")
	if t.ThreadID == "" {
		t.ThreadID = headerValue(req.Headers, "Thread_id")
	}
	if t.ThreadID == "" {
		t.ThreadID = get("thread_id")
	}
	t.ParentThreadID = headerValue(req.Headers, "X-Codex-Parent-Thread-Id")
	if t.ParentThreadID == "" {
		t.ParentThreadID = get("parent_thread_id")
	}
	t.TurnID = get("turn_id")
	t.AgentName = get("agent_name")
	// Thread is an explicit identity, but do not call it a known root session.
	if t.SessionID == "" && t.ThreadID != "" {
		t.SessionID = t.ThreadID
		t.IdentitySource = "Thread-Id (root unknown)"
	}
	t.path = filepath.Join(c.TraceDir, now.UTC().Format("2006-01-02"), id)
	cleanupTraces(c)
	t.save()
	return t
}
func (t *requestTrace) save() {
	t.UpdatedAt = time.Now().UnixMilli()
	if err := os.MkdirAll(filepath.Dir(t.path), 0700); err != nil {
		t.storageError()
		return
	}
	raw, err := json.Marshal(t)
	if err != nil {
		t.storageError()
		return
	}
	// Atomic replacement means management reads never see half a snapshot.
	f, err := os.CreateTemp(filepath.Dir(t.path), ".trace-*")
	if err != nil {
		t.storageError()
		return
	}
	name := f.Name()
	defer os.Remove(name)
	_, err = f.Write(raw)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, t.path+".json")
	}
	if err != nil {
		t.storageError()
	}
}
func (t *requestTrace) storageError() { t.StorageErrors++; traceStorageErrors.Add(1) }
func (t *requestTrace) startAttempt(a authEntry, n int) {
	t.Attempts = n
	t.Retries = n - 1
	t.Phase = "waiting_upstream"
	name := safeIdentity(a.Name)
	if name == "" {
		name = a.ID
	}
	t.LastAttempt = &attemptTrace{Number: n, CredentialID: a.ID, CredentialName: name, StartedAt: time.Now().UnixMilli(), Outcome: "waiting_upstream"}
	g := t.Credentials[a.ID]
	if g == nil {
		g = &credentialTotals{Name: name}
		t.Credentials[a.ID] = g
	}
	g.Attempts++
	t.save()
}
func (t *requestTrace) output() {
	t.Phase = "streaming"
	if t.FirstOutputAt == 0 {
		t.FirstOutputAt = time.Now().UnixMilli()
	}
	if t.LastAttempt != nil {
		t.LastAttempt.Outcome = "streaming"
	}
	t.save()
}
func (t *requestTrace) finishAttempt(c *Config, outcome string, err error) {
	a := t.LastAttempt
	if a == nil {
		return
	}
	a.EndedAt = time.Now().UnixMilli()
	a.Outcome = outcome
	status, code, reason, upstream := traceError(err)
	// Keep HTTP 200 when an SSE error arrives; HTTP and SSE outcomes are separate.
	if a.HTTPStatus == 0 {
		a.HTTPStatus = status
	}
	a.ErrorCode = code
	a.Reason = reason
	if upstream != "" {
		a.UpstreamTrace = upstream
	}
	g := t.Credentials[a.CredentialID]
	if outcome == "completed" {
		g.Completed++
	} else if outcome != "cancelled" {
		g.Failed++
		copy := *a
		t.LastFailure = &copy
	}
	if outcome == "retry" || outcome == "credential_rejected" {
		t.Phase = "retrying"
	}
	raw, _ := json.Marshal(a)
	raw = append(raw, '\n')
	if t.journalBytes+int64(len(raw)) <= c.TraceMaxBytes {
		f, e := os.OpenFile(t.path+".jsonl", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if e == nil {
			var n int
			n, e = f.Write(raw)
			ce := f.Close()
			if e == nil {
				e = ce
			}
			if e == nil && n != len(raw) {
				e = fmt.Errorf("short trace write")
			}
		}
		if e != nil {
			t.storageError()
		} else {
			t.journalBytes += int64(len(raw))
			t.RecordedAttempts++
		}
	} else {
		t.DetailsTruncated = true
	}
	t.save()
}
func (t *requestTrace) finish(completed, committed, cancelled bool, err error) {
	t.EndedAt = time.Now().UnixMilli()
	switch {
	case completed:
		t.Phase = "completed"
	case cancelled:
		t.Phase = "cancelled"
	case committed:
		t.Phase = "interrupted"
	default:
		t.Phase = "failed"
	}
	_, t.FinalCode, t.FinalReason, _ = traceError(err)
	t.save()
}

var traceCodeRE = regexp.MustCompile(`"(?:code|type)"\s*:\s*"([a-zA-Z0-9_.-]{1,100})"`)
var traceUpstreamRE = regexp.MustCompile(`(?i)request[ _-]?id[\s:="()]+([a-zA-Z0-9_-]{8,128})`)

func traceError(err error) (int, string, string, string) {
	if err == nil {
		return 0, "", "", ""
	}
	text := strings.ToLower(err.Error())
	status := 0
	var rpc *rpcError
	if errors.As(err, &rpc) {
		status = rpc.HTTPStatus
	}
	if status == 0 {
		if m := statusPattern.FindStringSubmatch(text); len(m) > 1 {
			status, _ = strconv.Atoi(m[1])
		}
	}
	upstream := ""
	if m := traceUpstreamRE.FindStringSubmatch(err.Error()); len(m) > 1 {
		upstream = m[1]
	}
	// Persist a normalized reason, never an untrusted upstream message that may
	// echo API keys, prompt contents, or conversation history.
	for _, item := range []struct{ code, reason string }{
		{"thinking_signature_invalid", "推理签名无效"}, {"context_length_exceeded", "上下文超过上限"},
		{"get_channel_failed", "没有可用上游渠道或容量不足"}, {"rate_limit_reached", "上游限流"}, {"rate_limit_exceeded", "上游限流"},
		{"too_many_requests", "上游限流"}, {"invalid_api_key", "凭据无效"}, {"model_not_found", "模型不可用"},
		{"stream closed before response.completed", "流结束时没有完成事件"}, {"unexpected eof", "上游连接提前结束"},
		{"connection reset", "连接被重置"}, {"connection refused", "连接被拒绝"}, {"timeout", "上游等待超时"},
		{"max_attempts exhausted", "达到尝试次数上限"}, {"all eligible AnyRouter credentials", "全部凭据认证或模型权限失败"},
		{"max_bootstrap_bytes", "启动事件或 SSE 帧超过大小上限"}, {"invalid_request_error", "无效请求"},
	} {
		if strings.Contains(text, strings.ToLower(item.code)) {
			return status, strings.ReplaceAll(item.code, " ", "_"), item.reason, upstream
		}
	}
	if errors.Is(err, context.Canceled) {
		return status, "cancelled", "请求已取消", upstream
	}
	code := "upstream_error"
	for _, m := range traceCodeRE.FindAllStringSubmatch(err.Error(), -1) {
		if m[1] != "error" {
			code = m[1]
		}
	}
	reason := "上游或宿主调用失败"
	if status > 0 {
		reason = fmt.Sprintf("HTTP %d", status)
	}
	return status, code, reason, upstream
}

func cleanupTraces(c *Config) {
	now := time.Now()
	next := nextTraceCleanup.Load()
	if now.Unix() < next || !nextTraceCleanup.CompareAndSwap(next, now.Add(time.Hour).Unix()) {
		return
	}
	dirs, _ := os.ReadDir(c.TraceDir)
	cutoff := now.Add(-time.Duration(c.TraceRetention) * 24 * time.Hour).UnixMilli()
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		date, e := time.Parse("2006-01-02", d.Name())
		if e != nil || date.Add(24*time.Hour).UnixMilli() >= cutoff {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(c.TraceDir, d.Name()))
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			path := filepath.Join(c.TraceDir, d.Name(), f.Name())
			raw, e := os.ReadFile(path)
			var t requestTrace
			if e == nil && json.Unmarshal(raw, &t) == nil && (t.EndedAt > 0 && t.EndedAt < cutoff || t.HostRun != traceHostRun) {
				_ = os.Remove(strings.TrimSuffix(path, ".json") + ".jsonl")
				_ = os.Remove(path)
			}
		}
		_ = os.Remove(filepath.Join(c.TraceDir, d.Name())) // only succeeds when empty
	}
}

func traceList(c *Config) ([]requestTrace, error) {
	dirs, e := os.ReadDir(c.TraceDir)
	if os.IsNotExist(e) {
		return []requestTrace{}, nil
	}
	if e != nil {
		return nil, e
	}
	var out []requestTrace
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if _, e := time.Parse("2006-01-02", d.Name()); e != nil {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(c.TraceDir, d.Name()))
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			path := filepath.Join(c.TraceDir, d.Name(), f.Name())
			raw, e := os.ReadFile(path)
			if e != nil {
				continue
			}
			var t requestTrace
			if json.Unmarshal(raw, &t) != nil {
				continue
			}
			t.path = strings.TrimSuffix(path, ".json")
			if t.EndedAt == 0 && t.HostRun != traceHostRun {
				t.Phase = "unknown_after_restart"
			}
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out, nil
}
func traceAttempts(t requestTrace, offset, limit int) ([]attemptTrace, int, error) {
	f, e := os.Open(t.path + ".jsonl")
	if os.IsNotExist(e) {
		return []attemptTrace{}, 0, nil
	}
	if e != nil {
		return nil, 0, e
	}
	defer f.Close()
	out := []attemptTrace{}
	total := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 65536)
	for scanner.Scan() {
		var a attemptTrace
		if json.Unmarshal(scanner.Bytes(), &a) != nil {
			continue
		}
		if total >= offset && len(out) < limit {
			out = append(out, a)
		}
		total++
	}
	return out, total, scanner.Err()
}
