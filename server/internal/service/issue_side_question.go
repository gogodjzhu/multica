package service

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgtype"

	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
)

var ErrSideQuestionAssigneeChanged = errors.New("issue assignee changed before side question enqueue")

func (s *TaskService) EnqueueIssueSideQuestion(ctx context.Context, source db.Issue, agent db.Agent, userID pgtype.UUID, prompt, actorType string, actorID pgtype.UUID) (*DirectChatSendResult, error) {
	if s.TxStarter == nil {
		return nil, errors.New("side questions require database transactions")
	}
	session := db.ChatSession{ID: dbid.NewV7(), AgentID: agent.ID, WorkspaceID: source.WorkspaceID, CreatorID: userID}
	return s.sendDirectChatMessage(ctx, session, agent, userID, prompt, nil, actorType, actorID,
		func(ctx context.Context, q *db.Queries) (db.ChatSession, error) {
			if _, err := q.LockIssueForDelete(ctx, db.LockIssueForDeleteParams{ID: source.ID, WorkspaceID: source.WorkspaceID}); err != nil {
				return db.ChatSession{}, err
			}
			current, err := q.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: source.ID, WorkspaceID: source.WorkspaceID})
			if err != nil {
				return db.ChatSession{}, err
			}
			if current.AssigneeType != source.AssigneeType || current.AssigneeID != source.AssigneeID {
				return db.ChatSession{}, ErrSideQuestionAssigneeChanged
			}
			switch current.AssigneeType.String {
			case "agent":
				if current.AssigneeID != agent.ID {
					return db.ChatSession{}, ErrSideQuestionAssigneeChanged
				}
			case "squad":
				squad, err := q.LockSquadForAutopilotAssignment(ctx, db.LockSquadForAutopilotAssignmentParams{ID: current.AssigneeID, WorkspaceID: current.WorkspaceID})
				if err != nil {
					return db.ChatSession{}, err
				}
				if squad.ArchivedAt.Valid || squad.LeaderID != agent.ID {
					return db.ChatSession{}, ErrSideQuestionAssigneeChanged
				}
			default:
				return db.ChatSession{}, ErrSideQuestionAssigneeChanged
			}
			return createSideQuestionForAgentTx(ctx, q, db.CreateSideQuestionChatSessionParams{
				ID: session.ID, WorkspaceID: source.WorkspaceID, AgentID: agent.ID, CreatorID: userID,
				SourceIssueID: source.ID,
			})
		})
}
