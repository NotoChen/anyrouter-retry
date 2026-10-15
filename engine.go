package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type hostCaller func(string, any, any) error

var hostCall hostCaller = nativeHostCall
var rotation atomic.Uint64
var active sync.Map
var lifecycleMu sync.Mutex
var workers sync.WaitGroup
var shuttingDown bool

func launch(r *run) error {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if shuttingDown {
		return fmt.Errorf("plugin is shutting down")
	}
	workers.Add(1)
	active.Store(r.req.StreamID, r)
	go func() {
		defer workers.Done()
		r.execute()
	}()
	return nil
}

func shutdown() {
	lifecycleMu.Lock()
	shuttingDown = true
	lifecycleMu.Unlock()
	stopAll()
	// The host frees its C callback table after this method returns. Do not
	// let async workers still call that table after it has been freed.
	workers.Wait()
}

type executorRequest struct {
	Model           string
	Format          string
	Stream          bool
	Alt             string
	Headers         http.Header
	Query           url.Values
	Payload         []byte
	OriginalRequest []byte
	SourceFormat    string
	Metadata        map[string]any
	StreamID        string `json:"stream_id"`
	CallbackID      string `json:"host_callback_id"`
}

type authEntry struct {
	ID       string `json:"id"`
	Index    string `json:"auth_index"`
	Provider string `json:"provider"`
	Disabled bool   `json:"disabled"`
	BaseURL  string `json:"base_url"`
	Name     string `json:"label"`
}

type modelRequest struct {
	CallbackID    string      `json:"host_callback_id"`
	EntryProtocol string      `json:"entry_protocol"`
	ExitProtocol  string      `json:"exit_protocol"`
	Model         string      `json:"model"`
	Stream        bool        `json:"stream"`
	Body          []byte      `json:"body"`
	Headers       http.Header `json:"headers"`
	Query         url.Values  `json:"query"`
	Alt           string      `json:"alt"`
	Provider      string      `json:"forced_provider"`
	AuthID        string      `json:"auth_id"`
}

type modelStream struct {
	Status  int         `json:"status_code"`
	ID      string      `json:"stream_id"`
	Headers http.Header `json:"headers"`
}
type modelChunk struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

type run struct {
	c         *Config
	req       executorRequest
	call      hostCaller
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	emitMu    sync.Mutex
	upstream  string
	committed bool
	completed bool
	trace     *requestTrace
}

func stopAll() { active.Range(func(_, v any) bool { v.(*run).stop(); return true }) }
func (r *run) stop() {
	r.cancel()
	r.mu.Lock()
	id := r.upstream
	r.upstream = ""
	r.mu.Unlock()
	if id != "" {
		_ = r.call("host.model.stream_close", map[string]any{"stream_id": id}, nil)
	}
}

func (r *run) emit(p []byte) error {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	err := r.call("host.stream.emit", map[string]any{"stream_id": r.req.StreamID, "payload": p}, nil)
	if err != nil {
		r.stop()
	}
	return err
}

// A data event is required: the CPA Responses handler retains SSE comments
// until the first data frame. Codex ignores this unknown event type.
var heartbeat = []byte("data: {\"type\":\"keepalive\"}\n\n")

func (r *run) heartbeats() {
	t := time.NewTicker(r.c.heartbeat)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			if r.emit(heartbeat) != nil {
				return
			}
		}
	}
}

func (r *run) log(attempt int, kind string) {
	if (kind == "retry" || kind == "runtime_sync") && attempt > 3 && attempt%25 != 0 {
		return
	}
	_ = r.call("host.log", map[string]any{"host_callback_id": r.req.CallbackID, "level": "info", "message": fmt.Sprintf("anyrouter-retry %s attempt=%d", kind, attempt), "fields": map[string]any{"model": r.req.Model, "stream_id": r.req.StreamID}}, nil)
}

func (r *run) discover() ([]authEntry, error) {
	configured, err := candidates(r.c, r.req.Model, true)
	if err != nil {
		return nil, err
	}
	var runtime struct {
		Files []authEntry `json:"files"`
	}
	if err = r.call("host.auth.list", map[string]any{}, &runtime); err != nil {
		return nil, err
	}
	var out []authEntry
	for _, c := range configured {
		// v8.0.22 host.auth.list omits API-key records without runtime_only
		// or a backing file. Derive their indexes from the same CPA contract,
		// while respecting a runtime entry when the host does expose one.
		a := authEntry{ID: c.ID, Index: c.Index, Provider: "codex", Name: c.DisplayName}
		for _, live := range runtime.Files {
			if live.ID == c.ID {
				a = live
				if a.Name == "" {
					a.Name = c.DisplayName
				}
				break
			}
		}
		if !a.Disabled {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no enabled eligible AnyRouter credential")
	}
	return out, nil
}

const (
	// CPA's provider watcher normally applies a config save quickly, but a
	// request can observe the file in the middle of an atomic/UI write.  This
	// small bounded retry is only used while discovering credentials; ordinary
	// upstream retries remain immediate when retry_delay is 0s.
	discoveryRetryCount = 20
	discoveryRetryDelay = 25 * time.Millisecond
)

func isTransientDiscoveryError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, text := range []string{
		"cannot read cpa config",
		"cannot parse cpa config",
		"cannot parse grouped codex config",
		"invalid codex group",
		"invalid codex key",
		"anyrouter credentials must set request-retry",
	} {
		if strings.Contains(s, text) {
			return true
		}
	}
	return false
}

func isNoEligibleCredentialError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no enabled eligible anyrouter credential")
}

func waitForDiscovery(ctx context.Context) error {
	t := time.NewTimer(discoveryRetryDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *run) discoverStable() ([]authEntry, error) {
	var last error
	for i := 0; i < discoveryRetryCount; i++ {
		entries, err := r.discover()
		if err == nil {
			return entries, nil
		}
		last = err
		if !isTransientDiscoveryError(err) || i == discoveryRetryCount-1 {
			return nil, err
		}
		if err = waitForDiscovery(r.ctx); err != nil {
			return nil, err
		}
	}
	return nil, last
}

func isAuthMissingError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "auth not found") || strings.Contains(s, "auth index not found") || strings.Contains(s, "auth_index not found") {
		return true
	}
	var rpc *rpcError
	if errors.As(err, &rpc) {
		code := strings.ToLower(rpc.Code)
		if strings.Contains(code, "auth") && strings.Contains(code, "not_found") {
			return true
		}
	}
	return false
}

func (r *run) refreshPool(pool *candidatePool) error {
	entries, err := r.discoverStable()
	if err != nil && isNoEligibleCredentialError(err) && len(pool.entries) > 0 {
		// A UI save can briefly expose a complete YAML document with no
		// compliant request-retry entry before the final provider state is
		// written.  Give the watcher one short generation to settle; a second
		// empty result still means the user removed every eligible credential.
		if waitErr := waitForDiscovery(r.ctx); waitErr != nil {
			return waitErr
		}
		entries, err = r.discoverStable()
	}
	if err != nil {
		return err
	}
	pool.refresh(entries)
	return nil
}

// configFingerprint is intentionally small because it is checked before every
// retry. The full CPA config is parsed only when this fingerprint changes.
// Size plus nanosecond mtime also catches the atomic replace used by CPAMA's
// config writer without adding a read or YAML parse to the hot retry path.
type configFingerprint struct {
	size    int64
	modTime int64
}

func currentConfigFingerprint(path string) (configFingerprint, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return configFingerprint{}, false
	}
	return configFingerprint{size: info.Size(), modTime: info.ModTime().UnixNano()}, true
}

// mergeNewCandidates observes a config save between upstream attempts. It is
// deliberately additive: deletions and API-key replacements remain in the
// request-local pool until CPA rejects their old auth identity, at which point
// refreshPool performs the authoritative removal. This keeps a config reorder
// from changing the already-running request's rotation order.
func (r *run) mergeNewCandidates(pool *candidatePool, last *configFingerprint, initialized *bool) error {
	fingerprint, ok := currentConfigFingerprint(r.c.ConfigPath)
	if !ok || *initialized && fingerprint == *last {
		return nil
	}
	entries, err := r.discoverStable()
	if err != nil {
		// Keep the current pool during an in-progress or temporarily incomplete
		// UI save. The next attempt will retry discovery because the last good
		// fingerprint remains unchanged.
		return err
	}
	pool.mergeAdditions(entries)
	*last = fingerprint
	*initialized = true
	return nil
}

func (r *run) execute() {
	r.trace = newRequestTrace(r.c, r.req)
	defer active.Delete(r.req.StreamID)
	var heartbeatDone chan struct{}
	var final error
	defer func() {
		if recover() != nil {
			final = fmt.Errorf("anyrouter-retry internal stream error")
		}
		r.trace.finish(r.completed, r.committed, r.ctx.Err() != nil, final)
		r.stop()
		if heartbeatDone != nil {
			<-heartbeatDone
		}
		msg := ""
		if final != nil && !errors.Is(final, context.Canceled) {
			msg = final.Error()
		}
		_ = r.call("host.stream.close", map[string]any{"stream_id": r.req.StreamID, "error": msg}, nil)
	}()
	if r.emit(heartbeat) != nil {
		return
	}
	heartbeatDone = make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		r.heartbeats()
	}()
	entries, err := r.discoverStable()
	if err != nil {
		final = err
		return
	}
	pool := newCandidatePool(entries)
	poolFingerprint, havePoolFingerprint := currentConfigFingerprint(r.c.ConfigPath)
	poolFingerprintInitialized := havePoolFingerprint
	blocked := map[string]bool{}
	// A reset/execute callback can race with CPA's asynchronous AuthManager
	// refresh.  These IDs are skipped for the current discovery generation;
	// when all candidates are in this state we wait briefly, rediscover, and
	// try the surviving/new set again.
	syncUnavailable := map[string]bool{}
	for attempt := 1; r.c.MaxAttempts == 0 || attempt <= r.c.MaxAttempts; {
		if r.ctx.Err() != nil {
			return
		}
		// A new supplier saved while this request is still waiting for a
		// generation can join the subsequent rotation. This check happens only
		// between upstream attempts; once output has been committed, the request
		// is never replayed and the loop exits below.
		if syncErr := r.mergeNewCandidates(pool, &poolFingerprint, &poolFingerprintInitialized); syncErr != nil {
			if !isTransientDiscoveryError(syncErr) && !isNoEligibleCredentialError(syncErr) {
				final = syncErr
				return
			}
		}
		skip := make(map[string]bool, len(blocked)+len(syncUnavailable))
		for id := range blocked {
			skip[id] = true
		}
		for id := range syncUnavailable {
			skip[id] = true
		}
		chosen, ok := pool.next(skip)
		if !ok && len(syncUnavailable) > 0 {
			// All currently visible candidates are waiting for CPA's runtime
			// AuthManager to catch up with the config file.
			if err = waitForDiscovery(r.ctx); err != nil {
				return
			}
			if err = r.refreshPool(pool); err != nil {
				final = err
				return
			}
			syncUnavailable = map[string]bool{}
			continue
		}
		if !ok {
			final = fmt.Errorf("all eligible AnyRouter credentials rejected authentication or model access")
			return
		}
		// Reset only this transient routing state so native cooldown cannot
		// turn subsequent pinned attempts into busy-spin auth_unavailable errors.
		if err = r.call("host.routing.reset_cooldown", map[string]any{"auth_index": chosen.Index}, nil); err != nil {
			if isAuthMissingError(err) {
				syncUnavailable[chosen.ID] = true
				if err = r.refreshPool(pool); err != nil {
					final = err
					return
				}
				continue
			}
			final = fmt.Errorf("cannot reset AnyRouter retry state")
			return
		}
		// A different credential becoming usable is evidence that the host
		// runtime has progressed.  Give previously missing IDs another chance
		// on the next round instead of starving them behind one surviving
		// credential that keeps returning transient upstream errors.
		if len(syncUnavailable) > 0 {
			syncUnavailable = map[string]bool{}
		}
		r.trace.startAttempt(chosen, attempt)
		err = r.attempt(chosen)
		if r.completed {
			r.trace.finishAttempt(r.c, "completed", nil)
			r.log(attempt, "completed")
			return
		}
		if r.ctx.Err() != nil {
			r.trace.finishAttempt(r.c, "cancelled", r.ctx.Err())
			return
		}
		if r.committed {
			r.trace.finishAttempt(r.c, "interrupted", err)
			final = err
			r.log(attempt, "interrupted_after_output")
			return
		}
		if isAuthMissingError(err) {
			r.trace.finishAttempt(r.c, "runtime_sync", err)
			r.log(attempt, "runtime_sync")
			syncUnavailable[chosen.ID] = true
			attempt++
			if err = r.refreshPool(pool); err != nil {
				final = err
				return
			}
			continue
		}
		status := 0
		var rpc *rpcError
		if errors.As(err, &rpc) {
			status = rpc.HTTPStatus
		}
		retry, credentialFault := classify(r.c, status, errorText(err))
		if credentialFault {
			blocked[chosen.ID] = true
			retry = true
		}
		if !retry {
			r.trace.finishAttempt(r.c, "permanent_error", err)
			final = err
			r.log(attempt, "permanent_error")
			return
		}
		outcome := "retry"
		if credentialFault {
			outcome = "credential_rejected"
		}
		r.trace.finishAttempt(r.c, outcome, err)
		r.log(attempt, "retry")
		if r.c.delay > 0 {
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(r.c.delay):
			}
		}
		attempt++
	}
	final = fmt.Errorf("anyrouter-retry max_attempts exhausted before generation")
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (r *run) attempt(a authEntry) error {
	format := r.req.SourceFormat
	if format == "" {
		format = r.req.Format
	}
	if format == "" {
		format = "codex"
	}
	body := r.req.OriginalRequest
	if len(body) == 0 {
		body = r.req.Payload
	}
	req := modelRequest{CallbackID: r.req.CallbackID, EntryProtocol: format, ExitProtocol: format, Model: r.req.Model, Stream: true, Body: body, Headers: r.req.Headers, Query: r.req.Query, Alt: r.req.Alt, Provider: "codex", AuthID: a.ID}
	var stream modelStream
	opened := time.Now()
	openErr := r.call("host.model.execute_stream", req, &stream)
	if r.trace != nil && r.trace.LastAttempt != nil {
		r.trace.LastAttempt.HostOpenMS = time.Since(opened).Milliseconds()
	}
	if openErr != nil {
		return openErr
	}
	if stream.ID == "" {
		return fmt.Errorf("host returned empty upstream stream")
	}
	if r.trace != nil && r.trace.LastAttempt != nil {
		r.trace.LastAttempt.HostOpenedAt = time.Now().UnixMilli()
		r.trace.LastAttempt.HTTPStatus = stream.Status
		for _, key := range []string{"X-Request-Id", "Request-Id", "X-Oneapi-Request-Id"} {
			if id := headerValue(stream.Headers, key); id != "" {
				r.trace.LastAttempt.UpstreamTrace = id
				break
			}
		}
		r.trace.save()
	}
	r.mu.Lock()
	r.upstream = stream.ID
	r.mu.Unlock()
	defer func() {
		_ = r.call("host.model.stream_close", map[string]any{"stream_id": stream.ID}, nil)
		r.mu.Lock()
		if r.upstream == stream.ID {
			r.upstream = ""
		}
		r.mu.Unlock()
	}()
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	if stream.Status >= 400 {
		return &rpcError{HTTPStatus: stream.Status, Message: fmt.Sprintf("upstream HTTP %d", stream.Status)}
	}
	var pending, bootstrap []byte
	for {
		var chunk modelChunk
		if err := r.call("host.model.stream_read", map[string]any{"stream_id": stream.ID}, &chunk); err != nil {
			return err
		}
		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}
		if len(chunk.Payload) > 0 && r.trace != nil && r.trace.LastAttempt != nil && r.trace.LastAttempt.FirstHostFrameAt == 0 {
			r.trace.LastAttempt.FirstHostFrameAt = time.Now().UnixMilli()
		}
		// Normalize CRLF only after a full line is present (including split CRLF).
		// CPA scans upstream SSE into separate chunks and removes each line's
		// newline. Preserve event:/data: boundaries when the next field arrives;
		// otherwise "event: ...data: {...}" silently loses the actual event.
		incoming := bytes.TrimSpace(chunk.Payload)
		if len(pending) > 0 && !bytes.HasSuffix(pending, []byte("\n")) && !dataStringOpen(pending) && (bytes.HasPrefix(incoming, []byte("data:")) || bytes.HasPrefix(incoming, []byte("event:")) || bytes.HasPrefix(incoming, []byte("id:")) || bytes.HasPrefix(incoming, []byte("retry:"))) {
			pending = append(pending, '\n')
		}
		pending = append(pending, chunk.Payload...)
		// The native Codex executor scans upstream line by line and its
		// host model callback can return a complete data: JSON line without
		// any delimiter. Accept that contract as well as fragmented SSE.
		trimmed := bytes.TrimSpace(pending)
		if !bytes.Contains(pending, []byte("\n\n")) && !bytes.Contains(pending, []byte("\r\n\r\n")) {
			var values [][]byte
			for _, line := range bytes.Split(trimmed, []byte("\n")) {
				line = bytes.TrimSuffix(line, []byte("\r"))
				if bytes.HasPrefix(line, []byte("data:")) {
					values = append(values, bytes.TrimSpace(line[5:]))
				}
			}
			value := bytes.Join(values, []byte("\n"))
			if len(values) > 0 && (json.Valid(value) || bytes.Equal(value, []byte("[DONE]"))) {
				pending = append(append([]byte(nil), trimmed...), '\n', '\n')
			}
		} else if len(trimmed) == 0 {
			pending = nil
		}
		for {
			end := bytes.Index(pending, []byte("\n\n"))
			width := 2
			if cr := bytes.Index(pending, []byte("\r\n\r\n")); cr >= 0 && (end < 0 || cr < end) {
				end = cr
				width = 4
			}
			if end < 0 {
				break
			}
			if end+width > r.c.MaxFrame {
				return fmt.Errorf("SSE frame exceeds max_sse_frame_bytes")
			}
			frame := append([]byte(nil), pending[:end+width]...)
			pending = pending[end+width:]
			parseStart := time.Now()
			kind, failure, err := parseFrame(frame)
			if r.trace != nil && r.trace.LastAttempt != nil {
				r.trace.LastAttempt.ParseMicros += time.Since(parseStart).Microseconds()
			}
			if err != nil {
				return err
			}
			if failure != "" {
				return &rpcError{Message: failure}
			}
			if kind == "" || kind == "keepalive" {
				continue
			}
			if !r.committed && isHandshake(kind) {
				bootstrap = append(bootstrap, frame...)
				if len(bootstrap) > r.c.MaxBuffer {
					return fmt.Errorf("bootstrap exceeds max_bootstrap_bytes")
				}
				continue
			}
			first := !r.committed
			// Limit only withheld handshake bytes. The first content event is
			// forwarded independently, even when larger than that small buffer.
			r.committed = true
			emitStart := time.Now()
			if len(bootstrap) > 0 {
				if err = r.emit(bootstrap); err != nil {
					return err
				}
				bootstrap = nil
			}
			if err = r.emit(frame); err != nil {
				return err
			}
			if r.trace != nil && r.trace.LastAttempt != nil {
				r.trace.LastAttempt.EmitMicros += time.Since(emitStart).Microseconds()
				if first {
					r.trace.LastAttempt.FirstForwardedAt = time.Now().UnixMilli()
					r.trace.output()
				}
			}
			if kind == "response.completed" {
				r.completed = true
				return nil
			}
		}
		if len(pending) > r.c.MaxFrame {
			return fmt.Errorf("SSE frame exceeds max_sse_frame_bytes")
		}
		if chunk.Error != "" {
			return &rpcError{Message: chunk.Error}
		}
		if chunk.Done {
			return &rpcError{Message: "stream closed before response.completed"}
		}
	}
}

func isHandshake(kind string) bool {
	switch kind {
	case "response.created", "response.in_progress", "response.queued", "response.metadata", "codex.response.metadata", "responsesapi.websocket_timing":
		return true
	}
	return false
}

// A fragmented JSON string may begin with the literal text "data:" or "id:".
// It is payload text, not the next undelimited SSE field from CPA's scanner.
func dataStringOpen(pending []byte) bool {
	inString, escaped := false, false
	for _, line := range bytes.Split(pending, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		for _, ch := range line[5:] {
			if escaped {
				escaped = false
				continue
			}
			if inString && ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = !inString
			}
		}
	}
	return inString
}

func parseFrame(frame []byte) (string, string, error) {
	var data []string
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, strings.TrimPrefix(string(line[5:]), " "))
		}
	}
	if len(data) == 0 {
		return "", "", nil
	}
	payload := strings.Join(data, "\n")
	if payload == "[DONE]" {
		return "", "", nil
	}
	var event struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(payload), &event) != nil {
		return "", "", fmt.Errorf("invalid upstream SSE JSON")
	}
	if event.Type == "error" || event.Type == "response.failed" || event.Type == "response.incomplete" || len(event.Error) > 0 && string(event.Error) != "null" {
		return event.Type, payload, nil
	}
	if event.Type == "" {
		return "", "", fmt.Errorf("upstream SSE event missing type")
	}
	return event.Type, "", nil
}

var statusPattern = regexp.MustCompile(`(?i)(?:status(?: code)?|http)[ :]+([45][0-9]{2})\b`)

// Request defects win over HTTP classification; retries cannot repair history.
func classify(c *Config, status int, message string) (retry, credentialFault bool) {
	s := strings.ToLower(message)
	for _, code := range []string{"thinking_signature_invalid", "invalid_request_error", "context_length_exceeded", "invalid_argument", "invalid_json", "unsupported_parameter"} {
		if strings.Contains(s, code) {
			return false, false
		}
	}
	if status == 0 {
		if m := statusPattern.FindStringSubmatch(s); len(m) > 1 {
			fmt.Sscanf(m[1], "%d", &status)
		}
	}
	if status == 401 || status == 403 || status == 404 || strings.Contains(s, "invalid_api_key") || strings.Contains(s, "model_not_found") || strings.Contains(s, "no auth available") || strings.Contains(s, "no auth candidates") {
		return false, true
	}
	for _, code := range c.TransientCodes {
		if code != "" && strings.Contains(s, strings.ToLower(code)) {
			return true, false
		}
	}
	switch status {
	case 408, 425, 429:
		return true, false
	case 400, 501, 505:
		return false, false
	}
	if status >= 500 && status <= 599 {
		return true, false
	}
	for _, text := range []string{"bad gateway", "service unavailable", "gateway timeout", "too many requests"} {
		if strings.Contains(s, text) {
			return true, false
		}
	}
	for _, code := range []string{"stream closed before response.completed", "unexpected eof", "connection reset", "connection refused", "broken pipe", "tls handshake timeout", "i/o timeout", "server closed idle connection", "temporary failure in name resolution"} {
		if strings.Contains(s, code) {
			return true, false
		}
	}
	return false, false
}
