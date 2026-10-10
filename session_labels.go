package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Optional local title index. It enriches management views only; request routing
// and conversation payloads never depend on this file.
type sessionLabel struct {
	Title   string `json:"title"`
	Project string `json:"project"`
	Agent   string `json:"agent,omitempty"`
}

func readSessionLabels(c *Config) map[string]sessionLabel {
	path := filepath.Join(c.TraceDir, "session-labels.json")
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > 2<<20 {
		return nil
	}
	var labels map[string]sessionLabel
	if json.NewDecoder(f).Decode(&labels) != nil {
		return nil
	}
	for id, label := range labels {
		label.Title = safeIdentity(label.Title)
		label.Project = safeIdentity(label.Project)
		label.Agent = safeIdentity(label.Agent)
		labels[id] = label
	}
	return labels
}
func enrichTrace(t *requestTrace, labels map[string]sessionLabel) {
	s := labels[t.SessionID]
	t.SessionTitle, t.Project = s.Title, s.Project
	thread := labels[t.ThreadID]
	t.ThreadTitle = thread.Title
	if t.AgentName == "" || t.AgentName == "/root" {
		if thread.Agent != "" {
			t.AgentName = thread.Agent
		}
	}
}
