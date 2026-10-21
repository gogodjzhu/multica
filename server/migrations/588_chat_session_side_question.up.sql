ALTER TABLE chat_session
    ADD COLUMN purpose TEXT NOT NULL DEFAULT 'chat'
        CHECK (purpose IN ('chat', 'side_question')),
    ADD COLUMN source_session_id UUID;
