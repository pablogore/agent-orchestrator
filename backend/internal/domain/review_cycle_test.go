package domain

import (
	"testing"
	"time"
)

func TestReviewCycleMemberQueue(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	never := ReviewCycleMember{Harness: ReviewerClaudeCode}
	if never.Waiting(time.Time{}, false) || !never.Counts(t0.Add(-time.Hour)) {
		t.Fatal("a member never queued waits for nothing and every pass counts")
	}
	queued := ReviewCycleMember{Harness: ReviewerCodex, QueuedAt: t0}
	if !queued.Waiting(time.Time{}, false) {
		t.Fatal("a queued member with no pass waits")
	}
	if !queued.Waiting(t0.Add(-time.Second), true) || queued.Counts(t0.Add(-time.Second)) {
		t.Fatal("a pass from before the queueing neither answers it nor counts")
	}
	if queued.Waiting(t0, true) || !queued.Counts(t0) {
		t.Fatal("a pass started at the queueing answers it and counts")
	}
}

func TestReviewCycleHelpers(t *testing.T) {
	cycle := ReviewCycle{Members: []ReviewCycleMember{{Harness: ReviewerClaudeCode}, {Harness: ReviewerCodex}}}
	if !cycle.Open() {
		t.Fatal("a cycle without SupersededAt must be open")
	}
	if got := cycle.Harnesses(); len(got) != 2 || got[1] != ReviewerCodex {
		t.Fatalf("harnesses = %v", got)
	}
	if cycle.LegacyHarness() != ReviewerClaudeCode || (ReviewCycle{}).LegacyHarness() != "" {
		t.Fatal("legacy runs belong to the first member only")
	}
	cycle.SupersededAt = time.Now()
	if cycle.Open() {
		t.Fatal("a superseded cycle must not be open")
	}
}
