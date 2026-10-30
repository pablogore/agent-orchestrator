// Package autoreview decides when the daemon may automatically review a
// worker's current pull-request heads.
package autoreview

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
)

const (
	// DefaultIdleThreshold is how long a worker must remain idle before an
	// automatic review may start.
	DefaultIdleThreshold = time.Minute
	// DefaultSweepInterval is the cadence for reevaluating live sessions.
	DefaultSweepInterval = time.Minute
	// autoReviewFailedRetryLimit bounds retries for the same current PR head.
	autoReviewFailedRetryLimit = 3
)

// Store provides the durable session, project, PR, and review facts used by
// the coordinator.
type Store interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	ListAllSessions(context.Context) ([]domain.SessionRecord, error)
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	ListPRsBySession(context.Context, domain.SessionID) ([]domain.PullRequest, error)
	ListReviewRunsBySession(context.Context, domain.SessionID) ([]domain.ReviewRun, error)
	ListOpenReviewCyclesBySession(context.Context, domain.SessionID) ([]domain.ReviewCycle, error)
	ListSessionsWithOpenReviewCycles(context.Context) ([]domain.SessionID, error)
}

// Trigger starts a source-tagged automatic review through the normal review
// engine, and moves a worker's review-cycle queue.
type Trigger interface {
	TriggerAuto(context.Context, domain.SessionID, domain.ReviewerHarness) (reviewcore.TriggerResult, error)
	AdvanceCycles(context.Context, domain.SessionID) (reviewcore.TriggerResult, error)
}

// Result describes whether an evaluation started a review and why it skipped
// or triggered.
type Result struct {
	Triggered bool
	Reason    string
}

// Coordinator evaluates auto-review policy and owns the periodic sweep.
type Coordinator struct {
	store         Store
	reviews       Trigger
	clock         func() time.Time
	idleThreshold time.Duration
	sweepInterval time.Duration
	logger        *slog.Logger
}

// Config customizes coordinator timing and logging.
type Config struct {
	Clock         func() time.Time
	IdleThreshold time.Duration
	SweepInterval time.Duration
	Logger        *slog.Logger
}

// New constructs an auto-review coordinator.
func New(store Store, reviews Trigger, cfg Config) *Coordinator {
	c := &Coordinator{store: store, reviews: reviews, clock: cfg.Clock, idleThreshold: cfg.IdleThreshold, sweepInterval: cfg.SweepInterval, logger: cfg.Logger}
	if c.clock == nil {
		c.clock = time.Now
	}
	if c.idleThreshold <= 0 {
		c.idleThreshold = DefaultIdleThreshold
	}
	if c.sweepInterval <= 0 {
		c.sweepInterval = DefaultSweepInterval
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}

// EvaluateSession evaluates one worker against the current project, activity,
// PR, and review-run facts.
func (c *Coordinator) EvaluateSession(ctx context.Context, id domain.SessionID) (Result, error) {
	session, ok, err := c.store.GetSession(ctx, id)
	if err != nil || !ok {
		if err == nil {
			return Result{Reason: "session_not_found"}, nil
		}
		return Result{}, err
	}
	if reason := sessionGate(session, c.clock(), c.idleThreshold); reason != "" {
		return Result{Reason: reason}, nil
	}
	project, ok, err := c.store.GetProject(ctx, string(session.ProjectID))
	if err != nil || !ok {
		if err == nil {
			return Result{Reason: "missing_reviewer_harness"}, nil
		}
		return Result{}, err
	}
	prs, err := c.store.ListPRsBySession(ctx, id)
	if err != nil {
		return Result{}, err
	}
	cycles, err := c.store.ListOpenReviewCyclesBySession(ctx, id)
	if err != nil {
		return Result{}, err
	}
	harnesses := reviewerHarnesses(session, project.Config, cycles, prs)
	if len(harnesses) == 0 {
		return Result{Reason: "missing_reviewer_harness"}, nil
	}
	if len(prs) == 0 {
		return Result{Reason: "no_pr"}, nil
	}
	runs, err := c.store.ListReviewRunsBySession(ctx, id)
	if err != nil {
		return Result{}, err
	}
	scope := newReviewerScope(prs, runs, cycles, harnesses)
	harness, reason := nextReviewerHarness(scope, harnesses)
	trigger := harness
	if len(harnesses) > 1 && (harness == "" || !scope.decide(harness).running) {
		var queued bool
		if trigger, queued = cycleTrigger(cycles, prs, runs, harness); queued {
			return Result{Reason: "review_queued"}, nil
		}
	}
	if harness == "" {
		return Result{Reason: reason}, nil
	}
	result, err := c.reviews.TriggerAuto(ctx, id, trigger)
	if err != nil {
		return Result{}, fmt.Errorf("trigger auto review for %s: %w", id, err)
	}
	if result.SkipReason != "" {
		return Result{Reason: result.SkipReason}, nil
	}
	if !result.Created {
		return Result{Reason: triggerResultReason(prs, result, harness, harnesses[0])}, nil
	}
	return Result{Triggered: true, Reason: "triggered"}, nil
}

func triggerResultReason(prs []domain.PullRequest, result reviewcore.TriggerResult, harness, legacy domain.ReviewerHarness) string {
	for _, state := range result.Reviews {
		if state.Status == reviewcore.ReviewStateRunning {
			return "review_running"
		}
	}
	runs := runsForHarness(result.Runs, harness, legacy)
	for _, pr := range prs {
		if blocked := existingHeadReason(runs, pr.URL, pr.HeadSHA); blocked != "" {
			return blocked
		}
	}
	return "planner_up_to_date"
}

func existingHeadReason(runs []domain.ReviewRun, prURL, targetSHA string) string {
	failedAutoRuns := 0
	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA != targetSHA {
			continue
		}
		if run.Status == domain.ReviewRunRunning {
			return "review_running"
		}
		if run.Status == domain.ReviewRunCancelled {
			return "cancelled_same_sha"
		}
		if run.Verdict == domain.VerdictApproved {
			return "already_approved"
		}
		if run.Verdict == domain.VerdictChangesRequested {
			return "changes_requested_same_sha"
		}
		if run.Status == domain.ReviewRunFailed && run.TriggerSource == domain.ReviewTriggerAuto {
			failedAutoRuns++
		}
	}
	if failedAutoRuns >= autoReviewFailedRetryLimit {
		return "failed_same_sha_retry_limit"
	}
	return ""
}

// expectedReviewerHarnesses lists, in order, the harnesses that must each
// review the worker's PR heads.
func expectedReviewerHarnesses(session domain.SessionRecord, config domain.ProjectConfig) []domain.ReviewerHarness {
	var harnesses []domain.ReviewerHarness
	for _, reviewer := range config.ExpectedReviewers(session) {
		if reviewer.Harness != "" {
			harnesses = append(harnesses, reviewer.Harness)
		}
	}
	return harnesses
}

// harnessDecision is one reviewer's auto-review decision for the worker's
// current PR heads.
type harnessDecision struct {
	eligible bool
	running  bool
	reason   string
}

// nextReviewerHarness picks the reviewer the engine should be called for.
// Reviewers run one at a time: a running pass by any expected reviewer is
// routed back to that reviewer so the engine can reconcile it, and no other
// reviewer starts. Otherwise the first expected reviewer that still owes an
// eligible head is chosen; a reviewer that already gave a verdict on the head
// is skipped so a rejection does not stop the remaining reviewers. With no
// reviewer to call, it returns the first reason that is not an approval.
func nextReviewerHarness(scope reviewerScope, harnesses []domain.ReviewerHarness) (domain.ReviewerHarness, string) {
	decisions := make([]harnessDecision, len(harnesses))
	for i, harness := range harnesses {
		decisions[i] = scope.decide(harness)
		if decisions[i].running {
			return harness, ""
		}
	}
	reason := ""
	for i, decision := range decisions {
		if decision.eligible {
			return harnesses[i], ""
		}
		if reason == "" && decision.reason != "already_approved" {
			reason = decision.reason
		}
	}
	if reason == "" {
		reason = decisions[0].reason
	}
	return "", reason
}

// runsForHarness is harness's runs. A run recorded before per-harness runs has
// no harness and belongs to legacy, the first expected reviewer, alone: one
// old approval never satisfies two reviewers.
func runsForHarness(runs []domain.ReviewRun, harness, legacy domain.ReviewerHarness) []domain.ReviewRun {
	harnessRuns := make([]domain.ReviewRun, 0, len(runs))
	for _, run := range runs {
		if run.Harness == harness || (run.Harness == "" && harness == legacy) {
			harnessRuns = append(harnessRuns, run)
		}
	}
	return harnessRuns
}

// decideForHarness applies the single-reviewer policy to one reviewer's runs.
func decideForHarness(prs []domain.PullRequest, harnessRuns []domain.ReviewRun) harnessDecision {
	decision := harnessDecision{reason: "planner_ineligible"}
	for _, state := range reviewcore.Plan(prs, harnessRuns) {
		switch state.Status {
		case reviewcore.ReviewStateRunning:
			decision.running = true
			decision.reason = "review_running"
		case reviewcore.ReviewStateUpToDate:
			decision.reason = "already_approved"
		case reviewcore.ReviewStateChangesRequested:
			decision.reason = "changes_requested_same_sha"
		case reviewcore.ReviewStateIneligible:
			decision.reason = ineligibleReason(prs, state.PRURL)
		case reviewcore.ReviewStateNeedsReview:
			if blocked := existingHeadReason(harnessRuns, state.PRURL, state.TargetSHA); blocked != "" {
				decision.reason = blocked
				continue
			}
			decision.eligible = true
		}
	}
	return decision
}

// advanceOpenCycles runs the queue step for every worker with an open review
// cycle. It ignores the auto-review gate: a manually triggered cycle must keep
// moving when a reviewer exits without submitting or the daemon restarts.
func (c *Coordinator) advanceOpenCycles(ctx context.Context) {
	ids, err := c.store.ListSessionsWithOpenReviewCycles(ctx)
	if err != nil {
		c.logger.Error("auto-review: list open review cycles failed", "err", err)
		return
	}
	for _, id := range ids {
		if _, err := c.reviews.AdvanceCycles(ctx, id); err != nil {
			c.logger.Error("auto-review: advance review cycles failed", "session_id", id, "err", err)
		}
	}
}

// reviewerScope decides one reviewer's auto review over the PR heads that owe
// it. When every reviewable head has an open cycle, a reviewer owes only the
// heads whose own frozen cycle lists it, and a pass recorded without a
// reviewer belongs to that cycle's first member; a reviewer one PR's cycle
// lists is never carried to another PR. Otherwise every head owes every
// reviewer, and such a pass belongs to the first expected one.
type reviewerScope struct {
	prs     []domain.PullRequest
	runs    []domain.ReviewRun
	cycles  map[string]domain.ReviewCycle // by PR URL, when every head is covered
	legacy  domain.ReviewerHarness
	covered bool
}

func newReviewerScope(prs []domain.PullRequest, runs []domain.ReviewRun, cycles []domain.ReviewCycle, harnesses []domain.ReviewerHarness) reviewerScope {
	scope := reviewerScope{prs: prs, runs: runs, covered: cyclesCoverHeads(cycles, prs)}
	if len(harnesses) > 0 {
		scope.legacy = harnesses[0]
	}
	if scope.covered {
		scope.cycles = map[string]domain.ReviewCycle{}
		for _, cycle := range cycles {
			if cycleOnReviewableHead(cycle, prs) {
				scope.cycles[cycle.PRURL] = cycle
			}
		}
	}
	return scope
}

func (s reviewerScope) decide(harness domain.ReviewerHarness) harnessDecision {
	if !s.covered {
		return decideForHarness(s.prs, runsForHarness(s.runs, harness, s.legacy))
	}
	var prs []domain.PullRequest
	var runs []domain.ReviewRun
	for _, pr := range s.prs {
		cycle, ok := s.cycles[pr.URL]
		if !ok || !slices.Contains(cycle.Harnesses(), harness) {
			continue
		}
		prs = append(prs, pr)
		for _, run := range runsForHarness(s.runs, harness, cycle.LegacyHarness()) {
			if run.PRURL == pr.URL {
				runs = append(runs, run)
			}
		}
	}
	if len(prs) == 0 {
		return harnessDecision{reason: "already_approved"}
	}
	return decideForHarness(prs, runs)
}

// cycleTrigger decides how a multi-reviewer worker's chosen reviewer (empty
// when none owes a head) is started. queued reports that an open cycle has a
// member waiting in its queue: the queue step (Sweep, or the submit that ends
// a pass) starts it, and triggering here would jump the queue. Otherwise, when
// no open cycle fixes every head's reviewers yet, it returns "" so the engine
// opens a cycle and launches the first reviewer the heads owe; and when every
// reviewer already had its turn on these heads, the chosen reviewer is retried
// by name within the retry limit.
func cycleTrigger(cycles []domain.ReviewCycle, prs []domain.PullRequest, runs []domain.ReviewRun, harness domain.ReviewerHarness) (domain.ReviewerHarness, bool) {
	if cyclesWaiting(cycles, runs) {
		return "", true
	}
	if harness != "" && !cyclesCoverHeads(cycles, prs) {
		return "", false
	}
	return harness, false
}

// reviewerHarnesses lists, in order, the reviewers the worker's PR heads owe.
//
// An open cycle froze its head's reviewers when it opened, so a later config
// edit must not change them: when every reviewable head (open, with a head
// SHA) is covered by an open cycle for that exact head, the reviewers are the
// ordered union of those cycles' members (cycle order, then member position),
// and the first of them is the first cycle's LegacyHarness. A reviewer dropped
// from the config is still retried there, and one added is not launched on a
// frozen head. While some head has no cycle yet, the current config decides,
// since the trigger opens a cycle from it.
func reviewerHarnesses(session domain.SessionRecord, config domain.ProjectConfig, cycles []domain.ReviewCycle, prs []domain.PullRequest) []domain.ReviewerHarness {
	if cyclesCoverHeads(cycles, prs) {
		if harnesses := coveringCycleHarnesses(cycles, prs); len(harnesses) > 0 {
			return harnesses
		}
	}
	return expectedReviewerHarnesses(session, config)
}

// coveringCycleHarnesses is the ordered union of the members of the open
// cycles fixed on a current reviewable head.
func coveringCycleHarnesses(cycles []domain.ReviewCycle, prs []domain.PullRequest) []domain.ReviewerHarness {
	var harnesses []domain.ReviewerHarness
	seen := map[domain.ReviewerHarness]bool{}
	for _, cycle := range cycles {
		if !cycleOnReviewableHead(cycle, prs) {
			continue
		}
		for _, harness := range cycle.Harnesses() {
			if harness != "" && !seen[harness] {
				seen[harness] = true
				harnesses = append(harnesses, harness)
			}
		}
	}
	return harnesses
}

func cycleOnReviewableHead(cycle domain.ReviewCycle, prs []domain.PullRequest) bool {
	for _, pr := range prs {
		if reviewableHead(pr) && cycle.PRURL == pr.URL && cycle.TargetSHA == pr.HeadSHA {
			return true
		}
	}
	return false
}

func reviewableHead(pr domain.PullRequest) bool {
	return !pr.Merged && !pr.Closed && pr.HeadSHA != ""
}

// cyclesWaiting reports whether any open cycle has a member waiting in its
// queue: queued, with no pass of its own on the cycle's head started at or
// after it was queued.
func cyclesWaiting(cycles []domain.ReviewCycle, runs []domain.ReviewRun) bool {
	for _, cycle := range cycles {
		for _, member := range cycle.Members {
			newest, hasRun := newestHeadRun(runs, cycle, member.Harness)
			if member.Waiting(newest, hasRun) {
				return true
			}
		}
	}
	return false
}

// newestHeadRun is the start of harness's newest run on cycle's head. A run
// recorded without a harness belongs to the cycle's LegacyHarness alone.
func newestHeadRun(runs []domain.ReviewRun, cycle domain.ReviewCycle, harness domain.ReviewerHarness) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, run := range runsForHarness(runs, harness, cycle.LegacyHarness()) {
		if run.PRURL != cycle.PRURL || run.TargetSHA != cycle.TargetSHA {
			continue
		}
		if !found || run.CreatedAt.After(newest) {
			newest, found = run.CreatedAt, true
		}
	}
	return newest, found
}

// cyclesCoverHeads reports whether every reviewable PR head has an open cycle
// for that exact head.
func cyclesCoverHeads(cycles []domain.ReviewCycle, prs []domain.PullRequest) bool {
	for _, pr := range prs {
		if !reviewableHead(pr) {
			continue
		}
		covered := false
		for _, cycle := range cycles {
			if cycle.PRURL == pr.URL && cycle.TargetSHA == pr.HeadSHA {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func ineligibleReason(prs []domain.PullRequest, url string) string {
	for _, pr := range prs {
		if pr.URL != url {
			continue
		}
		switch {
		case pr.Draft:
			return "draft_pr"
		case pr.Merged:
			return "merged_pr"
		case pr.Closed:
			return "closed_pr"
		case pr.HeadSHA == "":
			return "missing_head_sha"
		default:
			return "planner_ineligible"
		}
	}
	return "planner_ineligible"
}

func sessionGate(session domain.SessionRecord, now time.Time, threshold time.Duration) string {
	if !session.AutoReviewEnabled {
		return "disabled"
	}
	if session.Kind != domain.KindWorker {
		return "not_worker"
	}
	if session.IsTerminated {
		return "terminated"
	}
	if session.Activity.State != domain.ActivityIdle {
		return "not_idle"
	}
	if session.Activity.LastActivityAt.IsZero() || now.Sub(session.Activity.LastActivityAt) < threshold {
		return "idle_threshold_not_met"
	}
	return ""
}

// Sweep moves every open review-cycle queue, then evaluates every known
// session, isolating per-session failures.
func (c *Coordinator) Sweep(ctx context.Context) error {
	c.advanceOpenCycles(ctx)
	sessions, err := c.store.ListAllSessions(ctx)
	if err != nil {
		return err
	}
	for _, session := range sessions {
		if reason := sessionGate(session, c.clock(), c.idleThreshold); reason != "" {
			continue
		}
		if _, err := c.EvaluateSession(ctx, session.ID); err != nil {
			c.logger.Error("auto-review: evaluate session failed", "session_id", session.ID, "err", err)
		}
	}
	return nil
}

// Start evaluates persisted session and PR facts on the configured cadence
// until ctx is cancelled.
func (c *Coordinator) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(c.sweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := c.Sweep(ctx); err != nil {
					c.logger.Error("auto-review: sweep failed", "err", err)
				}
			}
		}
	}()
	return done
}
