import { describe, expect, it } from "vitest";
import {
  EMPTY_ISSUE_DOCUMENT_DETAIL,
  EMPTY_ISSUE_DOCUMENT_GROUP_LIST_RESPONSE,
  EMPTY_ISSUE_DOCUMENT_LIST_RESPONSE,
  EMPTY_ISSUE_DOCUMENT_VERSIONS_RESPONSE,
  IssueDocumentGroupListResponseSchema,
  IssueDocumentListResponseSchema,
  IssueDocumentDetailResponseSchema,
  IssueDocumentVersionsResponseSchema,
} from "./schemas";
import { parseWithFallback } from "./schema";

const baseDocument = {
  id: "11111111-1111-1111-1111-111111111111",
  workspace_id: "ws-1",
  issue_id: "22222222-2222-2222-2222-222222222222",
  issue_identifier: "MUL-1",
  issue_title: "Test issue",
  type: "requirements",
  title: "requirement.md",
  version: 2,
  status: "approved",
  content_type: "markdown",
  file_attachment_id: null,
  author_type: "member",
  author_id: "33333333-3333-3333-3333-333333333333",
  author_name: "Test User",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

describe("issue document schemas", () => {
  it("parses a well-formed list response", () => {
    const result = parseWithFallback(
      { items: [baseDocument], total: 1 },
      IssueDocumentListResponseSchema,
      EMPTY_ISSUE_DOCUMENT_LIST_RESPONSE,
      { endpoint: "test" },
    );
    expect(result.total).toBe(1);
    expect(result.items[0]?.issue_identifier).toBe("MUL-1");
    expect(result.items[0]?.version).toBe(2);
  });

  it("keeps an unknown document type / status lenient instead of failing", () => {
    const result = parseWithFallback(
      { items: [{ ...baseDocument, type: "future-type", status: "future-status" }], total: 1 },
      IssueDocumentListResponseSchema,
      EMPTY_ISSUE_DOCUMENT_LIST_RESPONSE,
      { endpoint: "test" },
    );
    expect(result.items[0]?.type).toBe("future-type");
    expect(result.items[0]?.status).toBe("future-status");
  });

  it("degrades a malformed list response to the empty fallback", () => {
    const result = parseWithFallback(
      { items: "not-an-array" },
      IssueDocumentListResponseSchema,
      EMPTY_ISSUE_DOCUMENT_LIST_RESPONSE,
      { endpoint: "test" },
    );
    expect(result).toEqual(EMPTY_ISSUE_DOCUMENT_LIST_RESPONSE);
  });

  it("parses a detail response with inline content", () => {
    const result = parseWithFallback(
      { ...baseDocument, content: "# heading" },
      IssueDocumentDetailResponseSchema,
      EMPTY_ISSUE_DOCUMENT_DETAIL,
      { endpoint: "test" },
    );
    expect(result.content).toBe("# heading");
  });

  it("parses a grouped-by-issue response", () => {
    const result = parseWithFallback(
      {
        groups: [
          {
            issue_id: "22222222-2222-2222-2222-222222222222",
            issue_identifier: "MUL-1",
            issue_title: "Test issue",
            items: [baseDocument],
            total: 1,
          },
        ],
        total: 1,
      },
      IssueDocumentGroupListResponseSchema,
      EMPTY_ISSUE_DOCUMENT_GROUP_LIST_RESPONSE,
      { endpoint: "test" },
    );
    expect(result.total).toBe(1);
    expect(result.groups).toHaveLength(1);
    expect(result.groups[0]?.issue_identifier).toBe("MUL-1");
    expect(result.groups[0]?.items[0]?.title).toBe("requirement.md");
    expect(result.groups[0]?.total).toBe(1);
  });

  it("degrades a malformed grouped response to the empty fallback", () => {
    const result = parseWithFallback(
      { groups: "not-an-array" },
      IssueDocumentGroupListResponseSchema,
      EMPTY_ISSUE_DOCUMENT_GROUP_LIST_RESPONSE,
      { endpoint: "test" },
    );
    expect(result).toEqual(EMPTY_ISSUE_DOCUMENT_GROUP_LIST_RESPONSE);
  });

  it("parses a versions response", () => {
    const result = parseWithFallback(
      {
        issue_id: baseDocument.issue_id,
        type: "requirements",
        items: [
          { id: baseDocument.id, version: 2, status: "approved", title: "requirement.md", content_type: "markdown", file_attachment_id: null, created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z" },
          { id: "44444444-4444-4444-4444-444444444444", version: 1, status: "superseded", title: "requirement.md", content_type: "markdown", file_attachment_id: null, created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z" },
        ],
      },
      IssueDocumentVersionsResponseSchema,
      EMPTY_ISSUE_DOCUMENT_VERSIONS_RESPONSE,
      { endpoint: "test" },
    );
    expect(result.items).toHaveLength(2);
    expect(result.items[0]?.version).toBe(2);
  });
});
