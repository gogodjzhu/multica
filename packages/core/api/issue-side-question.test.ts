// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "./client";

const id = "019a0100-0000-7000-8000-000000000001";
afterEach(() => vi.unstubAllGlobals());

describe("issue side question API", () => {
  it.each([{}, { id: "", status: "pending" }, { id, status: "completed", answer: 42 }])("rejects malformed creation and polling responses %j", async (body) => {
    vi.stubGlobal("fetch", vi.fn().mockImplementation(() => Promise.resolve(new Response(JSON.stringify(body)))));
    const client = new ApiClient("https://api.example.test");
    await expect(client.askIssueSideQuestion("issue", "Question?", "original-workspace")).rejects.toThrow("invalid side question response");
    await expect(client.getIssueSideQuestion("issue", id, "original-workspace")).rejects.toThrow("invalid side question response");
  });

  it("pins all operations to the source issue and workspace", async () => {
    const fetcher = vi.fn()
      .mockResolvedValueOnce(new Response(JSON.stringify({ id, status: "pending" }), { status: 202 }))
      .mockResolvedValueOnce(new Response(JSON.stringify({ id, status: "completed", answer: "**Answer**" })))
      .mockResolvedValueOnce(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetcher);
    const client = new ApiClient("https://api.example.test");
    await client.askIssueSideQuestion("MUL-123", "Question?", "original-workspace");
    await expect(client.getIssueSideQuestion("MUL-123", id, "original-workspace")).resolves.toMatchObject({ answer: "**Answer**" });
    await client.cancelIssueSideQuestion("MUL-123", id, "original-workspace");
    for (const call of fetcher.mock.calls) {
      expect(call[1].headers["X-Workspace-Slug"]).toBe("original-workspace");
      expect(call[0]).toContain("/api/issues/MUL-123/side-question");
    }
    expect(fetcher.mock.calls[0]![1]).toMatchObject({ method: "POST", body: JSON.stringify({ content: "Question?" }) });
    expect(fetcher.mock.calls[2]![0]).toContain(id);
    expect(fetcher.mock.calls[2]![1].method).toBe("DELETE");
  });

  it("rejects a result belonging to another run", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ id: "019a0100-0000-7000-8000-000000000002", status: "completed", answer: "Other" }))));
    await expect(new ApiClient("https://api.example.test").getIssueSideQuestion("issue", id, "ws")).rejects.toThrow("invalid side question response");
  });

  it("handles a future status through the shared response schema", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response(JSON.stringify({ id, status: "future_status" }))));
    await expect(new ApiClient("https://api.example.test").getIssueSideQuestion("issue", id, "ws")).resolves.toMatchObject({ status: "failed" });
  });
});
