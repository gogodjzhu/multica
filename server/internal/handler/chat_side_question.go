package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

const (
	sideQuestionMaxRunes        = 2000
	sideQuestionContextMessages = 12
	sideQuestionMessageRunes    = 1500
	sideQuestionAnswerRunes     = 8000
)

const sideQuestionInstructions = `Answer the question based on the provided context.
Keep all operations read-only, including commands, tools, and API calls.
If a requested action would modify files, tasks, or any other state,
stop and explain why it cannot be performed in this read-only request.`

type sideQuestionRequest struct {
	Content string `json:"content"`
}
type sideQuestionAccepted struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}
type sideQuestionStatusResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Answer string `json:"answer,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (h *Handler) AskChatSideQuestion(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	question, ok := readSideQuestion(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	source, ok := h.gatePublicChatSessionForUser(w, r, userID, workspaceID, chi.URLParam(r, "sessionId"))
	if !ok {
		return
	}
	if source.Status != "active" {
		writeError(w, http.StatusConflict, "chat session is archived")
		return
	}
	agent, err := h.Queries.GetAgent(r.Context(), source.AgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load chat agent")
		return
	}
	if agent.ArchivedAt.Valid || !agent.RuntimeID.Valid {
		writeError(w, http.StatusConflict, "chat agent is unavailable")
		return
	}
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if !h.canInvokeAgent(r.Context(), agent, actorType, actorID, h.invokeOriginatorFromRequest(r, actorType, actorID), workspaceID) {
		h.writeDispatchBlocked(w, http.StatusForbidden, ReasonInvocationNotAllowed)
		return
	}
	prompt, err := h.buildChatSideQuestionPrompt(r.Context(), source.ID, question)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load conversation snapshot")
		return
	}
	result, err := h.TaskService.EnqueueChatSideQuestion(r.Context(), source, agent, parseUUID(userID), prompt, actorType, parseUUID(actorID))
	if err != nil {
		switch {
		case errors.Is(err, service.ErrSideQuestionBusy):
			writeError(w, http.StatusTooManyRequests, "a side question is already pending for this chat or runtime")
		case errors.Is(err, service.ErrSideQuestionUnavailable):
			writeError(w, http.StatusServiceUnavailable, "chat agent runtime is offline")
		case errors.Is(err, service.ErrChatSessionArchived), errors.Is(err, service.ErrChatTaskAgentArchived), errors.Is(err, service.ErrChatTaskAgentNoRuntime):
			writeError(w, http.StatusConflict, "chat agent or session is unavailable")
		case errors.Is(err, pgx.ErrNoRows):
			writeError(w, http.StatusNotFound, "chat session not found")
		default:
			writeError(w, http.StatusInternalServerError, "failed to enqueue side question")
		}
		return
	}
	writeJSON(w, http.StatusAccepted, sideQuestionAccepted{ID: uuidToString(result.Task.ID), Status: "pending"})
}

func (h *Handler) loadSideQuestionForUser(w http.ResponseWriter, r *http.Request) (db.AgentTaskQueue, bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	source, ok := h.gatePublicChatSessionForUser(w, r, userID, ctxWorkspaceID(r.Context()), chi.URLParam(r, "sessionId"))
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	return h.loadSideQuestionTaskForUser(w, r, db.GetSideQuestionChatSessionParams{
		WorkspaceID: source.WorkspaceID, SourceSessionID: source.ID, CreatorID: parseUUID(userID),
	})
}

func (h *Handler) loadSideQuestionTaskForUser(w http.ResponseWriter, r *http.Request, source db.GetSideQuestionChatSessionParams) (db.AgentTaskQueue, bool) {
	id, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "requestId"), "request id")
	if !ok {
		return db.AgentTaskQueue{}, false
	}
	source.TaskID = id
	_, err := h.Queries.GetSideQuestionChatSession(r.Context(), source)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, "side question not found")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to load side question")
		}
		return db.AgentTaskQueue{}, false
	}
	task, err := h.Queries.GetAgentTask(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load side question")
		return db.AgentTaskQueue{}, false
	}
	return task, true
}

func (h *Handler) GetChatSideQuestion(w http.ResponseWriter, r *http.Request) {
	task, ok := h.loadSideQuestionForUser(w, r)
	if !ok {
		return
	}
	h.writeSideQuestionStatus(w, r, task)
}

func (h *Handler) writeSideQuestionStatus(w http.ResponseWriter, r *http.Request, task db.AgentTaskQueue) {
	resp := sideQuestionStatusResponse{ID: uuidToString(task.ID)}
	switch task.Status {
	case "queued", "dispatched", "deferred":
		resp.Status = "pending"
	case "running", "waiting_local_directory":
		resp.Status = "running"
	case "cancelled":
		resp.Status = "cancelled"
		if task.FailureReason.String == "side_question_timeout" {
			resp.Status = "timeout"
		}
	case "completed":
		msg, err := h.Queries.GetChatMessageByTaskAssistant(r.Context(), task.ID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "failed to load side question answer")
			return
		}
		if err != nil || msg.MessageKind != protocol.ChatMessageKindMessage || strings.TrimSpace(msg.Content) == "" {
			resp.Status, resp.Error = "failed", "The agent returned no text answer"
		} else {
			resp.Status, resp.Answer = "completed", sideQuestionCapRunes(msg.Content, sideQuestionAnswerRunes)
		}
	default:
		resp.Status, resp.Error = "failed", "The side question could not be completed"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) CancelChatSideQuestion(w http.ResponseWriter, r *http.Request) {
	task, ok := h.loadSideQuestionForUser(w, r)
	if !ok {
		return
	}
	h.cancelSideQuestion(w, r, task)
}

func (h *Handler) cancelSideQuestion(w http.ResponseWriter, r *http.Request, task db.AgentTaskQueue) {
	if _, err := h.TaskService.CancelTask(r.Context(), task.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to cancel side question")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) buildChatSideQuestionPrompt(ctx context.Context, sessionID pgtype.UUID, question string) (string, error) {
	rows, err := h.Queries.ListChatMessagesPage(ctx, db.ListChatMessagesPageParams{ChatSessionID: sessionID, Limit: sideQuestionContextMessages * 2})
	if err != nil {
		return "", err
	}
	selected := make([]db.ChatMessage, 0, sideQuestionContextMessages)
	for _, row := range rows {
		if (row.Role != "user" && row.Role != "assistant") || row.MessageKind != protocol.ChatMessageKindMessage || strings.TrimSpace(row.Content) == "" {
			continue
		}
		selected = append(selected, row)
		if len(selected) == sideQuestionContextMessages {
			break
		}
	}
	var b strings.Builder
	b.WriteString(sideQuestionInstructions)
	b.WriteString("\n\n<conversation_snapshot>\n")
	for i := len(selected) - 1; i >= 0; i-- {
		row := selected[i]
		fmt.Fprintf(&b, "%s: %s\n\n", row.Role, sideQuestionCapRunes(row.Content, sideQuestionMessageRunes))
	}
	b.WriteString("</conversation_snapshot>\n\nSide question:\n")
	b.WriteString(question)
	return b.String(), nil
}

func readSideQuestion(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req sideQuestionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return "", false
	}
	question := strings.TrimSpace(req.Content)
	if question == "" || utf8.RuneCountInString(question) > sideQuestionMaxRunes {
		writeError(w, http.StatusBadRequest, "question must contain 1 to 2000 characters")
		return "", false
	}
	if strings.ContainsRune(question, '\x00') {
		writeError(w, http.StatusBadRequest, "question contains an invalid character")
		return "", false
	}
	return question, true
}

func sideQuestionCapRunes(s string, limit int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "…"
}
