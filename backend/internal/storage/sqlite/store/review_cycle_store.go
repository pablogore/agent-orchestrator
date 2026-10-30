package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// InsertReviewCycle opens a review cycle and its members in one transaction
// (#5465). A second open cycle for the same (session, PR) hits the partial
// unique index and is surfaced as domain.ErrDuplicateReviewCycle, so a racing
// trigger can fall back to the cycle that won.
func (s *Store) InsertReviewCycle(ctx context.Context, c domain.ReviewCycle) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "insert review cycle", func(q *gen.Queries) error {
		if err := q.InsertReviewCycle(ctx, gen.InsertReviewCycleParams{
			ID:        c.ID,
			SessionID: c.SessionID,
			PRURL:     c.PRURL,
			TargetSha: c.TargetSHA,
			CreatedAt: c.CreatedAt,
		}); err != nil {
			return err
		}
		for _, m := range c.Members {
			agentConfig, err := marshalAgentConfig(m.AgentConfig)
			if err != nil {
				return err
			}
			if err := q.InsertReviewCycleMember(ctx, gen.InsertReviewCycleMemberParams{
				CycleID:      c.ID,
				Position:     int64(m.Position),
				Harness:      m.Harness,
				AgentConfig:  agentConfig,
				QueuedAt:     timeToNullTime(m.QueuedAt),
				QueuedSource: m.QueuedSource,
			}); err != nil {
				return fmt.Errorf("member %s: %w", m.Harness, err)
			}
		}
		return nil
	})
	if isSQLiteUnique(err) {
		return fmt.Errorf("insert review cycle for session %s pr %s: %w", c.SessionID, c.PRURL, domain.ErrDuplicateReviewCycle)
	}
	return err
}

// ListOpenReviewCyclesBySession returns the session's open cycles, oldest
// first, each with its members in queue order.
func (s *Store) ListOpenReviewCyclesBySession(ctx context.Context, id domain.SessionID) ([]domain.ReviewCycle, error) {
	bySession, err := s.ListOpenReviewCyclesForSessions(ctx, []domain.SessionID{id})
	if err != nil {
		return nil, err
	}
	return bySession[id], nil
}

// ListOpenReviewCyclesForSessions batches ListOpenReviewCyclesBySession for
// session-list reads, returning every requested session id with an empty slice
// when it has no open cycle.
func (s *Store) ListOpenReviewCyclesForSessions(ctx context.Context, ids []domain.SessionID) (map[domain.SessionID][]domain.ReviewCycle, error) {
	out := make(map[domain.SessionID][]domain.ReviewCycle, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("marshal session ids: %w", err)
	}
	// Cycles and members are read in one snapshot: a supersede committed
	// between the two reads would otherwise return an open cycle without its
	// members.
	var (
		cycles  []gen.ListOpenReviewCyclesBySessionsRow
		members []gen.ReviewCycleMember
	)
	err = s.inReadTx(ctx, "list open review cycles for sessions", func(q *gen.Queries) error {
		var err error
		if cycles, err = q.ListOpenReviewCyclesBySessions(ctx, string(encoded)); err != nil {
			return fmt.Errorf("cycles: %w", err)
		}
		if members, err = q.ListOpenReviewCycleMembersBySessions(ctx, string(encoded)); err != nil {
			return fmt.Errorf("members: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	membersByCycle := make(map[string][]domain.ReviewCycleMember, len(cycles))
	for _, m := range members {
		membersByCycle[m.CycleID] = append(membersByCycle[m.CycleID], domain.ReviewCycleMember{
			Position:     int(m.Position),
			Harness:      m.Harness,
			AgentConfig:  unmarshalAgentConfig(m.AgentConfig),
			QueuedAt:     nullTimeToTime(m.QueuedAt),
			QueuedSource: m.QueuedSource,
		})
	}
	for _, c := range cycles {
		out[c.SessionID] = append(out[c.SessionID], domain.ReviewCycle{
			ID:        c.ID,
			SessionID: c.SessionID,
			PRURL:     c.PRURL,
			TargetSHA: c.TargetSha,
			Members:   membersByCycle[c.ID],
			CreatedAt: c.CreatedAt,
		})
	}
	for _, id := range ids {
		if out[id] == nil {
			out[id] = []domain.ReviewCycle{}
		}
	}
	return out, nil
}

// ListSessionsWithOpenReviewCycles returns every session that still has an
// open cycle, so a sweep can advance their queues.
func (s *Store) ListSessionsWithOpenReviewCycles(ctx context.Context) ([]domain.SessionID, error) {
	ids, err := s.qr.ListSessionsWithOpenReviewCycles(ctx)
	if err != nil {
		return nil, fmt.Errorf("list sessions with open review cycles: %w", err)
	}
	return ids, nil
}

// SupersedeReviewCycle closes an open cycle. It reports false when the cycle
// does not exist or was already superseded.
func (s *Store) SupersedeReviewCycle(ctx context.Context, id, reason string, at time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SupersedeReviewCycle(ctx, gen.SupersedeReviewCycleParams{
		SupersededAt:     sql.NullTime{Time: at, Valid: true},
		SupersededReason: reason,
		ID:               id,
	})
	if err != nil {
		return false, fmt.Errorf("supersede review cycle %s: %w", id, err)
	}
	return n > 0, nil
}

// SetReviewCycleMemberQueued (re-)queues a member of an open cycle at the
// given time, under the trigger source its next pass is recorded with. It
// reports false when the member does not exist or its cycle was superseded.
func (s *Store) SetReviewCycleMemberQueued(ctx context.Context, cycleID string, harness domain.ReviewerHarness, at time.Time, source domain.ReviewTriggerSource) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.SetReviewCycleMemberQueued(ctx, gen.SetReviewCycleMemberQueuedParams{
		QueuedAt:     timeToNullTime(at),
		QueuedSource: source,
		CycleID:      cycleID,
		Harness:      harness,
	})
	if err != nil {
		return false, fmt.Errorf("queue review cycle %s member %s: %w", cycleID, harness, err)
	}
	return n > 0, nil
}
