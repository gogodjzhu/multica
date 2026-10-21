CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_side_question_source
    ON chat_session (source_session_id)
    WHERE purpose = 'side_question';
