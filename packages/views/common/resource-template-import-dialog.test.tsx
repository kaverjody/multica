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
  metadata: { name: "My Agent", description: "A test agent" },
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
  metadata: { name: "My Squad" },
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
  await screen.findByText(/target runtime/i);
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

    // Apply (default conflict policy is rename per PRD AC-Import-3-2).
    fireEvent.click(screen.getByRole("button", { name: /^import$/i }));
    await waitFor(() => expect(applySpy).toHaveBeenCalledTimes(1));
    const applyReq = applySpy.mock.calls[0]![0] as Record<string, unknown>;
    expect(applyReq.conflict_policy).toBe("rename");
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
});
