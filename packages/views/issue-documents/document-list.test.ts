import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiClient } from "@multica/core/api/client";
import {
  DEFAULT_DOCUMENT_SORT,
  type DocumentSort,
  type DocumentSortField,
} from "./components/document-list";
import { ISSUE_DOCUMENT_SORT_FIELDS } from "./components/document-toolbar";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("issue-documents server-side sort", () => {
  it("defaults to type (stage order) asc for the grouped view", () => {
    expect(DEFAULT_DOCUMENT_SORT).toEqual({ field: "type", direction: "asc" });
  });

  it("sends sort / order to the list endpoint", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(JSON.stringify({ items: [], total: 0 }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);

    await new ApiClient("https://api.example.test").listIssueDocuments({
      sort: "title",
      order: "asc",
      limit: 50,
      offset: 0,
    });

    const url = String(fetchMock.mock.calls[0]?.[0] ?? "");
    expect(url).toContain("sort=title");
    expect(url).toContain("order=asc");
    expect(url).toContain("limit=50");
    expect(url).toContain("offset=0");
  });

  it("omits sort/order when not passed (server keeps the default updated_at desc)", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(
        new Response(JSON.stringify({ items: [], total: 0 }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        }),
      );
    vi.stubGlobal("fetch", fetchMock);

    await new ApiClient("https://api.example.test").listIssueDocuments({ limit: 50 });

    const url = String(fetchMock.mock.calls[0]?.[0] ?? "");
    expect(url).toContain("limit=50");
    expect(url).not.toContain("sort=");
    expect(url).not.toContain("order=");
  });

  it("toggles direction on the same field and switches field to desc", () => {
    const toggle = (prev: DocumentSort, field: DocumentSortField): DocumentSort =>
      prev.field === field
        ? { field, direction: prev.direction === "asc" ? "desc" : "asc" }
        : { field, direction: "desc" };

    const first = toggle(DEFAULT_DOCUMENT_SORT, "title");
    expect(first).toEqual({ field: "title", direction: "desc" });
    const second = toggle(first, "title");
    expect(second).toEqual({ field: "title", direction: "asc" });
  });

  it("no longer offers status as a sort field (CLO-477)", () => {
    expect(ISSUE_DOCUMENT_SORT_FIELDS).not.toContain("status");
  });
});
