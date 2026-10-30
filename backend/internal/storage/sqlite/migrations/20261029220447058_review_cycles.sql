-- Multi-reviewer queue (#5465). A review_cycle snapshots, per trigger, the PR
-- head and the ordered reviewers that head owes, so a later config edit cannot
-- change what an in-flight head requires. A head change supersedes the cycle
-- (superseded_at set); at most one open cycle exists per (worker, PR).
-- review_cycle_member holds the ordered reviewers and when each last entered the
-- queue (queued_at, NULL = never queued) and by which trigger (queued_source).
-- Whether a member still waits is derived from its runs, not stored.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE review_cycle (
    id                TEXT PRIMARY KEY,
    session_id        TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    pr_url            TEXT NOT NULL,
    target_sha        TEXT NOT NULL,
    created_at        TIMESTAMP NOT NULL,
    superseded_at     TIMESTAMP,
    superseded_reason TEXT NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_review_cycle_open_session_pr
    ON review_cycle (session_id, pr_url)
    WHERE superseded_at IS NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE review_cycle_member (
    cycle_id      TEXT NOT NULL REFERENCES review_cycle (id) ON DELETE CASCADE,
    position      INTEGER NOT NULL,
    harness       TEXT NOT NULL,
    -- JSON of domain.AgentConfig; '' when the reviewer uses harness defaults.
    agent_config  TEXT NOT NULL,
    -- NULL = never queued; passes started at or after it answer the queueing.
    queued_at     TIMESTAMP,
    -- domain.ReviewTriggerSource of the trigger that queued the member.
    queued_source TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (cycle_id, harness)
);
-- +goose StatementEnd

-- The open cycle decides a worker's expected reviewers on the board, so
-- opening or superseding one refreshes the worker the same way other
-- session-scoped facts do (session_updated, no new change_log event type).
-- +goose StatementBegin
CREATE TRIGGER review_cycle_cdc_insert
AFTER INSERT ON review_cycle
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
        json_object('id', s.id, 'sessionId', s.id, 'reviewCycleId', NEW.id), NEW.created_at
    FROM sessions s
    WHERE s.id = NEW.session_id;
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER review_cycle_cdc_supersede
AFTER UPDATE OF superseded_at ON review_cycle
WHEN OLD.superseded_at IS NULL AND NEW.superseded_at IS NOT NULL
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
        json_object('id', s.id, 'sessionId', s.id, 'reviewCycleId', NEW.id), NEW.superseded_at
    FROM sessions s
    WHERE s.id = NEW.session_id;
END;
-- +goose StatementEnd

-- Re-queueing a member changes what the worker's card waits on, so it refreshes
-- the worker too. The cycle's open/superseded state is not checked here: the
-- store only re-queues members of open cycles.
-- +goose StatementBegin
CREATE TRIGGER review_cycle_member_cdc_queue
AFTER UPDATE OF queued_at ON review_cycle_member
WHEN NEW.queued_at IS NOT NULL
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, s.id, 'session_updated',
        json_object('id', s.id, 'sessionId', s.id, 'reviewCycleId', NEW.cycle_id), NEW.queued_at
    FROM review_cycle c
    JOIN sessions s ON s.id = c.session_id
    WHERE c.id = NEW.cycle_id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS review_cycle_member_cdc_queue;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS review_cycle_cdc_supersede;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TRIGGER IF EXISTS review_cycle_cdc_insert;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE review_cycle_member;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE review_cycle;
-- +goose StatementEnd
