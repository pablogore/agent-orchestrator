package autoreview

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// recordingLauncher is a reviewer launcher whose panes stay alive; it records
// every reviewer it starts.
type recordingLauncher struct{ launched []domain.ReviewerHarness }

func (l *recordingLauncher) InterfaceMode(domain.ReviewerHarness) domain.ReviewerInterfaceMode {
	return domain.ReviewerInterfaceTUI
}
func (l *recordingLauncher) Preflight(context.Context, domain.ReviewerHarness, string, ...domain.ReviewerInterfaceMode) error {
	return nil
}
func (l *recordingLauncher) Spawn(_ context.Context, spec reviewcore.LaunchSpec) (reviewcore.LaunchResult, error) {
	l.launched = append(l.launched, spec.Harness)
	return reviewcore.LaunchResult{HandleID: "review-" + string(spec.WorkerID), LaunchID: spec.LaunchID}, nil
}
func (l *recordingLauncher) RestoreTerminal(_ context.Context, spec reviewcore.LaunchSpec) (reviewcore.LaunchResult, error) {
	return reviewcore.LaunchResult{HandleID: "review-" + string(spec.WorkerID), LaunchID: spec.LaunchID}, nil
}
func (l *recordingLauncher) Notify(context.Context, string, reviewcore.LaunchSpec) error { return nil }
func (l *recordingLauncher) Alive(context.Context, string, string) (bool, error)         { return true, nil }
func (l *recordingLauncher) Reusable(domain.ReviewerHarness) bool                        { return false }
func (l *recordingLauncher) Cancel(context.Context, string, domain.ReviewerHarness) error {
	return nil
}
func (l *recordingLauncher) Destroy(context.Context, string) error { return nil }

type headPRs struct{ prs []domain.PullRequest }

func (p headPRs) ListPRsBySession(context.Context, domain.SessionID) ([]domain.PullRequest, error) {
	return p.prs, nil
}

// coordinatorStore is the SQLite store with the worker's PRs supplied by the
// test, as the SCM observer would have recorded them.
type coordinatorStore struct {
	*sqlite.Store
	prs headPRs
}

func (s coordinatorStore) ListPRsBySession(ctx context.Context, id domain.SessionID) ([]domain.PullRequest, error) {
	return s.prs.ListPRsBySession(ctx, id)
}

// engineTrigger drives the real review engine the way the review service does.
type engineTrigger struct{ eng *reviewcore.Engine }

func (t engineTrigger) TriggerAuto(ctx context.Context, id domain.SessionID, harness domain.ReviewerHarness) (reviewcore.TriggerResult, error) {
	return t.eng.TriggerWithOptions(ctx, id, reviewcore.TriggerOptions{Harness: harness, Source: domain.ReviewTriggerAuto})
}
func (t engineTrigger) AdvanceCycles(ctx context.Context, id domain.SessionID) (reviewcore.TriggerResult, error) {
	return t.eng.AdvanceCycles(ctx, id)
}

// The coordinator and the real engine together: the queue runs every reviewer
// once, one at a time, then auto review retries only the failed reviewer, and
// stops after three failed automatic passes on the head.
func TestAutoReviewRunsTheQueueThenRetriesFailedReviewerWithinLimit(t *testing.T) {
	ctx := context.Background()
	st := sqlitetest.MustOpen(t)
	base := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	worker := domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode,
		Metadata:          domain.SessionMetadata{WorkspacePath: "/ws/mer-1"},
		AutoReviewEnabled: true,
		Activity:          domain.Activity{State: domain.ActivityIdle, LastActivityAt: base.Add(-time.Hour)},
	}
	if err := st.UpsertProject(ctx, domain.ProjectRecord{
		ID: "mer", Path: "/tmp/mer", RegisteredAt: base,
		Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode}, {Harness: domain.ReviewerCodex}}},
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := st.CreateSession(ctx, worker); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	prs := headPRs{prs: []domain.PullRequest{{URL: "https://github.com/o/r/pull/1", Number: 1, HeadSHA: "sha1"}}}
	now := base
	clock := func() time.Time { now = now.Add(time.Second); return now }
	ids := 0
	launcher := &recordingLauncher{}
	eng := reviewcore.New(reviewcore.Deps{
		Store: st, Sessions: st, PRs: prs, Projects: st, Launcher: launcher, Clock: clock,
		NewID: func() string { ids++; return "id-" + strconv.Itoa(ids) },
	})
	coord := New(coordinatorStore{Store: st, prs: prs}, engineTrigger{eng: eng}, Config{Clock: clock})

	sweep := func() {
		t.Helper()
		if err := coord.Sweep(ctx); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		running := 0
		runs, err := st.ListReviewRunsBySession(ctx, worker.ID)
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		for _, run := range runs {
			if run.Status == domain.ReviewRunRunning {
				running++
			}
		}
		if running > 1 {
			t.Fatalf("%d reviewers running at once for one worker", running)
		}
	}
	finish := func(harness domain.ReviewerHarness, status domain.ReviewRunStatus, verdict domain.ReviewVerdict) {
		t.Helper()
		runs, err := st.ListReviewRunsBySession(ctx, worker.ID)
		if err != nil {
			t.Fatalf("list runs: %v", err)
		}
		for _, run := range runs {
			if run.Harness == harness && run.Status == domain.ReviewRunRunning {
				if _, err := st.UpdateReviewRunResult(ctx, run.ID, status, verdict, "body", "", false); err != nil {
					t.Fatalf("finish run: %v", err)
				}
				return
			}
		}
		t.Fatalf("no running %s pass to finish", harness)
	}

	sweep() // opens the cycle and starts claude-code
	finish(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	sweep() // the queue starts codex
	for attempt := 1; attempt <= 3; attempt++ {
		finish(domain.ReviewerCodex, domain.ReviewRunFailed, domain.VerdictNone)
		sweep() // retries codex by name until its third failure
	}

	want := []domain.ReviewerHarness{domain.ReviewerClaudeCode, domain.ReviewerCodex, domain.ReviewerCodex, domain.ReviewerCodex}
	if len(launcher.launched) != len(want) {
		t.Fatalf("launched = %v, want %v", launcher.launched, want)
	}
	for i := range want {
		if launcher.launched[i] != want[i] {
			t.Fatalf("launched = %v, want %v", launcher.launched, want)
		}
	}
	list, err := eng.List(ctx, worker.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := list.Reviews[0].Status; got != reviewcore.ReviewStateNeedsReview {
		t.Fatalf("status = %q, want needs_review: a failed reviewer never counts as approval", got)
	}
}
