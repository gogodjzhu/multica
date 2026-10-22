package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func sideQuestionRequestForTest(t *testing.T, method, source, task string, body any) *http.Request {
	t.Helper()
	req := testutil.WithHeaders(testutil.JSONRequest(method, "/side-question", body), "X-User-ID", testUserID, "X-Workspace-ID", testWorkspaceID)
	req = testutil.WithURLParams(req, "sessionId", source, "requestId", task)
	return withChatTestWorkspaceCtx(t, req)
}

func sideQuestionSourceForTest(t *testing.T, runtime string) (string, string) {
	t.Helper()
	agent := dbfx.Agent(t, "Side question "+uuid.NewString(), runtime)
	project := dbfx.Project(t, "Main project")
	source := dbfx.ChatSession(t, agent, testutil.Cols{
		"title": "Main chat", "runtime_id": runtime, "session_id": "main-provider-session",
		"work_dir": "/main/workdir", "project_id": project,
		"explicitly_created_at": testutil.Raw("now()"),
	})
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": source, "role": "user", "content": "Main conversation context"})
	// Product-created temporary sessions and their tasks are not fixture-owned.
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE source_session_id=$1`, source)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE chat_session_id IN (SELECT id FROM chat_session WHERE source_session_id=$1)`, source)
	return agent, source
}

func askSideQuestionForTest(t *testing.T, source, question string) sideQuestionAccepted {
	t.Helper()
	var out sideQuestionAccepted
	testutil.Call(t, testHandler.AskChatSideQuestion, sideQuestionRequestForTest(t, "POST", source, "", map[string]string{"content": question})).Want(http.StatusAccepted).JSON(&out)
	// Tasks survive session deletion, including TTL cleanup.
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE id=$1`, out.ID)
	return out
}

func TestChatSideQuestionIsolatedOrdinaryTask(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Side question isolated runtime")
	agentID, sourceID := sideQuestionSourceForTest(t, runtimeID)
	mainTask := dbfx.Task(t, agentID, testutil.Cols{"runtime_id": runtimeID, "chat_session_id": sourceID, "status": "running"})
	out := askSideQuestionForTest(t, sourceID, "What does the conversation say?")
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(out.ID))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" || task.ChatSessionID == parseUUID(sourceID) || task.ChatInputTaskID != task.ID || !service.IsSideQuestionTask(task) || !task.QuickActionsDisabled {
		t.Fatalf("not an isolated task-owned chat run: response=%+v task=%+v", out, task)
	}
	temp, err := testHandler.Queries.GetChatSession(ctx, task.ChatSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if temp.Purpose != "side_question" || temp.SourceSessionID != parseUUID(sourceID) || temp.ProjectID.Valid || temp.SessionID.Valid || temp.WorkDir.Valid || temp.RuntimeID != parseUUID(runtimeID) {
		t.Fatalf("temporary session inherited main context: %+v", temp)
	}
	msg, err := testHandler.Queries.ListChatInputMessages(ctx, task.ID)
	if err != nil || len(msg) != 1 || !strings.Contains(msg[0].Content, "Main conversation context") || !strings.Contains(msg[0].Content, "What does the conversation say?") {
		t.Fatalf("missing task-owned snapshot: messages=%+v err=%v", msg, err)
	}
	// The ordinary daemon claim builder sees a fresh direct-chat payload.
	runtime, err := testHandler.Queries.GetAgentRuntime(ctx, parseUUID(runtimeID))
	if err != nil {
		t.Fatal(err)
	}
	resp, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(httptest.NewRequest("GET", "/claim", nil), &task, runtime, runtimeID, testWorkspaceID)
	if failure != nil {
		t.Fatalf("ordinary claim failed: %+v", failure)
	}
	if resp.PriorSessionID != "" || resp.PriorWorkDir != "" || len(resp.ProjectResources) != 0 || resp.ChatSessionID == sourceID {
		t.Fatalf("claim inherited source execution context: %+v", resp)
	}
	if _, err := testHandler.Queries.GetPublicChatSessionInWorkspace(ctx, db.GetPublicChatSessionInWorkspaceParams{ID: temp.ID, WorkspaceID: temp.WorkspaceID}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("temporary session publicly accessible: %v", err)
	}
	sessions, err := testHandler.Queries.ListChatSessionsByCreator(ctx, db.ListChatSessionsByCreatorParams{WorkspaceID: temp.WorkspaceID, CreatorID: temp.CreatorID})
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == temp.ID {
			t.Fatal("temporary session leaked into chat list")
		}
	}
	// Remove the main pending run to isolate the FAB's aggregate query.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed', completed_at=now() WHERE id=$1`, mainTask)
	pending, err := testHandler.Queries.HasPendingChatTasksByCreator(ctx, db.HasPendingChatTasksByCreatorParams{WorkspaceID: temp.WorkspaceID, CreatorID: temp.CreatorID, AgentIds: []pgtype.UUID{parseUUID(agentID)}})
	if err != nil || pending {
		t.Fatalf("side task leaked into FAB: pending=%v err=%v", pending, err)
	}
	testutil.Call(t, testHandler.GetChatSession, sideQuestionRequestForTest(t, "GET", uuidToString(temp.ID), "", nil)).Want(http.StatusNotFound)
	// Cancellation targets only the side task, even with a main run in progress.
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', completed_at=NULL WHERE id=$1`, mainTask)
	testutil.Call(t, testHandler.CancelChatSideQuestion, sideQuestionRequestForTest(t, "DELETE", sourceID, out.ID, nil)).Want(http.StatusNoContent)
	main, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(mainTask))
	if err != nil || main.Status != "running" {
		t.Fatalf("main task changed: %+v %v", main, err)
	}
	source, err := testHandler.Queries.GetChatSession(ctx, parseUUID(sourceID))
	if err != nil || source.Title != "Main chat" || source.SessionID.String != "main-provider-session" || source.WorkDir.String != "/main/workdir" {
		t.Fatalf("main session changed: %+v %v", source, err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id=$1`, sourceID); n != 1 {
		t.Fatalf("main transcript mutated: %d messages", n)
	}
}

func TestChatSideQuestionSnapshotIsBoundedAndPersistedOnly(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "Side question snapshot runtime")
	_, sourceID := sideQuestionSourceForTest(t, runtimeID)
	dbfx.Exec(t, `UPDATE chat_message SET created_at=now()-interval '1 hour' WHERE chat_session_id=$1`, sourceID)
	for i := 0; i < 14; i++ {
		dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sourceID, "role": "assistant", "content": strings.Repeat("界", 2000), "created_at": time.Now().Add(time.Duration(i) * time.Second)})
	}
	dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": sourceID, "role": "assistant", "message_kind": "no_response", "content": "SYNTHETIC_SENTINEL", "created_at": testutil.Raw("now()+interval '30 seconds'")})
	prompt, err := testHandler.buildChatSideQuestionPrompt(context.Background(), parseUUID(sourceID), "QUESTION_SENTINEL")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "Main conversation context") || strings.Contains(prompt, "SYNTHETIC_SENTINEL") {
		t.Fatal("snapshot included old or synthetic messages")
	}
	if strings.Count(prompt, "assistant: ") != 12 || strings.Count(prompt, strings.Repeat("界", 1500)+"…") != 12 {
		t.Fatal("snapshot message count or content limit violated")
	}
	if !strings.HasSuffix(prompt, "Side question:\nQUESTION_SENTINEL") || !strings.Contains(prompt, "Keep all operations read-only") {
		t.Fatal("question or snapshot instructions missing")
	}
	for _, content := range []string{"", " ", strings.Repeat("界", 2001), "bad\x00question"} {
		testutil.Call(t, testHandler.AskChatSideQuestion, sideQuestionRequestForTest(t, "POST", sourceID, "", map[string]string{"content": content})).Want(400)
	}
}

func TestChatSideQuestionRuntimeAdmissionAndRollback(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "Side question admission runtime")
	_, sourceA := sideQuestionSourceForTest(t, runtimeID)
	_, sourceB := sideQuestionSourceForTest(t, runtimeID)
	requests := []*http.Request{
		sideQuestionRequestForTest(t, "POST", sourceA, "", map[string]string{"content": "A?"}),
		sideQuestionRequestForTest(t, "POST", sourceB, "", map[string]string{"content": "B?"}),
	}
	var wg sync.WaitGroup
	codes := make([]int, len(requests))
	for i, req := range requests {
		wg.Add(1)
		go func(i int, req *http.Request) {
			defer wg.Done()
			w := httptest.NewRecorder()
			testHandler.AskChatSideQuestion(w, req)
			codes[i] = w.Code
		}(i, req)
	}
	wg.Wait()
	if !((codes[0] == 202 && codes[1] == 429) || (codes[0] == 429 && codes[1] == 202)) {
		t.Fatalf("runtime admission was not atomic: %v", codes)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE source_session_id IN ($1,$2)`, sourceA, sourceB); n != 1 {
		t.Fatalf("rejected request left session: %d", n)
	}
	// A failure after session/task creation rolls back the whole operation.
	otherRuntime := dbfx.Runtime(t, "Side question rollback runtime")
	agentID, sourceID := sideQuestionSourceForTest(t, otherRuntime)
	ctx := context.Background()
	source, err := testHandler.Queries.GetChatSession(ctx, parseUUID(sourceID))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgent(ctx, parseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	_, err = testHandler.TaskService.EnqueueChatSideQuestion(ctx, source, agent, parseUUID(testUserID), "invalid\x00text", "member", parseUUID(testUserID))
	if err == nil {
		t.Fatal("expected PostgreSQL text insertion failure")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE source_session_id=$1`, sourceID); n != 0 {
		t.Fatalf("failed enqueue left %d sessions", n)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1`, agentID); n != 0 {
		t.Fatalf("failed enqueue left %d tasks", n)
	}
}

func TestChatSideQuestionStatusesAndAccess(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "Side question status runtime")
	_, source := sideQuestionSourceForTest(t, runtimeID)
	_, otherSource := sideQuestionSourceForTest(t, runtimeID)
	out := askSideQuestionForTest(t, source, "Question?")
	testutil.Call(t, testHandler.GetChatSideQuestion, sideQuestionRequestForTest(t, "GET", otherSource, out.ID, nil)).Want(404)
	testutil.Call(t, testHandler.CancelChatSideQuestion, sideQuestionRequestForTest(t, "DELETE", otherSource, out.ID, nil)).Want(404)
	testutil.Call(t, testHandler.GetChatSideQuestion, sideQuestionRequestForTest(t, "GET", source, "bad-id", nil)).Want(400)
	for _, tc := range []struct{ dbStatus, reason, want string }{
		{"queued", "", "pending"}, {"dispatched", "", "pending"}, {"deferred", "", "pending"},
		{"running", "", "running"}, {"waiting_local_directory", "", "running"},
		{"failed", "", "failed"}, {"cancelled", "", "cancelled"}, {"cancelled", "side_question_timeout", "timeout"},
	} {
		t.Run(tc.dbStatus+tc.reason, func(t *testing.T) {
			dbfx.Exec(t, `UPDATE agent_task_queue SET status=$2, failure_reason=$3 WHERE id=$1`, out.ID, tc.dbStatus, tc.reason)
			var status sideQuestionStatusResponse
			testutil.Call(t, testHandler.GetChatSideQuestion, sideQuestionRequestForTest(t, "GET", source, out.ID, nil)).Want(200).JSON(&status)
			if status.Status != tc.want {
				t.Fatalf("got %+v, want %s", status, tc.want)
			}
		})
	}
	task, err := testHandler.Queries.GetAgentTask(context.Background(), parseUUID(out.ID))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='completed' WHERE id=$1`, out.ID)
	message := dbfx.Insert(t, "chat_message", testutil.Cols{"chat_session_id": uuidToString(task.ChatSessionID), "role": "assistant", "content": "No response", "task_id": out.ID, "message_kind": "no_response"})
	var status sideQuestionStatusResponse
	testutil.Call(t, testHandler.GetChatSideQuestion, sideQuestionRequestForTest(t, "GET", source, out.ID, nil)).Want(200).JSON(&status)
	if status.Status != "failed" || status.Answer != "" {
		t.Fatalf("synthetic no_response became an answer: %+v", status)
	}
	dbfx.Exec(t, `UPDATE chat_message SET message_kind='message', content='Real answer' WHERE id=$1`, message)
	testutil.Call(t, testHandler.GetChatSideQuestion, sideQuestionRequestForTest(t, "GET", source, out.ID, nil)).Want(200).JSON(&status)
	if status.Status != "completed" || status.Answer != "Real answer" {
		t.Fatalf("missing answer: %+v", status)
	}
}

func TestChatSideQuestionDoesNotBroadcastOrRetry(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Side question events runtime")
	agentID, sourceID := sideQuestionSourceForTest(t, runtimeID)
	bus := events.New()
	var mu sync.Mutex
	var emitted []string
	bus.SubscribeAll(func(e events.Event) { mu.Lock(); defer mu.Unlock(); emitted = append(emitted, e.Type) })
	tasks := service.NewTaskService(testHandler.Queries, testPool, testHandler.Hub, bus)
	h := *testHandler
	h.TaskService, h.Bus = tasks, bus
	var out sideQuestionAccepted
	testutil.Call(t, h.AskChatSideQuestion, sideQuestionRequestForTest(t, "POST", sourceID, "", map[string]string{"content": "Question?"})).Want(202).JSON(&out)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE id=$1`, out.ID)
	claimed, err := tasks.ClaimTaskForRuntime(ctx, parseUUID(runtimeID))
	if err != nil || claimed == nil || claimed.ID != parseUUID(out.ID) {
		t.Fatalf("ordinary claim failed: %+v %v", claimed, err)
	}
	if _, err := tasks.StartTask(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	report := testutil.WithURLParams(newRequest("POST", "/messages", TaskMessageBatchRequest{Messages: []TaskMessageRequest{{Seq: 1, Type: "text", Content: "Private side execution"}}}), "taskId", out.ID)
	testutil.Call(t, h.ReportTaskMessages, report).Want(200)
	progress := testutil.WithURLParams(newRequest("POST", "/progress", TaskProgressRequest{Summary: "Working", Step: 1, Total: 1}), "taskId", out.ID)
	testutil.Call(t, h.ReportTaskProgress, progress).Want(200)
	if n := dbfx.Count(t, `SELECT count(*) FROM task_message WHERE task_id=$1`, out.ID); n != 1 {
		t.Fatal("execution log was not persisted")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET attempt=1, max_attempts=3 WHERE id=$1`, out.ID)
	if _, err := tasks.FailTask(ctx, claimed.ID, "Timed out", "", "", "", "timeout", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE parent_task_id=$1`, out.ID); n != 0 {
		t.Fatal("side task automatically retried")
	}
	// Completion follows the same suppression policy while preserving output.
	var second sideQuestionAccepted
	testutil.Call(t, h.AskChatSideQuestion, sideQuestionRequestForTest(t, "POST", sourceID, "", map[string]string{"content": "Answer?"})).Want(202).JSON(&second)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE id=$1`, second.ID)
	claimed, err = tasks.ClaimTaskForRuntime(ctx, parseUUID(runtimeID))
	if err != nil || claimed == nil {
		t.Fatalf("claim second: %v", err)
	}
	if _, err := tasks.StartTask(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.CompleteTask(ctx, claimed.ID, completeResult(t, "Answer"), "", "", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, event := range emitted {
		if strings.HasPrefix(event, "chat:") || (strings.HasPrefix(event, "task:") && event != "task:available") {
			t.Fatalf("side task emitted chat-facing event %q (%v)", event, emitted)
		}
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE agent_id=$1 AND status IN ('completed','failed')`, agentID); n != 2 {
		t.Fatal("side executions missing from ordinary run history")
	}
}

func TestChatSideQuestionAccessAndOrphanCleanup(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Side question access runtime")
	_, sourceID := sideQuestionSourceForTest(t, runtimeID)
	out := askSideQuestionForTest(t, sourceID, "Question?")
	otherUser := dbfx.User(t, "Other side question user", uuid.NewString()+"@test.local")
	dbfx.Member(t, testWorkspaceID, otherUser, "member")
	for _, method := range []string{"GET", "DELETE"} {
		req := sideQuestionRequestForTest(t, method, sourceID, out.ID, nil)
		req.Header.Set("X-User-ID", otherUser)
		req = chatPendingCtxAs(t, req, otherUser)
		handler := testHandler.GetChatSideQuestion
		if method == "DELETE" {
			handler = testHandler.CancelChatSideQuestion
		}
		testutil.Call(t, handler, req).Want(403)
	}
	otherWorkspace := dbfx.Workspace(t, "Other side workspace", "side-"+uuid.NewString())
	dbfx.Member(t, otherWorkspace, testUserID, "owner")
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: parseUUID(testUserID), WorkspaceID: parseUUID(otherWorkspace)})
	if err != nil {
		t.Fatal(err)
	}
	req := sideQuestionRequestForTest(t, "GET", sourceID, out.ID, nil)
	req = req.WithContext(middleware.SetMemberContext(req.Context(), otherWorkspace, member))
	testutil.Call(t, testHandler.GetChatSideQuestion, req).Want(404)
	// Source deletion does not cascade into the temporary session. The sweeper
	// cancels its task, then leaves the terminal record for the retention window.
	dbfx.Exec(t, `DELETE FROM chat_session WHERE id=$1`, sourceID)
	if err := testHandler.TaskService.SweepChatSideQuestions(ctx, 50); err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(out.ID))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "cancelled" || !task.ChatSessionID.Valid {
		t.Fatalf("orphan not cancelled and retained: %+v", task)
	}
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE id=$1`, uuidToString(task.ChatSessionID))
}

func TestChatSideQuestionSweepRetentionAndOrdinaryCompletion(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Side question cleanup runtime")
	agentID, sourceID := sideQuestionSourceForTest(t, runtimeID)
	label := dbfx.Insert(t, "issue_label", testutil.Cols{"workspace_id": testWorkspaceID, "name": "keep-side-question-agent-label", "color": "#ffffff", "resource_type": "agent"})
	dbfx.InsertNoID(t, "agent_to_label", testutil.Cols{"label_id": label, "agent_id": agentID}, "label_id=$1 AND agent_id=$2", label, agentID)
	out := askSideQuestionForTest(t, sourceID, "Cleanup?")
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(out.ID))
	if err != nil {
		t.Fatal(err)
	}
	tempID := uuidToString(task.ChatSessionID)
	dbfx.Exec(t, `UPDATE chat_session SET created_at=now()-interval '11 minutes' WHERE id=$1`, tempID)
	if err := testHandler.TaskService.SweepChatSideQuestions(ctx, 50); err != nil {
		t.Fatal(err)
	}
	var status sideQuestionStatusResponse
	testutil.Call(t, testHandler.GetChatSideQuestion, sideQuestionRequestForTest(t, "GET", sourceID, out.ID, nil)).Want(200).JSON(&status)
	if status.Status != "timeout" {
		t.Fatalf("overdue task not cancelled: %+v", status)
	}
	if err := testHandler.TaskService.SweepChatSideQuestions(ctx, 50); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id=$1`, tempID); n != 1 {
		t.Fatal("fresh terminal result deleted before retention")
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET completed_at=now()-interval '11 minutes' WHERE id=$1`, out.ID)
	if err := testHandler.TaskService.SweepChatSideQuestions(ctx, 50); err != nil {
		t.Fatal(err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE id=$1`, tempID); n != 0 {
		t.Fatal("expired session retained")
	}
	task, err = testHandler.Queries.GetAgentTask(ctx, parseUUID(out.ID))
	if err != nil || task.ChatSessionID.Valid || !service.IsSideQuestionTask(task) {
		t.Fatalf("cleanup lost run history: %+v %v", task, err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_to_label WHERE agent_id=$1 AND label_id=$2`, agentID, label); n != 1 {
		t.Fatal("cleanup removed agent labels")
	}
	// Use the existing completion path, including assistant persistence.
	completed := askSideQuestionForTest(t, sourceID, "Finish?")
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, completed.ID)
	if _, err := testHandler.TaskService.CompleteTask(ctx, parseUUID(completed.ID), completeResult(t, "Ordinary completion answer"), "side-session", "/side/workdir", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, testHandler.GetChatSideQuestion, sideQuestionRequestForTest(t, "GET", sourceID, completed.ID, nil)).Want(200).JSON(&status)
	if status.Status != "completed" || status.Answer != "Ordinary completion answer" {
		t.Fatalf("ordinary completion failed: %+v", status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_message WHERE chat_session_id=$1`, sourceID); n != 1 {
		t.Fatal("side completion wrote to main conversation")
	}
}
