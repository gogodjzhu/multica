package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multica-ai/multica/server/internal/middleware"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func issueSideQuestionRequestForTest(t *testing.T, method, issueID, taskID string, body any) *http.Request {
	t.Helper()
	req := sideQuestionRequestForTest(t, method, "", taskID, body)
	return testutil.WithURLParams(req, "id", issueID, "requestId", taskID)
}

func issueSideQuestionSourceForTest(t *testing.T, runtimeID string) (string, string) {
	t.Helper()
	agentID := dbfx.Agent(t, "Issue side question "+uuid.NewString(), runtimeID)
	issueID := dbfx.Issue(t, "Investigate the build", testutil.Cols{
		"description": "The release build fails on Linux", "assignee_type": "agent", "assignee_id": agentID,
	})
	dbfx.Cleanup(t, `DELETE FROM chat_session WHERE source_issue_id=$1`, issueID)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE chat_session_id IN (SELECT id FROM chat_session WHERE source_issue_id=$1)`, issueID)
	return agentID, issueID
}

func askIssueSideQuestionForTest(t *testing.T, issueID, question string) sideQuestionAccepted {
	t.Helper()
	var out sideQuestionAccepted
	testutil.Call(t, testHandler.AskIssueSideQuestion, issueSideQuestionRequestForTest(t, "POST", issueID, "", map[string]string{"content": question})).Want(http.StatusAccepted).JSON(&out)
	dbfx.Cleanup(t, `DELETE FROM agent_task_queue WHERE id=$1`, out.ID)
	return out
}

func TestIssueSideQuestionIsolatedAndCompleted(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Issue side question isolated runtime")
	agentID, issueID := issueSideQuestionSourceForTest(t, runtimeID)
	mainTask := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "running", "session_id": "main-session", "work_dir": "/main/workdir"})
	dbfx.Comment(t, issueID, "Reproduce with the release configuration")
	dbfx.Insert(t, "task_message", testutil.Cols{"task_id": mainTask, "seq": 1, "type": "text", "content": "The linker is missing a dependency"})
	out := askIssueSideQuestionForTest(t, issueID, "What have we learned?")
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(out.ID))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" || task.IssueID.Valid || !task.ChatSessionID.Valid || task.ChatInputTaskID != task.ID || !service.IsSideQuestionTask(task) || !task.QuickActionsDisabled {
		t.Fatalf("not an isolated task-owned side run: %+v", task)
	}
	temp, err := testHandler.Queries.GetChatSession(ctx, task.ChatSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if temp.SourceIssueID != parseUUID(issueID) || temp.SourceSessionID.Valid || temp.ProjectID.Valid || temp.SessionID.Valid || temp.WorkDir.Valid {
		t.Fatalf("side session inherited main run state: %+v", temp)
	}
	messages, err := testHandler.Queries.ListChatInputMessages(ctx, task.ID)
	if err != nil || len(messages) != 1 {
		t.Fatalf("missing side snapshot: %+v %v", messages, err)
	}
	for _, text := range []string{"Investigate the build", "release build fails on Linux", "release configuration", "linker is missing", "What have we learned?", "Keep all operations read-only"} {
		if !strings.Contains(messages[0].Content, text) {
			t.Fatalf("snapshot missing %q", text)
		}
	}
	// A live issue source must not be treated as an orphaned chat source.
	if err := testHandler.TaskService.SweepChatSideQuestions(ctx, 50); err != nil {
		t.Fatal(err)
	}
	var status sideQuestionStatusResponse
	testutil.Call(t, testHandler.GetIssueSideQuestion, issueSideQuestionRequestForTest(t, "GET", issueID, out.ID, nil)).Want(200).JSON(&status)
	if status.Status != "pending" {
		t.Fatalf("live issue side question was swept: %+v", status)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, out.ID)
	if _, err := testHandler.TaskService.CompleteTask(ctx, task.ID, completeResult(t, "**Missing dependency**"), "side-session", "/side/workdir", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	testutil.Call(t, testHandler.GetIssueSideQuestion, issueSideQuestionRequestForTest(t, "GET", issueID, out.ID, nil)).Want(200).JSON(&status)
	if status.Status != "completed" || status.Answer != "**Missing dependency**" {
		t.Fatalf("answer not persisted: %+v", status)
	}
	main, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(mainTask))
	if err != nil || main.Status != "running" || main.SessionID.String != "main-session" || main.WorkDir.String != "/main/workdir" {
		t.Fatalf("main run changed: %+v %v", main, err)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM comment WHERE issue_id=$1`, issueID); n != 1 {
		t.Fatal("side answer created an issue comment")
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM issue WHERE id=$1 AND status='todo'`, issueID); n != 1 {
		t.Fatal("side completion changed issue status")
	}
}

func TestIssueSideQuestionSnapshotIsBoundedAndScoped(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Issue snapshot runtime")
	agentID, issueID := issueSideQuestionSourceForTest(t, runtimeID)
	for i := 0; i < 15; i++ {
		dbfx.Comment(t, issueID, fmt.Sprintf("comment-%02d %s", i, strings.Repeat("界", 1700)), testutil.Cols{"created_at": testutil.Raw(fmt.Sprintf("now() + interval '%d seconds'", i))})
	}
	dbfx.Comment(t, issueID, "DELETED-CONTENT", testutil.Cols{"deleted_at": testutil.Raw("now()")})
	dbfx.Comment(t, issueID, "SYSTEM-EVENT", testutil.Cols{"type": "system"})
	otherWorkspace := dbfx.Workspace(t, "Foreign snapshot", "snapshot-"+uuid.NewString())
	dbfx.Comment(t, issueID, "FOREIGN-WORKSPACE", testutil.Cols{"workspace_id": otherWorkspace})
	taskID := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID})
	for i := 1; i <= 15; i++ {
		dbfx.Insert(t, "task_message", testutil.Cols{"task_id": taskID, "seq": i, "type": "text", "content": fmt.Sprintf("text-%02d", i)})
	}
	dbfx.Insert(t, "task_message", testutil.Cols{"task_id": taskID, "seq": 16, "type": "thinking", "content": "PRIVATE-THINKING"})
	otherAgent := dbfx.Agent(t, "Other snapshot agent", runtimeID)
	otherTask := dbfx.Task(t, otherAgent, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID})
	dbfx.Insert(t, "task_message", testutil.Cols{"task_id": otherTask, "seq": 1, "type": "text", "content": "OTHER-AGENT"})
	issue, err := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: parseUUID(issueID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := testHandler.buildIssueSideQuestionPrompt(ctx, issue, issue.AssigneeID, "Question?")
	if err != nil {
		t.Fatal(err)
	}
	for _, excluded := range []string{"comment-00", "comment-01", "comment-02", "DELETED-CONTENT", "SYSTEM-EVENT", "FOREIGN-WORKSPACE", "PRIVATE-THINKING", "OTHER-AGENT", "text-01", "text-02", "text-03", strings.Repeat("界", 1501)} {
		if strings.Contains(prompt, excluded) {
			t.Fatalf("snapshot leaked excluded or unbounded content: %q", excluded[:min(40, len(excluded))])
		}
	}
	if !strings.Contains(prompt, "comment-03") || !strings.Contains(prompt, "comment-14") || !strings.Contains(prompt, "text-04") || !strings.Contains(prompt, "text-15") || strings.Index(prompt, "text-04") > strings.Index(prompt, "text-15") {
		t.Fatal("snapshot did not keep the newest bounded chronological window")
	}
}

func TestIssueSideQuestionAdmissionAndRevalidation(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Issue admission runtime")
	agentID, issueID := issueSideQuestionSourceForTest(t, runtimeID)
	_, otherIssue := issueSideQuestionSourceForTest(t, runtimeID)
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for _, source := range []string{issueID, otherIssue} {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			resp := testutil.Call(t, testHandler.AskIssueSideQuestion, issueSideQuestionRequestForTest(t, "POST", source, "", map[string]string{"content": "Question?"}))
			statuses <- resp.Code
		}(source)
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[202] != 1 || counts[429] != 1 {
		t.Fatalf("runtime admission not atomic: %v", counts)
	}
	_, chat := sideQuestionSourceForTest(t, runtimeID)
	testutil.Call(t, testHandler.AskChatSideQuestion, sideQuestionRequestForTest(t, "POST", chat, "", map[string]string{"content": "Question?"})).Want(429)
	for _, question := range []string{"", "  ", strings.Repeat("界", 2001), "bad\x00input"} {
		testutil.Call(t, testHandler.AskIssueSideQuestion, issueSideQuestionRequestForTest(t, "POST", issueID, "", map[string]string{"content": question})).Want(400)
	}
	issue, err := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: parseUUID(issueID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := testHandler.Queries.GetAgent(ctx, parseUUID(agentID))
	if err != nil {
		t.Fatal(err)
	}
	dbfx.Exec(t, `UPDATE issue SET assignee_type='member', assignee_id=$2 WHERE id=$1`, issueID, testUserID)
	_, err = testHandler.TaskService.EnqueueIssueSideQuestion(ctx, issue, agent, parseUUID(testUserID), "Question?", "member", parseUUID(testUserID))
	if !errors.Is(err, service.ErrSideQuestionAssigneeChanged) {
		t.Fatalf("stale issue assignment accepted: %v", err)
	}
	testutil.Call(t, testHandler.AskIssueSideQuestion, issueSideQuestionRequestForTest(t, "POST", issueID, "", map[string]string{"content": "Question?"})).Want(409)
}

func TestIssueSideQuestionUnavailableAndInvocationPermissions(t *testing.T) {
	runtimeID := dbfx.Runtime(t, "Issue unavailable runtime")
	agentID, issueID := issueSideQuestionSourceForTest(t, runtimeID)
	otherUser := dbfx.User(t, "Issue side caller", uuid.NewString()+"@test.local")
	dbfx.Member(t, testWorkspaceID, otherUser, "member")
	req := issueSideQuestionRequestForTest(t, "POST", issueID, "", map[string]string{"content": "Question?"})
	req.Header.Set("X-User-ID", otherUser)
	req = chatPendingCtxAs(t, req, otherUser)
	testutil.Call(t, testHandler.AskIssueSideQuestion, req).Want(403)
	for _, tc := range []struct {
		name   string
		sql    string
		id     string
		status int
	}{
		{"offline runtime", `UPDATE agent_runtime SET status='offline' WHERE id=$1`, runtimeID, 503},
		{"missing runtime", `UPDATE agent SET runtime_id=NULL WHERE id=$1`, agentID, 409},
		{"archived agent", `UPDATE agent SET archived_at=now() WHERE id=$1`, agentID, 409},
		{"unassigned issue", `UPDATE issue SET assignee_type=NULL, assignee_id=NULL WHERE id=$1`, issueID, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbfx.Exec(t, `UPDATE agent_runtime SET status='online' WHERE id=$1`, runtimeID)
			dbfx.Exec(t, `UPDATE agent SET runtime_id=$2, archived_at=NULL WHERE id=$1`, agentID, runtimeID)
			dbfx.Exec(t, `UPDATE issue SET assignee_type='agent', assignee_id=$2 WHERE id=$1`, issueID, agentID)
			dbfx.Exec(t, tc.sql, tc.id)
			testutil.Call(t, testHandler.AskIssueSideQuestion, issueSideQuestionRequestForTest(t, "POST", issueID, "", map[string]string{"content": "Question?"})).Want(tc.status)
		})
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE source_issue_id=$1`, issueID); n != 0 {
		t.Fatal("rejected requests left temporary sessions")
	}
}

func TestIssueSideQuestionResolvesHumanIdentifier(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Issue identifier runtime")
	_, issueID := issueSideQuestionSourceForTest(t, runtimeID)
	var identifier string
	if err := testPool.QueryRow(ctx, `SELECT workspace.issue_prefix || '-' || issue.number FROM issue JOIN workspace ON workspace.id=issue.workspace_id WHERE issue.id=$1`, issueID).Scan(&identifier); err != nil {
		t.Fatal(err)
	}
	out := askIssueSideQuestionForTest(t, identifier, "Question?")
	var status sideQuestionStatusResponse
	testutil.Call(t, testHandler.GetIssueSideQuestion, issueSideQuestionRequestForTest(t, "GET", identifier, out.ID, nil)).Want(200).JSON(&status)
	if status.Status != "pending" {
		t.Fatalf("identifier did not resolve to source issue: %+v", status)
	}
	testutil.Call(t, testHandler.CancelIssueSideQuestion, issueSideQuestionRequestForTest(t, "DELETE", identifier, out.ID, nil)).Want(204)
}

func TestIssueSideQuestionSquadLeader(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Squad side question runtime")
	leaderID, issueID := issueSideQuestionSourceForTest(t, runtimeID)
	squadID := dbfx.Squad(t, "Side question squad", leaderID)
	dbfx.Exec(t, `UPDATE issue SET assignee_type='squad', assignee_id=$2 WHERE id=$1`, issueID, squadID)
	mainTask := dbfx.Task(t, leaderID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "running"})
	dbfx.Insert(t, "task_message", testutil.Cols{"task_id": mainTask, "seq": 1, "type": "text", "content": "Leader saved progress"})
	out := askIssueSideQuestionForTest(t, issueID, "What has the squad learned?")
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(out.ID))
	if err != nil {
		t.Fatal(err)
	}
	if task.AgentID != parseUUID(leaderID) || task.IssueID.Valid || !service.IsSideQuestionTask(task) {
		t.Fatalf("side question did not use an isolated leader run: %+v", task)
	}
	messages, err := testHandler.Queries.ListChatInputMessages(ctx, task.ID)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Content, "Leader saved progress") {
		t.Fatalf("leader snapshot missing: %+v %v", messages, err)
	}
	dbfx.Exec(t, `UPDATE agent_task_queue SET status='running', started_at=now() WHERE id=$1`, out.ID)
	if _, err := testHandler.TaskService.CompleteTask(ctx, task.ID, completeResult(t, "**Squad progress**"), "side-session", "/side/workdir", "", false, "", ""); err != nil {
		t.Fatal(err)
	}
	var status sideQuestionStatusResponse
	testutil.Call(t, testHandler.GetIssueSideQuestion, issueSideQuestionRequestForTest(t, "GET", issueID, out.ID, nil)).Want(200).JSON(&status)
	if status.Status != "completed" || status.Answer != "**Squad progress**" {
		t.Fatalf("leader answer missing: %+v", status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='running'`, mainTask); n != 1 {
		t.Fatal("side completion changed main run")
	}
}

func TestIssueSideQuestionSquadAdmissionAndRevalidation(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Squad admission runtime")
	leaderID, issueID := issueSideQuestionSourceForTest(t, runtimeID)
	squadID := dbfx.Squad(t, "Admission squad", leaderID)
	dbfx.Exec(t, `UPDATE issue SET assignee_type='squad', assignee_id=$2 WHERE id=$1`, issueID, squadID)
	otherUser := dbfx.User(t, "Squad side caller", uuid.NewString()+"@test.local")
	dbfx.Member(t, testWorkspaceID, otherUser, "member")
	req := issueSideQuestionRequestForTest(t, "POST", issueID, "", map[string]string{"content": "Question?"})
	req.Header.Set("X-User-ID", otherUser)
	req = chatPendingCtxAs(t, req, otherUser)
	testutil.Call(t, testHandler.AskIssueSideQuestion, req).Want(403)
	issue, err := testHandler.Queries.GetIssueInWorkspace(ctx, db.GetIssueInWorkspaceParams{ID: parseUUID(issueID), WorkspaceID: parseUUID(testWorkspaceID)})
	if err != nil {
		t.Fatal(err)
	}
	leader, err := testHandler.Queries.GetAgent(ctx, parseUUID(leaderID))
	if err != nil {
		t.Fatal(err)
	}
	replacement := dbfx.Agent(t, "Replacement leader", runtimeID)
	dbfx.Exec(t, `UPDATE squad SET leader_id=$2 WHERE id=$1`, squadID, replacement)
	_, err = testHandler.TaskService.EnqueueIssueSideQuestion(ctx, issue, leader, parseUUID(testUserID), "Question?", "member", parseUUID(testUserID))
	if !errors.Is(err, service.ErrSideQuestionAssigneeChanged) {
		t.Fatalf("stale squad leader accepted: %v", err)
	}
	dbfx.Exec(t, `UPDATE squad SET archived_at=now() WHERE id=$1`, squadID)
	testutil.Call(t, testHandler.AskIssueSideQuestion, issueSideQuestionRequestForTest(t, "POST", issueID, "", map[string]string{"content": "Question?"})).Want(409)
	dbfx.Exec(t, `UPDATE squad SET archived_at=NULL, workspace_id=$2 WHERE id=$1`, squadID, dbfx.Workspace(t, "Foreign squad", "foreign-squad-"+uuid.NewString()))
	testutil.Call(t, testHandler.AskIssueSideQuestion, issueSideQuestionRequestForTest(t, "POST", issueID, "", map[string]string{"content": "Question?"})).Want(409)
	if n := dbfx.Count(t, `SELECT count(*) FROM chat_session WHERE source_issue_id=$1`, issueID); n != 0 {
		t.Fatal("rejected squad requests left temporary sessions")
	}
}

func TestIssueSideQuestionAccessCancellationAndOrphans(t *testing.T) {
	ctx := context.Background()
	runtimeID := dbfx.Runtime(t, "Issue access runtime")
	agentID, issueID := issueSideQuestionSourceForTest(t, runtimeID)
	mainTask := dbfx.Task(t, agentID, testutil.Cols{"issue_id": issueID, "runtime_id": runtimeID, "status": "running"})
	out := askIssueSideQuestionForTest(t, issueID, "Question?")
	otherUser := dbfx.User(t, "Other issue side user", uuid.NewString()+"@test.local")
	dbfx.Member(t, testWorkspaceID, otherUser, "member")
	otherIssue := dbfx.Issue(t, "Unrelated issue")
	for _, method := range []string{"GET", "DELETE"} {
		handler := testHandler.GetIssueSideQuestion
		if method == "DELETE" {
			handler = testHandler.CancelIssueSideQuestion
		}
		testutil.Call(t, handler, issueSideQuestionRequestForTest(t, method, otherIssue, out.ID, nil)).Want(404)
		req := issueSideQuestionRequestForTest(t, method, issueID, out.ID, nil)
		req.Header.Set("X-User-ID", otherUser)
		req = chatPendingCtxAs(t, req, otherUser)
		testutil.Call(t, handler, req).Want(404)
		testutil.Call(t, handler, issueSideQuestionRequestForTest(t, method, issueID, "not-a-uuid", nil)).Want(400)
	}
	otherWorkspace := dbfx.Workspace(t, "Other issue side workspace", "side-issue-"+uuid.NewString())
	dbfx.Member(t, otherWorkspace, testUserID, "owner")
	member, err := testHandler.Queries.GetMemberByUserAndWorkspace(ctx, db.GetMemberByUserAndWorkspaceParams{UserID: parseUUID(testUserID), WorkspaceID: parseUUID(otherWorkspace)})
	if err != nil {
		t.Fatal(err)
	}
	req := issueSideQuestionRequestForTest(t, "GET", issueID, out.ID, nil)
	req = req.WithContext(middleware.SetMemberContext(req.Context(), otherWorkspace, member))
	testutil.Call(t, testHandler.GetIssueSideQuestion, req).Want(404)
	testutil.Call(t, testHandler.CancelIssueSideQuestion, issueSideQuestionRequestForTest(t, "DELETE", issueID, out.ID, nil)).Want(204)
	var status sideQuestionStatusResponse
	testutil.Call(t, testHandler.GetIssueSideQuestion, issueSideQuestionRequestForTest(t, "GET", issueID, out.ID, nil)).Want(200).JSON(&status)
	if status.Status != "cancelled" {
		t.Fatalf("side run not cancelled: %+v", status)
	}
	if n := dbfx.Count(t, `SELECT count(*) FROM agent_task_queue WHERE id=$1 AND status='running'`, mainTask); n != 1 {
		t.Fatal("side cancellation stopped the main run")
	}
	orphan := askIssueSideQuestionForTest(t, issueID, "Orphan?")
	dbfx.Exec(t, `DELETE FROM issue WHERE id=$1`, issueID)
	if err := testHandler.TaskService.SweepChatSideQuestions(ctx, 50); err != nil {
		t.Fatal(err)
	}
	task, err := testHandler.Queries.GetAgentTask(ctx, parseUUID(orphan.ID))
	if err != nil || task.Status != "cancelled" || task.FailureReason.String != "side_question_timeout" {
		t.Fatalf("orphan side run not cancelled: %+v %v", task, err)
	}
}
