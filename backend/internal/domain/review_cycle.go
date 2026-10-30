package domain

import (
	"errors"
	"time"
)

// ErrDuplicateReviewCycle is returned by InsertReviewCycle when the worker
// already has an open cycle for the PR.
var ErrDuplicateReviewCycle = errors.New("domain: open review cycle already exists for session and pr")

// ReviewCycle is one trigger's snapshot of the reviewers a PR head owes
// (#5465). It fixes the head and the ordered reviewer list when it opens, so a
// later config edit cannot change what that head requires. A head change
// supersedes it; the next trigger opens a new cycle for the new head.
type ReviewCycle struct {
	ID        string
	SessionID SessionID
	PRURL     string
	TargetSHA string
	// Members are ordered by Position: reviewers run one at a time, in order.
	Members   []ReviewCycleMember
	CreatedAt time.Time
	// SupersededAt is zero while the cycle is open.
	SupersededAt     time.Time
	SupersededReason string
}

// ReviewCycleMember is one reviewer a cycle requires.
//
// Whether the member still waits for a pass is not stored: it is derived from
// its runs on the cycle's head. QueuedAt records when the member last entered
// the queue; a pass it started at or after QueuedAt answers that queueing,
// and every earlier pass of it on the head no longer counts. A zero QueuedAt
// means the member was never queued: its passes on the head all count.
type ReviewCycleMember struct {
	Position    int
	Harness     ReviewerHarness
	AgentConfig AgentConfig
	QueuedAt    time.Time
	// QueuedSource is the trigger that queued the member; its next pass is
	// recorded under that source, whoever runs the queue step.
	QueuedSource ReviewTriggerSource
}

// Counts reports whether a pass of the member started at runCreatedAt counts
// for the cycle.
func (m ReviewCycleMember) Counts(runCreatedAt time.Time) bool {
	return m.QueuedAt.IsZero() || !runCreatedAt.Before(m.QueuedAt)
}

// Waiting reports whether the member waits in the queue: it was queued and has
// no counting pass on the head yet. newestRunAt is the start of its newest
// pass on the head (running or concluded); hasRun is false when it has none.
func (m ReviewCycleMember) Waiting(newestRunAt time.Time, hasRun bool) bool {
	if m.QueuedAt.IsZero() {
		return false
	}
	return !hasRun || !m.Counts(newestRunAt)
}

// Open reports whether the cycle still decides its head's review.
func (c ReviewCycle) Open() bool {
	return c.SupersededAt.IsZero()
}

// Harnesses lists the cycle's reviewers in queue order.
func (c ReviewCycle) Harnesses() []ReviewerHarness {
	harnesses := make([]ReviewerHarness, len(c.Members))
	for i, member := range c.Members {
		harnesses[i] = member.Harness
	}
	return harnesses
}

// LegacyHarness is the reviewer a run recorded without a harness belongs to on
// this cycle's head: the first member, the session's own reviewer.
func (c ReviewCycle) LegacyHarness() ReviewerHarness {
	if len(c.Members) == 0 {
		return ""
	}
	return c.Members[0].Harness
}
