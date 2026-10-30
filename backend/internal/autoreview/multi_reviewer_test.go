package autoreview

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
)

// #5465: auto review must keep reviewing a head until every configured
// reviewer has reviewed it, not stop after Reviewers[0] approved.
func TestEvaluateSessionReviewsHeadWithEveryConfiguredReviewer(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		session: domain.SessionRecord{ID: "s1", ProjectID: "p1", Kind: domain.KindWorker, Harness: domain.AgentHarness("codex"), AutoReviewEnabled: true, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Minute)}},
		project: domain.ProjectRecord{ID: "p1", Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{
			{Harness: domain.ReviewerClaudeCode},
			{Harness: domain.ReviewerCodex},
		}}},
		prs: []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha1"}},
		runs: []domain.ReviewRun{{
			PRURL: "pr1", TargetSHA: "sha1", Harness: domain.ReviewerClaudeCode,
			Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, TriggerSource: domain.ReviewTriggerAuto, CreatedAt: now,
		}},
	}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	// With several reviewers and no open cycle, auto review opens a cycle;
	// the engine skips claude-code's approval and launches codex.
	if !result.Triggered || trigger.calls != 1 || trigger.harness != "" {
		t.Fatalf("triggered=%v calls=%d harness=%q reason=%s, want a cycle opened for sha1", result.Triggered, trigger.calls, trigger.harness, result.Reason)
	}
}

func twoReviewerStore(now time.Time, runs ...domain.ReviewRun) *fakeStore {
	return &fakeStore{
		session: domain.SessionRecord{ID: "s1", ProjectID: "p1", Kind: domain.KindWorker, Harness: domain.HarnessCodex, AutoReviewEnabled: true, Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: now.Add(-time.Minute)}},
		project: domain.ProjectRecord{ID: "p1", Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{
			{Harness: domain.ReviewerClaudeCode},
			{Harness: domain.ReviewerCodex},
		}}},
		prs:  []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha1"}},
		runs: runs,
	}
}

func headRun(harness domain.ReviewerHarness, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, at time.Time) domain.ReviewRun {
	return domain.ReviewRun{PRURL: "pr1", TargetSHA: "sha1", Harness: harness, Status: status, Verdict: verdict, TriggerSource: domain.ReviewTriggerAuto, CreatedAt: at}
}

// A rejection from the first reviewer must not stop the remaining reviewers.
func TestEvaluateSessionRunsNextReviewerAfterChangesRequested(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now, headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictChangesRequested, now))
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered || trigger.calls != 1 || trigger.harness != "" {
		t.Fatalf("triggered=%v calls=%d harness=%q reason=%s, want a cycle opened so codex still reviews", result.Triggered, trigger.calls, trigger.harness, result.Reason)
	}
}

// Reviewers run one at a time: while the first reviewer is running, the next
// one is not started.
func TestEvaluateSessionWaitsForRunningReviewerBeforeNext(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	running := headRun(domain.ReviewerClaudeCode, domain.ReviewRunRunning, "", now)
	store := twoReviewerStore(now, running)
	trigger := &fakeTrigger{result: reviewcore.TriggerResult{
		Reviews: []reviewcore.PRReviewState{{PRURL: "pr1", TargetSHA: "sha1", Status: reviewcore.ReviewStateRunning}},
		Runs:    []domain.ReviewRun{running},
	}}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Triggered || result.Reason != "review_running" || trigger.harness == domain.ReviewerCodex {
		t.Fatalf("result=%+v harness=%q, want review_running without starting codex", result, trigger.harness)
	}
}

func TestEvaluateSessionSkipsHeadEveryReviewerApproved(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now,
		headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, now),
		headRun(domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictApproved, now.Add(time.Minute)),
	)
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Triggered || trigger.calls != 0 || result.Reason != "already_approved" {
		t.Fatalf("result=%+v calls=%d, want already_approved without trigger", result, trigger.calls)
	}
}

func TestEvaluateSessionSingleReviewerStopsAfterItsVerdict(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now, headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictChangesRequested, now))
	store.project.Config.Reviewers = store.project.Config.Reviewers[:1]
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Triggered || trigger.calls != 0 || result.Reason != "changes_requested_same_sha" {
		t.Fatalf("result=%+v calls=%d, want changes_requested_same_sha without trigger", result, trigger.calls)
	}
}

// Auto review and the engine agree on the reviewer a project with no
// configured reviewers gets: its default worker agent.
func TestEvaluateSessionUsesProjectDefaultWorkerReviewer(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now)
	store.project.Config = domain.ProjectConfig{Worker: domain.RoleOverride{Harness: domain.HarnessClaudeCode}}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered || trigger.harness != domain.ReviewerClaudeCode {
		t.Fatalf("triggered=%v harness=%q, want claude-code", result.Triggered, trigger.harness)
	}
}

// member is a cycle member; a zero queuedAt means it was never queued.
func member(harness domain.ReviewerHarness, queuedAt time.Time) domain.ReviewCycleMember {
	return domain.ReviewCycleMember{Harness: harness, QueuedAt: queuedAt}
}

// openCycle is the open cycle for pr1@sha1 with members in queue order.
func openCycle(members ...domain.ReviewCycleMember) domain.ReviewCycle {
	for i := range members {
		members[i].Position = i
	}
	return domain.ReviewCycle{ID: "c1", SessionID: "s1", PRURL: "pr1", TargetSHA: "sha1", Members: members}
}

// A cycle that still has a reviewer queued is moved by the queue step, not by
// a new auto-review trigger.
func TestEvaluateSessionLeavesQueuedCycleToTheQueue(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now, headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, now))
	store.cycles = []domain.ReviewCycle{openCycle(member(domain.ReviewerClaudeCode, time.Time{}), member(domain.ReviewerCodex, now))}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Triggered || trigger.calls != 0 || result.Reason != "review_queued" {
		t.Fatalf("result=%+v calls=%d, want review_queued without a trigger", result, trigger.calls)
	}
}

// A member queued again after its old approval waits for a new pass: the old
// approval no longer answers the queueing, so auto review leaves it to the
// queue instead of reporting the head approved.
func TestEvaluateSessionLeavesRequeuedReviewerToTheQueue(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now,
		headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, now),
		headRun(domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictApproved, now.Add(-2*time.Minute)),
	)
	store.cycles = []domain.ReviewCycle{openCycle(member(domain.ReviewerClaudeCode, time.Time{}), member(domain.ReviewerCodex, now.Add(-time.Minute)))}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Triggered || trigger.calls != 0 || result.Reason != "review_queued" {
		t.Fatalf("result=%+v calls=%d, want review_queued for the requeued codex", result, trigger.calls)
	}
}

// A queued member whose pass started after it was queued is no longer
// waiting: when that pass failed, auto review retries it by name.
func TestEvaluateSessionRetriesReviewerWhosePassAfterQueueingFailed(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now,
		headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, now),
		headRun(domain.ReviewerCodex, domain.ReviewRunFailed, "", now.Add(2*time.Minute)),
	)
	store.cycles = []domain.ReviewCycle{openCycle(member(domain.ReviewerClaudeCode, time.Time{}), member(domain.ReviewerCodex, now.Add(time.Minute)))}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered || trigger.harness != domain.ReviewerCodex {
		t.Fatalf("result=%+v harness=%q, want codex retried by name", result, trigger.harness)
	}
}

// The open cycle, not the current config, decides the reviewers of its head:
// dropping codex from the config must not leave the cycle's failed codex pass
// unretried.
func TestEvaluateSessionRetriesCycleReviewerRemovedFromConfig(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now,
		headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, now),
		headRun(domain.ReviewerCodex, domain.ReviewRunFailed, "", now.Add(time.Minute)),
	)
	store.project.Config.Reviewers = store.project.Config.Reviewers[:1]
	store.cycles = []domain.ReviewCycle{openCycle(member(domain.ReviewerClaudeCode, time.Time{}), member(domain.ReviewerCodex, time.Time{}))}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered || trigger.harness != domain.ReviewerCodex {
		t.Fatalf("result=%+v harness=%q, want the cycle's codex retried by name", result, trigger.harness)
	}
}

// A reviewer added to the config after the cycle opened does not review the
// cycle's frozen head.
func TestEvaluateSessionIgnoresReviewerAddedAfterCycleOpened(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now,
		headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, now),
		headRun(domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictApproved, now.Add(time.Minute)),
	)
	store.project.Config.Reviewers = append(store.project.Config.Reviewers, domain.ReviewerConfig{Harness: domain.ReviewerOpenCode})
	store.cycles = []domain.ReviewCycle{openCycle(member(domain.ReviewerClaudeCode, time.Time{}), member(domain.ReviewerCodex, time.Time{}))}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Triggered || trigger.calls != 0 || result.Reason != "already_approved" {
		t.Fatalf("result=%+v calls=%d harness=%q, want already_approved without starting opencode", result, trigger.calls, trigger.harness)
	}
}

// Once the queue ran every reviewer, auto review retries the failed one by
// name, within the existing retry limit.
func TestEvaluateSessionRetriesFailedReviewerAfterQueueDrained(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now,
		headRun(domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, now),
		headRun(domain.ReviewerCodex, domain.ReviewRunFailed, "", now.Add(time.Minute)),
	)
	store.cycles = []domain.ReviewCycle{openCycle(member(domain.ReviewerClaudeCode, time.Time{}), member(domain.ReviewerCodex, time.Time{}))}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered || trigger.harness != domain.ReviewerCodex {
		t.Fatalf("result=%+v harness=%q, want codex retried by name", result, trigger.harness)
	}
}

// The sweep moves every open cycle's queue, even for a session whose auto
// review is off or that is busy: a manual cycle must not stall when its
// reviewer exits without submitting.
func TestSweepAdvancesOpenCyclesRegardlessOfAutoReviewGate(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now)
	store.session.AutoReviewEnabled = false
	store.cycleSessions = []domain.SessionID{"s1"}
	trigger := &fakeTrigger{}

	if err := New(store, trigger, Config{Clock: func() time.Time { return now }}).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(trigger.advanced) != 1 || trigger.advanced[0] != "s1" || trigger.calls != 0 {
		t.Fatalf("advanced=%v calls=%d, want one queue step for s1 and no auto trigger", trigger.advanced, trigger.calls)
	}
}

// A run recorded without a reviewer counts for the first expected reviewer
// only, so the second still owes the head.
func TestEvaluateSessionLegacyRunSatisfiesOnlyFirstReviewer(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	store := twoReviewerStore(now, headRun("", domain.ReviewRunComplete, domain.VerdictApproved, now))
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered || trigger.calls != 1 {
		t.Fatalf("result=%+v calls=%d, want codex still owed after a legacy approval", result, trigger.calls)
	}
}

// With several PRs, a reviewer is owed only on the heads whose own frozen
// cycle lists it: every cycle fully approved means nothing to retry.
func TestEvaluateSessionDoesNotCarryReviewersAcrossPRCycles(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	run := func(pr string, h domain.ReviewerHarness, at time.Time) domain.ReviewRun {
		return domain.ReviewRun{PRURL: pr, TargetSHA: "sha1", Harness: h, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, TriggerSource: domain.ReviewTriggerAuto, CreatedAt: at}
	}
	store := twoReviewerStore(now,
		run("pr1", domain.ReviewerClaudeCode, now), run("pr1", domain.ReviewerCodex, now.Add(time.Second)),
		run("pr2", domain.ReviewerClaudeCode, now.Add(2*time.Second)), run("pr2", domain.ReviewerOpenCode, now.Add(3*time.Second)),
	)
	store.prs = []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha1"}, {URL: "pr2", Number: 2, HeadSHA: "sha1"}}
	store.cycles = []domain.ReviewCycle{
		{ID: "c1", SessionID: "s1", PRURL: "pr1", TargetSHA: "sha1", Members: []domain.ReviewCycleMember{{Position: 0, Harness: domain.ReviewerClaudeCode}, {Position: 1, Harness: domain.ReviewerCodex}}},
		{ID: "c2", SessionID: "s1", PRURL: "pr2", TargetSHA: "sha1", Members: []domain.ReviewCycleMember{{Position: 0, Harness: domain.ReviewerClaudeCode}, {Position: 1, Harness: domain.ReviewerOpenCode}}},
	}
	trigger := &fakeTrigger{}

	result, err := New(store, trigger, Config{Clock: func() time.Time { return now }}).EvaluateSession(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if trigger.calls != 0 || result.Reason != "already_approved" {
		t.Fatalf("result=%+v harness=%q calls=%d, want already_approved without a trigger", result, trigger.harness, trigger.calls)
	}
}
