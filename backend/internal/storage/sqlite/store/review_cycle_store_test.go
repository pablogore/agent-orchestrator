package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func seedReviewCycleSession(t *testing.T, s *sqlite.Store, project string) domain.SessionID {
	t.Helper()
	seedProject(t, s, project)
	rec, err := s.CreateSession(context.Background(), sampleRecord(project))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return rec.ID
}

func sampleReviewCycle(id string, session domain.SessionID, prURL string, at time.Time) domain.ReviewCycle {
	return domain.ReviewCycle{
		ID: id, SessionID: session, PRURL: prURL, TargetSHA: "sha-" + id, CreatedAt: at,
		Members: []domain.ReviewCycleMember{
			// Never queued: QueuedAt stays zero (stored as NULL).
			{Position: 0, Harness: domain.ReviewerCodex, AgentConfig: domain.AgentConfig{Model: "gpt-5", Effort: "high"}},
			{Position: 1, Harness: domain.ReviewerClaudeCode, QueuedAt: at, QueuedSource: domain.ReviewTriggerAuto},
		},
	}
}

func TestReviewCycleInsertAndListRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	session := seedReviewCycleSession(t, s, "cycle-rt")
	now := time.Now().UTC().Truncate(time.Second)

	// Members are inserted out of order; the list returns queue order.
	cycle := sampleReviewCycle("cycle-1", session, "https://github.com/o/r/pull/1", now)
	cycle.Members = []domain.ReviewCycleMember{cycle.Members[1], cycle.Members[0]}
	if err := s.InsertReviewCycle(ctx, cycle); err != nil {
		t.Fatalf("insert cycle: %v", err)
	}
	second := sampleReviewCycle("cycle-2", session, "https://github.com/o/r/pull/2", now.Add(time.Second))
	if err := s.InsertReviewCycle(ctx, second); err != nil {
		t.Fatalf("insert second cycle: %v", err)
	}

	got, err := s.ListOpenReviewCyclesBySession(ctx, session)
	if err != nil {
		t.Fatalf("list open cycles: %v", err)
	}
	if len(got) != 2 || got[0].ID != "cycle-1" || got[1].ID != "cycle-2" {
		t.Fatalf("open cycles = %+v, want cycle-1 then cycle-2", got)
	}
	want := sampleReviewCycle("cycle-1", session, "https://github.com/o/r/pull/1", now)
	if !got[0].CreatedAt.Equal(now) {
		t.Fatalf("created_at = %v, want %v", got[0].CreatedAt, now)
	}
	got[0].CreatedAt = want.CreatedAt
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("cycle = %+v\nwant  %+v", got[0], want)
	}
	if !got[0].Open() {
		t.Fatalf("listed cycle should be open")
	}
}

func TestInsertReviewCycleRejectsSecondOpenCycleForSamePR(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	session := seedReviewCycleSession(t, s, "cycle-dup")
	now := time.Now().UTC().Truncate(time.Second)
	pr := "https://github.com/o/r/pull/1"
	if err := s.InsertReviewCycle(ctx, sampleReviewCycle("cycle-1", session, pr, now)); err != nil {
		t.Fatalf("insert cycle: %v", err)
	}
	err := s.InsertReviewCycle(ctx, sampleReviewCycle("cycle-2", session, pr, now.Add(time.Second)))
	if !errors.Is(err, domain.ErrDuplicateReviewCycle) {
		t.Fatalf("second open cycle err = %v, want ErrDuplicateReviewCycle", err)
	}
	// The failed insert left no partial rows behind.
	got, err := s.ListOpenReviewCyclesBySession(ctx, session)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "cycle-1" || len(got[0].Members) != 2 {
		t.Fatalf("open cycles after duplicate = %+v", got)
	}
}

func TestSupersedeReviewCycleAllowsNewCycleForSamePR(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	session := seedReviewCycleSession(t, s, "cycle-sup")
	now := time.Now().UTC().Truncate(time.Second)
	pr := "https://github.com/o/r/pull/1"
	if err := s.InsertReviewCycle(ctx, sampleReviewCycle("cycle-1", session, pr, now)); err != nil {
		t.Fatalf("insert cycle: %v", err)
	}

	changed, err := s.SupersedeReviewCycle(ctx, "cycle-1", "head moved", now.Add(time.Minute))
	if err != nil || !changed {
		t.Fatalf("supersede = %v, %v; want true nil", changed, err)
	}
	if changed, err := s.SupersedeReviewCycle(ctx, "cycle-1", "again", now.Add(2*time.Minute)); err != nil || changed {
		t.Fatalf("second supersede = %v, %v; want false nil", changed, err)
	}
	if changed, err := s.SupersedeReviewCycle(ctx, "missing", "x", now); err != nil || changed {
		t.Fatalf("supersede missing = %v, %v; want false nil", changed, err)
	}

	if err := s.InsertReviewCycle(ctx, sampleReviewCycle("cycle-2", session, pr, now.Add(2*time.Minute))); err != nil {
		t.Fatalf("new cycle after supersede: %v", err)
	}
	got, err := s.ListOpenReviewCyclesBySession(ctx, session)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != "cycle-2" {
		t.Fatalf("open cycles = %+v, want only cycle-2", got)
	}
}

func TestSetReviewCycleMemberQueued(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	session := seedReviewCycleSession(t, s, "cycle-queued")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.InsertReviewCycle(ctx, sampleReviewCycle("cycle-1", session, "https://github.com/o/r/pull/1", now)); err != nil {
		t.Fatalf("insert cycle: %v", err)
	}

	queuedAt := now.Add(30 * time.Second)
	if changed, err := s.SetReviewCycleMemberQueued(ctx, "cycle-1", domain.ReviewerCodex, queuedAt, domain.ReviewTriggerManual); err != nil || !changed {
		t.Fatalf("queue codex = %v, %v; want true nil", changed, err)
	}
	got, err := s.ListOpenReviewCyclesBySession(ctx, session)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	codex, claude := got[0].Members[0], got[0].Members[1]
	if !codex.QueuedAt.Equal(queuedAt) || codex.QueuedSource != domain.ReviewTriggerManual {
		t.Fatalf("codex = %+v, want queued at %v by manual", codex, queuedAt)
	}
	if !claude.QueuedAt.Equal(now) || claude.QueuedSource != domain.ReviewTriggerAuto {
		t.Fatalf("claude = %+v, want untouched", claude)
	}

	if changed, err := s.SetReviewCycleMemberQueued(ctx, "cycle-1", domain.ReviewerOpenCode, queuedAt, domain.ReviewTriggerAuto); err != nil || changed {
		t.Fatalf("non-member = %v, %v; want false nil", changed, err)
	}
	if changed, err := s.SetReviewCycleMemberQueued(ctx, "missing", domain.ReviewerCodex, queuedAt, domain.ReviewTriggerAuto); err != nil || changed {
		t.Fatalf("missing cycle = %v, %v; want false nil", changed, err)
	}

	// A superseded cycle no longer queues anyone.
	if _, err := s.SupersedeReviewCycle(ctx, "cycle-1", "head moved", now.Add(time.Minute)); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if changed, err := s.SetReviewCycleMemberQueued(ctx, "cycle-1", domain.ReviewerCodex, now.Add(2*time.Minute), domain.ReviewTriggerAgent); err != nil || changed {
		t.Fatalf("queue on superseded cycle = %v, %v; want false nil", changed, err)
	}
}

// Re-queueing a member changes what the worker's board card waits on, so it
// refreshes the worker through session_updated stamped at the queue time.
func TestReviewCycleCDCEmitsSessionUpdatedOnRequeue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	session := seedReviewCycleSession(t, s, "cycle-requeue-cdc")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.InsertReviewCycle(ctx, sampleReviewCycle("cycle-1", session, "https://github.com/o/r/pull/1", now)); err != nil {
		t.Fatalf("insert cycle: %v", err)
	}
	before, err := s.EventsAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	queuedAt := now.Add(time.Minute)
	if changed, err := s.SetReviewCycleMemberQueued(ctx, "cycle-1", domain.ReviewerCodex, queuedAt, domain.ReviewTriggerManual); err != nil || !changed {
		t.Fatalf("queue = %v, %v; want true nil", changed, err)
	}
	after, err := s.EventsAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("events after re-queue = %d, want %d", len(after), len(before)+1)
	}
	row := after[len(after)-1]
	if row.Type != "session_updated" || row.SessionID != string(session) ||
		!strings.Contains(string(row.Payload), `"reviewCycleId":"cycle-1"`) {
		t.Fatalf("re-queue event = %+v", row)
	}
	if !row.CreatedAt.Equal(queuedAt) {
		t.Fatalf("re-queue event created_at = %v, want %v", row.CreatedAt, queuedAt)
	}
}

func TestListOpenReviewCyclesForSessionsAndSessionsWithOpenCycles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "cycle-bulk")
	newSession := func() domain.SessionID {
		rec, err := s.CreateSession(ctx, sampleRecord("cycle-bulk"))
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return rec.ID
	}
	a, b, c := newSession(), newSession(), newSession()
	now := time.Now().UTC().Truncate(time.Second)
	for _, cycle := range []domain.ReviewCycle{
		sampleReviewCycle("cycle-a", a, "https://github.com/o/r/pull/1", now),
		sampleReviewCycle("cycle-b-old", b, "https://github.com/o/r/pull/2", now),
		sampleReviewCycle("cycle-c", c, "https://github.com/o/r/pull/3", now),
	} {
		if err := s.InsertReviewCycle(ctx, cycle); err != nil {
			t.Fatalf("insert %s: %v", cycle.ID, err)
		}
	}
	if _, err := s.SupersedeReviewCycle(ctx, "cycle-b-old", "head moved", now.Add(time.Second)); err != nil {
		t.Fatalf("supersede: %v", err)
	}

	got, err := s.ListOpenReviewCyclesForSessions(ctx, []domain.SessionID{a, b})
	if err != nil {
		t.Fatalf("bulk list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("bulk keys = %v, want exactly a and b", got)
	}
	if len(got[a]) != 1 || got[a][0].ID != "cycle-a" || len(got[a][0].Members) != 2 {
		t.Fatalf("a = %+v", got[a])
	}
	if got[b] == nil || len(got[b]) != 0 {
		t.Fatalf("b = %#v, want empty non-nil slice", got[b])
	}
	if empty, err := s.ListOpenReviewCyclesForSessions(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty bulk = %v, %v", empty, err)
	}

	sessions, err := s.ListSessionsWithOpenReviewCycles(ctx)
	if err != nil {
		t.Fatalf("sessions with open cycles: %v", err)
	}
	wantSessions := map[domain.SessionID]bool{a: true, c: true}
	if len(sessions) != 2 || !wantSessions[sessions[0]] || !wantSessions[sessions[1]] {
		t.Fatalf("sessions = %v, want a and c", sessions)
	}
}

func TestReviewCyclesCascadeWhenSeedSessionDeleted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "cycle-cascade")
	now := time.Unix(1700000000, 0).UTC()
	sess, err := s.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "cycle-cascade",
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessCodex,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		CreatedAt: now,
		UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("create seed session: %v", err)
	}
	if err := s.InsertReviewCycle(ctx, sampleReviewCycle("cycle-1", sess.ID, "https://github.com/o/r/pull/1", now)); err != nil {
		t.Fatalf("insert cycle: %v", err)
	}
	deleted, err := s.DeleteSession(ctx, sess.ID)
	if err != nil || !deleted {
		t.Fatalf("delete seed session = %v, %v; want true nil", deleted, err)
	}
	sessions, err := s.ListSessionsWithOpenReviewCycles(ctx)
	if err != nil {
		t.Fatalf("sessions with open cycles: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("sessions after delete = %v, want none", sessions)
	}
}

// Opening or superseding a cycle changes the worker's expected reviewers, so it
// refreshes the worker through session_updated; a repeat supersede does not.
func TestReviewCycleCDCEmitsSessionUpdatedOnOpenAndSupersede(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	session := seedReviewCycleSession(t, s, "cycle-cdc")
	now := time.Now().UTC().Truncate(time.Second)
	countCycleEvents := func() int {
		t.Helper()
		rows, err := s.EventsAfter(ctx, 0, 1000)
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		n := 0
		for _, r := range rows {
			if r.Type == "session_updated" && strings.Contains(string(r.Payload), `"reviewCycleId":"cycle-1"`) {
				n++
			}
		}
		return n
	}
	if err := s.InsertReviewCycle(ctx, sampleReviewCycle("cycle-1", session, "https://github.com/o/r/pull/1", now)); err != nil {
		t.Fatalf("insert cycle: %v", err)
	}
	if got := countCycleEvents(); got != 1 {
		t.Fatalf("events after open = %d, want 1", got)
	}
	if _, err := s.SupersedeReviewCycle(ctx, "cycle-1", "head moved", now.Add(time.Minute)); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if _, err := s.SupersedeReviewCycle(ctx, "cycle-1", "again", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("repeat supersede: %v", err)
	}
	if got := countCycleEvents(); got != 2 {
		t.Fatalf("events after supersede = %d, want 2", got)
	}
}
