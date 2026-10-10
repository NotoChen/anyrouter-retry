package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionLabelsOnlyEnrichKnownIdentities(t *testing.T) {
	c := defaultConfig()
	c.TraceDir = t.TempDir()
	if got := readSessionLabels(&c); got != nil {
		t.Fatal("missing index should be optional")
	}
	path := filepath.Join(c.TraceDir, "session-labels.json")
	os.WriteFile(path, []byte(`{"session":{"title":"会话标题","project":"BalanceHub"},"child":{"title":"线程标题","agent":"worker"},"bad":{"title":"unsafe\nname"}}`), 0600)
	labels := readSessionLabels(&c)
	r := requestTrace{SessionID: "session", ThreadID: "child", Model: "gpt-6-astra", Attempts: 7}
	enrichTrace(&r, labels)
	if r.SessionTitle != "会话标题" || r.Project != "BalanceHub" || r.ThreadTitle != "线程标题" || r.AgentName != "worker" {
		t.Fatalf("wrong labels: %+v", r)
	}
	if r.Model != "gpt-6-astra" || r.Attempts != 7 || r.SessionID != "session" {
		t.Fatal("label lookup changed execution facts")
	}
	if labels["bad"].Title != "" {
		t.Fatal("control characters accepted")
	}
	unknown := requestTrace{SessionID: "unknown"}
	enrichTrace(&unknown, labels)
	if unknown.SessionTitle != "" {
		t.Fatal("invented session title")
	}
	r.AgentName = "actual-agent"
	enrichTrace(&r, labels)
	if r.AgentName != "actual-agent" {
		t.Fatal("overwrote client supplied identity")
	}
	os.WriteFile(path, []byte(`invalid`), 0600)
	if readSessionLabels(&c) != nil {
		t.Fatal("corrupt index not ignored")
	}
}
