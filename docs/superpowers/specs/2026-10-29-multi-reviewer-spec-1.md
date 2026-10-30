# Spec 1: the review aggregate knows every expected reviewer

Part of [#5465](2026-10-29-multi-reviewer-design.md). Next: Spec 2 (review cycles).

## Problem

AO decides whether a PR head is reviewed by looking at the newest run, regardless of who ran it. So when `claude-code` requests changes and `codex` approves a minute later, the PR shows as approved. When `claude-code` approves and `codex` fails, the Kanban board still says approved. Auto review stops after the first configured reviewer approves, so the second one never runs.

## What changes

AO computes a list of **expected reviewers** for a session. It is the session's own reviewer when one is set, otherwise the project's `reviewers` list, and any reviewer that already ran on the current head is added to it. Then it looks at each expected reviewer's newest run on the current head:

- if any reviewer is running → `running`
- else if any reviewer requested changes → `changes_requested`
- else if any reviewer failed, was cancelled or has not run → `needs_review`
- else (everyone approved) → `up_to_date`

Planner status, Kanban and auto review all use this one calculation. Auto review then triggers the first expected reviewer that has not approved, so with two reviewers configured it reviews with `claude-code`, and on the next sweep with `codex`.

A project with one reviewer behaves exactly as today: the list has one entry, and a re-run by the same reviewer still replaces its earlier verdict.

## Not in this spec

No new tables, and manual `ao review trigger` still launches one reviewer (Spec 2). Override changes, ad-hoc rules and timeouts are in Spec 3; API and UI changes are in Spec 4.

## Decision needed

Should the result keep using the four existing states, mapped as above? **Recommended: yes.** The alternative, a new `incomplete` state for failures, would change the generated API types and every frontend screen that reads them.

## Tasks

1. **Expected reviewers.** Add `ExpectedReviewers` in `backend/internal/domain/projectconfig.go`, and make `ProjectConfig.Validate` reject the same harness listed twice.
   *Check:* `go test ./internal/domain/`
2. **Planner.** `Plan` in `backend/internal/review/planner.go` receives the expected reviewers and applies the rule above.
   *Check:* `TestPlanChangesRequestedBlocksLaterApprovalOnSameHead` passes, and `go test ./internal/review/` stays green.
3. **Kanban.** `toContractKanbanPRFacts` in `backend/internal/service/session/kanban.go` counts as approved only when every expected reviewer approved.
   *Check:* `TestKanbanFailedReviewerBlocksApprovalOfCurrentHead` passes, and `go test ./internal/service/session/ ./pkg/contract/` stays green.
4. **Auto review.** `backend/internal/autoreview/coordinator.go` triggers the first expected reviewer that has not approved the head. That reviewer launches with its own `agentConfig` from `reviewers`, not an empty one (`TriggerWithOptions` in `backend/internal/review/review.go`).
   *Check:* `TestEvaluateSessionReviewsHeadWithEveryConfiguredReviewer` passes, and `go test ./internal/autoreview/` stays green.
