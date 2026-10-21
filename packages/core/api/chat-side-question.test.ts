// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";
import { ChatSideQuestionSchema } from "./schemas";

const id = "019a0100-0000-7000-8000-000000000001";
afterEach(() => vi.unstubAllGlobals());

describe("side question API compatibility", () => {
  it.each(["pending", "running", "completed", "failed", "cancelled", "timeout"])("accepts %s", (status) => {
    expect(ChatSideQuestionSchema.parse({ id, status, answer: "Answer", future_field: true })).toMatchObject({ id, status });
  });
  it("maps unknown statuses and empty completed answers to failure", () => {
    expect(ChatSideQuestionSchema.parse({ id, status: "future_status" }).status).toBe("failed");
    for (const answer of [undefined, "", "   "]) {
      expect(ChatSideQuestionSchema.parse({ id, status: "completed", answer }).status).toBe("failed");
    }
  });
  it.each([{}, { id: "", status: "pending" }, { id, status: "completed", answer: 42 }])("rejects malformed responses %j", async (body) => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify(body))));
    await expect(new ApiClient("https://api.example.test").askChatSideQuestion("source", "Question?", "original-workspace")).rejects.toThrow("invalid side question response");
  });
  it("pins creation, polling and cancellation to the original workspace", async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ id, status: "pending" }), { status: 202 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ id, status: "completed", answer: "Answer" })))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);
    const client = new ApiClient("https://api.example.test");
    await client.askChatSideQuestion("source", "Question?", "original-workspace");
    await expect(client.getChatSideQuestion("source", id, "original-workspace")).resolves.toMatchObject({ answer: "Answer" });
    await client.cancelChatSideQuestion("source", id, "original-workspace");
    for (const call of fetcher.mock.calls) {
      expect(call[1].headers["X-Workspace-Slug"]).toBe("original-workspace");
      expect(call[0]).toContain("/api/chat/sessions/source/side-question");
    }
    expect(fetcher.mock.calls[2]![0]).toContain(id);
    expect(fetcher.mock.calls[2]![1].method).toBe("DELETE");
  });
  it("rejects a result belonging to another task", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: "019a0100-0000-7000-8000-000000000002", status: "completed", answer: "Other" }))));
    await expect(new ApiClient("https://api.example.test").getChatSideQuestion("source", id, "ws")).rejects.toThrow("invalid side question response");
  });
});
