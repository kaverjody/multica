"use client";

import { useCallback, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { api } from "@multica/core/api";
import { useWorkspaceId } from "@multica/core/hooks";
import { workspaceKeys } from "@multica/core/workspace/queries";
import type {
  AgentRuntime,
  ApplyResourceTemplateRequest,
  ApplyResourceTemplateResponse,
  ConflictPolicy,
  ResourceTemplateDoc,
  TemplateMembersMode,
  ValidateResourceTemplateResponse,
} from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { Input } from "@multica/ui/components/ui/input";
import { Label } from "@multica/ui/components/ui/label";
import {
  NativeSelect,
  NativeSelectOption,
} from "@multica/ui/components/ui/native-select";
import { RadioGroup, RadioGroupItem } from "@multica/ui/components/ui/radio-group";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import { AlertCircle, CheckCircle2, Loader2, Lock, ShieldAlert, Upload } from "lucide-react";
import { useT } from "../i18n";
import {
  parseUploadedTemplates,
} from "./resource-template-export";

type Step = "upload" | "select" | "review" | "done";

/** Upload limit for template files (PRD CLO-403 AC-Import-1-2, 5 MB). */
const MAX_TEMPLATE_FILE_BYTES = 5 * 1024 * 1024;

/** Redacted-placeholder shapes produced by CLO-245 export redaction. */
const SECRET_PLACEHOLDER_RE = /<<SECRET:[^>]+>>/;
const MCP_PLACEHOLDER_RE = /<<MCP_REDACTED>>/;

interface TemplateEntry {
  doc: ResourceTemplateDoc;
  validate?: ValidateResourceTemplateResponse;
  validating?: boolean;
  applyError?: string;
  /** Pre-apply tweaks (PRD CLO-403 US-Import-Wizard-4): only lightweight
      metadata — name / description — is editable from the Web wizard;
      instructions / model / skills / env stay read-only in v1. */
  nameOverride?: string;
  descriptionOverride?: string;
}

/**
 * Resource-template import wizard (CLO-399). Reusable across the agents and
 * squads list surfaces. Flow: upload (.json) → pick a target runtime →
 * validate preview (errors / warnings / plan / conflicts / required inputs)
 * → resolve conflicts + supply required env + opt into skill install →
 * apply. Reuses the CLO-245 backend (POST /api/templates/{validate,apply}).
 *
 * The wizard is self-contained: it owns its state, calls the API client
 * directly, and invalidates the agents/squads React Query caches on a
 * successful apply so both list pages refresh.
 */
export function ResourceTemplateImportDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useT("templates");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();

  const [step, setStep] = useState<Step>("upload");
  const [entries, setEntries] = useState<TemplateEntry[]>([]);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [targetRuntimeId, setTargetRuntimeId] = useState("");
  const [conflictPolicy, setConflictPolicy] = useState<ConflictPolicy>("rename");
  const [envValues, setEnvValues] = useState<Record<string, Record<string, string>>>({});
  const [installSkills, setInstallSkills] = useState<Record<string, boolean>>({});
  const [membersMode, setMembersMode] = useState<TemplateMembersMode>("embedded");
  const [applying, setApplying] = useState(false);
  const [applyResult, setApplyResult] = useState<ApplyResourceTemplateResponse | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const [dragOver, setDragOver] = useState(false);
  // Bundle handling (PM decision Q1): a file may carry several templates.
  // parsedDocs holds everything the file contained; selectedIndices narrows
  // it before the review step (skipped when the bundle holds exactly one).
  const [parsedDocs, setParsedDocs] = useState<ResourceTemplateDoc[]>([]);
  const [selectedIndices, setSelectedIndices] = useState<Set<number>>(new Set());
  // Trust gate (PMO hard constraint from CLO-406 #2): files not exported from
  // the current workspace require an explicit trust confirmation.
  const [needsTrust, setNeedsTrust] = useState(false);
  const [trustConfirmed, setTrustConfirmed] = useState(false);
  // Conflict policy != fail with live conflicts requires an explicit
  // acknowledgement before apply (UX v1.1 "覆盖项二次确认").
  const [conflictAcknowledged, setConflictAcknowledged] = useState(false);

  const { data: runtimes = [] } = useQuery({
    queryKey: ["workspaces", wsId, "runtimes"],
    queryFn: () => api.listRuntimes({ workspace_id: wsId }),
    enabled: open && !!wsId,
  });

  const reset = useCallback(() => {
    setStep("upload");
    setEntries([]);
    setUploadError(null);
    setTargetRuntimeId("");
    setConflictPolicy("rename");
    setEnvValues({});
    setInstallSkills({});
    setMembersMode("embedded");
    setApplying(false);
    setApplyResult(null);
    setParsedDocs([]);
    setSelectedIndices(new Set());
    setNeedsTrust(false);
    setTrustConfirmed(false);
    setConflictAcknowledged(false);
  }, []);

  const handleOpenChange = useCallback(
    (next: boolean) => {
      if (!next) reset();
      onOpenChange(next);
    },
    [onOpenChange, reset],
  );

  const readFile = useCallback((file: File) => {
    setUploadError(null);
    // PRD CLO-403 AC-Import-1-2: .json files up to 5 MB (matches the CLO-245
    // server-side body limit).
    if (file.size > MAX_TEMPLATE_FILE_BYTES) {
      setUploadError(t(($) => $.import.upload_too_large));
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      const text = String(reader.result ?? "");
      if (text.trim() === "") {
        setUploadError(t(($) => $.import.upload_empty));
        return;
      }
      try {
        const parsed = JSON.parse(text);
        const docs = parseUploadedTemplates(parsed);
        if (docs.length === 0) {
          setUploadError(t(($) => $.import.upload_empty));
          return;
        }
        setParsedDocs(docs);
        setSelectedIndices(new Set(docs.map((_, i) => i)));
        // Trust gate (PMO hard constraint): templates not exported from this
        // workspace (or lacking provenance) come from an unknown source.
        const untrusted = docs.some(
          (doc) => doc.metadata?.source_workspace !== wsId,
        );
        setNeedsTrust(untrusted);
        setTrustConfirmed(false);
        if (docs.length === 1) {
          setEntries([{ doc: docs[0]! }]);
          setMembersMode(
            squadMembersMode(docs[0]!) ?? "embedded",
          );
          setStep(untrusted ? "upload" : "review");
        } else {
          // Q1: bundle with ≥2 templates → selection substep first.
          setEntries([]);
          setStep(untrusted ? "upload" : "select");
        }
      } catch (err) {
        setUploadError(
          t(($) => $.import.upload_invalid_json, {
            error: err instanceof Error ? err.message : String(err),
          }),
        );
      }
    };
    reader.onerror = () => setUploadError(t(($) => $.import.upload_invalid_json, { error: "read error" }));
    reader.readAsText(file);
  }, [t, wsId]);

  const onFileSelected = (files: FileList | null) => {
    const file = files?.[0];
    if (file) readFile(file);
  };

  /** Proceeds past the trust gate into the bundle-selection / review step. */
  const proceedAfterTrust = useCallback(() => {
    if (!trustConfirmed) return;
    setNeedsTrust(false);
    if (parsedDocs.length === 1) {
      setEntries([{ doc: parsedDocs[0]! }]);
      setStep("review");
    } else {
      setStep("select");
    }
  }, [trustConfirmed, parsedDocs]);

  const confirmSelection = useCallback(() => {
    const selected = [...selectedIndices]
      .sort((a, b) => a - b)
      .map((i) => parsedDocs[i]!)
      .filter(Boolean);
    if (selected.length === 0) return;
    const firstSquadMode = squadMembersMode(selected[0]!);
    if (firstSquadMode) setMembersMode(firstSquadMode);
    setEntries(selected.map((doc) => ({ doc })));
    setStep("review");
  }, [parsedDocs, selectedIndices]);

  // Aggregate validation state across every uploaded template.
  const allValidated = entries.length > 0 && entries.every((e) => e.validate);
  const anyValidating = entries.some((e) => e.validating);
  const blockingErrors = entries.reduce(
    (n, e) => n + (e.validate?.errors?.length ?? 0),
    0,
  );

  // Aggregated required inputs across templates for the review UI.
  const aggregatedEnvKeys = useMemo(() => {
    const seen = new Set<string>();
    const list: Array<{ agent_ref: string; key: string }> = [];
    for (const e of entries) {
      for (const k of e.validate?.required_inputs.env_keys ?? []) {
        const id = `${k.agent_ref}::${k.key}`;
        if (!seen.has(id)) {
          seen.add(id);
          list.push(k);
        }
      }
    }
    return list;
  }, [entries]);

  const aggregatedMissingSkills = useMemo(() => {
    const seen = new Set<string>();
    const list: Array<{ name: string; source_url: string; installable: boolean }> = [];
    for (const e of entries) {
      for (const s of e.validate?.required_inputs.missing_skills ?? []) {
        if (!seen.has(s.source_url || s.name)) {
          seen.add(s.source_url || s.name);
          list.push(s);
        }
      }
    }
    return list;
  }, [entries]);

  const aggregatedConflicts = useMemo(
    () => entries.flatMap((e) => e.validate?.plan.conflicts ?? []),
    [entries],
  );

  // Q4: skills the user did NOT opt into installing — surfaced as a
  // post-creation checklist on the done step.
  const remainingSkills = useMemo(
    () =>
      aggregatedMissingSkills.filter(
        (s) => !s.installable || !installSkills[s.source_url || s.name],
      ),
    [aggregatedMissingSkills, installSkills],
  );

  const validateAll = useCallback(async () => {
    if (!targetRuntimeId || entries.length === 0) return;
    setEntries((prev) => prev.map((e) => ({ ...e, validating: true, validate: undefined })));
    const results = await Promise.all(
      entries.map((e) =>
        api
          .validateResourceTemplate({
            template: e.doc,
            target_runtime_id: targetRuntimeId,
            members_mode: isSquadDoc(e.doc) ? membersMode : undefined,
          })
          .then((validate) => ({ doc: e.doc, validate }))
          .catch((err: unknown) => ({
            doc: e.doc,
            validate: {
              valid: false,
              errors: [
                {
                  code: "VALIDATE_REQUEST_FAILED",
                  message: err instanceof Error ? err.message : String(err),
                },
              ],
              required_inputs: {},
              plan: {},
            } as ValidateResourceTemplateResponse,
          })),
      ),
    );
    setEntries((prev) =>
      prev.map((e) => {
        const match = results.find((r) => r.doc === e.doc);
        return match ? { ...e, validate: match.validate, validating: false } : e;
      }),
    );
  }, [targetRuntimeId, entries, membersMode]);

  const canImport =
    allValidated &&
    blockingErrors === 0 &&
    !applying &&
    !anyValidating &&
    // fail policy + live conflicts = the apply would be rejected by design
    // (CLO-245 conflictPolicy=fail → 409); force rename/skip instead.
    (aggregatedConflicts.length === 0 || conflictPolicy !== "fail") &&
    // "overwrite-type" resolutions (rename/skip) need explicit acknowledgement
    // when conflicts actually exist (UX v1.1 second-confirm).
    (aggregatedConflicts.length === 0 || conflictAcknowledged);

  const doApply = useCallback(async () => {
    setApplying(true);
    const envPayload: Record<string, Record<string, string>> = {};
    for (const [ref, map] of Object.entries(envValues)) {
      const filtered: Record<string, string> = {};
      for (const [k, v] of Object.entries(map)) {
        if (v.trim() !== "") filtered[k] = v;
      }
      if (Object.keys(filtered).length > 0) envPayload[ref] = filtered;
    }
    const installUrls = aggregatedMissingSkills
      .filter((s) => s.installable && installSkills[s.source_url || s.name])
      .map((s) => s.source_url);

    try {
      let lastResult: ApplyResourceTemplateResponse | null = null;
      for (const e of entries) {
        if ((e.validate?.errors?.length ?? 0) > 0) continue;
        const overrides = buildOverrides(e);
        const result = await api.applyResourceTemplate({
          template: e.doc,
          target_runtime_id: targetRuntimeId,
          members_mode: isSquadDoc(e.doc) ? membersMode : undefined,
          conflict_policy: conflictPolicy,
          overrides: overrides ?? undefined,
          env: Object.keys(envPayload).length > 0 ? envPayload : undefined,
          install_missing_skills: installUrls.length > 0 ? installUrls : undefined,
          idempotency_key: crypto.randomUUID(),
        });
        lastResult = result;
        if (!result.applied || result.rolled_back) {
          throw new Error(
            result.errors?.[0]?.message ??
              t(($) => $.import.apply_rolled_back, { error: "" }),
          );
        }
      }
      setApplyResult(lastResult);
      await Promise.all([
        qc.invalidateQueries({ queryKey: workspaceKeys.agents(wsId) }),
        qc.invalidateQueries({ queryKey: workspaceKeys.squads(wsId) }),
        qc.invalidateQueries({ queryKey: workspaceKeys.skills(wsId) }),
      ]);
      setStep("done");
    } catch (err) {
      toast.error(
        t(($) => $.import.apply_failed, {
          error: err instanceof Error ? err.message : String(err),
        }),
      );
    } finally {
      setApplying(false);
    }
  }, [
    aggregatedMissingSkills,
    conflictPolicy,
    entries,
    envValues,
    installSkills,
    membersMode,
    qc,
    targetRuntimeId,
    t,
    wsId,
  ]);

  const onlineRuntimes = runtimes;
  const templateKindLabel = (doc: ResourceTemplateDoc) =>
    doc.kind === "squad"
      ? t(($) => $.import.plan_squads, { count: 1 })
      : t(($) => $.import.plan_agents, { count: 1 });

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t(($) => $.import.dialog_title)}</DialogTitle>
          <DialogDescription>
            {step === "upload"
              ? t(($) => $.import.upload_title)
              : step === "select"
                ? t(($) => $.import.select_step_title)
                : step === "review"
                  ? t(($) => $.import.step_review)
                  : t(($) => $.import.apply_done_title)}
          </DialogDescription>
        </DialogHeader>

        {step === "upload" && (
          <div className="space-y-3">
            <button
              type="button"
              onClick={() => fileInputRef.current?.click()}
              onDragOver={(e) => {
                e.preventDefault();
                setDragOver(true);
              }}
              onDragLeave={() => setDragOver(false)}
              onDrop={(e) => {
                e.preventDefault();
                setDragOver(false);
                onFileSelected(e.dataTransfer.files);
              }}
              className={`flex w-full flex-col items-center justify-center gap-2 rounded-lg border-2 border-dashed px-4 py-8 text-center transition-colors ${
                dragOver
                  ? "border-primary bg-accent"
                  : "border-input hover:border-primary/50"
              }`}
            >
              <Upload className="size-6 text-muted-foreground" />
              <span className="text-sm text-muted-foreground">
                {dragOver
                  ? t(($) => $.import.upload_drop)
                  : t(($) => $.import.upload_hint)}
              </span>
              <span className="mt-1 text-xs font-medium text-primary">
                {t(($) => $.import.upload_browse)}
              </span>
            </button>
            <input
              ref={fileInputRef}
              type="file"
              accept="application/json,.json"
              aria-label={t(($) => $.import.upload_browse)}
              className="hidden"
              onChange={(e) => onFileSelected(e.target.files)}
            />
            {uploadError && (
              <p className="flex items-start gap-2 text-sm text-destructive">
                <AlertCircle className="mt-0.5 size-4 shrink-0" />
                <span>{uploadError}</span>
              </p>
            )}
            {/* Trust gate (PMO hard constraint from CLO-406 #2): importing a
                template that was not exported from this workspace must be an
                explicit user decision. */}
            {needsTrust && parsedDocs.length > 0 && (
              <div className="space-y-2 rounded-lg border border-amber-300 bg-amber-50 p-3 text-sm">
                <p className="flex items-start gap-2 font-medium text-amber-800">
                  <ShieldAlert className="mt-0.5 size-4 shrink-0" />
                  {t(($) => $.import.trust_title)}
                </p>
                <p className="text-xs text-amber-700">
                  {t(($) => $.import.trust_body, {
                    count: parsedDocs.length,
                    file: fileInputRef.current?.files?.[0]?.name ?? "",
                  })}
                </p>
                <label className="flex cursor-pointer items-start gap-2 text-xs text-amber-800">
                  <Checkbox
                    checked={trustConfirmed}
                    onCheckedChange={(v) => setTrustConfirmed(!!v)}
                    className="mt-0.5"
                  />
                  <span>{t(($) => $.import.trust_confirm_label)}</span>
                </label>
                <Button
                  type="button"
                  size="sm"
                  disabled={!trustConfirmed}
                  onClick={proceedAfterTrust}
                >
                  {t(($) => $.import.trust_continue)}
                </Button>
              </div>
            )}
          </div>
        )}

        {step === "select" && (
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">
              {t(($) => $.import.select_step_hint, { count: parsedDocs.length })}
            </p>
            <div className="flex items-center gap-2">
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={() =>
                  setSelectedIndices(new Set(parsedDocs.map((_, i) => i)))
                }
              >
                {t(($) => $.import.select_all)}
              </Button>
              <Button
                type="button"
                size="sm"
                variant="outline"
                onClick={() => setSelectedIndices(new Set())}
              >
                {t(($) => $.import.select_none)}
              </Button>
              <span className="ml-auto text-xs text-muted-foreground">
                {t(($) => $.import.selected_count, {
                  count: selectedIndices.size,
                  total: parsedDocs.length,
                })}
              </span>
            </div>
            <div className="max-h-[45vh] space-y-1.5 overflow-y-auto pr-1">
              {parsedDocs.map((doc, i) => {
                const id = `bundle-doc-${i}`;
                return (
                  <label
                    key={id}
                    className="flex cursor-pointer items-start gap-2 rounded-lg border p-2.5 text-sm"
                  >
                    <Checkbox
                      checked={selectedIndices.has(i)}
                      onCheckedChange={(v) =>
                        setSelectedIndices((prev) => {
                          const next = new Set(prev);
                          if (v) next.add(i);
                          else next.delete(i);
                          return next;
                        })
                      }
                      className="mt-0.5"
                    />
                    <span className="min-w-0">
                      <span className="block truncate font-medium">
                        {displayName(doc) || templateKindLabel(doc)}
                      </span>
                      <span className="block truncate text-xs text-muted-foreground">
                        {doc.kind ?? "agent"}
                        {hasRedactedPlaceholder(doc)
                          ? ` · ${t(($) => $.import.redacted_note)}`
                          : ""}
                      </span>
                    </span>
                  </label>
                );
              })}
            </div>
          </div>
        )}

        {step === "review" && (
          <div className="max-h-[60vh] space-y-4 overflow-y-auto pr-1">
            {/* Target runtime */}
            <div className="space-y-1.5">
              <Label htmlFor="rt-import-target">{t(($) => $.import.runtime_label)}</Label>
              <NativeSelect
                id="rt-import-target"
                value={targetRuntimeId}
                onChange={(e) => setTargetRuntimeId(e.target.value)}
                className="w-full"
              >
                <option value="" disabled>
                  {t(($) => $.import.runtime_placeholder)}
                </option>
                {onlineRuntimes.map((rt: AgentRuntime) => (
                  <NativeSelectOption key={rt.id} value={rt.id}>
                    {rt.custom_name || rt.name}
                    {rt.status === "online" ? "" : ` (${rt.status})`}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
              {onlineRuntimes.length === 0 && (
                <p className="text-xs text-muted-foreground">
                  {t(($) => $.import.runtime_none)}
                </p>
              )}
              {/* Q2 (PM decision): members-mode override only surfaces when a
                  squad template actually hit member conflicts; default stays
                  the template's own mode. */}
              {allValidated &&
                entries.some((e) => isSquadDoc(e.doc)) &&
                aggregatedConflicts.length > 0 && (
                <div className="flex items-center gap-3 pt-1">
                  <Label htmlFor="rt-members-mode" className="text-xs text-muted-foreground">
                    {t(($) => $.export.members_mode_label)}
                  </Label>
                  <NativeSelect
                    id="rt-members-mode"
                    value={membersMode}
                    onChange={(e) =>
                      setMembersMode(e.target.value as TemplateMembersMode)
                    }
                  >
                    <NativeSelectOption value="embedded">
                      {t(($) => $.export.members_mode_embedded)}
                    </NativeSelectOption>
                    <NativeSelectOption value="references">
                      {t(($) => $.export.members_mode_references)}
                    </NativeSelectOption>
                  </NativeSelect>
                </div>
              )}
            </div>

            {/* Validate action */}
            <Button
              type="button"
              size="sm"
              variant="outline"
              disabled={!targetRuntimeId || anyValidating}
              onClick={validateAll}
            >
              {anyValidating ? (
                <Loader2 className="mr-1 size-3.5 animate-spin" />
              ) : null}
              {anyValidating ? t(($) => $.import.validating) : t(($) => $.import.validate_button)}
            </Button>

            {/* Per-template validation results */}
            {entries.map((e, i) => (
              <div key={i} className="rounded-lg border p-3 text-sm">
                <div className="mb-1 flex items-center gap-2 font-medium">
                  {e.validate ? (
                    e.validate.errors && e.validate.errors.length > 0 ? (
                      <AlertCircle className="size-4 text-destructive" />
                    ) : (
                      <CheckCircle2 className="size-4 text-emerald-600" />
                    )
                  ) : null}
                  <span className="truncate">
                    {displayName(e.doc) || templateKindLabel(e.doc)}
                  </span>
                  <span className="ml-auto text-xs font-normal text-muted-foreground">
                    {e.doc.kind ?? "agent"}
                  </span>
                </div>
                {e.validate?.errors && e.validate.errors.length > 0 && (
                  <ul className="list-inside list-disc space-y-0.5 text-xs text-destructive">
                    {e.validate.errors.map((err, j) => (
                      <li key={j} className="flex items-start gap-1.5">
                        {err.code && (
                          <code className="rounded bg-destructive/10 px-1 font-mono text-[10px]">
                            {err.code}
                          </code>
                        )}
                        <span>
                          {err.path ? `${err.path}: ` : ""}
                          {err.message}
                        </span>
                      </li>
                    ))}
                  </ul>
                )}
                {e.validate?.warnings && e.validate.warnings.length > 0 && (
                  <ul className="mt-1 list-inside list-disc space-y-0.5 text-xs text-muted-foreground">
                    {e.validate.warnings.map((w, j) => (
                      <li key={j}>
                        {w.path ? `${w.path}: ` : ""}
                        {w.message}
                      </li>
                    ))}
                  </ul>
                )}
                {e.validate?.plan && (
                  <PlanSummary plan={e.validate.plan} />
                )}
                {/* Pre-apply tweaks (PRD US-Import-Wizard-4): lightweight
                    metadata only — name / description. */}
                {e.validate && e.validate.errors && e.validate.errors.length === 0 && (
                  <div className="mt-2 grid grid-cols-1 gap-2 sm:grid-cols-2">
                    <div className="space-y-1">
                      <Label htmlFor={`tweak-name-${i}`} className="text-xs text-muted-foreground">
                        {t(($) => $.import.tweak_name_label)}
                      </Label>
                      <Input
                        id={`tweak-name-${i}`}
                        className="h-7 text-xs"
                        defaultValue={displayName(e.doc)}
                        placeholder={displayName(e.doc)}
                        onChange={(ev) =>
                          setEntries((prev) =>
                            prev.map((p, pi) =>
                              pi === i ? { ...p, nameOverride: ev.target.value } : p,
                            ),
                          )
                        }
                      />
                    </div>
                    <div className="space-y-1">
                      <Label htmlFor={`tweak-desc-${i}`} className="text-xs text-muted-foreground">
                        {t(($) => $.import.tweak_description_label)}
                      </Label>
                      <Input
                        id={`tweak-desc-${i}`}
                        className="h-7 text-xs"
                        defaultValue={e.doc.metadata?.description ?? ""}
                        placeholder={e.doc.metadata?.description ?? ""}
                        onChange={(ev) =>
                          setEntries((prev) =>
                            prev.map((p, pi) =>
                              pi === i ? { ...p, descriptionOverride: ev.target.value } : p,
                            ),
                          )
                        }
                      />
                    </div>
                  </div>
                )}
              </div>
            ))}

            {/* Conflict policy */}
            {allValidated && aggregatedConflicts.length > 0 && (
              <div className="space-y-1.5">
                <Label>{t(($) => $.import.conflict_policy_label)}</Label>
                <RadioGroup
                  value={conflictPolicy}
                  onValueChange={(v) => {
                    setConflictPolicy(v as ConflictPolicy);
                    setConflictAcknowledged(false);
                  }}
                  className="space-y-1.5"
                >
                  <ConflictPolicyOption value="fail" />
                  <ConflictPolicyOption value="rename" />
                  <ConflictPolicyOption value="skip" />
                </RadioGroup>
                {conflictPolicy === "fail" ? (
                  <p className="flex items-start gap-2 text-xs text-destructive">
                    <AlertCircle className="mt-0.5 size-3.5 shrink-0" />
                    <span>{t(($) => $.import.conflict_fail_hint)}</span>
                  </p>
                ) : (
                  <label className="flex cursor-pointer items-start gap-2 text-xs text-muted-foreground">
                    <Checkbox
                      checked={conflictAcknowledged}
                      onCheckedChange={(v) => setConflictAcknowledged(!!v)}
                      className="mt-0.5"
                    />
                    <span>
                      {t(($) => $.import.conflict_ack, {
                        count: aggregatedConflicts.length,
                        policy: conflictPolicy,
                      })}
                    </span>
                  </label>
                )}
              </div>
            )}

            {/* Required env keys */}
            {allValidated && aggregatedEnvKeys.length > 0 && (
              <div className="space-y-1.5">
                <Label>{t(($) => $.import.required_env_section)}</Label>
                <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
                  <Lock className="mt-0.5 size-3.5 shrink-0" />
                  <span>{t(($) => $.import.required_env_hint)}</span>
                </p>
                <div className="space-y-2">
                  {aggregatedEnvKeys.map((k) => (
                    <div key={`${k.agent_ref}:${k.key}`} className="space-y-1">
                      <Label className="flex items-center gap-1.5 text-xs font-mono">
                        <Lock className="size-3 text-muted-foreground" />
                        {k.agent_ref} · {k.key}
                      </Label>
                      <Input
                        type="password"
                        autoComplete="off"
                        placeholder={t(($) => $.import.redacted_rebind)}
                        value={envValues[k.agent_ref]?.[k.key] ?? ""}
                        onChange={(e) =>
                          setEnvValues((prev) => ({
                            ...prev,
                            [k.agent_ref]: {
                              ...(prev[k.agent_ref] ?? {}),
                              [k.key]: e.target.value,
                            },
                          }))
                        }
                      />
                    </div>
                  ))}
                </div>
              </div>
            )}

            {/* Missing skills (install opt-in) */}
            {allValidated && aggregatedMissingSkills.length > 0 && (
              <div className="space-y-1.5">
                <Label>{t(($) => $.import.missing_skills_section)}</Label>
                <p className="text-xs text-muted-foreground">
                  {t(($) => $.import.missing_skills_hint)}
                </p>
                <Button
                  type="button"
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    const next: Record<string, boolean> = {};
                    for (const s of aggregatedMissingSkills) {
                      if (s.installable) next[s.source_url || s.name] = true;
                    }
                    setInstallSkills(next);
                  }}
                >
                  {t(($) => $.import.skill_select_all)}
                </Button>
                <div className="space-y-1.5">
                  {aggregatedMissingSkills.map((s) => {
                    const id = s.source_url || s.name;
                    return (
                      <label
                        key={id}
                        className={`flex items-start gap-2 text-sm ${
                          s.installable ? "cursor-pointer" : "opacity-70"
                        }`}
                      >
                        {s.installable && (
                          <Checkbox
                            checked={!!installSkills[id]}
                            onCheckedChange={(v) =>
                              setInstallSkills((prev) => ({ ...prev, [id]: !!v }))
                            }
                            className="mt-0.5"
                          />
                        )}
                        <span>
                          {s.installable
                            ? t(($) => $.import.missing_skills_installable, {
                                name: s.name,
                                url: s.source_url,
                              })
                            : t(($) => $.import.missing_skills_not_installable, {
                                name: s.name,
                              })}
                        </span>
                      </label>
                    );
                  })}
                </div>
              </div>
            )}

            {allValidated && blockingErrors > 0 && (
              <p className="flex items-start gap-2 text-sm text-destructive">
                <AlertCircle className="mt-0.5 size-4 shrink-0" />
                <span>
                  {t(($) => $.import.valid_errors, { count: blockingErrors })}
                </span>
              </p>
            )}
          </div>
        )}

        {step === "done" && applyResult && (
          <div className="space-y-2 text-sm">
            <DoneSummary result={applyResult} remainingSkills={remainingSkills} />
          </div>
        )}

        <DialogFooter>
          {step === "upload" && (
            <Button type="button" variant="outline" size="sm" onClick={() => handleOpenChange(false)}>
              {t(($) => $.import.cancel)}
            </Button>
          )}
          {step === "select" && (
            <>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setStep("upload")}
              >
                {t(($) => $.import.back)}
              </Button>
              <Button
                type="button"
                size="sm"
                disabled={selectedIndices.size === 0}
                onClick={confirmSelection}
              >
                {t(($) => $.import.continue)}
              </Button>
            </>
          )}
          {step === "review" && (
            <>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setStep("upload")}
              >
                {t(($) => $.import.back)}
              </Button>
              <Button
                type="button"
                size="sm"
                disabled={!canImport}
                onClick={doApply}
              >
                {applying ? (
                  <Loader2 className="mr-1 size-3.5 animate-spin" />
                ) : null}
                {applying ? t(($) => $.import.applying) : t(($) => $.import.apply_button)}
              </Button>
            </>
          )}
          {step === "done" && (
            <Button type="button" size="sm" onClick={() => handleOpenChange(false)}>
              {t(($) => $.import.close)}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function displayName(doc: ResourceTemplateDoc): string {
  return (
    doc.metadata?.name ??
    doc.spec?.agent?.name ??
    doc.spec?.squad?.name ??
    ""
  );
}

/**
 * Maps the wizard's editable name/description fields onto the apply
 * request's top-level overrides (the CLO-245 handler applies them to the
 * materialised agent or squad; the member-agent map stays untouched).
 * Returns undefined when nothing was edited, so the request carries no
 * overrides at all.
 */
function buildOverrides(e: TemplateEntry): ApplyResourceTemplateRequest["overrides"] {
  const name = e.nameOverride?.trim();
  const description = e.descriptionOverride?.trim();
  const nameChanged =
    name !== undefined &&
    name !== "" &&
    name !== (e.doc.metadata?.name ?? e.doc.spec?.agent?.name ?? e.doc.spec?.squad?.name);
  const descriptionChanged =
    description !== undefined && description !== e.doc.metadata?.description;
  if (!nameChanged && !descriptionChanged) return undefined;
  return {
    ...(nameChanged ? { name } : {}),
    ...(descriptionChanged ? { description } : {}),
  };
}

function isSquadDoc(doc: ResourceTemplateDoc): boolean {
  return doc.kind === "squad";
}

/** The template's own members_mode, when it is a valid squad mode. */
function squadMembersMode(doc: ResourceTemplateDoc): TemplateMembersMode | null {
  const mode = doc.spec?.squad?.members_mode;
  return mode === "embedded" || mode === "references" ? mode : null;
}

/** True when the doc carries a redacted credential placeholder. */
function hasRedactedPlaceholder(doc: ResourceTemplateDoc): boolean {
  return SECRET_PLACEHOLDER_RE.test(JSON.stringify(doc)) ||
    MCP_PLACEHOLDER_RE.test(JSON.stringify(doc));
}

function PlanSummary({
  plan,
}: {
  plan: ValidateResourceTemplateResponse["plan"];
}) {
  const { t } = useT("templates");
  const agents = plan.agents_to_create?.length ?? 0;
  const squads = plan.squads_to_create?.length ?? 0;
  const conflicts = plan.conflicts?.length ?? 0;
  if (agents === 0 && squads === 0 && conflicts === 0) return null;
  return (
    <div className="mt-1.5 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
      {agents > 0 && <span>{t(($) => $.import.plan_agents, { count: agents })}</span>}
      {squads > 0 && <span>{t(($) => $.import.plan_squads, { count: squads })}</span>}
      {conflicts > 0 && (
        <span className="text-amber-600">
          {t(($) => $.import.conflicts_section)}: {conflicts}
        </span>
      )}
    </div>
  );
}

function DoneSummary({
  result,
  remainingSkills,
}: {
  result: ApplyResourceTemplateResponse;
  remainingSkills: Array<{ name: string; source_url: string; installable: boolean }>;
}) {
  const { t } = useT("templates");
  const agents = result.created.agents?.length ?? 0;
  const squads = result.created.squads?.length ?? 0;
  const skills = result.created.skills?.length ?? 0;
  const skipped = result.skipped?.length ?? 0;
  return (
    <div className="space-y-1.5">
      <p className="flex items-center gap-2 font-medium text-emerald-600">
        <CheckCircle2 className="size-4" />
        {t(($) => $.import.apply_done_title)}
      </p>
      <ul className="space-y-0.5 text-muted-foreground">
        {agents > 0 && (
          <li>{t(($) => $.import.apply_done_agents, { count: agents })}</li>
        )}
        {squads > 0 && (
          <li>{t(($) => $.import.apply_done_squads, { count: squads })}</li>
        )}
        {skills > 0 && (
          <li>{t(($) => $.import.apply_done_skills, { count: skills })}</li>
        )}
        {skipped > 0 && (
          <li>{t(($) => $.import.apply_done_skipped, { count: skipped })}</li>
        )}
      </ul>
      {remainingSkills.length > 0 && (
        <div className="mt-1 rounded-md border border-amber-300 bg-amber-50 p-2">
          <p className="text-xs font-medium text-amber-800">
            {t(($) => $.import.skill_remaining_section)}
          </p>
          <ul className="mt-1 list-inside list-disc text-xs text-amber-700">
            {remainingSkills.map((s, i) => (
              <li key={i}>{s.name}</li>
            ))}
          </ul>
        </div>
      )}
      {(result.warnings?.length ?? 0) > 0 && (
        <ul className="mt-1 list-inside list-disc text-xs text-muted-foreground">
          {result.warnings!.slice(0, 5).map((w, i) => (
            <li key={i}>{w.message}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

function ConflictPolicyOption({ value }: { value: ConflictPolicy }) {
  const { t } = useT("templates");
  const label =
    value === "fail"
      ? t(($) => $.import.conflict_policy_fail)
      : value === "rename"
        ? t(($) => $.import.conflict_policy_rename)
        : t(($) => $.import.conflict_policy_skip);
  return (
    <label className="flex cursor-pointer items-center gap-2 text-sm">
      <RadioGroupItem value={value} />
      <span>{label}</span>
    </label>
  );
}
