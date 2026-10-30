package review

import (
	stdctx "context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Review cycles (#5465). A trigger that names no reviewer opens one cycle per
// eligible PR head: a snapshot of the head SHA and the reviewers it owes, in
// order. Reviewers then run one at a time per worker, chosen by the
// idempotent queue step AdvanceCycles. The launch itself always goes through
// triggerHarnessLocked, the same single-reviewer path a named trigger uses.

const (
	cycleSupersededByNewerCommit = "superseded by a newer commit"
	cycleSupersededPRNotOpen     = "pull request is no longer open"
	cycleSupersededWorkerEnded   = "worker session ended"
	// reviewerLaunchIncomplete fails a terminal reviewer's pass whose launch
	// never recorded a pane, so the queue does not wait on it forever.
	reviewerLaunchIncomplete = "reviewer launch did not complete"
)

// cycleLaunch carries what the trigger that drives the queue asked for. The
// zero value is a queue step on its own (after a submit or on a sweep).
type cycleLaunch struct {
	source domain.ReviewTriggerSource
	mode   domain.ReviewerInterfaceMode
	// prURL, when set, puts that PR's cycle first in the queue, so a trigger
	// for one PR starts that PR's reviewer before another PR's.
	prURL string
}

// advanceOutcome is what one queue step did.
type advanceOutcome int

const (
	// advanceIdle: no open cycle has a pending member.
	advanceIdle advanceOutcome = iota
	// advanceBusy: a reviewer is running on a current PR head, so the queue waits.
	advanceBusy
	// advanceLaunched: a member was handed to the reviewer launch path.
	advanceLaunched
)

// triggerCyclesLocked opens or re-queues the cycles of the worker's PR heads,
// then runs one queue step. The caller holds the worker lock.
//
// When the queue has nothing to launch and no reviewer is running, the
// trigger falls through to the single-reviewer path for the session's own
// reviewer, exactly as before cycles existed: that path reuses the reviewed
// head, heals a stale reviewer surface, and releases another reviewer's idle
// pane. While a reviewer runs it is never called, so two reviewers never run
// at once for one worker.
func (e *Engine) triggerCyclesLocked(ctx stdctx.Context, workerID domain.SessionID, worker domain.SessionRecord, opts TriggerOptions) (TriggerResult, error) {
	prs, err := e.triggerTargetPRs(ctx, workerID, opts.PRURL)
	if err != nil {
		return TriggerResult{}, err
	}
	if _, err := e.reconciledRuns(ctx, workerID, worker, true); err != nil {
		return TriggerResult{}, err
	}
	queued, err := e.queueCycles(ctx, workerID, worker, prs, opts)
	if err != nil {
		return TriggerResult{}, err
	}
	result, outcome, err := e.advanceLocked(ctx, workerID, worker, cycleLaunch{source: opts.Source, mode: opts.InterfaceMode, prURL: opts.PRURL})
	if err != nil {
		return TriggerResult{}, err
	}
	if outcome == advanceIdle {
		result, err = e.triggerHarnessLocked(ctx, workerID, worker, opts, nil)
		if err != nil {
			return TriggerResult{}, err
		}
	}
	reviews, runs, err := e.cyclePlan(ctx, workerID, worker, prs)
	if err != nil {
		return TriggerResult{}, err
	}
	if outcome != advanceBusy {
		result.Reviews = reviews
		return result, nil
	}
	if !queued && (opts.RejectReviewedHead || (opts.Rerun && opts.Source == domain.ReviewTriggerAgent)) {
		harness, _, err := e.reviewerSelection(ctx, worker)
		if err != nil {
			return TriggerResult{}, err
		}
		return TriggerResult{}, nothingToReviewError(reviews, runs, harness, opts.Rerun)
	}
	row, err := e.runningReviewerRow(ctx, workerID, worker, runs)
	if err != nil {
		return TriggerResult{}, err
	}
	return TriggerResult{
		Run:              firstReusableRun(reviews),
		ReviewerHandleID: legacyReviewerHandle(row),
		Reviews:          reviews,
		Runs:             runs,
		ReviewerSurface:  reviewerSurface(row),
	}, nil
}

// runningReviewerRow is the review row of the reviewer currently running, so
// a trigger that waits on it can point the caller at its pane.
func (e *Engine) runningReviewerRow(ctx stdctx.Context, workerID domain.SessionID, worker domain.SessionRecord, runs []domain.ReviewRun) (domain.Review, error) {
	harness, _, err := e.reviewerSelection(ctx, worker)
	if err != nil {
		return domain.Review{}, err
	}
	for _, run := range runs {
		if run.Status == domain.ReviewRunRunning && run.Harness != "" {
			harness = run.Harness
			break
		}
	}
	row, _, err := e.store.GetReviewBySessionAndHarness(ctx, workerID, harness)
	return row, err
}

// queueCycles makes sure every eligible PR head in prs has an open cycle and
// queues the members the trigger asks for. It reports whether any member was
// newly queued.
//
// A new cycle snapshots the worker's expected reviewers and queues every
// member that owes the head under the trigger's rules. A member whose latest
// pass on this exact head already approved it is not queued, so a cycle
// opened after one reviewer approved goes straight to the next one.
//
// An existing cycle keeps its snapshot. A manual or agent trigger re-queues
// the members memberOwesHead selects: without --rerun approvals stand, with
// it every member runs again. An automatic trigger re-queues nothing.
//
// Queueing stamps the member with the time and the trigger's source. Its
// passes on the head from before that time stop counting, so an earlier
// result never answers the new request; its next pass answers it.
func (e *Engine) queueCycles(ctx stdctx.Context, workerID domain.SessionID, worker domain.SessionRecord, prs []domain.PullRequest, opts TriggerOptions) (bool, error) {
	runs, err := e.store.ListReviewRunsBySession(ctx, workerID)
	if err != nil {
		return false, err
	}
	cycles, err := e.store.ListOpenReviewCyclesBySession(ctx, workerID)
	if err != nil {
		return false, err
	}
	cfg, err := e.projectConfig(ctx, worker)
	if err != nil {
		return false, err
	}
	expected := cfg.ExpectedReviewers(worker)
	legacy := domain.ReviewerHarness("")
	if len(expected) > 0 {
		legacy = expected[0].Harness
	}
	source := opts.Source
	now := e.clock()
	queued := false
	for _, pr := range prs {
		if !prReviewable(pr) {
			continue
		}
		var current *domain.ReviewCycle
		for i := range cycles {
			c := cycles[i]
			if c.PRURL != pr.URL {
				continue
			}
			if c.TargetSHA != pr.HeadSHA {
				if _, err := e.store.SupersedeReviewCycle(ctx, c.ID, cycleSupersededByNewerCommit, now); err != nil {
					return false, err
				}
				continue
			}
			current = &cycles[i]
		}
		if current == nil {
			cycle := domain.ReviewCycle{ID: e.newID(), SessionID: workerID, PRURL: pr.URL, TargetSHA: pr.HeadSHA, CreatedAt: now}
			for i, reviewer := range expected {
				member := domain.ReviewCycleMember{Position: i, Harness: reviewer.Harness, AgentConfig: reviewer.AgentConfig}
				if memberOwesHead(runs, pr.URL, pr.HeadSHA, reviewer.Harness, legacy, opts) {
					member.QueuedAt, member.QueuedSource = queueStamp(now, harnessRunsOnHead(runs, pr.URL, pr.HeadSHA, reviewer.Harness, legacy)), source
					queued = true
				}
				cycle.Members = append(cycle.Members, member)
			}
			if err := e.store.InsertReviewCycle(ctx, cycle); err != nil {
				if errors.Is(err, domain.ErrDuplicateReviewCycle) {
					return false, fmt.Errorf("%w: review cycle for %s opened concurrently", ErrConflict, pr.URL)
				}
				return false, err
			}
			continue
		}
		if opts.Source == domain.ReviewTriggerAuto {
			continue
		}
		for _, member := range current.Members {
			if memberWaiting(*current, member, runs) || !memberOwesHead(runs, pr.URL, pr.HeadSHA, member.Harness, current.LegacyHarness(), opts) {
				continue
			}
			at := queueStamp(now, harnessRunsOnHead(runs, pr.URL, pr.HeadSHA, member.Harness, current.LegacyHarness()))
			if _, err := e.store.SetReviewCycleMemberQueued(ctx, current.ID, member.Harness, at, source); err != nil {
				return false, err
			}
			queued = true
		}
	}
	return queued, nil
}

// memberOwesHead reports whether a trigger should queue harness for the head.
// A reviewer already running on the head never is. Otherwise a rerun queues
// every reviewer, approvals included; an automatic trigger follows the
// auto-review retry rules; any other trigger, including `ao review trigger`
// (RejectReviewedHead), queues every reviewer that has not approved the head:
// pending, changes requested, failed or cancelled. The rule is the same with
// one reviewer or several.
//
// legacy is the harness that runs recorded before per-harness runs (empty
// Harness) belong to: the first expected reviewer, the session's own.
func memberOwesHead(runs []domain.ReviewRun, prURL, targetSHA string, harness, legacy domain.ReviewerHarness, opts TriggerOptions) bool {
	own := harnessRunsOnHead(runs, prURL, targetSHA, harness, legacy)
	latest, ok := latestReviewPass(own)
	switch {
	case ok && latest.Status == domain.ReviewRunRunning:
		return false
	case opts.Rerun || !ok:
		return true
	case opts.Source == domain.ReviewTriggerAuto:
		// Only this reviewer's passes (a legacy pass only for the first
		// reviewer) count toward its retry limit and verdicts.
		return !autoReviewHeadBlocked(own, prURL, targetSHA, harness)
	default:
		return latest.Verdict != domain.VerdictApproved
	}
}

// harnessRunsOnHead is harness's passes on one PR head. A pass recorded
// before per-harness runs has no harness; it belongs to legacy, the first
// expected reviewer, alone, so one old result never answers two reviewers.
func harnessRunsOnHead(runs []domain.ReviewRun, prURL, targetSHA string, harness, legacy domain.ReviewerHarness) []domain.ReviewRun {
	var out []domain.ReviewRun
	for _, run := range runs {
		if run.PRURL != prURL || run.TargetSHA != targetSHA {
			continue
		}
		if run.Harness == harness || (run.Harness == "" && harness == legacy) {
			out = append(out, run)
		}
	}
	return out
}

// latestReviewPass is the authoritative pass among one reviewer's passes: a
// running one, else the newest.
func latestReviewPass(runs []domain.ReviewRun) (domain.ReviewRun, bool) {
	var latest domain.ReviewRun
	found := false
	for _, run := range runs {
		if !found || newerReviewPass(run, latest) {
			latest, found = run, true
		}
	}
	return latest, found
}

// queueStamp is the time a reviewer is queued at: now, but always after every
// pass it already has on the head, so none of those passes can answer the new
// queueing even when one was stamped ahead of a wall clock that later stepped
// back.
func queueStamp(now time.Time, own []domain.ReviewRun) time.Time {
	for _, run := range own {
		if !run.CreatedAt.Before(now) {
			now = run.CreatedAt.Add(time.Nanosecond)
		}
	}
	return now
}

// memberWaiting reports whether a cycle member waits in the queue: it was
// queued and started no pass on the cycle's head since.
func memberWaiting(c domain.ReviewCycle, m domain.ReviewCycleMember, runs []domain.ReviewRun) bool {
	var newest domain.ReviewRun
	found := false
	for _, run := range harnessRunsOnHead(runs, c.PRURL, c.TargetSHA, m.Harness, c.LegacyHarness()) {
		if !found || run.CreatedAt.After(newest.CreatedAt) {
			newest, found = run, true
		}
	}
	return m.Waiting(newest.CreatedAt, found)
}

func prReviewable(pr domain.PullRequest) bool {
	return pr.URL != "" && pr.HeadSHA != "" && !pr.Merged && !pr.Closed
}

// AdvanceCycles is the review queue step for one worker. It closes cycles
// whose head moved or whose PR closed, and, when no reviewer is running for
// the worker, launches the next waiting member of its oldest open cycle. That
// reviewer reviews, in one batch, every open head whose cycle has it waiting.
// It is idempotent: with a reviewer running, or nobody waiting, it does
// nothing. The returned result has Created set only when it launched.
//
// Nothing is cleared after a launch: a member stops waiting because its new
// pass exists. A launch that fails before recording a pass leaves it waiting,
// and the next step (a submit, a trigger, or the minute sweep) tries again.
func (e *Engine) AdvanceCycles(ctx stdctx.Context, workerID domain.SessionID) (TriggerResult, error) {
	if workerID == "" {
		return TriggerResult{}, fmt.Errorf("%w: worker session id is required", ErrInvalid)
	}
	if err := e.waitForChatRecovery(ctx); err != nil {
		return TriggerResult{}, err
	}
	unlock := e.lockWorker(workerID)
	defer unlock()

	worker, ok, err := e.sessions.GetSession(ctx, workerID)
	if err != nil {
		return TriggerResult{}, err
	}
	if !ok {
		return TriggerResult{}, nil
	}
	if worker.IsTerminated {
		// A worker that ended owes no reviews; close its cycles so the sweep
		// stops visiting it.
		return TriggerResult{}, e.supersedeAllCycles(ctx, workerID, cycleSupersededWorkerEnded)
	}
	if worker.Metadata.WorkspacePath == "" {
		return TriggerResult{}, nil
	}
	result, _, err := e.advanceLocked(ctx, workerID, worker, cycleLaunch{})
	return result, err
}

func (e *Engine) supersedeAllCycles(ctx stdctx.Context, workerID domain.SessionID, reason string) error {
	cycles, err := e.store.ListOpenReviewCyclesBySession(ctx, workerID)
	if err != nil {
		return err
	}
	now := e.clock()
	for _, c := range cycles {
		if _, err := e.store.SupersedeReviewCycle(ctx, c.ID, reason, now); err != nil {
			return err
		}
	}
	return nil
}

// advanceLocked is AdvanceCycles under the worker lock.
func (e *Engine) advanceLocked(ctx stdctx.Context, workerID domain.SessionID, worker domain.SessionRecord, launch cycleLaunch) (TriggerResult, advanceOutcome, error) {
	open, prs, err := e.reconcileCycles(ctx, workerID)
	if err != nil {
		return TriggerResult{}, advanceIdle, err
	}
	runs, err := e.reconciledRuns(ctx, workerID, worker, false)
	if err != nil {
		return TriggerResult{}, advanceIdle, err
	}
	if reviewerRunningOnCurrentHead(runs, prs) {
		return TriggerResult{}, advanceBusy, nil
	}
	member, first, found := nextWaitingMember(open, runs, launch.prURL)
	if !found {
		return TriggerResult{}, advanceIdle, nil
	}
	// The reviewer reviews, in one batch, every open head that has it
	// waiting; onlyPRs keeps the launch to those heads.
	onlyPRs := make(map[string]bool)
	for _, c := range open {
		for _, m := range c.Members {
			if m.Harness == member.Harness && memberWaiting(c, m, runs) {
				onlyPRs[c.PRURL] = true
			}
		}
	}
	source := member.QueuedSource
	if source == "" {
		source = launch.source
	}
	if source == "" {
		source = cycleSource(runs, first)
	}
	// The member runs as a named reviewer with its snapshotted config. The
	// launch path treats onlyPRs heads as owed (the queue already decided
	// that), so a head another reviewer, or this one before a rerun, already
	// concluded is reviewed again.
	result, err := e.triggerHarnessLocked(ctx, workerID, worker, TriggerOptions{
		Harness:       member.Harness,
		Config:        member.AgentConfig,
		Source:        source,
		InterfaceMode: launch.mode,
		notBefore:     latestQueueing(open, member.Harness),
	}, onlyPRs)
	if err == nil && !result.Created {
		// A launch can rightly reuse a pass (a concurrent insert of the same
		// pass, say); only a member still waiting means nothing started.
		after, listErr := e.store.ListReviewRunsBySession(ctx, workerID)
		if listErr != nil {
			return result, advanceLaunched, listErr
		}
		if memberWaiting(first, member, after) {
			err = fmt.Errorf("%w: reviewer %s started no review pass for its queued heads", ErrConflict, member.Harness)
		}
	}
	return result, advanceLaunched, err
}

// latestQueueing is the latest time harness was queued in any open cycle, the
// earliest start its next pass may record.
func latestQueueing(open []domain.ReviewCycle, harness domain.ReviewerHarness) time.Time {
	var latest time.Time
	for _, c := range open {
		for _, m := range c.Members {
			if m.Harness == harness && m.QueuedAt.After(latest) {
				latest = m.QueuedAt
			}
		}
	}
	return latest
}

// nextWaitingMember is the queue's next reviewer: the first waiting member, in
// member order, of the first open cycle with one. preferPR's cycle goes first.
func nextWaitingMember(open []domain.ReviewCycle, runs []domain.ReviewRun, preferPR string) (domain.ReviewCycleMember, domain.ReviewCycle, bool) {
	ordered := make([]domain.ReviewCycle, 0, len(open))
	for _, c := range open {
		if preferPR != "" && c.PRURL == preferPR {
			ordered = append(ordered, c)
		}
	}
	for _, c := range open {
		if preferPR == "" || c.PRURL != preferPR {
			ordered = append(ordered, c)
		}
	}
	for _, c := range ordered {
		for _, m := range c.Members {
			if memberWaiting(c, m, runs) {
				return m, c, true
			}
		}
	}
	return domain.ReviewCycleMember{}, domain.ReviewCycle{}, false
}

// reviewerRunningOnCurrentHead reports a running pass on any PR's current
// head. A pass still running on an older head does not hold the queue: the
// next launch supersedes it.
func reviewerRunningOnCurrentHead(runs []domain.ReviewRun, prs []domain.PullRequest) bool {
	heads := make(map[string]bool, len(prs))
	for _, pr := range prs {
		if prReviewable(pr) {
			heads[pr.URL+"\x00"+pr.HeadSHA] = true
		}
	}
	for _, run := range runs {
		if run.Status == domain.ReviewRunRunning && heads[run.PRURL+"\x00"+run.TargetSHA] {
			return true
		}
	}
	return false
}

// reconcileCycles supersedes open cycles whose PR head moved or whose PR is
// closed, and returns the remaining cycles that match their PR's current head,
// oldest first. A cycle whose PR is not tracked or has no head right now is
// kept but not returned, so a transient PR read never closes it.
func (e *Engine) reconcileCycles(ctx stdctx.Context, workerID domain.SessionID) ([]domain.ReviewCycle, []domain.PullRequest, error) {
	prs, err := e.prs.ListPRsBySession(ctx, workerID)
	if err != nil {
		return nil, nil, err
	}
	cycles, err := e.store.ListOpenReviewCyclesBySession(ctx, workerID)
	if err != nil {
		return nil, nil, err
	}
	byURL := make(map[string]domain.PullRequest, len(prs))
	for _, pr := range prs {
		byURL[pr.URL] = pr
	}
	now := e.clock()
	open := make([]domain.ReviewCycle, 0, len(cycles))
	for _, c := range cycles {
		pr, ok := byURL[c.PRURL]
		reason := ""
		switch {
		case !ok || (pr.HeadSHA == "" && !pr.Merged && !pr.Closed):
			continue
		case pr.Merged || pr.Closed:
			reason = cycleSupersededPRNotOpen
		case pr.HeadSHA != c.TargetSHA:
			reason = cycleSupersededByNewerCommit
		}
		if reason == "" {
			open = append(open, c)
			continue
		}
		if _, err := e.store.SupersedeReviewCycle(ctx, c.ID, reason, now); err != nil {
			return nil, nil, err
		}
	}
	sort.SliceStable(open, func(i, j int) bool {
		if !open[i].CreatedAt.Equal(open[j].CreatedAt) {
			return open[i].CreatedAt.Before(open[j].CreatedAt)
		}
		return open[i].ID < open[j].ID
	})
	return open, prs, nil
}

// reconciledRuns lists the worker's runs after failing the running passes of
// any reviewer whose pane exited without submitting (the same check List
// makes), so a crashed reviewer does not hold the queue. With ownReviewer set
// it also makes the check a trigger for the session's own reviewer makes:
// that reviewer's running passes, legacy ones included, are cancelled when
// its terminal is gone.
func (e *Engine) reconciledRuns(ctx stdctx.Context, workerID domain.SessionID, worker domain.SessionRecord, ownReviewer bool) ([]domain.ReviewRun, error) {
	runs, err := e.store.ListReviewRunsBySession(ctx, workerID)
	if err != nil {
		return nil, err
	}
	if ownReviewer {
		harness, _, err := e.reviewerSelection(ctx, worker)
		if err != nil {
			return nil, err
		}
		row, hasRow, err := e.store.GetReviewBySessionAndHarness(ctx, workerID, harness)
		if err != nil {
			return nil, err
		}
		if changed, err := e.cancelStaleRunningRuns(ctx, workerID, harness, row, hasRow, runs); err != nil {
			return nil, err
		} else if changed {
			if runs, err = e.store.ListReviewRunsBySession(ctx, workerID); err != nil {
				return nil, err
			}
		}
	}
	rows, err := e.store.ListReviewsBySession(ctx, workerID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		changed, err := e.reconcileExitedReviewer(ctx, &rows[i], runs)
		if err != nil {
			return nil, err
		}
		if changed {
			if runs, err = e.store.ListReviewRunsBySession(ctx, workerID); err != nil {
				return nil, err
			}
		}
	}
	// Every launch runs under the worker lock the caller holds, so none is in
	// flight now. A terminal reviewer's running pass whose review row never
	// got a pane handle was orphaned by a crash mid-launch (a daemon restart,
	// say): fail it, or the one-reviewer-at-a-time queue waits on it forever.
	orphaned := false
	for _, row := range rows {
		if row.ReviewerHandleID != "" || row.InterfaceMode == domain.ReviewerInterfaceChat {
			continue
		}
		for _, run := range runs {
			if run.ReviewID != row.ID || run.Status != domain.ReviewRunRunning {
				continue
			}
			if _, err := e.store.UpdateReviewRunResult(ctx, run.ID, domain.ReviewRunFailed, domain.VerdictNone, reviewerLaunchIncomplete, "", run.AutoInjectReview); err != nil {
				return nil, err
			}
			orphaned = true
		}
	}
	if orphaned {
		if runs, err = e.store.ListReviewRunsBySession(ctx, workerID); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

// cycleSource is the trigger source a queue step records on the runs it
// starts: that of the newest pass on the cycle's head, so a cycle an
// automatic review opened keeps counting toward the auto retry limit.
func cycleSource(runs []domain.ReviewRun, c domain.ReviewCycle) domain.ReviewTriggerSource {
	var newest *domain.ReviewRun
	for i := range runs {
		run := runs[i]
		if run.PRURL != c.PRURL || run.TargetSHA != c.TargetSHA || run.TriggerSource == "" {
			continue
		}
		if newest == nil || run.CreatedAt.After(newest.CreatedAt) {
			newest = &runs[i]
		}
	}
	if newest == nil {
		return domain.ReviewTriggerManual
	}
	return newest.TriggerSource
}

// cyclePlan is the worker's per-PR review state for prs, expecting the open
// cycle's reviewers on each head that has one.
func (e *Engine) cyclePlan(ctx stdctx.Context, workerID domain.SessionID, worker domain.SessionRecord, prs []domain.PullRequest) ([]PRReviewState, []domain.ReviewRun, error) {
	runs, err := e.store.ListReviewRunsBySession(ctx, workerID)
	if err != nil {
		return nil, nil, err
	}
	expected, err := e.expectedReviewersByHead(ctx, workerID, worker)
	if err != nil {
		return nil, nil, err
	}
	return PlanWithExpected(prs, runs, expected), runs, nil
}

// expectedReviewersByHead resolves each PR head's expected reviewers: the
// open cycle's members when the head has one, else the worker's expected
// reviewers from the project config (Spec 1).
func (e *Engine) expectedReviewersByHead(ctx stdctx.Context, workerID domain.SessionID, worker domain.SessionRecord) (func(prURL, targetSHA string) HeadReviewers, error) {
	cfg, err := e.projectConfig(ctx, worker)
	if err != nil {
		return nil, err
	}
	reviewers := cfg.ExpectedReviewers(worker)
	fallback := make([]domain.ReviewerHarness, len(reviewers))
	for i, reviewer := range reviewers {
		fallback[i] = reviewer.Harness
	}
	cycles, err := e.store.ListOpenReviewCyclesBySession(ctx, workerID)
	if err != nil {
		return nil, err
	}
	byHead := make(map[string]HeadReviewers, len(cycles))
	for _, c := range cycles {
		head := HeadReviewers{Harnesses: c.Harnesses(), Since: map[domain.ReviewerHarness]time.Time{}}
		for _, member := range c.Members {
			if !member.QueuedAt.IsZero() {
				head.Since[member.Harness] = member.QueuedAt
			}
		}
		byHead[c.PRURL+"\x00"+c.TargetSHA] = head
	}
	return func(prURL, targetSHA string) HeadReviewers {
		if head, ok := byHead[prURL+"\x00"+targetSHA]; ok {
			return head
		}
		return HeadReviewers{Harnesses: fallback}
	}, nil
}
