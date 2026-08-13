// @vitest-environment jsdom

import { describe, expect, it, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { renderWithI18n } from "../test/i18n";

const listRuntimesSpy = vi.hoisted(() => vi.fn(async () => []));
const validateSpy = vi.hoisted(() =>
  vi.fn(async (_req: Record<string, unknown>) => ({})),
);
const applySpy = vi.hoisted(() =>
  vi.fn(async (_req: Record<string, unknown>) => ({})),
);
const invalidateQueries = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/api", () => ({
  api: {
    listRuntimes: listRuntimesSpy,
    validateResourceTemplate: validateSpy,
    applyResourceTemplate: applySpy,
  },
}));
vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "ws-1",
}));
vi.mock("@tanstack/react-query", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-query")>();
  return {
    ...actual,
    useQuery: () => ({
      data: [{ id: "rt-1", name: "Local", custom_name: "Local RT", status: "online" }],
      isLoading: false,
    }),
    useQueryClient: () => ({ invalidateQueries }),
  };
});
vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn() },
}));

import { ResourceTemplateImportDialog } from "./resource-template-import-dialog";

const AGENT_DOC = {
  schema_version: "1.0",
  template_id: "tpl-1",
  kind: "agent",
  metadata: {
    name: "My Agent",
    description: "A test agent",
    source_workspace: "ws-1",
  },
  spec: {
    agent: {
      name: "My Agent",
      description: "A test agent",
      model: "",
      skills: [],
    },
  },
};

const SQUAD_DOC = {
  schema_version: "1.0",
  template_id: "tpl-2",
  kind: "squad",
  metadata: { name: "My Squad", source_workspace: "ws-1" },
  spec: { squad: { name: "My Squad", members_mode: "embedded", members: [] } },
};

function validResponse(plan: Record<string, unknown> = {}, overrides: Record<string, unknown> = {}) {
  return {
    valid: true,
    errors: [],
    warnings: [],
    required_inputs: {},
    plan: { agents_to_create: ["My Agent"], squads_to_create: [], conflicts: [], ...plan },
    ...overrides,
  };
}

function renderDialog() {
  const onOpenChange = vi.fn();
  renderWithI18n(
    <ResourceTemplateImportDialog open onOpenChange={onOpenChange} />,
  );
  return { onOpenChange };
}

async function uploadFile(name: string, contents: unknown) {
  const input = screen.getByLabelText(/browse files/i) as HTMLInputElement;
  const file = new File([JSON.stringify(contents)], name, {
    type: "application/json",
  });
  fireEvent.change(input, { target: { files: [file] } });
  // The wizard may land on the trust gate, the bundle-selection substep, or
  // the review step depending on the file; callers assert their step.
  await waitFor(() => expect(input.files?.[0]).toBe(file));
}

describe("ResourceTemplateImportDialog", () => {
  beforeEach(() => {
    listRuntimesSpy.mockClear();
    validateSpy.mockClear();
    applySpy.mockClear();
    invalidateQueries.mockClear();
    listRuntimesSpy.mockResolvedValue([]);
  });

  it("rejects files larger than 5 MB (PRD AC-Import-1-2)", async () => {
    renderDialog();
    const input = screen.getByLabelText(/browse files/i) as HTMLInputElement;
    const big = new File([new Uint8Array(5 * 1024 * 1024 + 1)], "big.json", {
      type: "application/json",
    });
    fireEvent.change(input, { target: { files: [big] } });
    expect(await screen.findByText(/larger than 5 MB/i)).toBeInTheDocument();
  });

  it("shows a parse error for invalid JSON", async () => {
    renderDialog();
    const input = screen.getByLabelText(/browse files/i) as HTMLInputElement;
    fireEvent.change(input, {
      target: { files: [new File(["not json {"], "bad.json", { type: "application/json" })] },
    });
    expect(await screen.findByText(/isn't valid template JSON/i)).toBeInTheDocument();
  });

  it("runs the full wizard: upload → validate → apply → done summary", async () => {
    validateSpy.mockResolvedValue(validResponse());
    applySpy.mockResolvedValue({
      applied: true,
      dry_run: false,
      created: {
        agents: [{ ref: "My Agent", id: "ag-1", name: "My Agent" }],
        squads: [],
        skills: [],
      },
      resource_mapping: { "My Agent": "ag-1" },
      rolled_back: false,
      idempotent_replay: false,
    });

    renderDialog();
    await uploadFile("agent.json", AGENT_DOC);
    await screen.findByText(/target runtime/i);

    // Pick the target runtime and validate.
    fireEvent.change(screen.getByLabelText(/target runtime/i), {
      target: { value: "rt-1" },
    });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await waitFor(() =>
      expect(validateSpy).toHaveBeenCalledWith({
        template: AGENT_DOC,
        target_runtime_id: "rt-1",
      }),
    );
    await screen.findByText(/My Agent/i);

    // Apply (default conflict policy is fail per CLO-417).
    fireEvent.click(screen.getByRole("button", { name: /^import$/i }));
    await waitFor(() => expect(applySpy).toHaveBeenCalledTimes(1));
    const applyReq = applySpy.mock.calls[0]![0] as Record<string, unknown>;
    expect(applyReq.conflict_policy).toBe("fail");
    expect(applyReq.template).toEqual(AGENT_DOC);
    expect(applyReq.target_runtime_id).toBe("rt-1");
    expect(applyReq.idempotency_key).toBeTruthy();

    await screen.findAllByText(/import complete/i);
    expect(invalidateQueries).toHaveBeenCalled();
  });

  it("sends name/description tweaks as apply overrides (PRD US-Import-Wizard-4)", async () => {
    validateSpy.mockResolvedValue(validResponse());
    applySpy.mockResolvedValue({
      applied: true,
      dry_run: false,
      created: { agents: [{ ref: "My Agent", id: "ag-1", name: "Renamed" }], squads: [], skills: [] },
      resource_mapping: {},
      rolled_back: false,
      idempotent_replay: false,
    });

    renderDialog();
    await uploadFile("agent.json", AGENT_DOC);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), {
      target: { value: "rt-1" },
    });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await screen.findByText(/My Agent/i);

    fireEvent.change(screen.getByLabelText(/^name$/i), {
      target: { value: "Renamed Agent" },
    });
    fireEvent.change(screen.getByLabelText(/description/i), {
      target: { value: "New description" },
    });
    fireEvent.click(screen.getByRole("button", { name: /^import$/i }));

    await waitFor(() => expect(applySpy).toHaveBeenCalledTimes(1));
    const applyReq = applySpy.mock.calls[0]![0] as {
      overrides?: { name?: string; description?: string };
    };
    expect(applyReq.overrides).toEqual({
      name: "Renamed Agent",
      description: "New description",
    });
  });

  it("requires an explicit trust confirmation for files from other workspaces", async () => {
    const FOREIGN = {
      schema_version: "1.0",
      template_id: "tpl-x",
      kind: "agent",
      metadata: { name: "Foreign Agent", source_workspace: "ws-other" },
      spec: { agent: { name: "Foreign Agent" } },
    };
    renderDialog();
    await uploadFile("foreign.json", FOREIGN);

    // Trust gate blocks the review step.
    expect(await screen.findByText(/untrusted template file/i)).toBeInTheDocument();
    expect(screen.queryByText(/target runtime/i)).not.toBeInTheDocument();

    // Confirming proceeds into review.
    fireEvent.click(screen.getByRole("checkbox", { name: /i trust this file/i }));
    fireEvent.click(screen.getByRole("button", { name: /continue to review/i }));
    expect(await screen.findByText(/target runtime/i)).toBeInTheDocument();
  });

  it("offers a template-selection substep for bundles (Q1)", async () => {
    const bundle = {
      kind: "bundle",
      schema_version: "multica-template-bundle/v1",
      templates: [
        { ...AGENT_DOC, template_id: "tpl-1" },
        { ...AGENT_DOC, template_id: "tpl-2", metadata: { ...AGENT_DOC.metadata, name: "Second Agent" } },
      ],
    };
    renderDialog();
    await uploadFile("bundle.json", bundle);

    expect(await screen.findByText(/choose templates to import/i)).toBeInTheDocument();

    // Deselect the second template, then continue.
    fireEvent.click(screen.getAllByRole("checkbox")[1]!);
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByText(/target runtime/i);

    // Only one template reaches the review step.
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await waitFor(() => expect(validateSpy).toHaveBeenCalledTimes(1));
    expect((validateSpy.mock.calls[0]![0] as { template: { template_id: string } }).template.template_id).toBe("tpl-1");
  });

  it("fail policy: conflict zone shows the new-name input and a visible blocked reason (CLO-503)", async () => {
    validateSpy.mockResolvedValue(
      validResponse({
        agents_to_create: [],
        squads_to_create: [],
        conflicts: [
          { kind: "agent", name: "My Agent", existing_id: "ag-0" },
        ],
      }),
    );

    renderDialog();
    await uploadFile("agent.json", AGENT_DOC);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));

    // The conflict zone itself carries the editable new-name input, prefilled
    // with the original name (no hidden "tweak" field).
    await screen.findByText(/conflicts with 1 existing resource/i);
    const nameInput = screen.getByLabelText(/^new name$/i) as HTMLInputElement;
    expect(nameInput.value).toBe("My Agent");

    // The fail policy states why the import button is disabled.
    expect(screen.getByText(/1 name conflict\(s\) block the import/i)).toBeInTheDocument();
    expect(screen.getByText(/1 unresolved name conflicts/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^import$/i })).toBeDisabled();
  });

  it("fail policy: editing the new name resolves the conflict and applies with the override (CLO-417/503)", async () => {
    validateSpy.mockResolvedValue(
      validResponse({
        agents_to_create: [],
        squads_to_create: [],
        conflicts: [
          { kind: "agent", name: "My Agent", existing_id: "ag-0" },
        ],
      }),
    );
    applySpy.mockResolvedValue({
      applied: true,
      dry_run: false,
      created: { agents: [{ ref: "My Agent", id: "ag-1", name: "My Agent-renamed" }], squads: [], skills: [] },
      resource_mapping: {},
      rolled_back: false,
      idempotent_replay: false,
    });

    renderDialog();
    await uploadFile("agent.json", AGENT_DOC);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await screen.findByText(/conflicts with 1 existing resource/i);

    // Default policy is fail, and the apply stays disabled until the name is
    // edited to a non-conflicting value.
    const apply = screen.getByRole("button", { name: /^import$/i });
    expect(apply).toBeDisabled();

    // Renaming via the conflict-zone input resolves the conflict.
    fireEvent.change(screen.getByLabelText(/^new name$/i), {
      target: { value: "My Agent-renamed" },
    });
    await waitFor(() => expect(apply).toBeEnabled());
    fireEvent.click(apply);
    await waitFor(() => expect(applySpy).toHaveBeenCalledTimes(1));

    // The pre-apply re-validation ran with the overridden name.
    const revalidated = validateSpy.mock.calls.at(-1)![0] as {
      template: { metadata: { name: string } };
    };
    expect(revalidated.template.metadata.name).toBe("My Agent-renamed");

    const applyReq = applySpy.mock.calls[0]![0] as Record<string, unknown>;
    expect(applyReq.conflict_policy).toBe("fail");
    expect((applyReq.overrides as { name?: string }).name).toBe("My Agent-renamed");
  });

  it("rename policy: requires a manually typed new name and never sends conflict_policy=rename (CLO-503)", async () => {
    validateSpy.mockResolvedValue(
      validResponse({
        agents_to_create: [],
        squads_to_create: [],
        conflicts: [
          { kind: "agent", name: "My Agent", existing_id: "ag-0" },
        ],
      }),
    );
    applySpy.mockResolvedValue({
      applied: true,
      dry_run: false,
      created: { agents: [{ ref: "My Agent", id: "ag-1", name: "My Agent-renamed" }], squads: [], skills: [] },
      resource_mapping: {},
      rolled_back: false,
      idempotent_replay: false,
    });

    renderDialog();
    await uploadFile("agent.json", AGENT_DOC);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await screen.findByText(/conflicts with 1 existing resource/i);

    // Switch to the rename policy: the ack checkbox is gone — the manual
    // rename is the confirmation. Without a new name the apply stays disabled.
    fireEvent.click(screen.getAllByLabelText(/create with a new name/i)[0]!);
    const apply = screen.getByRole("button", { name: /^import$/i });
    expect(apply).toBeDisabled();
    expect(screen.queryByLabelText(/i understand/i)).not.toBeInTheDocument();

    // Typing a different name in the conflict zone enables the import.
    fireEvent.change(screen.getByLabelText(/^new name$/i), {
      target: { value: "My Agent-renamed" },
    });
    await waitFor(() => expect(apply).toBeEnabled());
    fireEvent.click(apply);
    await waitFor(() => expect(applySpy).toHaveBeenCalledTimes(1));

    // The wizard never asks the backend to auto-rename: effective policy is
    // fail and the manual name travels as an override.
    const applyReq = applySpy.mock.calls[0]![0] as Record<string, unknown>;
    expect(applyReq.conflict_policy).toBe("fail");
    expect((applyReq.overrides as { name?: string }).name).toBe("My Agent-renamed");
  });

  it("rename policy: blocks apply when the typed new name still conflicts — no auto suffix (CLO-503)", async () => {
    // First validation reports the original name as conflicting; the
    // pre-apply re-validation (with the override applied) reports the NEW
    // name as still taken.
    validateSpy.mockImplementation(
      async (req: Record<string, unknown>) => {
        const template = req.template as { metadata: { name: string } };
        const name = template.metadata.name;
        return validResponse({
          agents_to_create: [],
          squads_to_create: [],
          conflicts:
            name === "My Agent-renamed"
              ? [{ kind: "agent", name: "My Agent-renamed", existing_id: "ag-9" }]
              : [{ kind: "agent", name: "My Agent", existing_id: "ag-0" }],
        });
      },
    );

    renderDialog();
    await uploadFile("agent.json", AGENT_DOC);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await screen.findByText(/conflicts with 1 existing resource/i);

    fireEvent.click(screen.getAllByLabelText(/create with a new name/i)[0]!);
    fireEvent.change(screen.getByLabelText(/^new name$/i), {
      target: { value: "My Agent-renamed" },
    });
    const apply = screen.getByRole("button", { name: /^import$/i });
    await waitFor(() => expect(apply).toBeEnabled());

    fireEvent.click(apply);

    // The import is blocked with a visible reason and apply is never called —
    // the backend would have auto-suffixed this name under the rename policy.
    expect(await screen.findByText(/still conflicts with an existing resource/i)).toBeInTheDocument();
    expect(applySpy).not.toHaveBeenCalled();
  });

  it("skip policy: requires the acknowledgement and applies with conflict_policy=skip", async () => {
    validateSpy.mockResolvedValue(
      validResponse({
        agents_to_create: [],
        squads_to_create: [],
        conflicts: [
          { kind: "agent", name: "My Agent", existing_id: "ag-0" },
        ],
      }),
    );
    applySpy.mockResolvedValue({
      applied: true,
      dry_run: false,
      created: { agents: [], squads: [], skills: [] },
      resource_mapping: {},
      skipped: [{ kind: "agent", name: "My Agent", reason: "name_conflict" }],
      rolled_back: false,
      idempotent_replay: false,
    });

    renderDialog();
    await uploadFile("agent.json", AGENT_DOC);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await screen.findByText(/conflicts with 1 existing resource/i);

    // Skip needs no new name, but the explicit acknowledgement gates the apply.
    fireEvent.click(screen.getAllByLabelText(/skip and keep the existing one/i)[0]!);
    const apply = screen.getByRole("button", { name: /^import$/i });
    expect(apply).toBeDisabled();
    expect(screen.getByText(/confirm the skip to enable import/i)).toBeInTheDocument();

    fireEvent.click(
      screen.getByRole("checkbox", { name: /i understand 1/i }),
    );
    await waitFor(() => expect(apply).toBeEnabled());
    fireEvent.click(apply);
    await waitFor(() => expect(applySpy).toHaveBeenCalledTimes(1));
    const applyReq = applySpy.mock.calls[0]![0] as Record<string, unknown>;
    expect(applyReq.conflict_policy).toBe("skip");
  });

  it("passes members_mode for squad templates and blocks apply on validate errors", async () => {
    validateSpy.mockResolvedValue({
      valid: false,
      errors: [{ code: "E", path: "spec", message: "bad spec" }],
      warnings: [],
      required_inputs: {},
      plan: {},
    });

    renderDialog();
    await uploadFile("squad.json", SQUAD_DOC);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), {
      target: { value: "rt-1" },
    });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));

    await screen.findByText(/bad spec/i);
    expect(validateSpy).toHaveBeenCalledWith({
      template: SQUAD_DOC,
      target_runtime_id: "rt-1",
      members_mode: "embedded",
    });
    // The import button stays disabled while validation errors block.
    expect(screen.getByRole("button", { name: /^import$/i })).toBeDisabled();
  });

  // --- CLO-503 round 2: squad templates with member-agent conflicts ---

  const SQUAD_WITH_MEMBERS = {
    schema_version: "1.0",
    template_id: "tpl-3",
    kind: "squad",
    metadata: { name: "Ops Squad", source_workspace: "ws-1" },
    spec: {
      squad: {
        name: "Ops Squad",
        members_mode: "embedded",
        leader_ref: "qa-review-agent",
        members: [
          { ref: "qa-review-agent", role: "leader", agent: { name: "qa-review-agent" } },
          { ref: "dev-helper", role: "member", agent: { name: "dev-helper" } },
        ],
      },
    },
  };

  const MEMBER_CONFLICTS = [
    { kind: "squad", name: "Ops Squad", existing_id: "sq-0" },
    { kind: "agent", name: "qa-review-agent", existing_id: "ag-1" },
    { kind: "agent", name: "dev-helper", existing_id: "ag-2" },
  ];

  it("squad import: every member-agent conflict gets its own new-name input; apply stays disabled until all are renamed (CLO-503)", async () => {
    validateSpy.mockResolvedValue(
      validResponse({
        agents_to_create: ["qa-review-agent", "dev-helper"],
        squads_to_create: ["Ops Squad"],
        conflicts: MEMBER_CONFLICTS,
      }),
    );

    renderDialog();
    await uploadFile("squad-members.json", SQUAD_WITH_MEMBERS);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await screen.findByText(/conflicts with 3 existing resource/i);

    // The squad-level input plus one input per conflicting member agent,
    // all prefilled with the original names.
    expect((screen.getByLabelText(/^new name$/i) as HTMLInputElement).value).toBe("Ops Squad");
    const memberInputs = screen.getAllByLabelText(/new name for member/i);
    expect(memberInputs).toHaveLength(2);
    expect((memberInputs[0] as HTMLInputElement).value).toBe("qa-review-agent");
    expect((memberInputs[1] as HTMLInputElement).value).toBe("dev-helper");

    const apply = screen.getByRole("button", { name: /^import$/i });
    expect(apply).toBeDisabled();
    expect(screen.getByText(/3 unresolved name conflicts/i)).toBeInTheDocument();

    // Renaming only the squad keeps the members unresolved: 2 left.
    fireEvent.change(screen.getByLabelText(/^new name$/i), {
      target: { value: "Ops Squad-renamed" },
    });
    expect(apply).toBeDisabled();
    expect(screen.getByText(/2 unresolved name conflicts/i)).toBeInTheDocument();

    // Renaming one member leaves exactly one conflict.
    fireEvent.change(
      screen.getByLabelText(/new name for member qa-review-agent/i),
      { target: { value: "qa-review-agent-v2" } },
    );
    expect(apply).toBeDisabled();
    expect(screen.getByText(/1 unresolved name conflicts/i)).toBeInTheDocument();

    // All three renamed → import enabled.
    fireEvent.change(
      screen.getByLabelText(/new name for member dev-helper/i),
      { target: { value: "dev-helper-v2" } },
    );
    await waitFor(() => expect(apply).toBeEnabled());
  });

  it("squad import: renamed members travel as overrides.agents and re-validation runs with member names applied (CLO-503)", async () => {
    validateSpy.mockResolvedValue(
      validResponse({
        agents_to_create: ["qa-review-agent", "dev-helper"],
        squads_to_create: ["Ops Squad"],
        conflicts: MEMBER_CONFLICTS,
      }),
    );
    applySpy.mockResolvedValue({
      applied: true,
      dry_run: false,
      created: {
        agents: [
          { ref: "qa-review-agent", id: "ag-3", name: "qa-review-agent-v2" },
          { ref: "dev-helper", id: "ag-4", name: "dev-helper-v2" },
        ],
        squads: [{ id: "sq-1", name: "Ops Squad-renamed" }],
        skills: [],
      },
      resource_mapping: {},
      rolled_back: false,
      idempotent_replay: false,
    });

    renderDialog();
    await uploadFile("squad-members.json", SQUAD_WITH_MEMBERS);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await screen.findByText(/conflicts with 3 existing resource/i);

    fireEvent.change(screen.getByLabelText(/^new name$/i), {
      target: { value: "Ops Squad-renamed" },
    });
    fireEvent.change(
      screen.getByLabelText(/new name for member qa-review-agent/i),
      { target: { value: "qa-review-agent-v2" } },
    );
    fireEvent.change(
      screen.getByLabelText(/new name for member dev-helper/i),
      { target: { value: "dev-helper-v2" } },
    );
    const apply = screen.getByRole("button", { name: /^import$/i });
    await waitFor(() => expect(apply).toBeEnabled());
    fireEvent.click(apply);
    await waitFor(() => expect(applySpy).toHaveBeenCalledTimes(1));

    // The pre-apply re-validation carried BOTH the squad rename and the
    // member renames in the template doc.
    const revalidated = validateSpy.mock.calls.at(-1)![0] as {
      template: {
        metadata: { name: string };
        spec: { squad: { members: Array<{ agent: { name: string } }> } };
      };
    };
    expect(revalidated.template.metadata.name).toBe("Ops Squad-renamed");
    expect(revalidated.template.spec.squad.members[0]!.agent.name).toBe("qa-review-agent-v2");
    expect(revalidated.template.spec.squad.members[1]!.agent.name).toBe("dev-helper-v2");

    // The apply carries the effective fail policy (never rename → no auto
    // suffix) and the member overrides keyed by the template's member refs.
    const applyReq = applySpy.mock.calls[0]![0] as {
      conflict_policy?: string;
      members_mode?: string;
      overrides?: {
        name?: string;
        agents?: Record<string, { name?: string }>;
      };
    };
    expect(applyReq.conflict_policy).toBe("fail");
    expect(applyReq.members_mode).toBe("embedded");
    expect(applyReq.overrides?.name).toBe("Ops Squad-renamed");
    expect(applyReq.overrides?.agents).toEqual({
      "qa-review-agent": { name: "qa-review-agent-v2" },
      "dev-helper": { name: "dev-helper-v2" },
    });
  });

  it("squad import: a member new name that still collides blocks apply with a visible reason — no auto suffix (CLO-503)", async () => {
    validateSpy.mockImplementation(async (req: Record<string, unknown>) => {
      const template = req.template as {
        spec: { squad: { members?: Array<{ agent?: { name?: string } }> } };
      };
      const memberName = template.spec?.squad?.members?.[0]?.agent?.name;
      return validResponse({
        agents_to_create: ["qa-review-agent", "dev-helper"],
        squads_to_create: ["Ops Squad"],
        conflicts:
          memberName === "qa-review-agent-v2"
            ? [
                { kind: "squad", name: "Ops Squad-renamed", existing_id: "sq-0" },
                { kind: "agent", name: "qa-review-agent-v2", existing_id: "ag-9" },
                { kind: "agent", name: "dev-helper-v2", existing_id: "ag-2" },
              ]
            : MEMBER_CONFLICTS,
      });
    });

    renderDialog();
    await uploadFile("squad-members.json", SQUAD_WITH_MEMBERS);
    await screen.findByText(/target runtime/i);
    fireEvent.change(screen.getByLabelText(/target runtime/i), { target: { value: "rt-1" } });
    fireEvent.click(screen.getByRole("button", { name: /validate/i }));
    await screen.findByText(/conflicts with 3 existing resource/i);

    fireEvent.change(screen.getByLabelText(/^new name$/i), {
      target: { value: "Ops Squad-renamed" },
    });
    fireEvent.change(
      screen.getByLabelText(/new name for member qa-review-agent/i),
      { target: { value: "qa-review-agent-v2" } },
    );
    fireEvent.change(
      screen.getByLabelText(/new name for member dev-helper/i),
      { target: { value: "dev-helper-v2" } },
    );
    const apply = screen.getByRole("button", { name: /^import$/i });
    await waitFor(() => expect(apply).toBeEnabled());

    fireEvent.click(apply);

    // The member's new name still collides → visible red block, apply never
    // called (the backend would have auto-suffixed under rename).
    expect(await screen.findByText(/still conflicts with an existing resource/i)).toBeInTheDocument();
    expect(applySpy).not.toHaveBeenCalled();
  });
});
