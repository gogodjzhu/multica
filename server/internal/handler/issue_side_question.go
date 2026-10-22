package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (h *Handler) AskIssueSideQuestion(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	question, ok := readSideQuestion(w, r)
	if !ok {
		return
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if !issue.AssigneeID.Valid || (issue.AssigneeType.String != "agent" && issue.AssigneeType.String != "squad") {
		writeError(w, http.StatusConflict, "issue must be assigned to an agent or squad")
		return
	}
	agentID := issue.AssigneeID
	if issue.AssigneeType.String == "squad" {
		squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{ID: issue.AssigneeID, WorkspaceID: issue.WorkspaceID})
		if err != nil || squad.ArchivedAt.Valid {
			writeError(w, http.StatusConflict, "assigned squad is unavailable")
			return
		}
		agentID = squad.LeaderID
	}
	agent, err := h.Queries.GetAgent(r.Context(), agentID)
	if err != nil || agent.WorkspaceID != issue.WorkspaceID || agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
		writeError(w, http.StatusConflict, "assigned agent is unavailable")
		return
	}
	workspaceID := uuidToString(issue.WorkspaceID)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if !h.canInvokeAgent(r.Context(), agent, actorType, actorID, h.invokeOriginatorFromRequest(r, actorType, actorID), workspaceID) {
		h.writeDispatchBlocked(w, http.StatusForbidden, ReasonInvocationNotAllowed)
		return
	}
	prompt, err := h.buildIssueSideQuestionPrompt(r.Context(), issue, agent.ID, question)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load issue snapshot")
		return
	}
	result, err := h.TaskService.EnqueueIssueSideQuestion(r.Context(), issue, agent, parseUUID(userID), prompt, actorType, parseUUID(actorID))
	if err != nil {
		switch {
		case errors.Is(err, service.ErrSideQuestionBusy):
			writeError(w, http.StatusTooManyRequests, "a side question is already pending for this issue or runtime")
		case errors.Is(err, service.ErrSideQuestionUnavailable):
			writeError(w, http.StatusServiceUnavailable, "assigned agent runtime is offline")
		case errors.Is(err, service.ErrSideQuestionAssigneeChanged), errors.Is(err, service.ErrChatTaskAgentArchived), errors.Is(err, service.ErrChatTaskAgentNoRuntime):
			writeError(w, http.StatusConflict, "assigned agent is unavailable or changed")
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "issue or agent not found")
		default:
			writeError(w, http.StatusInternalServerError, "failed to enqueue side question")
		}
		return
	}
	writeJSON(w, http.StatusAccepted, sideQuestionAccepted{ID: uuidToString(result.Task.ID), Status: "pending"})
}

func (h *Handler) loadIssueSideQuestionForUser(w http.ResponseWriter, r *http.Request) (db.AgentTaskQueue, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	return h.loadSideQuestionTaskForUser(w, r, db.GetSideQuestionChatSessionParams{
		WorkspaceID: issue.WorkspaceID, SourceIssueID: issue.ID, CreatorID: parseUUID(userID),
	})
}

func (h *Handler) GetIssueSideQuestion(w http.ResponseWriter, r *http.Request) {
	task, ok := h.loadIssueSideQuestionForUser(w, r)
	if ok {
		h.writeSideQuestionStatus(w, r, task)
	}
}

func (h *Handler) CancelIssueSideQuestion(w http.ResponseWriter, r *http.Request) {
	task, ok := h.loadIssueSideQuestionForUser(w, r)
	if ok {
		h.cancelSideQuestion(w, r, task)
	}
}

func (h *Handler) buildIssueSideQuestionPrompt(ctx context.Context, issue db.Issue, agentID pgtype.UUID, question string) (string, error) {
	comments, err := h.Queries.ListIssueSideQuestionComments(ctx, db.ListIssueSideQuestionCommentsParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID})
	if err != nil {
		return "", err
	}
	messages, err := h.Queries.ListIssueSideQuestionMessages(ctx, db.ListIssueSideQuestionMessagesParams{IssueID: issue.ID, WorkspaceID: issue.WorkspaceID, AgentID: agentID})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(sideQuestionInstructions)
	b.WriteString("\nThe conversation below belongs to an issue. Its assigned agent answers this side question independently.\n\n<issue_snapshot>\n")
	fmt.Fprintf(&b, "Title: %s\nStatus: %s\nDescription: %s\n", sideQuestionCapRunes(issue.Title, 500), issue.Status, sideQuestionCapRunes(issue.Description.String, 6000))
	b.WriteString("\nRecent comments (oldest to newest):\n")
	for i := len(comments) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "%s: %s\n\n", comments[i].AuthorType, sideQuestionCapRunes(comments[i].Content, sideQuestionMessageRunes))
	}
	b.WriteString("\nAssigned agent's persisted run text (oldest to newest):\n")
	for i := len(messages) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "assistant: %s\n\n", sideQuestionCapRunes(messages[i].Content, sideQuestionMessageRunes))
	}
	b.WriteString("</issue_snapshot>\n\nSide question:\n")
	b.WriteString(question)
	return b.String(), nil
}
