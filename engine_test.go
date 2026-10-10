package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	c, _ := parseConfig(nil)
	c.ConfigPath = filepath.Join(t.TempDir(), "config.yaml")
	c.TraceDir = filepath.Join(t.TempDir(), "traces")
	data := `codex-api-key:
  - api-key: dummy
    base-url: https://anyrouter.top/v1
    request-retry: 0
    models: [{name: gpt-6-astra}]
  - api-key: excluded
    base-url: https://anyrouter.top/v1
    request-retry: 0
    models: [{name: gpt-6-astra}]
    excluded-models: ['*']
`
	if err := os.WriteFile(c.ConfigPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return c
}

type mockHost struct {
	mu          sync.Mutex
	t           *testing.T
	auth        []authEntry
	attempt     int
	read        int
	failures    int
	fail        string
	partial     bool
	undelimited bool
	eventLines  bool
	output      strings.Builder
	closed      string
	blocking    bool
	block       chan struct{}
	closeOnce   sync.Once
	pause       chan struct{}
	chunks      [][]byte
}

func assign(out, v any) error { raw, _ := json.Marshal(v); return json.Unmarshal(raw, out) }
func event(kind, extra string) []byte {
	return []byte(fmt.Sprintf("data: {\"type\":%q%s}\n\n", kind, extra))
}

func (m *mockHost) call(method string, p any, out any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch method {
	case "host.auth.list":
		return assign(out, map[string]any{"files": m.auth})
	case "host.routing.reset_cooldown", "host.log":
		return nil
	case "host.stream.emit":
		m.output.Write(p.(map[string]any)["payload"].([]byte))
		return nil
	case "host.stream.close":
		m.closed = p.(map[string]any)["error"].(string)
		return nil
	case "host.model.execute_stream":
		m.attempt++
		m.read = 0
		return assign(out, modelStream{Status: 200, ID: "upstream"})
	case "host.model.stream_close":
		if m.blocking {
			m.closeOnce.Do(func() { close(m.block) })
		}
		return nil
	case "host.model.stream_read":
		if m.chunks != nil {
			if m.read >= len(m.chunks) {
				return assign(out, modelChunk{Done: true})
			}
			payload := m.chunks[m.read]
			m.read++
			return assign(out, modelChunk{Payload: payload})
		}
		if m.pause != nil {
			p := m.pause
			m.pause = nil
			m.mu.Unlock()
			<-p
			m.mu.Lock()
		}
		if m.blocking {
			m.mu.Unlock()
			<-m.block
			m.mu.Lock()
			return assign(out, modelChunk{Done: true})
		}
		if m.eventLines && m.read%2 == 0 {
			m.read++
			return assign(out, modelChunk{Payload: []byte("event: response.event")})
		}
		m.read++
		read := m.read
		if m.eventLines {
			read /= 2
		}
		var payload []byte
		if read == 1 {
			payload = event("response.created", fmt.Sprintf(",\"response\":{\"id\":\"attempt-%d\"}", m.attempt))
		} else if read == 2 && m.partial {
			payload = event("response.output_text.delta", ",\"delta\":\"partial\"")
		} else if m.attempt <= m.failures {
			payload = []byte("data: " + m.fail + "\n\n")
		} else if read == 2 {
			payload = event("response.output_text.delta", ",\"delta\":\"success\"")
		} else {
			payload = event("response.completed", ",\"response\":{\"id\":\"success\"}")
		}
		if m.undelimited {
			payload = []byte(strings.TrimSpace(string(payload)))
		}
		return assign(out, modelChunk{Payload: payload})
	}
	return fmt.Errorf("unexpected callback %s", method)
}

func setupRun(t *testing.T, c *Config, m *mockHost) *run {
	auths, err := candidates(c, "gpt-6-astra", true)
	if err != nil || len(auths) != 1 {
		t.Fatalf("candidates: %d %v", len(auths), err)
	}
	m.t = t
	m.auth = []authEntry{{ID: auths[0].ID, Index: "index", Provider: "codex"}}
	ctx, cancel := context.WithCancel(context.Background())
	return &run{c: c, ctx: ctx, cancel: cancel, req: executorRequest{Model: "gpt-6-astra", StreamID: "outer", CallbackID: "cb"}, call: m.call}
}

func TestMoreThan100RetriesOneStream(t *testing.T) {
	c := testConfig(t)
	m := &mockHost{failures: 110, fail: `{"type":"error","error":{"type":"too_many_requests","code":"rate_limit_reached"}}`}
	r := setupRun(t, c, m)
	r.execute()
	if m.attempt != 111 || !r.completed || m.closed != "" {
		t.Fatalf("attempts=%d completed=%v error=%s", m.attempt, r.completed, m.closed)
	}
	s := m.output.String()
	if strings.Contains(s, "rate_limit") || strings.Contains(s, "attempt-110\"") || strings.Count(s, "response.created") != 1 || strings.Count(s, "response.completed") != 1 {
		t.Fatal("failed attempt escaped into downstream")
	}
}

func TestNativeHostUndelimitedFrames(t *testing.T) {
	c := testConfig(t)
	m := &mockHost{failures: 2, fail: `{"type":"error","error":{"code":"rate_limit_reached"}}`, undelimited: true}
	r := setupRun(t, c, m)
	r.execute()
	if !r.completed || m.attempt != 3 || strings.Count(m.output.String(), "response.created") != 1 {
		t.Fatal("native undelimited frames not handled")
	}
}

func TestNativeHostSeparateEventAndDataLines(t *testing.T) {
	c := testConfig(t)
	c.MaxAttempts = 4
	m := &mockHost{failures: 1, fail: `{"type":"error","error":{"code":"rate_limit_reached"}}`, undelimited: true, eventLines: true}
	r := setupRun(t, c, m)
	r.execute()
	if !r.completed || m.attempt != 2 || m.closed != "" || strings.Count(m.output.String(), "response.created") != 1 || strings.Count(m.output.String(), "response.completed") != 1 || strings.Contains(m.output.String(), "rate_limit_reached") {
		t.Fatalf("native event/data lines lost: attempts=%d completed=%v error=%s output=%s", m.attempt, r.completed, m.closed, m.output.String())
	}
}

func TestPermanentAndPartialDoNotReplay(t *testing.T) {
	for _, tc := range []struct {
		name, err string
		partial   bool
	}{
		{"invalid-history", `{"type":"response.failed","response":{"error":{"code":"thinking_signature_invalid"}}}`, false},
		{"partial-output", `{"type":"error","error":{"code":"rate_limit_reached"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			m := &mockHost{failures: 4, fail: tc.err, partial: tc.partial}
			r := setupRun(t, c, m)
			r.execute()
			if m.attempt != 1 || m.closed == "" || r.completed {
				t.Fatal("unexpected replay or false success")
			}
		})
	}
}

func TestCancellationClosesBlockedUpstream(t *testing.T) {
	c := testConfig(t)
	c.heartbeat = 100 * time.Millisecond
	m := &mockHost{blocking: true, block: make(chan struct{})}
	r := setupRun(t, c, m)
	done := make(chan struct{})
	go func() { r.execute(); close(done) }()
	deadline := time.After(time.Second)
	for {
		r.mu.Lock()
		ready := r.upstream != ""
		r.mu.Unlock()
		if ready {
			break
		}
		select {
		case <-deadline:
			t.Fatal("upstream did not open")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	r.stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation leaked stream worker")
	}
	if m.attempt != 1 {
		t.Fatal("retried after cancellation")
	}
}

func TestQuiescePreservesWaitingStream(t *testing.T) {
	c := testConfig(t)
	pause := make(chan struct{})
	m := &mockHost{pause: pause, failures: 2, fail: `{"type":"error","error":{"code":"rate_limit_reached"}}`}
	r := setupRun(t, c, m)
	active.Store(r.req.StreamID, r)
	done := make(chan struct{})
	go func() { r.execute(); close(done) }()
	deadline := time.After(time.Second)
	for {
		r.mu.Lock()
		ready := r.upstream != ""
		r.mu.Unlock()
		if ready {
			break
		}
		select {
		case <-deadline:
			t.Fatal("upstream did not open")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	_, err := dispatch("plugin.quiesce", []byte(`{}`))
	if err != nil || r.ctx.Err() != nil {
		t.Fatal("quiesce canceled waiting client stream")
	}
	close(pause)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retired stream did not complete")
	}
	if !r.completed || m.attempt != 3 || m.closed != "" {
		t.Fatal("retired stream failed to retry and complete")
	}
}

func TestClassification(t *testing.T) {
	c := defaultConfig()
	for _, status := range []int{408, 425, 429, 500, 502, 503, 504, 520, 521, 522, 523, 524, 529} {
		yes, _ := classify(&c, status, "temporary upstream error")
		if !yes {
			t.Errorf("HTTP %d not retried", status)
		}
	}
	for _, code := range []string{"rate_limit_reached", "rate_limit_exceeded", "get_channel_failed", "too_many_requests"} {
		yes, _ := classify(&c, 0, code)
		if !yes {
			t.Errorf("%s not retried", code)
		}
	}
	yes, _ := classify(&c, 502, "thinking_signature_invalid")
	if yes {
		t.Fatal("retrying invalid history")
	}
	yes, cred := classify(&c, 401, "unauthorized")
	if yes || !cred {
		t.Fatal("authentication classification")
	}
}

func TestFrameCRLFAndMultiline(t *testing.T) {
	k, f, e := parseFrame([]byte("event: response.created\r\ndata: {\r\ndata: \"type\": \"response.created\"}\r\n\r\n"))
	if e != nil || k != "response.created" || f != "" {
		t.Fatalf("%s %s %v", k, f, e)
	}
}

func TestMixedSupplierRouteFallsThrough(t *testing.T) {
	c := testConfig(t)
	f, _ := os.OpenFile(c.ConfigPath, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("  - api-key: other\n    base-url: https://other.invalid/v1\n    models: [{name: gpt-6-astra}]\n")
	f.Close()
	a, e := candidates(c, "gpt-6-astra", true)
	if e != nil || len(a) != 0 {
		t.Fatal("router hijacked mixed supplier model")
	}
}

func TestAttemptLimit(t *testing.T) {
	c := testConfig(t)
	c.MaxAttempts = 3
	m := &mockHost{failures: 100, fail: `{"type":"error","error":{"code":"rate_limit_reached"}}`}
	r := setupRun(t, c, m)
	r.execute()
	if m.attempt != 3 || !strings.Contains(m.closed, "max_attempts") {
		t.Fatal("limit not enforced")
	}
}

func TestGracefulPauseKeepsAcceptedRequestAndResumesRouting(t *testing.T) {
	c := testConfig(t)
	currentConfig.Store(c)
	defer currentConfig.Store(nil)
	request := []byte(`{"RequestedModel":"gpt-6-astra","SourceFormat":"openai-response","Stream":true}`)
	checkRoute := func(want bool) {
		t.Helper()
		v, err := dispatch("model.route", request)
		if err != nil || v.(map[string]any)["Handled"] != want {
			t.Fatalf("route=%v error=%v want=%v", v, err, want)
		}
	}
	checkRoute(true)
	pause := make(chan struct{})
	m := &mockHost{pause: pause, failures: 2, fail: `{"type":"error","error":{"code":"rate_limit_reached"}}`}
	r := setupRun(t, c, m)
	active.Store(r.req.StreamID, r)
	done := make(chan struct{})
	go func() { r.execute(); close(done) }()
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		ready := r.upstream != ""
		r.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upstream not open")
		}
		time.Sleep(time.Millisecond)
	}
	next := *c
	next.AcceptNew = false
	next.MaxAttempts = 1
	currentConfig.Store(&next)
	checkRoute(false)
	if r.ctx.Err() != nil {
		t.Fatal("pause canceled accepted stream")
	}
	close(pause)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old stream did not finish")
	}
	if !r.completed || m.attempt != 3 {
		t.Fatal("configuration change affected accepted request")
	}
	currentConfig.Store(c)
	checkRoute(true)
}

func TestFullFrameSizeLimit(t *testing.T) {
	c := testConfig(t)
	c.MaxFrame = 1024
	c.MaxAttempts = 2
	m := &mockHost{chunks: [][]byte{event("response.output_text.delta", `,"delta":"`+strings.Repeat("x", 2048)+`"`)}}
	r := setupRun(t, c, m)
	r.execute()
	if r.completed || m.attempt != 1 || !strings.Contains(m.closed, "exceeds") || strings.Contains(m.output.String(), "output_text") {
		t.Fatal("oversized complete frame bypassed limit")
	}
}

func TestFragmentedSSEIncludingDataLikeText(t *testing.T) {
	frames := string(event("response.created", "")) + string(event("response.output_text.delta", `,"delta":"data: test"`)) + string(event("response.completed", ""))
	for split := 1; split < len(frames); split++ {
		c := testConfig(t)
		c.MaxAttempts = 1
		m := &mockHost{chunks: [][]byte{[]byte(frames[:split]), []byte(frames[split:])}}
		r := setupRun(t, c, m)
		r.execute()
		if !r.completed || m.attempt != 1 {
			t.Fatalf("valid fragmented SSE lost at byte %d: %s", split, m.closed)
		}
	}
}

func TestShutdownJoinsWorkersBeforeCallbackTableIsFreed(t *testing.T) {
	c := testConfig(t)
	m := &mockHost{blocking: true, block: make(chan struct{})}
	r := setupRun(t, c, m)
	lifecycleMu.Lock()
	shuttingDown = false
	lifecycleMu.Unlock()
	defer func() {
		lifecycleMu.Lock()
		shuttingDown = false
		lifecycleMu.Unlock()
	}()
	if err := launch(r); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		ready := r.upstream != ""
		r.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upstream not open")
		}
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() { shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown leaked worker")
	}
	if _, stillActive := active.Load(r.req.StreamID); stillActive || m.attempt != 1 {
		t.Fatal("shutdown returned before worker completed")
	}
	if err := launch(r); err == nil {
		t.Fatal("accepted work during shutdown")
	}
}

func TestFirstContentCanExceedHandshakeBuffer(t *testing.T) {
	c := testConfig(t)
	c.MaxBuffer = 1024
	c.MaxFrame = 16384
	m := &mockHost{chunks: [][]byte{event("response.created", ""), event("response.output_text.delta", `,"delta":"`+strings.Repeat("x", 8192)+`"`), event("response.completed", "")}}
	r := setupRun(t, c, m)
	r.execute()
	if !r.completed || m.attempt != 1 || !strings.Contains(m.output.String(), strings.Repeat("x", 8192)) {
		t.Fatal("first content incorrectly limited by handshake budget")
	}
	if r.trace.LastAttempt.FirstForwardedAt == 0 || r.trace.LastAttempt.FirstHostFrameAt == 0 {
		t.Fatal("missing first frame/forward timing")
	}
}

func TestTotalStreamIsNotLimitedByFrameBudget(t *testing.T) {
	c := testConfig(t)
	c.MaxFrame = 1024
	c.MaxBuffer = 1024
	frames := [][]byte{event("response.created", "")}
	for i := 0; i < 30; i++ {
		frames = append(frames, event("response.output_text.delta", `,"delta":"`+strings.Repeat("y", 500)+`"`))
	}
	frames = append(frames, event("response.completed", ""))
	m := &mockHost{chunks: frames}
	r := setupRun(t, c, m)
	r.execute()
	if !r.completed || len(m.output.String()) < 10000 {
		t.Fatal("limited total streamed answer instead of individual frame")
	}
}
