package review

import (
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/pkg/contract"
)

// StateStatus is the per-PR review planning state.
type StateStatus = contract.AOReviewState

const (
	// ReviewStateNeedsReview means an eligible PR has no current AO approval or running pass.
	ReviewStateNeedsReview = contract.AOReviewNeedsReview
	// ReviewStateRunning means a review run is already active for the PR's current head.
	ReviewStateRunning = contract.AOReviewRunning
	// ReviewStateUpToDate means AO approved the PR's current head.
	ReviewStateUpToDate = contract.AOReviewUpToDate
	// ReviewStateChangesRequested means AO requested changes on the PR's current head.
	ReviewStateChangesRequested = contract.AOReviewChangesRequested
	// ReviewStateIneligible means the PR is closed, merged, or missing required facts.
	ReviewStateIneligible = contract.AOReviewIneligible
)

// PRReviewState is one PR-scoped review decision for a worker session.
type PRReviewState struct {
	PRURL       string            `json:"prUrl"`
	PRNumber    int               `json:"prNumber"`
	Title       string            `json:"title"`
	TargetSHA   string            `json:"targetSha"`
	Status      StateStatus       `json:"status" enum:"needs_review,running,up_to_date,changes_requested,ineligible"`
	LatestRun   *domain.ReviewRun `json:"latestRun,omitempty"`
	PreviousRun *domain.ReviewRun `json:"previousRun,omitempty"`
}

// Plan computes per-PR review work from the currently observed PRs and existing
// review runs, expecting only the reviewers that already ran on each head. It is
// pure so the trigger path and API list path share exactly the same
// eligibility/status rules.
func Plan(prs []domain.PullRequest, runs []domain.ReviewRun) []PRReviewState {
	return PlanForReviewers(prs, runs, nil)
}

// PlanForReviewers is Plan for a known set of expected reviewers. A head is up
// to date only when every expected reviewer, and every other reviewer that ran
// on it, approved it.
func PlanForReviewers(prs []domain.PullRequest, runs []domain.ReviewRun, expected []domain.ReviewerHarness) []PRReviewState {
	return PlanWithExpected(prs, runs, func(string, string) HeadReviewers { return HeadReviewers{Harnesses: expected} })
}

// HeadReviewers is who must approve one PR head. Since holds, for a reviewer
// queued for a new pass on the head (a rerun, or a re-queued changes request),
// when it was queued: its passes there from before that no longer count.
type HeadReviewers struct {
	Harnesses []domain.ReviewerHarness
	Since     map[domain.ReviewerHarness]time.Time
}

// PlanWithExpected is PlanForReviewers with each PR head's expected reviewers
// resolved separately, so a head with an open review cycle expects that
// cycle's reviewers while the others fall back to the config.
func PlanWithExpected(prs []domain.PullRequest, runs []domain.ReviewRun, expectedFor func(prURL, targetSHA string) HeadReviewers) []PRReviewState {
	latest := latestRunsByPRAndSHA(runs)
	reviews := make([]PRReviewState, 0, len(prs))
	for _, pr := range prs {
		review := PRReviewState{
			PRURL:     pr.URL,
			PRNumber:  pr.Number,
			Title:     pr.Title,
			TargetSHA: pr.HeadSHA,
			Status:    ReviewStateNeedsReview,
		}
		if run, ok := latestCompletedRunForOtherSHA(runs, review.PRURL, review.TargetSHA); ok {
			review.PreviousRun = &run
		}
		if pr.URL == "" || pr.HeadSHA == "" || pr.Merged || pr.Closed {
			review.Status = ReviewStateIneligible
			if run, ok := latest[review.PRURL+"\x00"+review.TargetSHA]; ok {
				review.LatestRun = &run
			}
			reviews = append(reviews, review)
			continue
		}
		if run, ok := latest[review.PRURL+"\x00"+review.TargetSHA]; ok {
			review.LatestRun = &run
			review.Status, review.LatestRun = headReviewStatus(runs, review.PRURL, review.TargetSHA, expectedFor(review.PRURL, review.TargetSHA), review.LatestRun)
		}
		reviews = append(reviews, review)
	}
	sort.SliceStable(reviews, func(i, j int) bool {
		if reviews[i].PRNumber != reviews[j].PRNumber {
			return reviews[i].PRNumber < reviews[j].PRNumber
		}
		return reviews[i].PRURL < reviews[j].PRURL
	})
	return reviews
}

// headReviewStatus combines every reviewer's newest pass on one PR head. Any
// running pass keeps the head running; otherwise any changes_requested blocks
// it; otherwise it is up to date only when every required reviewer approved.
// fallback is the run reported when no single run decided the status.
func headReviewStatus(
	runs []domain.ReviewRun,
	prURL, targetSHA string,
	head HeadReviewers,
	fallback *domain.ReviewRun,
) (StateStatus, *domain.ReviewRun) {
	expected := head.Harnesses
	byHarness := make(map[domain.ReviewerHarness]domain.ReviewRun)
	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA != targetSHA {
			continue
		}
		// A run recorded before per-harness runs has no harness; it belongs to
		// the session's own reviewer, the first expected one.
		harness := run.Harness
		if harness == "" && len(expected) > 0 {
			harness = expected[0]
		}
		if since, ok := head.Since[harness]; ok && run.CreatedAt.Before(since) {
			continue
		}
		if existing, ok := byHarness[harness]; !ok || newerReviewPass(run, existing) {
			byHarness[harness] = run
		}
	}
	var running, changesRequested *domain.ReviewRun
	approved := true
	for harness := range byHarness {
		run := byHarness[harness]
		switch {
		case run.Status == domain.ReviewRunRunning:
			if running == nil || run.CreatedAt.After(running.CreatedAt) {
				running = &run
			}
		case run.Verdict == domain.VerdictChangesRequested:
			if changesRequested == nil || run.CreatedAt.After(changesRequested.CreatedAt) {
				changesRequested = &run
			}
		}
		if run.Verdict != domain.VerdictApproved {
			approved = false
		}
	}
	for _, harness := range expected {
		if _, ok := byHarness[harness]; !ok {
			approved = false
		}
	}
	switch {
	case running != nil:
		return ReviewStateRunning, running
	case changesRequested != nil:
		return ReviewStateChangesRequested, changesRequested
	case approved:
		return ReviewStateUpToDate, fallback
	default:
		return ReviewStateNeedsReview, fallback
	}
}

// newerReviewPass reports whether run should replace existing as one
// reviewer's authoritative pass: a running pass wins, then the newest.
func newerReviewPass(run, existing domain.ReviewRun) bool {
	runRunning := run.Status == domain.ReviewRunRunning
	existingRunning := existing.Status == domain.ReviewRunRunning
	if runRunning != existingRunning {
		return runRunning
	}
	return run.CreatedAt.After(existing.CreatedAt)
}

func latestCompletedRunForOtherSHA(runs []domain.ReviewRun, prURL, targetSHA string) (domain.ReviewRun, bool) {
	if prURL == "" || targetSHA == "" {
		return domain.ReviewRun{}, false
	}
	var latest domain.ReviewRun
	found := false
	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA == "" || run.TargetSHA == targetSHA {
			continue
		}
		if run.Status != domain.ReviewRunComplete && run.Status != domain.ReviewRunDelivered {
			continue
		}
		if run.Verdict != domain.VerdictApproved && run.Verdict != domain.VerdictChangesRequested {
			continue
		}
		if !found || run.CreatedAt.After(latest.CreatedAt) {
			latest = run
			found = true
		}
	}
	return latest, found
}

// latestRunsByPRAndSHA picks the authoritative run per PR head: the newest
// running pass while any reviewer is still working on that head, otherwise the
// newest pass. Several reviewers may review one head at once, so a reviewer that
// finished first must not hide one that is still running.
func latestRunsByPRAndSHA(runs []domain.ReviewRun) map[string]domain.ReviewRun {
	latest := make(map[string]domain.ReviewRun)
	for _, run := range runs {
		if run.PRURL == "" || run.TargetSHA == "" {
			continue
		}
		key := run.PRURL + "\x00" + run.TargetSHA
		if existing, ok := latest[key]; !ok || newerReviewPass(run, existing) {
			latest[key] = run
		}
	}
	return latest
}
