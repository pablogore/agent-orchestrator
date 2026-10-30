package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/pkg/contract"
)

// #5465: one reviewer's approval must not approve the head while another
// reviewer's pass on the same head failed without a verdict.
func TestKanbanFailedReviewerBlocksApprovalOfCurrentHead(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prs := []domain.PRFacts{{URL: "pr1", Number: 1, HeadSHA: "sha1", Mergeability: domain.MergeMergeable}}
	runs := []domain.CurrentHeadReviewRun{
		{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now},
		{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunFailed, CreatedAt: now.Add(time.Minute)},
	}
	session := contract.KanbanSessionFacts{AutoReview: true}

	got := contract.DeriveKanbanPresentation(session, toContractKanbanPRFacts(prs, runs, expectedReviewers{}), now, time.Minute)
	if got.Column != contract.KanbanValidating {
		t.Fatalf("column = %q, want validating while codex has not approved sha1", got.Column)
	}
}

func TestKanbanApprovalRequiresEveryExpectedReviewer(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prs := []domain.PRFacts{{URL: "pr1", Number: 1, HeadSHA: "sha1", Mergeability: domain.MergeMergeable}}
	approvedByClaude := []domain.CurrentHeadReviewRun{
		{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now},
	}
	session := contract.KanbanSessionFacts{AutoReview: true}

	for _, tc := range []struct {
		name     string
		expected []domain.ReviewerHarness
		runs     []domain.CurrentHeadReviewRun
		want     contract.KanbanColumn
	}{
		{"expected reviewer never ran", []domain.ReviewerHarness{domain.ReviewerClaudeCode, domain.ReviewerCodex}, approvedByClaude, contract.KanbanValidating},
		{"single expected reviewer approved", []domain.ReviewerHarness{domain.ReviewerClaudeCode}, approvedByClaude, contract.KanbanReady},
		{"no expected list falls back to the reviewers that ran", nil, approvedByClaude, contract.KanbanReady},
		{"every expected reviewer approved", []domain.ReviewerHarness{domain.ReviewerClaudeCode, domain.ReviewerCodex}, append(append([]domain.CurrentHeadReviewRun(nil), approvedByClaude...),
			domain.CurrentHeadReviewRun{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now}), contract.KanbanReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := contract.DeriveKanbanPresentation(session, toContractKanbanPRFacts(prs, tc.runs, expectedReviewers{project: tc.expected}), now, time.Minute)
			if got.Column != tc.want {
				t.Fatalf("column = %q, want %q", got.Column, tc.want)
			}
		})
	}
}

func TestKanbanPRWithoutAORunsIgnoresExpectedReviewers(t *testing.T) {
	prs := []domain.PRFacts{{URL: "pr1", Mergeability: domain.MergeMergeable}}
	got := toContractKanbanPRFacts(prs, nil, expectedReviewers{project: []domain.ReviewerHarness{domain.ReviewerClaudeCode, domain.ReviewerCodex}})
	if got[0].ReviewRun != (contract.KanbanReviewRunFacts{}) {
		t.Fatalf("review run facts = %+v, want zero value for a PR AO never reviewed", got[0].ReviewRun)
	}
}

// countingProjectStore counts project lookups and can fail them, so the list
// path's per-project cache and error handling are observable.
type countingProjectStore struct {
	*fakeStore
	calls int
	err   error
}

func (c *countingProjectStore) GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error) {
	c.calls++
	if c.err != nil {
		return domain.ProjectRecord{}, false, c.err
	}
	return c.fakeStore.GetProject(ctx, id)
}

func multiReviewerFixture(st *fakeStore, ids ...domain.SessionID) []domain.SessionRecord {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{
		{Harness: domain.ReviewerClaudeCode}, {Harness: domain.ReviewerCodex},
	}}}
	recs := make([]domain.SessionRecord, 0, len(ids))
	for _, id := range ids {
		url := "https://github.com/o/r/pull/" + string(id)
		rec := domain.SessionRecord{ID: id, ProjectID: "mer", UpdatedAt: now, AutoReviewEnabled: true}
		st.prFacts[id] = []domain.PRFacts{{URL: url, Number: 1, HeadSHA: "sha1", Mergeability: domain.MergeMergeable, UpdatedAt: now}}
		st.reviewRuns[id] = []domain.CurrentHeadReviewRun{{
			SessionID: id, PRURL: url, Harness: domain.ReviewerClaudeCode,
			Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, ID: "run-" + string(id), CreatedAt: now,
		}}
		recs = append(recs, rec)
	}
	return recs
}

func TestSessionReadRequiresEveryProjectReviewerToApprove(t *testing.T) {
	st := newFakeStore()
	rec := multiReviewerFixture(st, "mer-1")[0]
	svc := &Service{store: st, clock: func() time.Time { return rec.UpdatedAt.Add(time.Minute) }}

	sess, err := svc.toSessionWithFacts(context.Background(), rec, st.prFacts[rec.ID], st.reviewRuns[rec.ID], false, "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.KanbanColumn != contract.KanbanValidating {
		t.Fatalf("column = %q, want validating until codex approves", sess.KanbanColumn)
	}
}

func TestSessionListLooksUpEachProjectOnce(t *testing.T) {
	st := &countingProjectStore{fakeStore: newFakeStore()}
	recs := multiReviewerFixture(st.fakeStore, "mer-1", "mer-2")
	svc := &Service{store: st, clock: func() time.Time { return recs[0].UpdatedAt.Add(time.Minute) }}

	sessions, err := svc.sessionsFromRecords(context.Background(), recs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.calls != 1 {
		t.Fatalf("GetProject calls = %d, want 1 for two sessions of one project", st.calls)
	}
	for _, sess := range sessions {
		if sess.KanbanColumn != contract.KanbanValidating {
			t.Fatalf("%s column = %q, want validating until codex approves", sess.ID, sess.KanbanColumn)
		}
	}
}

func TestSessionListUnknownProjectFallsBackToReviewersThatRan(t *testing.T) {
	st := newFakeStore()
	recs := multiReviewerFixture(st, "mer-1")
	delete(st.projects, "mer")
	svc := &Service{store: st, clock: func() time.Time { return recs[0].UpdatedAt.Add(time.Minute) }}

	sessions, err := svc.sessionsFromRecords(context.Background(), recs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sessions[0].KanbanColumn != contract.KanbanReady {
		t.Fatalf("column = %q, want ready when only the reviewer that ran is required", sessions[0].KanbanColumn)
	}
}

func TestSessionListReturnsProjectLookupError(t *testing.T) {
	boom := errors.New("db closed")
	st := &countingProjectStore{fakeStore: newFakeStore(), err: boom}
	recs := multiReviewerFixture(st.fakeStore, "mer-1")
	svc := &Service{store: st, clock: func() time.Time { return recs[0].UpdatedAt.Add(time.Minute) }}

	if _, err := svc.sessionsFromRecords(context.Background(), recs, 0); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want project lookup error", err)
	}
}

// openCycleFixture narrows the project to claude-code alone, as if the config
// was edited after a cycle opened, and gives each session an open cycle whose
// snapshot still owes claude-code and codex for targetSHA.
func openCycleFixture(st *fakeStore, targetSHA string, recs []domain.SessionRecord) {
	project := st.projects["mer"]
	project.Config.Reviewers = []domain.ReviewerConfig{{Harness: domain.ReviewerClaudeCode}}
	st.projects["mer"] = project
	st.reviewCycles = map[domain.SessionID][]domain.ReviewCycle{}
	for _, rec := range recs {
		st.reviewCycles[rec.ID] = []domain.ReviewCycle{{
			ID: "cycle-" + string(rec.ID), SessionID: rec.ID, PRURL: st.prFacts[rec.ID][0].URL, TargetSHA: targetSHA,
			Members: []domain.ReviewCycleMember{
				{Position: 0, Harness: domain.ReviewerClaudeCode},
				{Position: 1, Harness: domain.ReviewerCodex, QueuedAt: time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)},
			},
		}}
	}
}

func TestSessionReadUsesOpenCycleSnapshotOverEditedConfig(t *testing.T) {
	st := newFakeStore()
	recs := multiReviewerFixture(st, "mer-1")
	openCycleFixture(st, "sha1", recs)
	rec := recs[0]
	svc := &Service{store: st, clock: func() time.Time { return rec.UpdatedAt.Add(time.Minute) }}

	sess, err := svc.toSessionWithFacts(context.Background(), rec, st.prFacts[rec.ID], st.reviewRuns[rec.ID], false, "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.KanbanColumn != contract.KanbanValidating {
		t.Fatalf("column = %q, want validating: the cycle for sha1 still owes codex", sess.KanbanColumn)
	}
	if st.listCyclesCalls != 1 {
		t.Fatalf("single-session cycle reads = %d, want 1", st.listCyclesCalls)
	}
}

func TestSessionReadIgnoresCycleForOlderHead(t *testing.T) {
	st := newFakeStore()
	recs := multiReviewerFixture(st, "mer-1")
	openCycleFixture(st, "sha0", recs)
	rec := recs[0]
	svc := &Service{store: st, clock: func() time.Time { return rec.UpdatedAt.Add(time.Minute) }}

	sess, err := svc.toSessionWithFacts(context.Background(), rec, st.prFacts[rec.ID], st.reviewRuns[rec.ID], false, "")
	if err != nil {
		t.Fatal(err)
	}
	if sess.KanbanColumn != contract.KanbanReady {
		t.Fatalf("column = %q, want ready: a cycle for sha0 does not decide sha1, the config (claude-code only) does", sess.KanbanColumn)
	}
}

func TestSessionListReadsOpenCyclesInOneQuery(t *testing.T) {
	st := newFakeStore()
	recs := multiReviewerFixture(st, "mer-1", "mer-2")
	openCycleFixture(st, "sha1", recs)
	svc := &Service{store: st, clock: func() time.Time { return recs[0].UpdatedAt.Add(time.Minute) }}

	sessions, err := svc.sessionsFromRecords(context.Background(), recs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.listCyclesBatchCalls != 1 || st.listCyclesCalls != 0 {
		t.Fatalf("cycle reads: batched = %d, single = %d; want one batched read", st.listCyclesBatchCalls, st.listCyclesCalls)
	}
	for _, sess := range sessions {
		if sess.KanbanColumn != contract.KanbanValidating {
			t.Fatalf("%s column = %q, want validating: the cycle snapshot still owes codex", sess.ID, sess.KanbanColumn)
		}
	}
}

func TestSessionListWithoutReviewRunsSkipsCycleQuery(t *testing.T) {
	st := newFakeStore()
	recs := multiReviewerFixture(st, "mer-1")
	delete(st.reviewRuns, "mer-1")
	svc := &Service{store: st, clock: func() time.Time { return recs[0].UpdatedAt.Add(time.Minute) }}

	if _, err := svc.sessionsFromRecords(context.Background(), recs, 0); err != nil {
		t.Fatal(err)
	}
	if st.listCyclesBatchCalls != 0 {
		t.Fatalf("batched cycle reads = %d, want 0 when no session has review runs", st.listCyclesBatchCalls)
	}
}

// queuedCodexCycle is an open cycle for pr1 owing claude-code then codex, with
// codex queued for a new pass at queuedAt.
func queuedCodexCycle(queuedAt time.Time) expectedReviewers {
	return expectedReviewers{
		byPR: map[string][]domain.ReviewerHarness{"pr1": {domain.ReviewerClaudeCode, domain.ReviewerCodex}},
		members: map[string]map[domain.ReviewerHarness]domain.ReviewCycleMember{"pr1": {
			domain.ReviewerClaudeCode: {Position: 0, Harness: domain.ReviewerClaudeCode},
			domain.ReviewerCodex:      {Position: 1, Harness: domain.ReviewerCodex, QueuedAt: queuedAt},
		}},
	}
}

// A reviewer queued for a new pass (a rerun) owes the head again: its approval
// from before it was queued does not count.
func TestKanbanQueuedReviewerOldApprovalDoesNotCount(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prs := []domain.PRFacts{{URL: "pr1", Number: 1, HeadSHA: "sha1", Mergeability: domain.MergeMergeable}}
	runs := []domain.CurrentHeadReviewRun{
		{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now},
		{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now},
	}

	got := contract.DeriveKanbanPresentation(contract.KanbanSessionFacts{AutoReview: true}, toContractKanbanPRFacts(prs, runs, queuedCodexCycle(now.Add(time.Minute))), now, time.Minute)
	if got.Column != contract.KanbanValidating {
		t.Fatalf("column = %q, want validating while codex is queued", got.Column)
	}
}

// A queued reviewer's pass started at or after it was queued answers the
// queueing: an approval counts, and a pass in flight shows as running.
func TestKanbanQueuedReviewerPassStartedAfterQueueingCounts(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prs := []domain.PRFacts{{URL: "pr1", Number: 1, HeadSHA: "sha1", Mergeability: domain.MergeMergeable}}
	claude := domain.CurrentHeadReviewRun{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now}
	expected := queuedCodexCycle(now)

	approved := []domain.CurrentHeadReviewRun{claude, {SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now}}
	got := contract.DeriveKanbanPresentation(contract.KanbanSessionFacts{AutoReview: true}, toContractKanbanPRFacts(prs, approved, expected), now, time.Minute)
	if got.Column != contract.KanbanReady {
		t.Fatalf("column = %q, want ready: codex approved at its queueing time", got.Column)
	}

	running := []domain.CurrentHeadReviewRun{claude, {SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerCodex, Status: domain.ReviewRunRunning, CreatedAt: now.Add(time.Second)}}
	facts := toContractKanbanPRFacts(prs, running, expected)[0].ReviewRun
	if !facts.Running || facts.Outcome {
		t.Fatalf("facts = %+v, want the re-run shown as running without an outcome", facts)
	}
}

// The store returns a legacy run (no harness) and the first reviewer's own run
// separately; only the newer of the two decides that reviewer, as in the planner.
func TestKanbanLegacyRunFoldsIntoFirstReviewerNewestWins(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prs := []domain.PRFacts{{URL: "pr1", Number: 1, HeadSHA: "sha1", Mergeability: domain.MergeMergeable}}
	session := contract.KanbanSessionFacts{AutoReview: true}
	one := expectedReviewers{project: []domain.ReviewerHarness{domain.ReviewerClaudeCode}}

	failedAfterApproval := []domain.CurrentHeadReviewRun{
		{SessionID: "s1", PRURL: "pr1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now},
		{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunFailed, CreatedAt: now.Add(time.Minute)},
	}
	if got := contract.DeriveKanbanPresentation(session, toContractKanbanPRFacts(prs, failedAfterApproval, one), now, time.Minute); got.Column != contract.KanbanValidating {
		t.Fatalf("legacy approval + newer failure: column = %q, want validating", got.Column)
	}

	approvalAfterChanges := []domain.CurrentHeadReviewRun{
		{SessionID: "s1", PRURL: "pr1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested, CreatedAt: now},
		{SessionID: "s1", PRURL: "pr1", Harness: domain.ReviewerClaudeCode, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now.Add(time.Minute)},
	}
	if got := contract.DeriveKanbanPresentation(session, toContractKanbanPRFacts(prs, approvalAfterChanges, one), now, time.Minute); got.Column != contract.KanbanReady {
		t.Fatalf("legacy changes request + newer approval: column = %q, want ready", got.Column)
	}
}

// A run recorded without a reviewer counts for the first expected reviewer
// only: it cannot satisfy a second one.
func TestKanbanLegacyRunSatisfiesOnlyFirstExpectedReviewer(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prs := []domain.PRFacts{{URL: "pr1", Number: 1, HeadSHA: "sha1", Mergeability: domain.MergeMergeable}}
	legacy := []domain.CurrentHeadReviewRun{{SessionID: "s1", PRURL: "pr1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now}}
	session := contract.KanbanSessionFacts{AutoReview: true}

	one := contract.DeriveKanbanPresentation(session, toContractKanbanPRFacts(prs, legacy, expectedReviewers{project: []domain.ReviewerHarness{domain.ReviewerClaudeCode}}), now, time.Minute)
	if one.Column != contract.KanbanReady {
		t.Fatalf("one reviewer: column = %q, want ready", one.Column)
	}
	two := contract.DeriveKanbanPresentation(session, toContractKanbanPRFacts(prs, legacy, expectedReviewers{project: []domain.ReviewerHarness{domain.ReviewerClaudeCode, domain.ReviewerCodex}}), now, time.Minute)
	if two.Column != contract.KanbanValidating {
		t.Fatalf("two reviewers: column = %q, want validating", two.Column)
	}
}
