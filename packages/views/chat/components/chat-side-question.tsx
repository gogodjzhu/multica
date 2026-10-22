"use client";

import { SideQuestion } from "../../common/side-question";

export function ChatSideQuestion({ sessionId, disabled, readDraft }: {
  sessionId: string;
  disabled?: boolean;
  readDraft: () => string;
}) {
  return <SideQuestion source="chat" sourceId={sessionId} disabled={disabled} readDraft={readDraft}
    triggerClassName="absolute bottom-1 right-11" />;
}
