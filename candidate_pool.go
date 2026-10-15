package main

// candidatePool is a request-local view of the eligible credentials. The CPA
// config file may change while a request is retrying, so a request must not
// use a fresh slice index on every attempt. The pool keeps a stable ID cursor,
// appends newly discovered credentials, and performs a full replacement only
// when the host says that a selected credential no longer exists.
type candidatePool struct {
	entries []authEntry
	nextID  string
}

func newCandidatePool(entries []authEntry) *candidatePool {
	p := &candidatePool{entries: cloneAuthEntries(entries)}
	if len(p.entries) > 0 {
		p.nextID = p.entries[int(rotation.Add(1)-1)%len(p.entries)].ID
	}
	return p
}

func cloneAuthEntries(entries []authEntry) []authEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]authEntry, len(entries))
	copy(out, entries)
	return out
}

func (p *candidatePool) contains(id string) bool {
	for _, entry := range p.entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}

// next returns the next unblocked credential and advances the cursor by
// stable credential identity.  The cursor points at the successor of the
// returned entry, so deleting the entry that just failed naturally selects
// the next surviving credential after a refresh.
func (p *candidatePool) next(blocked map[string]bool) (authEntry, bool) {
	if len(p.entries) == 0 {
		return authEntry{}, false
	}
	start := 0
	if p.nextID != "" {
		for i, entry := range p.entries {
			if entry.ID == p.nextID {
				start = i
				break
			}
		}
	}
	for offset := 0; offset < len(p.entries); offset++ {
		i := (start + offset) % len(p.entries)
		entry := p.entries[i]
		if blocked[entry.ID] {
			continue
		}
		p.nextID = p.entries[(i+1)%len(p.entries)].ID
		return entry, true
	}
	return authEntry{}, false
}

// refresh replaces entries that still exist with their newest runtime values,
// removes deleted credentials, and appends new credentials.  Existing order
// is deliberately preserved for this request.  A UI save can reorder the
// YAML list, but that must not make the same request revisit or skip a
// credential merely because its list position changed.
func (p *candidatePool) refresh(fresh []authEntry) {
	old := cloneAuthEntries(p.entries)
	oldNext := p.nextID
	freshByID := make(map[string]authEntry, len(fresh))
	freshOrder := make([]string, 0, len(fresh))
	for _, entry := range fresh {
		if entry.ID == "" {
			continue
		}
		if _, exists := freshByID[entry.ID]; exists {
			continue
		}
		freshByID[entry.ID] = entry
		freshOrder = append(freshOrder, entry.ID)
	}

	merged := make([]authEntry, 0, len(freshByID))
	seen := make(map[string]bool, len(freshByID))
	for _, entry := range old {
		updated, exists := freshByID[entry.ID]
		if !exists {
			continue
		}
		merged = append(merged, updated)
		seen[entry.ID] = true
	}
	for _, id := range freshOrder {
		if seen[id] {
			continue
		}
		merged = append(merged, freshByID[id])
	}
	p.entries = merged
	if len(merged) == 0 {
		p.nextID = ""
		return
	}
	if p.contains(oldNext) {
		p.nextID = oldNext
		return
	}

	// The cursor target may itself have been deleted.  Continue from the
	// first surviving old entry after that target before considering newly
	// appended credentials.
	oldNextAt := -1
	for i, entry := range old {
		if entry.ID == oldNext {
			oldNextAt = i
			break
		}
	}
	if oldNextAt >= 0 {
		for step := 1; step <= len(old); step++ {
			id := old[(oldNextAt+step)%len(old)].ID
			if p.contains(id) {
				p.nextID = id
				return
			}
		}
	}
	p.nextID = merged[0].ID
}

// mergeAdditions incorporates credentials discovered after this request has
// already started. Existing entries keep their request-local order and cursor
// position. Entries absent from fresh are deliberately retained: a config save
// can remove or replace a credential before CPA's AuthManager has caught up,
// and only an explicit auth_not_found result is strong enough to remove it.
// New IDs are appended, so a config reorder cannot make the current request
// skip or revisit an already scheduled credential.
func (p *candidatePool) mergeAdditions(fresh []authEntry) {
	if len(fresh) == 0 {
		return
	}
	freshByID := make(map[string]authEntry, len(fresh))
	freshOrder := make([]string, 0, len(fresh))
	for _, entry := range fresh {
		if entry.ID == "" {
			continue
		}
		if _, exists := freshByID[entry.ID]; exists {
			continue
		}
		freshByID[entry.ID] = entry
		freshOrder = append(freshOrder, entry.ID)
	}
	if len(freshByID) == 0 {
		return
	}

	seen := make(map[string]bool, len(p.entries)+len(freshByID))
	for i, entry := range p.entries {
		if updated, ok := freshByID[entry.ID]; ok {
			p.entries[i] = updated
		}
		seen[p.entries[i].ID] = true
	}
	for _, id := range freshOrder {
		if seen[id] {
			continue
		}
		p.entries = append(p.entries, freshByID[id])
		seen[id] = true
	}
	if p.nextID == "" && len(p.entries) > 0 {
		p.nextID = p.entries[0].ID
	}
}
