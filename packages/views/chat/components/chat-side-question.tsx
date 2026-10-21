"use client";

import { useLayoutEffect, useRef, useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { MessageCircleQuestion, X } from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "@multica/core/api";
import { chatSideQuestionOptions } from "@multica/core/chat/queries";
import { useWorkspaceId } from "@multica/core/hooks";
import { useRequiredWorkspaceSlug } from "@multica/core/paths";
import { Button } from "@multica/ui/components/ui/button";
import { SubmitButton } from "@multica/ui/components/common/submit-button";
import { ContentEditor, type ContentEditorRef } from "../../editor";
import { RichContent } from "../../rich-content";
import { useT } from "../../i18n";

export function ChatSideQuestion({ sessionId, disabled, readDraft }: {
  sessionId: string;
  disabled?: boolean;
  readDraft: () => string;
}) {
  const wsId = useWorkspaceId();
  const workspaceSlug = useRequiredWorkspaceSlug();
  return <SideQuestionControl key={`${wsId}:${sessionId}`} wsId={wsId} workspaceSlug={workspaceSlug}
    sessionId={sessionId} disabled={disabled} readDraft={readDraft} />;
}

function SideQuestionControl({ wsId, workspaceSlug, sessionId, disabled, readDraft }: {
  wsId: string;
  workspaceSlug: string;
  sessionId: string;
  disabled?: boolean;
  readDraft: () => string;
}) {
  const { t } = useT("chat");
  const [draft, setDraft] = useState<string | null>(null);
  return (
    <>
      <Button type="button" variant="ghost" size="icon-sm" className="absolute bottom-1 right-11"
        disabled={disabled} aria-label={t(($) => $.side_question.title)}
        title={t(($) => $.side_question.title)}
        onClick={() => setDraft(readDraft())}>
        <MessageCircleQuestion />
      </Button>
      {draft !== null && !disabled && (
        <SideQuestionPanel key={`${wsId}:${sessionId}`} wsId={wsId} workspaceSlug={workspaceSlug}
          sessionId={sessionId} initialQuestion={draft} onClose={() => setDraft(null)} />
      )}
    </>
  );
}

function SideQuestionPanel({ wsId, workspaceSlug, sessionId, initialQuestion, onClose }: {
  wsId: string;
  workspaceSlug: string;
  sessionId: string;
  initialQuestion: string;
  onClose: () => void;
}) {
  const { t } = useT("chat");
  const [question, setQuestion] = useState(initialQuestion);
  const editorRef = useRef<ContentEditorRef>(null);
  const [requestId, setRequestId] = useState("");
  const taskRef = useRef("");
  const generation = useRef(0);
  const sending = useRef(false);
  const cancel = (id: string) => api.cancelChatSideQuestion(sessionId, id, workspaceSlug)
    .catch(() => toast.error(t(($) => $.side_question.cancel_error)));
  const cancelRef = useRef(cancel);
  cancelRef.current = cancel;
  useLayoutEffect(() => {
    editorRef.current?.focus();
    return () => {
      generation.current += 1;
      if (taskRef.current) void cancelRef.current(taskRef.current);
    };
  }, []);

  const create = useMutation({
    mutationFn: async (content: string) => {
      const started = generation.current;
      const response = await api.askChatSideQuestion(sessionId, content, workspaceSlug);
      if (generation.current !== started) {
        await cancelRef.current(response.id);
        return;
      }
      taskRef.current = response.id;
      setRequestId(response.id);
    },
    onSettled: () => { sending.current = false; },
  });
  const result = useQuery(chatSideQuestionOptions(wsId, workspaceSlug, sessionId, requestId));
  const status = result.data?.status;
  const active = create.isPending || (!!requestId && (!status || status === "pending" || status === "running"));
  const submit = () => {
    const content = (editorRef.current?.getMarkdown() ?? question).trim();
    if (active || sending.current || !content || Array.from(content).length > 2000) return;
    sending.current = true;
    create.mutate(content);
  };
  const askLabel = create.isError || status === "failed" || status === "timeout" || status === "cancelled"
    ? t(($) => $.side_question.retry) : t(($) => $.side_question.ask);
  const statusText = create.isPending ? t(($) => $.side_question.pending)
    : create.isError ? create.error instanceof ApiError && create.error.status === 429
      ? t(($) => $.side_question.busy)
      : create.error instanceof ApiError && [403, 409, 503].includes(create.error.status)
        ? t(($) => $.side_question.unavailable) : t(($) => $.side_question.failed)
    : result.isError ? t(($) => $.side_question.status_error)
    : status === "failed" ? t(($) => $.side_question.failed)
    : status === "timeout" ? t(($) => $.side_question.timeout)
    : status === "cancelled" ? t(($) => $.side_question.cancelled)
    : status === "running" ? t(($) => $.side_question.running)
    : active ? t(($) => $.side_question.pending) : null;
  const questionLength = Array.from(question.trim()).length;

  return (
    <section aria-label={t(($) => $.side_question.title)}
      onKeyDown={(event) => {
        if (event.key === "Escape") { event.stopPropagation(); onClose(); }
      }}
      className="absolute bottom-full left-0 right-0 z-20 mb-2 flex max-h-[min(36rem,60vh)] flex-col gap-3 overflow-hidden rounded-xl border border-surface-border bg-surface p-3 shadow-lg">
      <div className="flex shrink-0 items-center justify-between gap-2">
        <div className="flex min-w-0 items-center gap-2">
          <span className="shrink-0 text-body font-medium">{t(($) => $.side_question.title)}</span>
          <span className="truncate text-caption text-muted-foreground" title={t(($) => $.side_question.notice)}>
            {t(($) => $.side_question.notice)}
          </span>
        </div>
        <Button type="button" variant="ghost" size="icon-xs" aria-label={t(($) => $.side_question.close)} onClick={onClose}><X /></Button>
      </div>
      <div role="status" aria-live="polite" className="min-h-0 overflow-y-auto break-words text-body empty:hidden">
        {statusText ? <p className="text-muted-foreground">{statusText}</p>
          : status === "completed" && result.data?.answer ? (
            <RichContent content={result.data.answer} density="compact" phase="settled" className="leading-relaxed" />
          ) : null}
      </div>
      <div className="relative flex min-h-24 shrink-0 flex-col rounded-lg border border-surface-border bg-surface pb-9 transition-[border-color,box-shadow] focus-within:border-brand focus-within:ring-2 focus-within:ring-ring/20">
        <div inert={active} aria-disabled={active || undefined}
          className="min-h-0 max-h-[min(10rem,20vh)] overflow-y-auto overscroll-contain px-3 py-2 inert:opacity-60">
          <ContentEditor ref={editorRef} defaultValue={initialQuestion}
            ariaLabel={t(($) => $.side_question.question)}
            placeholder={t(($) => $.side_question.placeholder)}
            onUpdate={setQuestion} onSubmit={submit} debounceMs={0} disableMentions showBubbleMenu />
        </div>
        {questionLength > 2000 && (
          <span className="absolute bottom-2 left-3 text-caption text-destructive">
            {t(($) => $.side_question.character_count, { count: questionLength, max: 2000 })}
          </span>
        )}
        <div className="absolute bottom-1 right-1.5 flex items-center gap-2">
          {result.isError && <Button type="button" variant="outline" size="sm" onClick={() => void result.refetch()}>{t(($) => $.side_question.refresh)}</Button>}
          <SubmitButton disabled={active || !question.trim() || questionLength > 2000}
            loading={active} onClick={submit} ariaLabel={askLabel} tooltip={askLabel} />
        </div>
      </div>
    </section>
  );
}
