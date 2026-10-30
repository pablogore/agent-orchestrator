# Spec 2: review cycles

Part of [#5465](2026-10-29-multi-reviewer-design.md). Previous: [Spec 1](2026-10-29-multi-reviewer-spec-1.md). Next: Spec 3 (override changes, ad-hoc reviewers, timeout).

## Problem

After Spec 1, AO knows that a head needs every expected reviewer, but only auto review acts on that, one reviewer per sweep. A manual `ao review trigger` still launches one reviewer and stops; the second reviewer never runs unless someone asks for it by name. The set of expected reviewers is also re-read from the config on every check, so editing the project's reviewers while a review is under way changes what that head owes.

## What changes

A trigger now opens a **review cycle** for each PR head it covers. The cycle is a saved snapshot of the head SHA and the ordered list of expected reviewers at that moment. Example: a trigger on `sha1` with reviewers `claude-code, codex` saves a cycle `{sha1: [claude-code, codex]}` and launches `claude-code`.

The next reviewer is chosen by one idempotent step, `AdvanceCycles(worker)`. It does nothing if a reviewer is already running for the worker. Otherwise it launches the first cycle member that has no run on the cycle's head yet, through the existing single-reviewer trigger and with that member's own `agentConfig`. Because a member that failed or requested changes already has a run, the queue moves past it. AO calls this step:

- right after a reviewer submits its result,
- at the end of every trigger,
- on the auto-review coordinator's sweep, every minute, for every session with an open cycle, whether or not auto review is on. This is what moves the queue after a reviewer crashes or the daemon restarts.

When the PR's head moves, its cycle is closed as superseded, and the next trigger opens a new cycle for the new head. The planner, Kanban and auto review read the expected reviewers from the open cycle when one exists, and fall back to the project config (Spec 1) when none does.

The reviewer launch itself (`TriggerWithOptions`: terminal or chat, pane reuse, rollback) is not touched. Reviewers still share one terminal handle per worker, which is safe because only one runs at a time.

## Not in this spec

- Changing the session reviewer during a cycle, and `--rerun --agent X` joining the cycle: Spec 3.
- Timeout: Spec 3. A hung reviewer still blocks the queue until it exits or is cancelled.
- API, UI and docs: Spec 4.

## Decisions needed (recommendation first)

1. **Editing reviewers mid-cycle.** *Recommended:* the open cycle keeps its snapshot, and the new list applies from the next head. The alternative, re-reading the config, is what Spec 1 does today, and it lets a config edit approve or block a head after the fact.
2. **Manual trigger on a head with an open cycle.** *Recommended:* no new cycle. Members that did not approve (requested changes, failed, cancelled) are queued again, approvals are kept, and the queue advances. This keeps today's rule that a manual trigger can re-review a changes-requested head.
3. **Failed members under auto review.** *Recommended:* the queue first runs every member once. After that, auto review retries only the failed members, within the existing limit of 3 failed auto runs per head and reviewer.
4. **A worker with several PRs.** *Recommended:* one queue per worker, as today. The chosen reviewer reviews every head that still owes it, in one batch.

## Tasks

1. **Storage.** A migration (`npm run migration:create -- review_cycles`) adds `review_cycle(id, session_id, pr_url, target_sha, created_at, superseded_at, superseded_reason)` and `review_cycle_member(cycle_id, position, harness, agent_config)`. It also adds queries in `backend/internal/storage/sqlite/queries/review.sql`, then `npm run sqlc`, and store methods to open, list open, and supersede cycles.
   *Check:* store tests, plus the `shippedMigrations` test.
2. **Trigger opens cycles.** Manual, agent and auto triggers that don't name a reviewer open a cycle per eligible head, snapshotting `ExpectedReviewers`, then call `AdvanceCycles`.
   *Check:* `TestTriggerStartsWithFirstConfiguredReviewer`; a config edit after the cycle opened does not change its members.
3. **`AdvanceCycles`.** Implemented in `backend/internal/review`, and called after submit (`backend/internal/service/review/review.go` `submitOne`) and at the end of every trigger. It supersedes a cycle whose head moved.
   *Check:* `TestTriggerReviewsHeadWithNextConfiguredReviewer` (un-skipped); the queue advances after approve, after changes requested and after failure; it is idempotent; a head change mid-cycle starts nothing on the old head.
4. **Expected reviewers from the cycle.** The planner, Kanban (`backend/internal/service/session/kanban.go`) and the coordinator use the open cycle's members, falling back to Spec 1 when no cycle is open.
   *Check:* planner and Kanban tests where the config changed after the cycle opened.
5. **Sweep.** The coordinator's sweep calls `AdvanceCycles` for every session with an open cycle, independently of the auto-review gate.
   *Check:* a coordinator test where the reviewer exited without submitting, and the next sweep starts the next member.

## As implemented

Decisions confirmed after the first implementation (2026-10-29):

- **Manual triggers.** `ao review trigger` without `--agent` keeps approvals on the same head and re-queues every reviewer that has not approved it: never run, requested changes, failed or cancelled. On `main`, this command refused any head that any reviewer had already reviewed; that refusal now applies only when every expected reviewer approved. `--rerun` without `--agent` re-runs every expected reviewer, approvals included. The rule is the same with one reviewer or several. `--agent` stays as on `main` until Spec 3.
- **Queue state is derived, not stored.** A cycle member stores when it was last queued (`queued_at`) and by which trigger (`queued_source`). It waits while it has no pass of its own on the cycle's head started at or after `queued_at`; its passes from before that no longer count. So a rerun's own verdict decides, and a launch that fails before recording a pass leaves the reviewer waiting instead of letting an old approval count.
- **Closed or merged PR.** Its cycle is closed and nothing more launches for it, including when the close lands between a submit and its queue step. A worker that ends closes its cycles too.
- **One reviewer.** Auto review keeps the old path without cycles; results and retries are the same as on `main`.
- **Legacy passes** (no reviewer recorded) count only for the first expected reviewer, on their own PR head. The planner, the board and the coordinator all keep the newest pass per reviewer, so one old approval never satisfies two reviewers or another head.
- **Auto review** runs the whole queue first, then retries only failed reviewers by name, within 3 failed automatic passes per head and reviewer. A named automatic retry waits while another reviewer runs. When every head has an open cycle, auto review takes the reviewers from the cycle, not from the edited config.
- **Recovery.** The minute sweep moves every open queue. A reviewer whose pane died is failed and the queue moves on; a running pass whose launch never recorded a pane (a crash mid-launch) is failed as `reviewer launch did not complete`.

Known limits:

- The queue step after a submit waits 2 seconds (`submitAdvanceDelay`) before the next reviewer takes the worker's shared reviewer terminal, so the submitting reviewer's `ao review submit` has its answer first. It is a delay, not a handshake: under heavy load the previous reviewer's pane can still close before its CLI prints the response. The result is already saved by then. The step is dropped on daemon shutdown and the sweep picks the queue up after a restart.
- `--rerun` does not re-queue a reviewer whose pass is already running; that running pass answers the rerun, as on `main` with one reviewer.
- A launch that keeps failing before it records a pass (for example, the old pane of another reviewer cannot be destroyed) leaves the reviewer waiting, and the minute sweep tries it again with no backoff; reviewers behind it wait too. Nothing is approved by mistake, and the failure shows in the daemon log.
- Switching a reviewer from Chat to the terminal while a Chat pass is still running ends that pass; the next queue step records it as `reviewer launch did not complete` rather than as a cancelled Chat pass.
