// @vitest-environment jsdom

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  exportAndDownload,
  parseUploadedTemplates,
} from "./resource-template-export";

const exportTemplateSpy = vi.hoisted(() =>
  vi.fn(
    async (_req: unknown): Promise<{ template: Record<string, unknown> }> => ({
      template: { schema_version: "1.0", kind: "agent", metadata: {}, spec: {} },
    }),
  ),
);

vi.mock("@multica/core/api", () => ({
  api: { exportResourceTemplate: exportTemplateSpy },
}));

function stubDownloadEnvironment() {
  const payloads: unknown[] = [];
  const createObjectURL = vi.fn((blob: Blob) => {
    payloads.push(blob);
    return "blob:mock";
  });
  const revokeObjectURL = vi.fn();
  const click = vi.fn();
  vi.stubGlobal("URL", { ...URL, createObjectURL, revokeObjectURL });
  const originalCreateElement = document.createElement.bind(document);
  const createElementSpy = vi.spyOn(document, "createElement");
  createElementSpy.mockImplementation((tag) => {
    const el = originalCreateElement(tag) as HTMLAnchorElement;
    el.click = click;
    return el;
  });
  return { payloads, createElementSpy, revokeObjectURL, click };
}

describe("parseUploadedTemplates", () => {
  it("accepts a bare template object", () => {
    const doc = { schema_version: "1.0", kind: "agent" };
    expect(parseUploadedTemplates(doc)).toEqual([doc]);
  });

  it("accepts an array of templates", () => {
    const list = [
      { schema_version: "1.0", kind: "agent" },
      { schema_version: "1.0", kind: "squad" },
    ];
    expect(parseUploadedTemplates(list)).toEqual(list);
  });

  it("accepts the bundle shape (items + count)", () => {
    const bundle = {
      schema_version: "multica-template-bundle/v1",
      items: [{ schema_version: "1.0", kind: "agent" }],
      count: 1,
    };
    expect(parseUploadedTemplates(bundle)).toEqual(bundle.items);
  });

  it("accepts the legacy bundle shape (templates key)", () => {
    const bundle = {
      schema_version: "multica-template-bundle/v1",
      templates: [{ schema_version: "1.0", kind: "agent" }],
    };
    expect(parseUploadedTemplates(bundle)).toEqual(bundle.templates);
  });

  it("rejects non-object input", () => {
    expect(() => parseUploadedTemplates("nope")).toThrow(/not a JSON object/);
    expect(() => parseUploadedTemplates(null)).toThrow(/not a JSON object/);
  });

  it("rejects empty arrays and empty bundles", () => {
    expect(() => parseUploadedTemplates([])).toThrow(/no templates/);
    expect(() =>
      parseUploadedTemplates({
        schema_version: "multica-template-bundle/v1",
        items: [],
        count: 0,
      }),
    ).toThrow(/no templates/);
  });
});

describe("exportAndDownload", () => {
  beforeEach(() => {
    exportTemplateSpy.mockClear();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("downloads a single template with the PRD filename convention", async () => {
    const { payloads, createElementSpy } = stubDownloadEnvironment();
    exportTemplateSpy.mockResolvedValueOnce({
      template: { schema_version: "1.0", kind: "agent", metadata: { name: "My Agent" } },
    });

    const result = await exportAndDownload([
      { kind: "agent", resourceId: "a-1", name: "My Agent" },
    ]);

    expect(result.failed).toEqual([]);
    expect(result.exported).toHaveLength(1);
    expect(exportTemplateSpy).toHaveBeenCalledWith({
      kind: "agent",
      resource_id: "a-1",
    });
    // multica-agent-{snake-name}-{yyyymmdd-hhmm}.json
    const anchor = createElementSpy.mock.results[0]!.value as HTMLAnchorElement;
    expect(anchor.download).toMatch(/^multica-agent-My-Agent-\d{8}-\d{4}\.json$/);
    // The downloaded payload is the bare template document.
    const blob = payloads[0] as Blob;
    expect(JSON.parse(await blob.text())).toEqual({
      schema_version: "1.0",
      kind: "agent",
      metadata: { name: "My Agent" },
    });
  });

  it("downloads a bundle with items + count for multiple targets", async () => {
    const { payloads, createElementSpy } = stubDownloadEnvironment();
    exportTemplateSpy.mockResolvedValue({
      template: { schema_version: "1.0", kind: "agent", metadata: {} },
    });

    const result = await exportAndDownload([
      { kind: "agent", resourceId: "a-1", name: "Alpha" },
      { kind: "agent", resourceId: "a-2", name: "Beta" },
    ]);

    expect(result.exported).toHaveLength(2);
    const anchor = createElementSpy.mock.results[0]!.value as HTMLAnchorElement;
    expect(anchor.download).toMatch(/^multica-agents-bundle-2-\d{8}-\d{4}\.json$/);
    const blob = payloads[0] as Blob;
    const bundle = JSON.parse(await blob.text()) as Record<string, unknown>;
    expect(bundle.schema_version).toBe("multica-template-bundle/v1");
    expect(bundle.count).toBe(2);
    expect((bundle.items as unknown[]).length).toBe(2);
  });

  it("collects per-target failures without aborting the batch", async () => {
    const { click } = stubDownloadEnvironment();
    exportTemplateSpy
      .mockResolvedValueOnce({
        template: { schema_version: "1.0", kind: "agent", metadata: {} },
      })
      .mockRejectedValueOnce(new Error("forbidden"));

    const result = await exportAndDownload([
      { kind: "agent", resourceId: "a-1", name: "Alpha" },
      { kind: "agent", resourceId: "a-2", name: "Beta" },
    ]);

    expect(result.exported).toHaveLength(1);
    expect(result.failed).toHaveLength(1);
    expect(result.failed[0]!.target.name).toBe("Beta");
    expect(result.failed[0]!.error).toBe("forbidden");
    // The successful template still downloads as a single file.
    expect(click).toHaveBeenCalledTimes(1);
  });

  it("passes members_mode through for squad exports", async () => {
    stubDownloadEnvironment();
    await exportAndDownload(
      [{ kind: "squad", resourceId: "s-1", name: "Squad" }],
      "references",
    );
    expect(exportTemplateSpy).toHaveBeenCalledWith({
      kind: "squad",
      resource_id: "s-1",
      members_mode: "references",
    });
  });

  it("does not download anything when every target fails", async () => {
    const { click } = stubDownloadEnvironment();
    exportTemplateSpy.mockRejectedValue(new Error("boom"));
    const result = await exportAndDownload([
      { kind: "agent", resourceId: "a-1", name: "Alpha" },
    ]);
    expect(result.exported).toHaveLength(0);
    expect(result.failed).toHaveLength(1);
    expect(click).not.toHaveBeenCalled();
  });
});
