package main

import (
	"context"
	"encoding/json"
	"fmt"
)

func dispatch(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		c, err := parseConfig(req.ConfigYAML)
		if err != nil {
			return nil, err
		}
		currentConfig.Store(c)
		if method == "plugin.register" {
			lifecycleMu.Lock()
			shuttingDown = false
			lifecycleMu.Unlock()
		}
		return registration(), nil
	case "plugin.quiesce":
		// The host retires this instance from routing but keeps its callback
		// scope alive. Existing client streams must finish normally.
		return map[string]any{}, nil
	case "plugin.shutdown":
		shutdown()
		return map[string]any{}, nil
	case "executor.identifier":
		return map[string]string{"identifier": pluginID}, nil
	case "management.register":
		return managementRegistration(), nil
	case "management.handle":
		return handleManagement(raw)
	case "model.route":
		var req struct {
			RequestedModel string
			SourceFormat   string
			Stream         bool
			Metadata       map[string]any
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		c := currentConfig.Load()
		if c == nil || !c.Enabled || !c.AcceptNew || !req.Stream || req.SourceFormat != "codex" && req.SourceFormat != "openai-response" {
			return map[string]any{"Handled": false}, nil
		}
		auths, err := candidates(c, req.RequestedModel, true)
		if err != nil {
			return nil, err
		}
		if len(auths) == 0 {
			return map[string]any{"Handled": false}, nil
		}
		return map[string]any{"Handled": true, "TargetKind": "self", "Reason": "exclusive AnyRouter streaming model"}, nil
	case "executor.execute_stream":
		var req executorRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.StreamID == "" || req.CallbackID == "" {
			return nil, fmt.Errorf("missing host streaming callback IDs")
		}
		c := currentConfig.Load()
		if c == nil || !c.Enabled {
			return nil, fmt.Errorf("plugin disabled")
		}
		ctx, cancel := context.WithCancel(context.Background())
		r := &run{c: c, req: req, call: hostCall, ctx: ctx, cancel: cancel}
		if err := launch(r); err != nil {
			cancel()
			return nil, err
		}
		return map[string]any{"headers": map[string][]string{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}, "X-Accel-Buffering": {"no"}}}, nil
	default:
		return nil, fmt.Errorf("unsupported plugin method: %s", method)
	}
}
