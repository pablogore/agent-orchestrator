package review

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestPlanStatuses(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	tests := []struct {
		name string
		pr   domain.PullRequest
		runs []domain.ReviewRun
		want StateStatus
	}{
		{name: "open needs review", pr: planPR("pr1", 1, "sha1"), want: ReviewStateNeedsReview},
		{name: "draft needs review", pr: withDraft(planPR("pr1", 1, "sha1")), want: ReviewStateNeedsReview},
		{name: "merged ineligible", pr: withMerged(planPR("pr1", 1, "sha1")), want: ReviewStateIneligible},
		{name: "closed ineligible", pr: withClosed(planPR("pr1", 1, "sha1")), want: ReviewStateIneligible},
		{name: "approved current sha up to date", pr: planPR("pr1", 1, "sha1"), runs: []domain.ReviewRun{
			{ID: "run-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now},
		}, want: ReviewStateUpToDate},
		{name: "changes requested current sha", pr: planPR("pr1", 1, "sha1"), runs: []domain.ReviewRun{
			{ID: "run-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested, CreatedAt: now},
		}, want: ReviewStateChangesRequested},
		{name: "running current sha", pr: planPR("pr1", 1, "sha1"), runs: []domain.ReviewRun{
			{ID: "run-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunRunning, CreatedAt: now},
		}, want: ReviewStateRunning},
		{name: "different sha needs review", pr: planPR("pr1", 1, "sha2"), runs: []domain.ReviewRun{
			{ID: "run-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now},
		}, want: ReviewStateNeedsReview},
		{name: "failed current sha retryable", pr: planPR("pr1", 1, "sha1"), runs: []domain.ReviewRun{
			{ID: "run-1", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunFailed, CreatedAt: now},
		}, want: ReviewStateNeedsReview},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Plan([]domain.PullRequest{tt.pr}, tt.runs)
			if len(got) != 1 {
				t.Fatalf("review states = %d, want 1", len(got))
			}
			if got[0].Status != tt.want {
				t.Fatalf("status = %s, want %s; item=%+v", got[0].Status, tt.want, got[0])
			}
		})
	}
}

func TestPlanPreservesPreviousVerdictWithoutChangingCurrentHeadState(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	tests := []struct {
		name       string
		currentRun *domain.ReviewRun
		wantStatus StateStatus
	}{
		{name: "new head needs review", wantStatus: ReviewStateNeedsReview},
		{
			name: "new head running",
			currentRun: &domain.ReviewRun{
				ID: "run-current", PRURL: "pr1", TargetSHA: "sha2", Status: domain.ReviewRunRunning, CreatedAt: now.Add(time.Second),
			},
			wantStatus: ReviewStateRunning,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runs := []domain.ReviewRun{{
				ID: "run-previous", PRURL: "pr1", TargetSHA: "sha1", Status: domain.ReviewRunDelivered,
				Verdict: domain.VerdictChangesRequested, CreatedAt: now,
			}}
			if tt.currentRun != nil {
				runs = append(runs, *tt.currentRun)
			}

			got := Plan([]domain.PullRequest{planPR("pr1", 1, "sha2")}, runs)
			if got[0].Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s", got[0].Status, tt.wantStatus)
			}
			if got[0].PreviousRun == nil || got[0].PreviousRun.ID != "run-previous" {
				t.Fatalf("previous run = %+v, want run-previous", got[0].PreviousRun)
			}
			if tt.currentRun == nil {
				if got[0].LatestRun != nil {
					t.Fatalf("latest run = %+v, want nil for unreviewed current head", got[0].LatestRun)
				}
			} else if got[0].LatestRun == nil || got[0].LatestRun.ID != tt.currentRun.ID {
				t.Fatalf("latest run = %+v, want current-head run %q", got[0].LatestRun, tt.currentRun.ID)
			}
		})
	}
}

func TestPlanPreviousRunChoosesNewestQualifyingDifferentHead(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	got := Plan([]domain.PullRequest{planPR("pr1", 1, "sha-current")}, []domain.ReviewRun{
		{ID: "older", PRURL: "pr1", TargetSHA: "sha-old", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now},
		{ID: "newest-other", PRURL: "pr1", TargetSHA: "sha-newer", Status: domain.ReviewRunDelivered, Verdict: domain.VerdictChangesRequested, CreatedAt: now.Add(time.Second)},
		{ID: "failed-other", PRURL: "pr1", TargetSHA: "sha-failed", Status: domain.ReviewRunFailed, Verdict: domain.VerdictApproved, CreatedAt: now.Add(2 * time.Second)},
		{ID: "current", PRURL: "pr1", TargetSHA: "sha-current", Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: now.Add(3 * time.Second)},
	})

	if got[0].PreviousRun == nil || got[0].PreviousRun.ID != "newest-other" {
		t.Fatalf("previous run = %+v, want newest qualifying different-head run", got[0].PreviousRun)
	}
	if got[0].LatestRun == nil || got[0].LatestRun.ID != "current" || got[0].Status != ReviewStateUpToDate {
		t.Fatalf("current-head state = %+v, want current approved run", got[0])
	}
}

func planPR(url string, n int, sha string) domain.PullRequest {
	return domain.PullRequest{URL: url, Number: n, HeadSHA: sha}
}

func withDraft(pr domain.PullRequest) domain.PullRequest {
	pr.Draft = true
	return pr
}

func withMerged(pr domain.PullRequest) domain.PullRequest {
	pr.Merged = true
	return pr
}

func withClosed(pr domain.PullRequest) domain.PullRequest {
	pr.Closed = true
	return pr
}

func TestPlanAnotherReviewersApprovalDoesNotHideRejection(t *testing.T) {
	// #5465: with several reviewers on one head, the latest pass per reviewer
	// decides, and any of those rejecting keeps the head unresolved.
	const pr, head, stale = "https://github.com/o/r/pull/1", "sha1", "sha0"
	codex, claude := domain.ReviewerCodex, domain.ReviewerClaudeCode
	run := func(id string, h domain.ReviewerHarness, sha string, st domain.ReviewRunStatus, v domain.ReviewVerdict, ts int64) domain.ReviewRun {
		return domain.ReviewRun{ID: id, SessionID: "mer-1", Harness: h, PRURL: pr, TargetSHA: sha,
			Status: st, Verdict: v, CreatedAt: time.Unix(ts, 0).UTC()}
	}
	done, running, failed := domain.ReviewRunComplete, domain.ReviewRunRunning, domain.ReviewRunFailed
	rej, ok, none := domain.VerdictChangesRequested, domain.VerdictApproved, domain.VerdictNone

	cases := []struct {
		name       string
		runs       []domain.ReviewRun
		want       StateStatus
		wantLatest string
	}{
		{"approval after rejection", []domain.ReviewRun{run("a", codex, head, done, rej, 1), run("b", claude, head, done, ok, 2)}, ReviewStateChangesRequested, "a"},
		{"rejection after approval", []domain.ReviewRun{run("b", claude, head, done, ok, 1), run("a", codex, head, done, rej, 2)}, ReviewStateChangesRequested, "a"},
		{"same reviewer re-approves its own rejection", []domain.ReviewRun{run("a1", codex, head, done, rej, 1), run("a2", codex, head, done, ok, 2), run("b", claude, head, done, ok, 3)}, ReviewStateUpToDate, "b"},
		{"running pass still wins", []domain.ReviewRun{run("a", codex, head, done, rej, 1), run("b", claude, head, running, none, 2)}, ReviewStateRunning, "b"},
		{"rejection on a stale head does not block", []domain.ReviewRun{run("a", codex, stale, done, rej, 1), run("b", claude, head, done, ok, 2)}, ReviewStateUpToDate, "b"},
		{"another reviewer's failure does not hide a rejection", []domain.ReviewRun{run("a", codex, head, done, rej, 1), run("b", claude, head, failed, none, 2)}, ReviewStateChangesRequested, "a"},
		{"a later failure still masks an approval", []domain.ReviewRun{run("a", codex, head, done, ok, 1), run("b", claude, head, failed, none, 2)}, ReviewStateNeedsReview, "b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Plan([]domain.PullRequest{{URL: pr, Number: 1, HeadSHA: head}}, tc.runs)
			if len(got) != 1 {
				t.Fatalf("states = %d, want 1", len(got))
			}
			if got[0].Status != tc.want {
				t.Errorf("status = %s, want %s", got[0].Status, tc.want)
			}
			if got[0].LatestRun == nil || got[0].LatestRun.ID != tc.wantLatest {
				t.Errorf("latestRun = %+v, want %s", got[0].LatestRun, tc.wantLatest)
			}
		})
	}
}

func TestPlanSameTimeApprovalsAcrossReviewersAreDeterministic(t *testing.T) {
	// #5465: map iteration is random, so equal CreatedAt across reviewers must
	// still resolve to the same authoritative run every time.
	const pr, head = "https://github.com/o/r/pull/1", "sha1"
	at := time.Unix(1, 0).UTC()
	runs := []domain.ReviewRun{
		{ID: "b", SessionID: "mer-1", Harness: domain.ReviewerClaudeCode, PRURL: pr, TargetSHA: head, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: at},
		{ID: "a", SessionID: "mer-1", Harness: domain.ReviewerCodex, PRURL: pr, TargetSHA: head, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: at},
	}
	prs := []domain.PullRequest{{URL: pr, Number: 1, HeadSHA: head}}
	var first string
	for i := 0; i < 50; i++ {
		got := Plan(prs, runs)
		if len(got) != 1 || got[0].LatestRun == nil {
			t.Fatalf("iteration %d: states = %+v", i, got)
		}
		if got[0].Status != ReviewStateUpToDate {
			t.Fatalf("iteration %d: status = %s, want %s", i, got[0].Status, ReviewStateUpToDate)
		}
		if i == 0 {
			first = got[0].LatestRun.ID
			continue
		}
		if got[0].LatestRun.ID != first {
			t.Fatalf("iteration %d: latestRun = %s, want stable %s", i, got[0].LatestRun.ID, first)
		}
	}
}
