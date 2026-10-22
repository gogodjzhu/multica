CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_session_side_question_issue
    ON chat_session (source_issue_id)
    WHERE purpose = 'side_question';
