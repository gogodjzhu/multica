CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_chat_side_question_expiry
    ON chat_session (created_at)
    WHERE purpose = 'side_question';
