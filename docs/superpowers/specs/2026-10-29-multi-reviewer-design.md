# Multiple reviewers per PR head (#5465)

`ao project set-config --reviewer` accepts several reviewers, but the daemon only ever uses the first one. This chain makes every configured reviewer review the PR head, one after another, and approves the head only when all of them approved it. It covers the local daemon only; Cloud keeps its one-reviewer limit.

The agreed rules: reviewers run sequentially; a rejection, failure or timeout does not stop the rest; approval needs every expected reviewer to approve the same current head; a new head starts over; a session override replaces the project's reviewers; an explicit ad-hoc reviewer joins the set for that head.

## Chain

1. **[Spec 1](2026-10-29-multi-reviewer-spec-1.md): the aggregate knows every expected reviewer.** Status, Kanban and auto review stop treating one reviewer's approval as the whole answer. Auto review then reaches every reviewer, one per sweep.
2. **[Spec 2](2026-10-29-multi-reviewer-spec-2.md): review cycles.** Each trigger saves the head and its reviewers, and the queue advances on each result.
3. **Spec 3: override changes, ad-hoc reviewers and timeout.** *(later)*
4. **Spec 4: API, settings form that keeps reviewers 2..N, inspector and docs.** *(later)*

Outside this chain: a multi-reviewer editor in settings, multiple reviewers in Cloud, and parallel execution.
