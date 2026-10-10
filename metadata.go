package main

var pluginVersion = "0.1.12"

func registration() any {
	fields := []map[string]string{
		{"Name": "trace_dir", "Type": "string", "Description": "Persistent session/request/attempt records; default /CLIProxyAPI/plugins/anyrouter-retry-data"},
		{"Name": "trace_retention_days", "Type": "integer", "Description": "Days to retain completed request traces, default 7; running requests are retained"},
		{"Name": "trace_max_bytes_per_request", "Type": "integer", "Description": "Maximum detailed attempt journal per request, default 33554432 bytes; counters continue after the limit"},
		{"Name": "accept_new_requests", "Type": "boolean", "Description": "Graceful on/off: false sends new requests to native CPA and lets existing requests finish; keep host enabled true"},
		{"Name": "config_path", "Type": "string", "Description": "CPA config path used to discover eligible existing credentials; no copied keys"},
		{"Name": "upstream_hosts", "Type": "array", "Description": "Exact AnyRouter upstream hostnames"},
		{"Name": "models", "Type": "array", "Description": "Exclusive AnyRouter models handled by this plugin"},
		{"Name": "max_attempts", "Type": "integer", "Description": "Total attempts per client request; 0 retries until success or cancellation"},
		{"Name": "heartbeat_interval", "Type": "string", "Description": "SSE keepalive interval, default 5s"},
		{"Name": "retry_delay", "Type": "string", "Description": "Delay between attempts; 0s means immediate retries"},
		{"Name": "max_sse_frame_bytes", "Type": "integer", "Description": "Maximum individual SSE frame bytes, default 67108864; not a total response limit"},
		{"Name": "max_bootstrap_bytes", "Type": "integer", "Description": "Maximum withheld handshake bytes only, default 8388608"},
		{"Name": "transient_codes", "Type": "array", "Description": "Retryable upstream error codes and types"},
	}
	// The ABI requires a nonempty repository field, even for private local
	// plugins. Mark it unpublished instead of claiming an external repository.
	return map[string]any{"schema_version": 6, "metadata": map[string]any{
		"Name": pluginID, "Version": pluginVersion, "Author": "NotoChen", "GitHubRepository": "https://github.com/NotoChen/anyrouter-retry", "ConfigFields": fields,
	}, "capabilities": map[string]any{"management_api": true, "model_router": true, "executor": true, "executor_model_scope": "static", "executor_input_formats": []string{"codex", "openai-response"}, "executor_output_formats": []string{"codex", "openai-response"}}}
}
