package main

import "testing"

func entry(id string) authEntry { return authEntry{ID: id, Index: id, Name: id} }

func poolIDs(p *candidatePool) []string {
	ids := make([]string, 0, len(p.entries))
	for _, entry := range p.entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

func TestCandidatePoolPreservesCursorWhenConfigReorders(t *testing.T) {
	rotation.Store(0)
	p := newCandidatePool([]authEntry{entry("A"), entry("B"), entry("C")})
	first, ok := p.next(nil)
	if !ok || first.ID != "A" {
		t.Fatalf("first=%+v ok=%v", first, ok)
	}
	p.refresh([]authEntry{entry("C"), entry("A"), entry("D"), entry("B")})
	if got := poolIDs(p); len(got) != 4 || got[0] != "A" || got[1] != "B" || got[2] != "C" || got[3] != "D" {
		t.Fatalf("merged order=%v", got)
	}
	next, ok := p.next(nil)
	if !ok || next.ID != "B" {
		t.Fatalf("next=%+v ok=%v", next, ok)
	}
}

func TestCandidatePoolSkipsDeletedCursorToNextSurvivor(t *testing.T) {
	rotation.Store(0)
	p := newCandidatePool([]authEntry{entry("A"), entry("B"), entry("C")})
	first, ok := p.next(nil)
	if !ok || first.ID != "A" {
		t.Fatalf("first=%+v ok=%v", first, ok)
	}
	p.refresh([]authEntry{entry("C"), entry("D"), entry("B")})
	next, ok := p.next(nil)
	if !ok || next.ID != "B" {
		t.Fatalf("next=%+v ok=%v", next, ok)
	}
	for _, id := range poolIDs(p) {
		if id == "A" {
			t.Fatalf("deleted credential survived refresh: %v", poolIDs(p))
		}
	}
}

func TestCandidatePoolSkipsBlockedAndReportsExhaustion(t *testing.T) {
	rotation.Store(0)
	p := newCandidatePool([]authEntry{entry("A"), entry("B")})
	if got, ok := p.next(map[string]bool{"A": true}); !ok || got.ID != "B" {
		t.Fatalf("got=%+v ok=%v", got, ok)
	}
	if _, ok := p.next(map[string]bool{"A": true, "B": true}); ok {
		t.Fatal("expected exhausted pool")
	}
}

func TestCandidatePoolMergeAdditionsPreservesCursorAndRetainsPendingDeletion(t *testing.T) {
	rotation.Store(0)
	p := newCandidatePool([]authEntry{entry("A"), entry("B")})
	first, ok := p.next(nil)
	if !ok || first.ID != "A" {
		t.Fatalf("first=%+v ok=%v", first, ok)
	}
	updatedB := entry("B")
	updatedB.Name = "B-updated"
	p.mergeAdditions([]authEntry{updatedB, entry("C")})
	if got := poolIDs(p); len(got) != 3 || got[0] != "A" || got[1] != "B" || got[2] != "C" {
		t.Fatalf("additive order=%v", got)
	}
	if p.entries[1].Name != "B-updated" {
		t.Fatalf("existing runtime fields were not updated: %+v", p.entries[1])
	}
	next, ok := p.next(nil)
	if !ok || next.ID != "B" {
		t.Fatalf("next=%+v ok=%v", next, ok)
	}
	next, ok = p.next(nil)
	if !ok || next.ID != "C" {
		t.Fatalf("new candidate was not appended to rotation: %+v ok=%v", next, ok)
	}
}
