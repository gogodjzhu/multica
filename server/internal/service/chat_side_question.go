package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

const (
	SideQuestionMaxAge    = 10 * time.Minute
	SideQuestionRetention = 10 * time.Minute
)

var (
	ErrSideQuestionBusy        = errors.New("a side question is already pending for this chat or runtime")
	ErrSideQuestionUnavailable = errors.New("side question runtime is unavailable")
)

// IsSideQuestionTask survives session cleanup, which preserves ordinary run history.
func IsSideQuestionTask(task db.AgentTaskQueue) bool {
	var meta struct {
		Purpose string `json:"purpose"`
	}
	return json.Unmarshal(task.Context, &meta) == nil && meta.Purpose == "side_question"
}

func (s *TaskService) EnqueueChatSideQuestion(ctx context.Context, source db.ChatSession, agent db.Agent, userID pgtype.UUID, prompt, actorType string, actorID pgtype.UUID) (*DirectChatSendResult, error) {
	if s.TxStarter == nil {
		return nil, errors.New("side questions require database transactions")
	}
	session := db.ChatSession{ID: dbid.NewV7(), AgentID: source.AgentID, WorkspaceID: source.WorkspaceID, CreatorID: userID}
	return s.sendDirectChatMessage(ctx, session, agent, userID, prompt, nil, actorType, actorID,
		func(ctx context.Context, q *db.Queries) (db.ChatSession, error) {
			return s.createSideQuestionSessionTx(ctx, q, source, session.ID, userID)
		})
}

func (s *TaskService) createSideQuestionSessionTx(ctx context.Context, q *db.Queries, source db.ChatSession, id, userID pgtype.UUID) (db.ChatSession, error) {
	if _, err := q.LockChatSessionForRuntimeBind(ctx, source.ID); err != nil {
		return db.ChatSession{}, err
	}
	current, err := q.GetChatSessionInWorkspace(ctx, db.GetChatSessionInWorkspaceParams{ID: source.ID, WorkspaceID: source.WorkspaceID})
	if err != nil {
		return db.ChatSession{}, err
	}
	if current.Purpose != "chat" || current.CreatorID != userID || current.AgentID != source.AgentID {
		return db.ChatSession{}, pgx.ErrNoRows
	}
	if current.Status != "active" {
		return db.ChatSession{}, ErrChatSessionArchived
	}
	return createSideQuestionForAgentTx(ctx, q, db.CreateSideQuestionChatSessionParams{
		ID: id, WorkspaceID: source.WorkspaceID, AgentID: current.AgentID, CreatorID: userID,
		SourceSessionID: source.ID,
	})
}

func createSideQuestionForAgentTx(ctx context.Context, q *db.Queries, session db.CreateSideQuestionChatSessionParams) (db.ChatSession, error) {
	agent, err := q.GetAgentForClaimUpdate(ctx, session.AgentID)
	if err != nil {
		return db.ChatSession{}, err
	}
	if agent.WorkspaceID != session.WorkspaceID {
		return db.ChatSession{}, pgx.ErrNoRows
	}
	if agent.ArchivedAt.Valid {
		return db.ChatSession{}, ErrChatTaskAgentArchived
	}
	if !agent.RuntimeID.Valid {
		return db.ChatSession{}, ErrChatTaskAgentNoRuntime
	}
	if err := q.LockSideQuestionRuntime(ctx, agent.RuntimeID); err != nil {
		return db.ChatSession{}, err
	}
	runtime, err := q.GetAgentRuntime(ctx, agent.RuntimeID)
	if err != nil {
		return db.ChatSession{}, err
	}
	if runtime.Status != "online" {
		return db.ChatSession{}, ErrSideQuestionUnavailable
	}
	busy, err := q.HasPendingSideQuestion(ctx, db.HasPendingSideQuestionParams{
		WorkspaceID: session.WorkspaceID, SourceSessionID: session.SourceSessionID, SourceIssueID: session.SourceIssueID, RuntimeID: agent.RuntimeID,
	})
	if err != nil {
		return db.ChatSession{}, err
	}
	if busy {
		return db.ChatSession{}, ErrSideQuestionBusy
	}
	session.RuntimeID = agent.RuntimeID
	return q.CreateSideQuestionChatSession(ctx, session)
}

// SweepChatSideQuestions cancels overdue runs before retiring their result sessions.
func (s *TaskService) SweepChatSideQuestions(ctx context.Context, limit int32) error {
	if s.TxStarter == nil {
		return errors.New("side question cleanup requires database transactions")
	}
	candidates, err := s.Queries.ListSideQuestionSweepCandidates(ctx, db.ListSideQuestionSweepCandidatesParams{
		MaxAgeSeconds: SideQuestionMaxAge.Seconds(), RetentionSeconds: SideQuestionRetention.Seconds(), BatchLimit: limit,
	})
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err := s.sweepChatSideQuestion(ctx, candidate); err != nil {
			return fmt.Errorf("sweep side question: %w", err)
		}
	}
	return nil
}

func (s *TaskService) sweepChatSideQuestion(ctx context.Context, session db.ChatSession) error {
	var cancelled []db.AgentTaskQueue
	err := s.runInTx(ctx, func(q *db.Queries) error {
		if _, err := q.LockChatSessionForDelete(ctx, session.ID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if _, err := q.GetAgentForClaimUpdate(ctx, session.AgentID); err != nil {
			return err
		}
		tasks, err := q.ListSideQuestionTasks(ctx, session.ID)
		if err != nil {
			return err
		}
		latest := session.CreatedAt.Time
		for _, task := range tasks {
			switch task.Status {
			case "queued", "dispatched", "running", "waiting_local_directory", "deferred":
				// Only overdue or orphaned active sessions are selected. A concurrent
				// sweeper may have settled them while this one waited for the lock.
				row, err := q.CancelAgentTaskWithReason(ctx, db.CancelAgentTaskWithReasonParams{
					ID: task.ID, Error: pgtype.Text{String: "Side question expired", Valid: true},
					FailureReason: pgtype.Text{String: "side_question_timeout", Valid: true},
				})
				if err != nil {
					return err
				}
				cancelled = append(cancelled, row)
			}
			if task.CompletedAt.Valid && task.CompletedAt.Time.After(latest) {
				latest = task.CompletedAt.Time
			}
		}
		if len(cancelled) > 0 {
			return SettleTerminalTaskState(ctx, q, cancelled...)
		}
		if time.Since(latest) < SideQuestionRetention {
			return nil
		}
		if err := q.DeleteChatDraftRestoresBySession(ctx, session.ID); err != nil {
			return err
		}
		if err := q.DeleteSideQuestionMessages(ctx, session.ID); err != nil {
			return err
		}
		return q.DeleteChatSession(ctx, db.DeleteChatSessionParams{ID: session.ID, WorkspaceID: session.WorkspaceID})
	})
	if err == nil && len(cancelled) > 0 {
		s.ReconcileAgentStatus(ctx, session.AgentID)
		for _, task := range cancelled {
			s.NotifyTaskFinished(task)
		}
	}
	return err
}
