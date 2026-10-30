package session

import (
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/pkg/contract"
)

func deriveKanbanPresentation(
	rec domain.SessionRecord,
	prs []domain.PRFacts,
	runs []domain.CurrentHeadReviewRun,
	expected expectedReviewers,
	interrupted bool,
	now time.Time,
	signalCapable bool,
) contract.KanbanPresentation {
	return contract.DeriveKanbanPresentation(
		toContractKanbanSessionFacts(rec, interrupted, signalCapable),
		toContractKanbanPRFacts(prs, runs, expected),
		now,
		noSignalGrace,
	)
}

// toContractKanbanSessionFacts reads rec.OutputType, the persisted column
// lifecycle.Manager.ReconcileSessionOutputType maintains, as the source of
// truth for artifact-output placement rather than re-scanning the artifact
// directory on every Kanban derivation.
func toContractKanbanSessionFacts(rec domain.SessionRecord, interrupted, signalCapable bool) contract.KanbanSessionFacts {
	return contract.KanbanSessionFacts{
		SessionFacts:      toContractSessionFacts(rec, signalCapable),
		Interrupted:       interrupted,
		HasArtifactOutput: rec.OutputType.HasArtifact(),
		AutoReview:        rec.AutoReviewEnabled,
		AutoInjectReview:  rec.AutoInjectReview,
		AutoInjectCI:      rec.AutoInjectCI,
	}
}

// expectedReviewers names the reviewers each PR head owes (#5465). A PR whose
// current head has an open review cycle owes that cycle's snapshot, even when
// the project config changed after the cycle opened; every other PR owes the
// project's list.
type expectedReviewers struct {
	// byPR holds, by PR URL, the snapshot of the open cycle for the PR's
	// current head.
	byPR map[string][]domain.ReviewerHarness
	// project is the project config's list (domain.ProjectConfig.ExpectedReviewers).
	project []domain.ReviewerHarness
	// members holds, by PR URL, the open cycle's members by reviewer: a run
	// of a member counts only when member.Counts(run.CreatedAt).
	members map[string]map[domain.ReviewerHarness]domain.ReviewCycleMember
}

func (e expectedReviewers) forPR(url string) []domain.ReviewerHarness {
	if harnesses, ok := e.byPR[url]; ok {
		return harnesses
	}
	return e.project
}

// counts reports whether a run of harness on url's current head still counts:
// a cycle member queued after the run started owes the head a new pass.
func (e expectedReviewers) counts(url string, harness domain.ReviewerHarness, createdAt time.Time) bool {
	member, ok := e.members[url][harness]
	return !ok || member.Counts(createdAt)
}

// newerKanbanPass reports whether run should replace existing as one
// reviewer's pass on the head: a running pass wins, then the newest. It
// mirrors the review planner's newerReviewPass.
func newerKanbanPass(run, existing domain.CurrentHeadReviewRun) bool {
	runRunning := run.Status == domain.ReviewRunRunning
	existingRunning := existing.Status == domain.ReviewRunRunning
	if runRunning != existingRunning {
		return runRunning
	}
	return run.CreatedAt.After(existing.CreatedAt)
}

// foldReviewRuns keeps, per PR, each reviewer's one pass that decides the
// head. A run recorded without a harness belongs to the PR's first expected
// reviewer, as in the review planner, and competes with that reviewer's own
// runs. A run that no longer counts (its member was queued after it started)
// is dropped, so the reviewer still owes the head. present lists every PR
// with at least one run on its current head, counting or not.
func foldReviewRuns(runs []domain.CurrentHeadReviewRun, expected expectedReviewers) (folded map[string]map[domain.ReviewerHarness]domain.CurrentHeadReviewRun, present map[string]bool) {
	folded = make(map[string]map[domain.ReviewerHarness]domain.CurrentHeadReviewRun, len(runs))
	present = make(map[string]bool, len(runs))
	for _, run := range runs {
		present[run.PRURL] = true
		if owed := expected.forPR(run.PRURL); run.Harness == "" && len(owed) > 0 {
			run.Harness = owed[0]
		}
		if !expected.counts(run.PRURL, run.Harness, run.CreatedAt) {
			continue
		}
		byHarness := folded[run.PRURL]
		if byHarness == nil {
			byHarness = map[domain.ReviewerHarness]domain.CurrentHeadReviewRun{}
			folded[run.PRURL] = byHarness
		}
		if existing, ok := byHarness[run.Harness]; !ok || newerKanbanPass(run, existing) {
			byHarness[run.Harness] = run
		}
	}
	return folded, present
}

// reviewRunFacts builds one PR's KanbanReviewRunFacts from each reviewer's
// deciding pass. Outcome is set only when every owed reviewer and every other
// reviewer that ran returned a verdict.
func reviewRunFacts(byHarness map[domain.ReviewerHarness]domain.CurrentHeadReviewRun, owed []domain.ReviewerHarness) contract.KanbanReviewRunFacts {
	facts := contract.KanbanReviewRunFacts{Present: true, Outcome: true}
	for _, run := range byHarness {
		facts.Running = facts.Running || run.Status == domain.ReviewRunRunning
		facts.ChangesRequested = facts.ChangesRequested || run.Verdict == domain.VerdictChangesRequested
		facts.Failed = facts.Failed || run.Status == domain.ReviewRunFailed
		facts.Cancelled = facts.Cancelled || run.Status == domain.ReviewRunCancelled
		facts.Outcome = facts.Outcome && run.Verdict.Valid()
	}
	for _, harness := range owed {
		if _, ok := byHarness[harness]; !ok {
			facts.Outcome = false
		}
	}
	return facts
}

// toContractKanbanPRFacts folds each PR's current-head review runs into one
// KanbanReviewRunFacts (see foldReviewRuns and reviewRunFacts), so Outcome &&
// !ChangesRequested means every required reviewer approved the head (#5465).
// A PR with no runs keeps the zero value, whatever is expected.
func toContractKanbanPRFacts(prs []domain.PRFacts, runs []domain.CurrentHeadReviewRun, expected expectedReviewers) []contract.KanbanPRFacts {
	folded, present := foldReviewRuns(runs, expected)
	byPR := make(map[string]contract.KanbanReviewRunFacts, len(present))
	for url := range present {
		byPR[url] = reviewRunFacts(folded[url], expected.forPR(url))
	}

	out := make([]contract.KanbanPRFacts, len(prs))
	for i, pr := range prs {
		out[i] = contract.KanbanPRFacts{
			URL:          pr.URL,
			Draft:        pr.Draft,
			Merged:       pr.Merged,
			Closed:       pr.Closed,
			CI:           pr.CI,
			Review:       pr.Review,
			Mergeability: pr.Mergeability,
			UpdatedAt:    pr.UpdatedAt,
			ReviewRun:    byPR[pr.URL],
			ExternalReview: contract.KanbanExternalReviewFacts{
				Approved:         pr.ExternalApproved,
				ChangesRequested: pr.ExternalChangesRequested,
				Comments:         pr.ExternalComments,
			},
		}
	}
	return out
}
