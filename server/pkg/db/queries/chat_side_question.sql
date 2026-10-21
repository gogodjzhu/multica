-- name: CreateSideQuestionChatSession :one
INSERT INTO chat_session (id, workspace_id, agent_id, creator_id, title, runtime_id, purpose, source_session_id)
VALUES (@id, @workspace_id, @agent_id, @creator_id, 'Side question', @runtime_id, 'side_question', @source_session_id)
RETURNING *;

-- name: LockSideQuestionRuntime :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(runtime_id)::uuid::text, 90003));

-- name: HasPendingSideQuestion :one
SELECT EXISTS (
    SELECT 1 FROM chat_session cs
    JOIN agent_task_queue task ON task.chat_session_id = cs.id
    WHERE cs.purpose = 'side_question'
      AND (cs.source_session_id = @source_session_id OR task.runtime_id = @runtime_id)
      AND task.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred')
) AS pending;

-- name: GetSideQuestionChatSession :one
SELECT cs.* FROM chat_session cs
JOIN agent_task_queue task ON task.chat_session_id = cs.id
WHERE task.id = @task_id
  AND cs.workspace_id = @workspace_id
  AND cs.purpose = 'side_question'
  AND cs.source_session_id = @source_session_id
  AND cs.creator_id = @creator_id;

-- name: ListSideQuestionSweepCandidates :many
SELECT cs.* FROM chat_session cs
WHERE cs.purpose = 'side_question'
  AND (
    (EXISTS (SELECT 1 FROM agent_task_queue task WHERE task.chat_session_id = cs.id
             AND task.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred'))
     AND (cs.created_at < now() - make_interval(secs => @max_age_seconds::double precision)
          OR NOT EXISTS (SELECT 1 FROM chat_session source WHERE source.id = cs.source_session_id)))
    OR
    (NOT EXISTS (SELECT 1 FROM agent_task_queue task WHERE task.chat_session_id = cs.id
                 AND task.status IN ('queued', 'dispatched', 'running', 'waiting_local_directory', 'deferred'))
     AND COALESCE((SELECT MAX(task.completed_at) FROM agent_task_queue task WHERE task.chat_session_id = cs.id), cs.created_at)
         < now() - make_interval(secs => @retention_seconds::double precision))
  )
ORDER BY cs.created_at
LIMIT @batch_limit;

-- name: ListSideQuestionTasks :many
SELECT * FROM agent_task_queue
WHERE chat_session_id = @chat_session_id
ORDER BY created_at;

-- name: MarkSideQuestionTask :one
UPDATE agent_task_queue
SET context = jsonb_build_object('purpose', 'side_question'), quick_actions_disabled = true
WHERE id = @id
RETURNING *;

-- name: DeleteSideQuestionMessages :exec
DELETE FROM chat_message WHERE chat_session_id = @chat_session_id;
