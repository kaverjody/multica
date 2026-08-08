// Wire types for the CLO-245 resource-template capability
// (POST /api/templates/export|validate|apply). These mirror the Go structs in
// server/internal/handler/resource_template.go and the contract doc
// docs/plans/2026-08-05-001-feat-agent-squad-template-design.md.
//
// The exported/imported template document is an opaque JSON object on the
// client: the server is the sole authority over its structure (schema_version,
// template_id, kind, spec, metadata). We expose only the handful of fields the
// Web wizard reads for display; everything else travels through verbatim.

export type ResourceTemplateKind = "agent" | "squad";

export type TemplateMembersMode = "embedded" | "references";

/**
 * A portable resource template document. Treated as opaque JSON on the client
 * — the server validates it. The named fields are convenience accessors used
 * by the import wizard for display only.
 */
export interface ResourceTemplateDoc {
  schema_version?: string;
  template_id?: string;
  kind?: ResourceTemplateKind;
  metadata?: {
    name?: string;
    description?: string;
    version?: string;
    author?: { id?: string; display_name?: string };
    tags?: string[];
    /** Workspace the template was exported from (display-only provenance;
        drives the Web wizard's trust gate for foreign files). */
    source_workspace?: string;
  };
  spec?: {
    agent?: {
      name?: string;
      description?: string;
      model?: string;
      permission_mode?: string;
      skills?: Array<{ name?: string; enabled?: boolean }>;
    };
    squad?: {
      name?: string;
      description?: string;
      members_mode?: TemplateMembersMode;
      leader_ref?: string;
      members?: Array<{
        ref?: string;
        role?: string;
        agent?: { name?: string };
      }>;
    };
  };
  [key: string]: unknown;
}

export interface TemplateWarning {
  code: string;
  path?: string;
  message: string;
}

export interface TemplateError {
  code: string;
  path?: string;
  message: string;
}

// --- Export (POST /api/templates/export) ---

export interface ExportMetadataInput {
  version?: string;
  tags?: string[];
  description?: string;
}

export interface ExportResourceTemplateRequest {
  kind: ResourceTemplateKind;
  resource_id: string;
  members_mode?: TemplateMembersMode;
  metadata?: ExportMetadataInput;
}

export interface ExportResourceTemplateResponse {
  template: ResourceTemplateDoc;
  warnings?: TemplateWarning[];
}

// --- Validate (POST /api/templates/validate) ---

export interface RequiredEnvKeyInput {
  agent_ref: string;
  key: string;
}

export interface MissingSkillInput {
  name: string;
  source_url: string;
  installable: boolean;
}

export interface MissingMcpServerInput {
  agent_ref: string;
  name: string;
}

export interface MissingAgentInput {
  ref: string;
  name: string;
}

export interface RequiredInputs {
  env_keys?: RequiredEnvKeyInput[];
  missing_skills?: MissingSkillInput[];
  mcp_servers?: MissingMcpServerInput[];
  missing_agents?: MissingAgentInput[];
}

export interface NameConflict {
  kind: ResourceTemplateKind;
  name: string;
  existing_id: string;
}

export interface ApplyPlan {
  agents_to_create?: string[];
  squads_to_create?: string[];
  conflicts?: NameConflict[];
}

export interface ValidateResourceTemplateRequest {
  template: ResourceTemplateDoc;
  target_runtime_id: string;
  members_mode?: TemplateMembersMode;
}

export interface ValidateResourceTemplateResponse {
  valid: boolean;
  errors?: TemplateError[];
  warnings?: TemplateWarning[];
  required_inputs: RequiredInputs;
  plan: ApplyPlan;
}

// --- Apply (POST /api/templates/apply) ---

export type ConflictPolicy = "fail" | "rename" | "skip";

export interface AgentOverride {
  name?: string;
  description?: string;
  instructions?: string;
  model?: string;
  permission_mode?: string;
}

export interface ApplyOverrides {
  name?: string;
  description?: string;
  instructions?: string;
  model?: string;
  permission_mode?: string;
  agents?: Record<string, AgentOverride>;
}

export interface ApplyResourceTemplateRequest {
  template: ResourceTemplateDoc;
  target_runtime_id: string;
  members_mode?: TemplateMembersMode;
  overrides?: ApplyOverrides;
  env?: Record<string, Record<string, string>>;
  install_missing_skills?: string[];
  conflict_policy?: ConflictPolicy;
  idempotency_key?: string;
  dry_run?: boolean;
}

export interface CreatedAgent {
  ref: string;
  id?: string;
  name: string;
}

export interface CreatedSquad {
  id?: string;
  name: string;
}

export interface CreatedSkill {
  id?: string;
  name: string;
  reused: boolean;
}

export interface ApplyCreated {
  agents?: CreatedAgent[];
  squads?: CreatedSquad[];
  skills?: CreatedSkill[];
}

export interface SkippedResource {
  kind: ResourceTemplateKind;
  name: string;
  reason: string;
}

export interface ApplyResourceTemplateResponse {
  applied: boolean;
  valid?: boolean;
  dry_run: boolean;
  errors?: TemplateError[];
  warnings?: TemplateWarning[];
  required_inputs?: RequiredInputs;
  plan?: ApplyPlan;
  created: ApplyCreated;
  resource_mapping: Record<string, string>;
  skipped?: SkippedResource[];
  rolled_back: boolean;
  idempotent_replay: boolean;
}
