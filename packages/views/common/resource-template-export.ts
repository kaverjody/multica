import { api } from "@multica/core/api";
import type {
  ExportResourceTemplateResponse,
  ResourceTemplateDoc,
  TemplateMembersMode,
} from "@multica/core/types";

/**
 * CLO-399 Web template export/import — shared helpers used by the agents and
 * squads list surfaces. The backend (CLO-245) exposes
 * POST /api/templates/export|validate|apply; these helpers wrap export +
 * file download and are UI-framework-agnostic so both pages reuse them.
 */

export type ResourceTemplateKind = "agent" | "squad";

export interface ExportTarget {
  kind: ResourceTemplateKind;
  /** Agent/squad UUID (or workspace-unique name). */
  resourceId: string;
  /** Display name, used only for the download filename. */
  name: string;
}

export interface ExportResult {
  exported: ExportedTemplate[];
  failed: Array<{ target: ExportTarget; error: string }>;
}

export interface ExportedTemplate {
  target: ExportTarget;
  response: ExportResourceTemplateResponse;
}

/**
 * Exports one or more resources as portable templates and downloads the
 * result. A single target produces a bare template document
 * (`multica-{kind}-{name}-{yyyymmdd-hhmm}.json`); multiple targets produce a
 * bundle `{ schema_version, items, count }` named
 * `multica-{kind}s-bundle-{N}-{yyyymmdd-hhmm}.json` (PRD CLO-403 §2.1.4 /
 * US-Agent-Export-2-2) so an import can distinguish a single doc from a
 * bundle and the file stays self-describing.
 *
 * Per-item failures (e.g. a selected resource the caller may not read) are
 * collected rather than aborting the whole batch: every exportable resource
 * still downloads, and the failures are returned for the caller to toast.
 */
export async function exportAndDownload(
  targets: ExportTarget[],
  membersMode?: TemplateMembersMode,
): Promise<ExportResult> {
  const exported: ExportedTemplate[] = [];
  const failed: Array<{ target: ExportTarget; error: string }> = [];
  for (const target of targets) {
    try {
      const response = await api.exportResourceTemplate({
        kind: target.kind,
        resource_id: target.resourceId,
        ...(membersMode ? { members_mode: membersMode } : {}),
      });
      exported.push({ target, response });
    } catch (err) {
      failed.push({
        target,
        error: err instanceof Error ? err.message : String(err),
      });
    }
  }

  const stamp = timestampSuffix();
  if (exported.length === 1) {
    const single = exported[0]!;
    triggerDownload(
      `multica-${single.target.kind}-${sanitizeFilename(single.target.name)}-${stamp}.json`,
      single.response.template,
    );
  } else if (exported.length > 1) {
    // All targets share one kind per call site (agents page vs squads page).
    const kind = exported[0]!.target.kind;
    const bundle = {
      schema_version: "multica-template-bundle/v1",
      items: exported.map((e) => e.response.template),
      count: exported.length,
    };
    triggerDownload(
      `multica-${kind}s-bundle-${exported.length}-${stamp}.json`,
      bundle,
    );
  }
  return { exported, failed };
}

function sanitizeFilename(name: string): string {
  const cleaned = name
    .trim()
    .replace(/[\\/:*?"<>|]+/g, "-")
    .replace(/\s+/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "");
  return cleaned || "template";
}

/** Local-time `yyyymmdd-hhmm` stamp for download filenames (PRD CLO-403). */
function timestampSuffix(): string {
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}` +
    `-${pad(d.getHours())}${pad(d.getMinutes())}`
  );
}

function triggerDownload(filename: string, payload: unknown): void {
  const json = JSON.stringify(payload, null, 2);
  const blob = new Blob([json], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  // Defer revoke so the click has a chance to start the navigation.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/**
 * Normalises whatever the user uploaded into a list of template documents.
 * Accepts a bare template object, an array of templates, or the bundle shape
 * produced by {@link exportAndDownload} (`schema_version` + `items`/`count`;
 * the earlier `templates` key is accepted for backwards compatibility).
 * Throws on non-object / empty input.
 */
export function parseUploadedTemplates(raw: unknown): ResourceTemplateDoc[] {
  if (raw === null || typeof raw !== "object") {
    throw new Error("file contents are not a JSON object");
  }
  const candidate = raw as Record<string, unknown>;
  if (
    typeof candidate.schema_version === "string" &&
    candidate.schema_version.startsWith("multica-template-bundle/")
  ) {
    const list = Array.isArray(candidate.items)
      ? (candidate.items as unknown[])
      : Array.isArray(candidate.templates)
        ? (candidate.templates as unknown[])
        : null;
    if (!list || list.length === 0) {
      throw new Error("bundle contains no templates");
    }
    return list as ResourceTemplateDoc[];
  }
  if (Array.isArray(raw)) {
    if (raw.length === 0) throw new Error("file contains no templates");
    return raw as ResourceTemplateDoc[];
  }
  return [raw as ResourceTemplateDoc];
}
