package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Regression coverage for #5465: every configured reviewer must review the PR
// head, not only Reviewers[0].

func twoConfiguredReviewers() fakeProjects {
	return fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{
		{Harness: domain.ReviewerClaudeCode},
		{Harness: domain.ReviewerCodex},
	}}}
}

// Sequential execution starts with the first configured reviewer only. This
// already holds on main and must keep holding.
func TestTriggerStartsWithFirstConfiguredReviewer(t *testing.T) {
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(&fakeStore{}, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)

	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if len(launcher.specs) != 1 || launcher.specs[0].Harness != domain.ReviewerClaudeCode {
		t.Fatalf("launched = %+v, want only claude-code first", launcher.specs)
	}
}

// After the first reviewer approved the head, the head still owes the second
// configured reviewer a pass.
func TestTriggerReviewsHeadWithNextConfiguredReviewer(t *testing.T) {
	store := &fakeStore{runs: []domain.ReviewRun{{
		ID: "run-1", SessionID: "mer-1", PRURL: "https://github.com/o/r/pull/1", TargetSHA: "sha1",
		Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
	}}}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)

	_, _ = trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{})

	if len(launcher.specs) != 1 || launcher.specs[0].Harness != domain.ReviewerCodex {
		t.Fatalf("launched = %+v, want codex to review sha1 after claude-code approved", launcher.specs)
	}
}

// A changes_requested verdict on the current head blocks approval even when
// another reviewer approved the same head later.
func TestPlanChangesRequestedBlocksLaterApprovalOnSameHead(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prs := []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha1"}}
	runs := []domain.ReviewRun{
		{ID: "a", PRURL: "pr1", TargetSHA: "sha1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested, CreatedAt: t0},
		{ID: "b", PRURL: "pr1", TargetSHA: "sha1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: t0.Add(time.Minute)},
	}

	got := Plan(prs, runs)
	if len(got) != 1 || got[0].Status != ReviewStateChangesRequested {
		t.Fatalf("status = %q, want changes_requested", got[0].Status)
	}
}

func TestPlanForReviewersRequiresEveryExpectedReviewerOnCurrentHead(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	both := []domain.ReviewerHarness{domain.ReviewerClaudeCode, domain.ReviewerCodex}
	run := func(harness domain.ReviewerHarness, sha string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, at time.Duration) domain.ReviewRun {
		return domain.ReviewRun{ID: string(harness) + sha + string(status), PRURL: "pr1", TargetSHA: sha, Harness: harness, Status: status, Verdict: verdict, CreatedAt: t0.Add(at)}
	}
	approved := func(h domain.ReviewerHarness, sha string, at time.Duration) domain.ReviewRun {
		return run(h, sha, domain.ReviewRunComplete, domain.VerdictApproved, at)
	}
	tests := []struct {
		name     string
		expected []domain.ReviewerHarness
		runs     []domain.ReviewRun
		want     StateStatus
	}{
		{"single reviewer approved", []domain.ReviewerHarness{domain.ReviewerClaudeCode}, []domain.ReviewRun{approved(domain.ReviewerClaudeCode, "sha1", 0)}, ReviewStateUpToDate},
		{"single reviewer re-review replaces its changes request", []domain.ReviewerHarness{domain.ReviewerClaudeCode}, []domain.ReviewRun{
			run(domain.ReviewerClaudeCode, "sha1", domain.ReviewRunComplete, domain.VerdictChangesRequested, 0),
			approved(domain.ReviewerClaudeCode, "sha1", time.Minute),
		}, ReviewStateUpToDate},
		{"every expected reviewer approved", both, []domain.ReviewRun{approved(domain.ReviewerClaudeCode, "sha1", 0), approved(domain.ReviewerCodex, "sha1", time.Minute)}, ReviewStateUpToDate},
		{"expected reviewer has not run", both, []domain.ReviewRun{approved(domain.ReviewerClaudeCode, "sha1", 0)}, ReviewStateNeedsReview},
		{"expected reviewer failed", both, []domain.ReviewRun{approved(domain.ReviewerClaudeCode, "sha1", 0), run(domain.ReviewerCodex, "sha1", domain.ReviewRunFailed, "", time.Minute)}, ReviewStateNeedsReview},
		{"expected reviewer cancelled", both, []domain.ReviewRun{approved(domain.ReviewerClaudeCode, "sha1", 0), run(domain.ReviewerCodex, "sha1", domain.ReviewRunCancelled, "", time.Minute)}, ReviewStateNeedsReview},
		{"approval on an older head does not count", both, []domain.ReviewRun{approved(domain.ReviewerClaudeCode, "sha1", 0), approved(domain.ReviewerCodex, "old", time.Minute)}, ReviewStateNeedsReview},
		{"a reviewer still running keeps the head running", both, []domain.ReviewRun{
			run(domain.ReviewerClaudeCode, "sha1", domain.ReviewRunComplete, domain.VerdictChangesRequested, 0),
			run(domain.ReviewerCodex, "sha1", domain.ReviewRunRunning, "", time.Minute),
		}, ReviewStateRunning},
		{"an unexpected reviewer's changes request blocks", []domain.ReviewerHarness{domain.ReviewerClaudeCode}, []domain.ReviewRun{
			approved(domain.ReviewerClaudeCode, "sha1", time.Minute),
			run(domain.ReviewerCodex, "sha1", domain.ReviewRunComplete, domain.VerdictChangesRequested, 0),
		}, ReviewStateChangesRequested},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanForReviewers([]domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha1"}}, tt.runs, tt.expected)
			if got[0].Status != tt.want {
				t.Fatalf("status = %q, want %q", got[0].Status, tt.want)
			}
		})
	}
}

// Triggering a later configured reviewer by harness launches it with the
// agent config configured for it, not an empty one.
func TestTriggerConfiguredReviewerUsesItsAgentConfig(t *testing.T) {
	projects := fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{
		{Harness: domain.ReviewerClaudeCode, AgentConfig: domain.AgentConfig{Model: "a"}},
		{Harness: domain.ReviewerCodex, AgentConfig: domain.AgentConfig{Model: "b"}},
	}}}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(&fakeStore{}, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), projects, launcher)

	if _, err := trigger(context.Background(), eng, "mer-1", domain.ReviewerCodex, domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if launcher.gotSpec.Harness != domain.ReviewerCodex || launcher.gotSpec.AgentConfig.Model != "b" {
		t.Fatalf("launched %q with model %q, want codex with model b", launcher.gotSpec.Harness, launcher.gotSpec.AgentConfig.Model)
	}
}

// An explicit config still overrides the configured reviewer's config.
func TestTriggerConfiguredReviewerMergesExplicitConfig(t *testing.T) {
	projects := fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{
		{Harness: domain.ReviewerClaudeCode, AgentConfig: domain.AgentConfig{Model: "a"}},
		{Harness: domain.ReviewerCodex, AgentConfig: domain.AgentConfig{Model: "b", Effort: "high"}},
	}}}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(&fakeStore{}, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), projects, launcher)

	if _, err := trigger(context.Background(), eng, "mer-1", domain.ReviewerCodex, domain.AgentConfig{Model: "c"}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if got := launcher.gotSpec.AgentConfig; got.Model != "c" || got.Effort != "high" {
		t.Fatalf("launched with %+v, want model c and effort high", got)
	}
}

// --- review cycles (Spec 2) ---

const cyclePR = "https://github.com/o/r/pull/1"

// mutablePRs lets a test move the PR head after the engine is built.
type mutablePRs struct{ prs []domain.PullRequest }

func (f *mutablePRs) ListPRsBySession(context.Context, domain.SessionID) ([]domain.PullRequest, error) {
	return f.prs, nil
}

// mutableProjects lets a test edit the project's reviewers mid-cycle.
type mutableProjects struct{ cfg domain.ProjectConfig }

func (f *mutableProjects) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	return domain.ProjectRecord{ID: id, Config: f.cfg}, true, nil
}

func launchedHarnesses(l *fakeLauncher) []domain.ReviewerHarness {
	out := make([]domain.ReviewerHarness, len(l.specs))
	for i, spec := range l.specs {
		out[i] = spec.Harness
	}
	return out
}

// finishRuns records verdict/status on every running run of harness.
func finishRuns(t *testing.T, store *fakeStore, harness domain.ReviewerHarness, status domain.ReviewRunStatus, verdict domain.ReviewVerdict) {
	t.Helper()
	finished := 0
	for _, run := range store.runs {
		if run.Harness == harness && run.Status == domain.ReviewRunRunning {
			if _, err := store.UpdateReviewRunResult(context.Background(), run.ID, status, verdict, "body", "", false); err != nil {
				t.Fatalf("finish run: %v", err)
			}
			finished++
		}
	}
	if finished == 0 {
		t.Fatalf("no running %s run to finish; runs = %+v", harness, store.runs)
	}
}

// steppingClock moves one second per read, so each pass on a head is newer
// than the one before it, as it is outside tests.
func steppingClock() func() time.Time {
	now := time.Unix(0, 0).UTC()
	return func() time.Time {
		now = now.Add(time.Second)
		return now
	}
}

// waitingHarnesses lists, across open cycles, the members still waiting for a
// pass on their cycle's head.
func waitingHarnesses(store *fakeStore) []domain.ReviewerHarness {
	var out []domain.ReviewerHarness
	for _, c := range openCycles(store) {
		for _, m := range c.Members {
			if memberWaiting(c, m, store.runs) {
				out = append(out, m.Harness)
			}
		}
	}
	return out
}

func openCycles(store *fakeStore) []domain.ReviewCycle {
	var out []domain.ReviewCycle
	for _, c := range store.cycles {
		if c.Open() {
			out = append(out, c)
		}
	}
	return out
}

func TestTriggerOpensCycleWithEveryExpectedReviewer(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)

	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	cycles := openCycles(store)
	if len(cycles) != 1 {
		t.Fatalf("open cycles = %+v, want one", cycles)
	}
	c := cycles[0]
	if c.PRURL != cyclePR || c.TargetSHA != "sha1" || c.SessionID != "mer-1" {
		t.Fatalf("cycle = %+v, want mer-1 PR 1 at sha1", c)
	}
	if len(c.Members) != 2 || c.Members[0].Harness != domain.ReviewerClaudeCode || c.Members[1].Harness != domain.ReviewerCodex {
		t.Fatalf("members = %+v, want claude-code then codex", c.Members)
	}
	for _, m := range c.Members {
		if m.QueuedAt.IsZero() || m.QueuedSource != domain.ReviewTriggerManual {
			t.Fatalf("member %+v, want queued by the manual trigger", m)
		}
	}
	// claude-code's pass answered its queueing; codex still waits.
	if got := waitingHarnesses(store); len(got) != 1 || got[0] != domain.ReviewerCodex {
		t.Fatalf("waiting = %v, want codex", got)
	}
}

func TestAdvanceCyclesLaunchesNextMemberAfterFirstFinishes(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  domain.ReviewRunStatus
		verdict domain.ReviewVerdict
	}{
		{"approved", domain.ReviewRunComplete, domain.VerdictApproved},
		{"changes requested", domain.ReviewRunComplete, domain.VerdictChangesRequested},
		{"failed", domain.ReviewRunFailed, domain.VerdictNone},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeStore{}
			launcher := &fakeLauncher{handle: "review-mer-1"}
			eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)
			if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
				t.Fatalf("Trigger: %v", err)
			}
			finishRuns(t, store, domain.ReviewerClaudeCode, tt.status, tt.verdict)

			result, err := eng.AdvanceCycles(context.Background(), "mer-1")
			if err != nil {
				t.Fatalf("AdvanceCycles: %v", err)
			}
			got := launchedHarnesses(launcher)
			if len(got) != 2 || got[1] != domain.ReviewerCodex {
				t.Fatalf("launched = %v, want claude-code then codex", got)
			}
			if !result.Created || result.Run.Harness != domain.ReviewerCodex || result.Run.TargetSHA != "sha1" {
				t.Fatalf("result = %+v, want a created codex run on sha1", result)
			}
			if got := waitingHarnesses(store); len(got) != 0 {
				t.Fatalf("waiting = %v after codex launched, want nobody", got)
			}
		})
	}
}

func TestAdvanceCyclesWaitsWhileAReviewerRuns(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	result, err := eng.AdvanceCycles(context.Background(), "mer-1")
	if err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if result.Created || len(launcher.specs) != 1 {
		t.Fatalf("launched = %v created=%v, want nothing new while claude-code runs", launchedHarnesses(launcher), result.Created)
	}
	if got := waitingHarnesses(store); len(got) != 1 || got[0] != domain.ReviewerCodex {
		t.Fatalf("waiting = %v, want codex still queued", got)
	}
}

func TestAdvanceCyclesDoesNothingWhenNoMemberIsPending(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	if _, err := eng.AdvanceCycles(context.Background(), "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	finishRuns(t, store, domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictApproved)

	for i := 0; i < 2; i++ {
		result, err := eng.AdvanceCycles(context.Background(), "mer-1")
		if err != nil {
			t.Fatalf("AdvanceCycles: %v", err)
		}
		if result.Created {
			t.Fatalf("call %d created %+v, want nothing", i, result.Run)
		}
	}
	if len(launcher.specs) != 2 {
		t.Fatalf("launched = %v, want exactly claude-code and codex", launchedHarnesses(launcher))
	}
}

func TestAdvanceCyclesSupersedesCycleWhenHeadMoves(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	prs := &mutablePRs{prs: prAt("sha1").prs}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prs, twoConfiguredReviewers(), launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	prs.prs = prAt("sha2").prs

	result, err := eng.AdvanceCycles(context.Background(), "mer-1")
	if err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if result.Created || len(launcher.specs) != 1 {
		t.Fatalf("launched = %v, want nothing on the old head", launchedHarnesses(launcher))
	}
	if len(store.cycles) != 1 || store.cycles[0].Open() || store.cycles[0].SupersededReason == "" {
		t.Fatalf("cycles = %+v, want the sha1 cycle superseded", store.cycles)
	}
}

func TestOpenCycleKeepsItsMembersWhenConfigChanges(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	projects := &mutableProjects{cfg: twoConfiguredReviewers().cfg}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), projects, launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	projects.cfg = domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode}, {Harness: domain.ReviewerOpenCode}}}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)

	if _, err := eng.AdvanceCycles(context.Background(), "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 2 || got[1] != domain.ReviewerCodex {
		t.Fatalf("launched = %v, want the cycle's codex, not the newly configured opencode", got)
	}
	if got := openCycles(store)[0].Harnesses(); len(got) != 2 || got[1] != domain.ReviewerCodex {
		t.Fatalf("cycle members = %v, want unchanged", got)
	}
}

func TestListExpectsTheOpenCycleMembers(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	projects := &mutableProjects{cfg: twoConfiguredReviewers().cfg}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), projects, launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	// Dropping codex from the config must not approve the head the open cycle
	// still holds codex for.
	projects.cfg = domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode}}}

	list, err := eng.List(context.Background(), "mer-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Reviews) != 1 || list.Reviews[0].Status != ReviewStateNeedsReview {
		t.Fatalf("reviews = %+v, want needs_review while codex owes the head", list.Reviews)
	}
}

func TestManualRetriggerRequeuesOnlyMembersThatDidNotApprove(t *testing.T) {
	store := &fakeStore{
		runs: []domain.ReviewRun{
			{ID: "run-a", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved},
			{ID: "run-b", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested},
		},
		cycles: []domain.ReviewCycle{{
			ID: "cycle-1", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1",
			Members: []domain.ReviewCycleMember{
				{Position: 0, Harness: domain.ReviewerClaudeCode},
				{Position: 1, Harness: domain.ReviewerCodex},
			},
		}},
	}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)

	result, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 1 || got[0] != domain.ReviewerCodex {
		t.Fatalf("launched = %v, want only codex re-reviewing", got)
	}
	if !result.Created {
		t.Fatalf("result = %+v, want created", result)
	}
	if len(store.cycles) != 1 {
		t.Fatalf("cycles = %+v, want the open cycle reused", store.cycles)
	}
}

// A named reviewer keeps today's single-reviewer path and opens no cycle.
func TestNamedReviewerTriggerOpensNoCycle(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)

	if _, err := trigger(context.Background(), eng, "mer-1", domain.ReviewerCodex, domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if len(store.cycles) != 0 {
		t.Fatalf("cycles = %+v, want none for a named reviewer", store.cycles)
	}
}

// A member whose launch fails gets a failed run and leaves the queue, so the
// next step moves on instead of retrying it forever.
func TestAdvanceCyclesFailedLaunchClearsTheMember(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	launcher.preflightErr = errors.New("codex missing")

	if _, err := eng.AdvanceCycles(context.Background(), "mer-1"); err == nil {
		t.Fatal("AdvanceCycles: want the launch error")
	}
	if got := waitingHarnesses(store); len(got) != 0 {
		t.Fatalf("waiting = %v after codex's launch recorded a failed pass", got)
	}
	last := store.runs[len(store.runs)-1]
	if last.Harness != domain.ReviewerCodex || last.Status != domain.ReviewRunFailed {
		t.Fatalf("last run = %+v, want a failed codex run", last)
	}
}

// `ao review trigger` (which rejects reviewed heads) still starts a reviewer
// the head owes when another reviewer already approved it.
func TestRejectReviewedHeadTriggerStartsOwedReviewer(t *testing.T) {
	store := &fakeStore{runs: []domain.ReviewRun{{
		ID: "run-1", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1",
		Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
	}}}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)
	eng.clock = steppingClock()

	if _, err := eng.TriggerWithOptions(context.Background(), "mer-1", TriggerOptions{Source: domain.ReviewTriggerAgent, RejectReviewedHead: true}); err != nil {
		t.Fatalf("TriggerWithOptions: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 1 || got[0] != domain.ReviewerCodex {
		t.Fatalf("launched = %v, want codex", got)
	}
	// A changes request is re-queued by the same trigger; approvals stand.
	finishRuns(t, store, domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	if _, err := eng.TriggerWithOptions(context.Background(), "mer-1", TriggerOptions{Source: domain.ReviewTriggerAgent, RejectReviewedHead: true}); err != nil {
		t.Fatalf("re-trigger after changes requested: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 2 || got[1] != domain.ReviewerCodex {
		t.Fatalf("launched = %v, want codex again and claude-code not re-run", got)
	}
	// Once every member approved the head, nothing is owed and the trigger
	// is told so.
	finishRuns(t, store, domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictApproved)
	_, err := eng.TriggerWithOptions(context.Background(), "mer-1", TriggerOptions{Source: domain.ReviewTriggerAgent, RejectReviewedHead: true})
	if !errors.Is(err, ErrHeadAlreadyReviewed) {
		t.Fatalf("err = %v, want ErrHeadAlreadyReviewed", err)
	}
}

// One reviewer follows the same rule: a plain trigger re-runs a reviewer that
// requested changes on the same head (main refused any reviewed head).
func TestPlainTriggerRequeuesSingleReviewerThatRequestedChanges(t *testing.T) {
	store := &fakeStore{runs: []domain.ReviewRun{{
		ID: "run-1", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1",
		Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested,
	}}}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	projects := fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode}}}}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), projects, launcher)

	if _, err := eng.TriggerWithOptions(context.Background(), "mer-1", TriggerOptions{Source: domain.ReviewTriggerAgent, RejectReviewedHead: true}); err != nil {
		t.Fatalf("TriggerWithOptions: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 1 || got[0] != domain.ReviewerClaudeCode {
		t.Fatalf("launched = %v, want claude-code re-run", got)
	}
}

// --rerun re-runs every expected reviewer, approvals included, and an earlier
// approval never satisfies the new run: until a re-queued reviewer reviews
// again, the head is not up to date.
func TestRerunRequeuesEveryMemberAndOldApprovalsDoNotCount(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)
	eng.clock = steppingClock()
	if _, err := trigger(ctx, eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	finishRuns(t, store, domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictApproved)

	if _, err := eng.TriggerWithOptions(ctx, "mer-1", TriggerOptions{Source: domain.ReviewTriggerManual, Rerun: true}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 3 || got[2] != domain.ReviewerClaudeCode {
		t.Fatalf("launched = %v, want claude-code re-run first", got)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	list, err := eng.List(ctx, "mer-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := list.Reviews[0].Status; got == ReviewStateUpToDate {
		t.Fatalf("status = %q while codex is queued for the rerun; its old approval must not count", got)
	}
	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 4 || got[3] != domain.ReviewerCodex {
		t.Fatalf("launched = %v, want codex re-run even though it approved before", got)
	}
}

// A PR closed while the queue step after a submit is still pending closes its
// cycle and launches nothing.
func TestAdvanceCyclesAfterPRClosedLaunchesNothing(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	prs := &mutablePRs{prs: []domain.PullRequest{{URL: cyclePR, Number: 1, HeadSHA: "sha1"}}}
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prs, twoConfiguredReviewers(), launcher)
	if _, err := trigger(ctx, eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	prs.prs[0].Merged = true

	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 1 {
		t.Fatalf("launched = %v, want nothing after the PR merged", got)
	}
	if len(openCycles(store)) != 0 || store.cycles[0].SupersededReason != cycleSupersededPRNotOpen {
		t.Fatalf("cycles = %+v, want the cycle closed as no longer open", store.cycles)
	}
}

// A run recorded without a reviewer counts only for the first expected
// reviewer, and only on its own PR head.
func TestLegacyRunSatisfiesOnlyFirstReviewerOnItsHead(t *testing.T) {
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	legacy := domain.ReviewRun{ID: "old", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: t0}
	prs := []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha1"}}

	if got := PlanForReviewers(prs, []domain.ReviewRun{legacy}, []domain.ReviewerHarness{domain.ReviewerClaudeCode})[0].Status; got != ReviewStateUpToDate {
		t.Fatalf("one reviewer: status = %q, want up_to_date", got)
	}
	if got := PlanForReviewers(prs, []domain.ReviewRun{legacy}, []domain.ReviewerHarness{domain.ReviewerClaudeCode, domain.ReviewerCodex})[0].Status; got == ReviewStateUpToDate {
		t.Fatal("a legacy approval must not satisfy two reviewers")
	}
	moved := []domain.PullRequest{{URL: "pr1", Number: 1, HeadSHA: "sha2"}}
	if got := PlanForReviewers(moved, []domain.ReviewRun{legacy}, []domain.ReviewerHarness{domain.ReviewerClaudeCode})[0].Status; got == ReviewStateUpToDate {
		t.Fatal("a legacy approval must not carry over to another head")
	}
}

// --- independent review regressions (Spec 2) ---

// newCycleEngine is newEngineForTest with a clock that moves, so passes and
// queueings are ordered as they are outside tests.
func newCycleEngine(store *fakeStore, prs PRs, projects Projects, launcher *fakeLauncher) *Engine {
	eng := newEngineForTest(store, fakeSessions{rec: liveWorker(), ok: true}, prs, projects, launcher)
	eng.clock = steppingClock()
	return eng
}

func approveBothReviewers(t *testing.T, eng *Engine, store *fakeStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := trigger(ctx, eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	finishRuns(t, store, domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictApproved)
}

func headStatus(t *testing.T, eng *Engine) StateStatus {
	t.Helper()
	list, err := eng.List(context.Background(), "mer-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return list.Reviews[0].Status
}

// A rerun whose launch fails before recording a pass leaves the reviewer
// queued: its approval from before the rerun never counts again.
func TestRerunLaunchErrorBeforePassKeepsReviewerQueued(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), launcher)
	approveBothReviewers(t, eng, store)
	before := len(store.runs)

	launcher.destroyErr = errors.New("tmux busy")
	if _, err := eng.TriggerWithOptions(ctx, "mer-1", TriggerOptions{Source: domain.ReviewTriggerManual, Rerun: true}); err == nil {
		t.Fatal("rerun: want the launch error")
	}
	if len(store.runs) != before {
		t.Fatalf("runs = %d, want no pass recorded by the failed launch", len(store.runs))
	}
	if got := waitingHarnesses(store); len(got) != 2 {
		t.Fatalf("waiting = %v, want both reviewers still queued", got)
	}
	if got := headStatus(t, eng); got == ReviewStateUpToDate {
		t.Fatalf("status = %q, want old approvals not to count", got)
	}
	launcher.destroyErr = nil
	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := launchedHarnesses(launcher); got[len(got)-1] != domain.ReviewerClaudeCode {
		t.Fatalf("launched = %v, want claude-code's rerun retried", got)
	}
}

// After --rerun, the rerun's own verdict decides, not the approval before it.
func TestRerunVerdictReplacesTheEarlierApproval(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), launcher)
	approveBothReviewers(t, eng, store)
	if _, err := eng.TriggerWithOptions(ctx, "mer-1", TriggerOptions{Source: domain.ReviewTriggerManual, Rerun: true}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	finishRuns(t, store, domain.ReviewerCodex, domain.ReviewRunComplete, domain.VerdictApproved)
	if got := headStatus(t, eng); got != ReviewStateChangesRequested {
		t.Fatalf("status = %q, want the rerun's changes request", got)
	}
}

// A legacy approval (no reviewer recorded) belongs to the first reviewer; the
// second reviewer is still launched, manually and automatically.
func TestLegacyApprovalStillLaunchesTheSecondReviewer(t *testing.T) {
	for _, source := range []domain.ReviewTriggerSource{domain.ReviewTriggerManual, domain.ReviewTriggerAuto} {
		t.Run(string(source), func(t *testing.T) {
			store := &fakeStore{runs: []domain.ReviewRun{{
				ID: "old", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1",
				Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved,
			}}}
			launcher := &fakeLauncher{handle: "review-mer-1"}
			eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), launcher)

			if _, err := eng.TriggerWithOptions(context.Background(), "mer-1", TriggerOptions{Source: source}); err != nil {
				t.Fatalf("TriggerWithOptions: %v", err)
			}
			if got := launchedHarnesses(launcher); len(got) != 1 || got[0] != domain.ReviewerCodex {
				t.Fatalf("launched = %v, want codex", got)
			}
		})
	}
}

// An automatic retry named from stale facts waits while another reviewer
// runs: one reviewer at a time per worker.
func TestNamedAutoRetryWaitsWhileAnotherReviewerRuns(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1", alive: true}
	eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), launcher)
	if _, err := trigger(ctx, eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}

	res, err := eng.TriggerWithOptions(ctx, "mer-1", TriggerOptions{Harness: domain.ReviewerCodex, Source: domain.ReviewTriggerAuto})
	if err != nil {
		t.Fatalf("auto retry: %v", err)
	}
	if res.SkipReason != "review_running" || len(launcher.specs) != 1 {
		t.Fatalf("result = %+v launched = %v, want review_running and codex not started", res, launchedHarnesses(launcher))
	}
}

// A reviewer whose pane died without submitting is failed by the sweep's
// queue step, which then starts the next reviewer.
func TestDeadReviewerPaneAdvancesTheQueue(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1", alive: true}
	eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// The fake reports a pane alive while alive or spawned is set.
	launcher.alive, launcher.spawned = false, false

	if _, err := eng.AdvanceCycles(context.Background(), "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 2 || got[1] != domain.ReviewerCodex {
		t.Fatalf("launched = %v, want codex after claude-code's pane died", got)
	}
}

// A crash between recording a reviewer's pass and saving its pane leaves an
// orphan running pass; the next queue step fails it and moves on.
func TestOrphanedReviewerLaunchIsFailedSoTheQueueMoves(t *testing.T) {
	store := &fakeStore{
		runs: []domain.ReviewRun{
			{ID: "a", SessionID: "mer-1", ReviewID: "rev-a", PRURL: cyclePR, TargetSHA: "sha1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved},
			{ID: "b", SessionID: "mer-1", ReviewID: "rev-b", PRURL: cyclePR, TargetSHA: "sha1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunRunning},
		},
		cycles: []domain.ReviewCycle{{ID: "c1", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1", Members: []domain.ReviewCycleMember{
			{Position: 0, Harness: domain.ReviewerClaudeCode}, {Position: 1, Harness: domain.ReviewerCodex}}}},
		reviews: map[domain.ReviewerHarness]domain.Review{
			domain.ReviewerCodex: {ID: "rev-b", SessionID: "mer-1", Harness: domain.ReviewerCodex, InterfaceMode: domain.ReviewerInterfaceTUI, ReviewerActivityState: domain.ActivityActive},
		},
	}
	eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), &fakeLauncher{handle: "review-mer-1"})

	if _, err := eng.AdvanceCycles(context.Background(), "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := store.runs[1]; got.Status != domain.ReviewRunFailed || got.Body != reviewerLaunchIncomplete {
		t.Fatalf("orphan run = %+v, want failed as an incomplete launch", got)
	}
}

// A worker that ended closes its cycles, so the sweep stops visiting it.
func TestAdvanceCyclesClosesCyclesOfAnEndedWorker(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	worker := liveWorker()
	eng := newEngineForTest(store, fakeSessions{rec: worker, ok: true}, prAt("sha1"), twoConfiguredReviewers(), launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	worker.IsTerminated = true
	eng.sessions = fakeSessions{rec: worker, ok: true}

	if _, err := eng.AdvanceCycles(context.Background(), "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if len(openCycles(store)) != 0 || store.cycles[0].SupersededReason != cycleSupersededWorkerEnded {
		t.Fatalf("cycles = %+v, want closed because the worker ended", store.cycles)
	}
}

func twoPRs() *mutablePRs {
	return &mutablePRs{prs: []domain.PullRequest{
		{URL: cyclePR, Number: 1, HeadSHA: "sha1"},
		{URL: "https://github.com/o/r/pull/2", Number: 2, HeadSHA: "shaB"},
	}}
}

// One launch reviews every head that owes the reviewer, in one batch.
func TestQueueLaunchBatchesEveryHeadThatOwesTheReviewer(t *testing.T) {
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newCycleEngine(store, twoPRs(), twoConfiguredReviewers(), launcher)
	if _, err := trigger(context.Background(), eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	heads := map[string]bool{}
	for _, run := range store.runs {
		if run.Harness == domain.ReviewerClaudeCode {
			heads[run.PRURL] = true
		}
	}
	if len(launcher.specs) != 1 || len(heads) != 2 {
		t.Fatalf("launched = %v heads = %v, want one claude-code launch covering both PRs", launchedHarnesses(launcher), heads)
	}
}

// A trigger for one PR starts that PR's reviewer before another PR's, even
// when an older cycle of another PR has a reviewer waiting.
func TestPRTargetedTriggerStartsThatPRFirst(t *testing.T) {
	pr2 := "https://github.com/o/r/pull/2"
	t0 := time.Unix(-100, 0).UTC()
	store := &fakeStore{
		runs: []domain.ReviewRun{{ID: "old", SessionID: "mer-1", PRURL: pr2, TargetSHA: "shaB", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: t0}},
		cycles: []domain.ReviewCycle{{ID: "c2", SessionID: "mer-1", PRURL: pr2, TargetSHA: "shaB", CreatedAt: t0, Members: []domain.ReviewCycleMember{
			{Position: 0, Harness: domain.ReviewerClaudeCode},
			{Position: 1, Harness: domain.ReviewerCodex, QueuedAt: t0, QueuedSource: domain.ReviewTriggerManual},
		}}},
	}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newCycleEngine(store, twoPRs(), twoConfiguredReviewers(), launcher)

	if _, err := eng.TriggerWithOptions(context.Background(), "mer-1", TriggerOptions{Source: domain.ReviewTriggerManual, PRURL: cyclePR}); err != nil {
		t.Fatalf("TriggerWithOptions: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 1 || got[0] != domain.ReviewerClaudeCode {
		t.Fatalf("launched = %v, want claude-code for PR 1", got)
	}
	if last := store.runs[len(store.runs)-1]; last.PRURL != cyclePR {
		t.Fatalf("launched run = %+v, want PR 1", last)
	}
}

// A queued reviewer runs with the agent config frozen in its cycle, not one
// merged with a later config edit, and under the source that queued it.
func TestQueuedReviewerKeepsItsFrozenConfigAndQueueingSource(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	projects := &mutableProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{
		{Harness: domain.ReviewerClaudeCode},
		{Harness: domain.ReviewerCodex, AgentConfig: domain.AgentConfig{Model: "frozen"}},
	}}}
	eng := newCycleEngine(store, prAt("sha1"), projects, launcher)
	if _, err := eng.TriggerWithOptions(ctx, "mer-1", TriggerOptions{Source: domain.ReviewTriggerAgent}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	projects.cfg.Reviewers[1].AgentConfig = domain.AgentConfig{Model: "edited", Effort: "high"}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)

	// The queue step on its own (as after a submit) launches codex.
	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := launcher.gotSpec.AgentConfig; got.Model != "frozen" || got.Effort != "" {
		t.Fatalf("codex config = %+v, want the frozen snapshot only", got)
	}
	if last := store.runs[len(store.runs)-1]; last.Harness != domain.ReviewerCodex || last.TriggerSource != domain.ReviewTriggerAgent {
		t.Fatalf("codex run = %+v, want it recorded under the agent trigger that queued it", last)
	}
}

// A wall clock that steps back after a reviewer was queued must not hide the
// pass that answers the queueing.
func TestPassAnswersItsQueueingDespiteAClockStepBack(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), launcher)
	base := time.Unix(1000, 0).UTC()
	calls := 0
	eng.clock = func() time.Time {
		calls++
		if calls == 1 { // the queueing stamp, taken while the clock ran 5s ahead
			return base.Add(5 * time.Second)
		}
		return base.Add(time.Duration(calls) * time.Millisecond)
	}
	if _, err := trigger(ctx, eng, "mer-1", "", domain.AgentConfig{}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 2 || got[1] != domain.ReviewerCodex {
		t.Fatalf("launched = %v, want claude-code then codex (claude-code's pass answered its queueing)", got)
	}
}

// A legacy approval belongs to the first reviewer only, so it does not block
// the automatic retry of the second reviewer's failed pass.
func TestLegacyApprovalDoesNotBlockTheSecondReviewersAutoRetry(t *testing.T) {
	q := time.Unix(10, 0).UTC()
	store := &fakeStore{
		runs: []domain.ReviewRun{
			{ID: "old", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: time.Unix(1, 0).UTC()},
			{ID: "b1", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1", Harness: domain.ReviewerCodex, TriggerSource: domain.ReviewTriggerAuto, Status: domain.ReviewRunFailed, CreatedAt: time.Unix(20, 0).UTC()},
		},
		cycles: []domain.ReviewCycle{{ID: "c1", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1", CreatedAt: q, Members: []domain.ReviewCycleMember{
			{Position: 0, Harness: domain.ReviewerClaudeCode}, {Position: 1, Harness: domain.ReviewerCodex, QueuedAt: q, QueuedSource: domain.ReviewTriggerAuto}}}},
	}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), launcher)
	eng.clock = func() time.Time { q = q.Add(time.Minute); return q }

	res, err := eng.TriggerWithOptions(context.Background(), "mer-1", TriggerOptions{Harness: domain.ReviewerCodex, Source: domain.ReviewTriggerAuto})
	if err != nil {
		t.Fatalf("auto retry: %v", err)
	}
	if !res.Created || launchedHarnesses(launcher)[0] != domain.ReviewerCodex {
		t.Fatalf("result = %+v launched = %v, want codex retried", res, launchedHarnesses(launcher))
	}
}

// An automatic retry by name reviews only the heads whose open cycle lists
// that reviewer: it never adds a reviewer to another PR's frozen cycle.
func TestNamedAutoRetryStaysOnHeadsWhoseCycleListsTheReviewer(t *testing.T) {
	pr2 := "https://github.com/o/r/pull/2"
	at := func(s int64) time.Time { return time.Unix(s, 0).UTC() }
	run := func(id, pr, sha string, h domain.ReviewerHarness, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, s int64) domain.ReviewRun {
		return domain.ReviewRun{ID: id, SessionID: "mer-1", PRURL: pr, TargetSHA: sha, Harness: h, TriggerSource: domain.ReviewTriggerAuto, Status: status, Verdict: verdict, CreatedAt: at(s)}
	}
	store := &fakeStore{
		runs: []domain.ReviewRun{
			run("a1", cyclePR, "sha1", domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, 1),
			run("b1", cyclePR, "sha1", domain.ReviewerCodex, domain.ReviewRunFailed, "", 2),
			run("a2", pr2, "shaB", domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved, 3),
			run("o2", pr2, "shaB", domain.ReviewerOpenCode, domain.ReviewRunComplete, domain.VerdictApproved, 4),
		},
		cycles: []domain.ReviewCycle{
			{ID: "c1", SessionID: "mer-1", PRURL: cyclePR, TargetSHA: "sha1", CreatedAt: at(0), Members: []domain.ReviewCycleMember{{Position: 0, Harness: domain.ReviewerClaudeCode}, {Position: 1, Harness: domain.ReviewerCodex}}},
			{ID: "c2", SessionID: "mer-1", PRURL: pr2, TargetSHA: "shaB", CreatedAt: at(0), Members: []domain.ReviewCycleMember{{Position: 0, Harness: domain.ReviewerClaudeCode}, {Position: 1, Harness: domain.ReviewerOpenCode}}},
		},
	}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newCycleEngine(store, twoPRs(), twoConfiguredReviewers(), launcher)
	before := len(store.runs)

	if _, err := eng.TriggerWithOptions(context.Background(), "mer-1", TriggerOptions{Harness: domain.ReviewerCodex, Source: domain.ReviewTriggerAuto}); err != nil {
		t.Fatalf("auto retry: %v", err)
	}
	for _, r := range store.runs[before:] {
		if r.PRURL == pr2 {
			t.Fatalf("codex launched on PR 2, whose frozen cycle is [claude-code, opencode]: %+v", r)
		}
	}
}

// After a backward clock step, a --rerun is still answered only by a pass that
// starts after it: an earlier pass stamped in the future does not count.
func TestRerunAfterAClockStepBackIsNotAnsweredByAnEarlierPass(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	launcher := &fakeLauncher{handle: "review-mer-1"}
	eng := newCycleEngine(store, prAt("sha1"), twoConfiguredReviewers(), launcher)
	base := time.Unix(1000, 0).UTC()
	calls := 0
	eng.clock = func() time.Time {
		calls++
		if calls == 1 { // the first queueing stamp, taken while the clock ran an hour ahead
			return base.Add(time.Hour)
		}
		return base.Add(time.Duration(calls) * time.Millisecond)
	}
	approveBothReviewers(t, eng, store)
	if _, err := eng.TriggerWithOptions(ctx, "mer-1", TriggerOptions{Source: domain.ReviewTriggerManual, Rerun: true}); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	finishRuns(t, store, domain.ReviewerClaudeCode, domain.ReviewRunComplete, domain.VerdictApproved)
	if _, err := eng.AdvanceCycles(ctx, "mer-1"); err != nil {
		t.Fatalf("AdvanceCycles: %v", err)
	}
	if got := launchedHarnesses(launcher); len(got) != 4 || got[3] != domain.ReviewerCodex {
		t.Fatalf("launched = %v, want codex re-run after claude-code", got)
	}
}
