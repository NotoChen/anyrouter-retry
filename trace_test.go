package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTraceIdentityRequestsAndAttemptSequence(t *testing.T) {
	c := testConfig(t)
	// Four actual attempts across distinct credentials; record identity without payloads.
	req := executorRequest{Model: "gpt-6-astra", Headers: http.Header{"session-id": {"root-session"}, "Thread-Id": {"child-thread"}, "X-Codex-Turn-Metadata": {`{"session_id":"root-session","thread_id":"child-thread","turn_id":"turn-1","agent_name":"/root/review","parent_thread_id":"root-session"}`}, "Authorization": {"Bearer NEVER_STORE_KEY"}}, OriginalRequest: []byte(`{"input":"NEVER_STORE_PROMPT"}`)}
	trace := newRequestTrace(c, req)
	names := []string{"AnyRouter-kj", "AnyRouter-qm", "AnyRouter-kj", "AnyRouter-bl"}
	for i, name := range names {
		trace.startAttempt(authEntry{ID: name, Name: name}, i+1)
		if i < 3 {
			trace.finishAttempt(c, "retry", &rpcError{HTTPStatus: 502, Message: "bad gateway NEVER_STORE_KEY NEVER_STORE_PROMPT"})
		} else {
			trace.output()
			trace.finishAttempt(c, "completed", nil)
		}
	}
	trace.finish(true, true, false, nil)
	second := newRequestTrace(c, req)
	second.finish(false, false, true, context.Canceled)
	list, e := traceList(c)
	if e != nil || len(list) != 2 {
		t.Fatalf("list=%v error=%v", list, e)
	}
	if trace.ID == second.ID || trace.SessionID != "root-session" || trace.ThreadID != "child-thread" || trace.TurnID != "turn-1" || trace.AgentName != "/root/review" || trace.ParentThreadID != "root-session" {
		t.Fatalf("bad identity: %+v", trace)
	}
	if trace.Attempts != 4 || trace.Retries != 3 || trace.Credentials["AnyRouter-kj"].Attempts != 2 || trace.Phase != "completed" {
		t.Fatalf("bad totals %+v", trace)
	}
	attempts, total, e := traceAttempts(*trace, 0, 10)
	if e != nil || total != 4 {
		t.Fatal(total, e)
	}
	for i, a := range attempts {
		if a.Number != i+1 || a.CredentialName != names[i] {
			t.Fatalf("wrong sequence: %+v", attempts)
		}
	}
	for _, ext := range []string{".json", ".jsonl"} {
		b, _ := os.ReadFile(trace.path + ext)
		if strings.Contains(string(b), "NEVER_STORE") {
			t.Fatal("secret or prompt persisted")
		}
	}
	page, total, e := traceAttempts(*trace, 2, 1)
	if e != nil || total != 4 || len(page) != 1 || page[0].Number != 3 {
		t.Fatal("bad attempt paging")
	}
}
func TestTraceLimitsCancellationAndRestart(t *testing.T) {
	c := testConfig(t)
	c.TraceMaxBytes = 4096
	tr := newRequestTrace(c, executorRequest{Model: "gpt-6-astra"})
	if tr.SessionID != "" || tr.IdentitySource != "missing" {
		t.Fatal("invented session")
	}
	for n := 1; n <= 100; n++ {
		tr.startAttempt(authEntry{ID: "kj", Name: "AnyRouter-kj"}, n)
		tr.finishAttempt(c, "retry", &rpcError{HTTPStatus: 429, Message: "rate_limit_reached"})
	}
	tr.finish(false, false, true, context.Canceled)
	if !tr.DetailsTruncated || tr.Attempts != 100 || tr.Retries != 99 || tr.Phase != "cancelled" {
		t.Fatalf("bad truncated trace %+v", tr)
	}
	if st, e := os.Stat(tr.path + ".jsonl"); e != nil || st.Size() > 4096 {
		t.Fatal("journal exceeded limit", e)
	}
	old := newRequestTrace(c, executorRequest{Headers: http.Header{"Thread-Id": {"thread-only"}}})
	old.HostRun = "prior-host"
	old.save()
	list, _ := traceList(c)
	for _, r := range list {
		if r.ID == old.ID && r.Phase != "unknown_after_restart" {
			t.Fatal("old request incorrectly shown as live")
		}
	}
	if old.IdentitySource != "Thread-Id (root unknown)" {
		t.Fatal("thread fallback falsely called root")
	}
}
func TestTraceStorageFailureDoesNotChangeModelResult(t *testing.T) {
	c := testConfig(t)
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if e := os.WriteFile(file, []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	c.TraceDir = file
	m := &mockHost{failures: 2, fail: `{"type":"error","error":{"code":"rate_limit_reached"}}`}
	r := setupRun(t, c, m)
	r.execute()
	if !r.completed || m.attempt != 3 || m.closed != "" || r.trace.StorageErrors == 0 {
		t.Fatal("trace failure affected generation")
	}
}
func TestConcurrentTraceWritersAndReadOnlyAuthentication(t *testing.T) {
	c := testConfig(t)
	currentConfig.Store(c)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr := newRequestTrace(c, executorRequest{Headers: http.Header{"Session-Id": {fmt.Sprint("session-", i%2)}, "Thread-Id": {fmt.Sprint("thread-", i)}}})
			tr.startAttempt(authEntry{ID: "kj", Name: "AnyRouter-kj"}, 1)
			tr.finishAttempt(c, "completed", nil)
			tr.finish(true, true, false, nil)
		}(i)
	}
	wg.Wait()
	list, e := traceList(c)
	if e != nil || len(list) != 12 {
		t.Fatal(len(list), e)
	}
	call := func(path, method string, headers http.Header, query url.Values) map[string]any {
		raw, _ := json.Marshal(map[string]any{"Path": path, "Method": method, "Headers": headers, "Query": query})
		v, e := handleManagement(raw)
		if e != nil {
			t.Fatal(e)
		}
		return v.(map[string]any)
	}
	v := call("/v0/resource/plugins/anyrouter-retry/view/requests", "GET", nil, nil)
	if v["StatusCode"] != 404 {
		t.Fatal("resource route exposes private trace data")
	}
	v = call("/v0/management/plugins/anyrouter-retry/requests", "GET", nil, url.Values{"session": {"session-0"}, "limit": {"2"}})
	var page struct {
		Total    int
		Requests []requestTrace
	}
	if e := json.Unmarshal(v["Body"].([]byte), &page); e != nil {
		t.Fatal(e)
	}
	if page.Total != 6 || len(page.Requests) != 2 {
		t.Fatal("bad session filter or paging", page)
	}

}
func TestTraceCleanupPreservesActiveAndRecentCompletion(t *testing.T) {
	c := testConfig(t)
	oldDir := filepath.Join(c.TraceDir, time.Now().Add(-10*24*time.Hour).UTC().Format("2006-01-02"))
	if e := os.MkdirAll(oldDir, 0700); e != nil {
		t.Fatal(e)
	}
	mk := func(id string, end int64) {
		tr := requestTrace{ID: id, HostRun: traceHostRun, EndedAt: end}
		raw, _ := json.Marshal(tr)
		_ = os.WriteFile(filepath.Join(oldDir, id+".json"), raw, 0600)
		_ = os.WriteFile(filepath.Join(oldDir, id+".jsonl"), []byte("{}\n"), 0600)
	}
	mk("active", 0)
	mk("recent", time.Now().UnixMilli())
	mk("expired", time.Now().Add(-9*24*time.Hour).UnixMilli())
	nextTraceCleanup.Store(0)
	cleanupTraces(c)
	for _, id := range []string{"active", "recent"} {
		if _, e := os.Stat(filepath.Join(oldDir, id+".json")); e != nil {
			t.Fatal("deleted live/recent request", id)
		}
	}
	if _, e := os.Stat(filepath.Join(oldDir, "expired.json")); !os.IsNotExist(e) {
		t.Fatal("expired record retained")
	}
}
