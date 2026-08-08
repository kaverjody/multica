// Package resourcetmpl defines the portable, file-based resource template
// format used by the "export my agent/squad as a reusable template" feature
// (CLO-245). A Template is a plain JSON document that captures the portable
// parts of an agent or squad definition so a teammate can load it, tweak it,
// and materialise their own copy in a different workspace.
//
// The package is deliberately self-contained: it depends only on the standard
// library and carries no knowledge of the HTTP handler, the database, or the
// agent/squad domain models. The curated, repo-only templates in
// server/internal/agenttmpl are a separate concern and are intentionally left
// untouched — the two packages do not import each other.
//
// The four files split responsibilities as described in the design doc:
//
//   - types.go    — Template / AgentSpec / SquadSpec wire types + error model.
//   - version.go  — SchemaVersion constant and compatibility decision.
//   - validate.go — structural, enum, unknown-field and referential checks.
//   - redact.go   — plaintext-secret detection shared by export and import.
package resourcetmpl

import "encoding/json"

// Schema-wide enum constants. Centralised here so validate.go and callers
// (CLI, future handler) agree on the exact literal strings.

// Kind is the resource type a template materialises. v1 ships agent and squad
// only; a bundle kind is explicitly out of scope.
const (
	KindAgent = "agent"
	KindSquad = "squad"
)

// Visibility is the sharing scope. Phase 1 is workspace-only; cross-workspace
// and community sharing are reserved for later by validating against this set.
const (
	VisibilityWorkspace = "workspace"
)

// PermissionMode mirrors the agent invocation-permission tiers (MUL-3963). Only
// the two portable tiers are representable; the legacy allow-list target set is
// not portable and is dropped on export.
const (
	PermissionPrivate  = "private"
	PermissionPublicTo = "public_to"
)

// MembersMode controls how a squad template describes its members.
const (
	MembersEmbedded   = "embedded"   // each member embeds a full AgentSpec
	MembersReferences = "references" // members resolve by ref at apply time
)

// RoleLeader is the only role value that may sit under the leader_ref member.
const RoleLeader = "leader"

// RoleMember is the default role applied to a squad member whose stored role
// is empty (legacy data), so an exported template always carries a valid,
// non-empty member role (CLO-418).
const RoleMember = "member"

// Template is the top-level wire representation of an exported resource
// template. It is the single contract every exporter, importer, validator and
// CLI command operates on; nothing in the package mutates a Template in place.
type Template struct {
	// SchemaVersion is the format version, e.g. "1.0". Required; a major
	// version the package does not support is rejected outright (no guess,
	// no downgrade). See version.go.
	SchemaVersion string `json:"schema_version"`

	// TemplateID is a UUID the server derives deterministically from the
	// exported resource, so re-exporting the same resource yields the same id
	// and repeat exports stay recognisable as the same template. It is an
	// idempotency / provenance handle only — never an import authorisation
	// source (source_workspace isn't either). metadata.version is the
	// user-managed SemVer and moves independently of this id.
	TemplateID string `json:"template_id,omitempty"`

	// Kind selects which Spec payload is present. Must be agent or squad and
	// must match the single key nested under Spec.
	Kind string `json:"kind"`

	// Metadata carries authorship, versioning and display-only provenance.
	Metadata Metadata `json:"metadata"`

	// Spec wraps the kind-specific payload. Exactly one of Agent or Squad is
	// populated; Validate enforces the key matches Kind.
	Spec Spec `json:"spec"`
}

// Metadata is the human/template-management metadata. The created_at and
// source_workspace fields are display-only and carry no authorisation weight.
type Metadata struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Author      Author   `json:"author"`
	Version     string   `json:"version"`    // SemVer
	Visibility  string   `json:"visibility"` // workspace in v1
	Tags        []string `json:"tags,omitempty"`
	// SourceWorkspace is the UUID of the workspace the template was exported
	// from. Shown in the UI for context; never trusted on import.
	SourceWorkspace string `json:"source_workspace,omitempty"`
	// CreatedAt is an RFC3339 timestamp of export time.
	CreatedAt string `json:"created_at,omitempty"`
	// Readme names a companion prose file shipped alongside the template
	// (conventionally "README.md"). JSON cannot carry comments, so anything a
	// human needs to read — what the template does, which credentials to
	// supply after apply — lives in that sibling file rather than in the
	// template body. Display-only: it is a filename, never a path to resolve
	// or fetch, and the import path must not read it.
	Readme string `json:"readme,omitempty"`
}

// Author identifies who produced the template. Both fields are display-only.
type Author struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// Spec wraps the kind-specific payload. Only the field matching Kind should be
// set; Validate rejects a Spec carrying both keys or a key that disagrees with
// Kind.
type Spec struct {
	Agent *AgentSpec `json:"agent,omitempty"`
	Squad *SquadSpec `json:"squad,omitempty"`
}

// AgentSpec is the portable subset of an agent definition. Machine-local
// fields (runtime_id, runtime_config, owner_id, the live custom_env value map,
// the raw mcp_config with auth material, invocation_targets, composio
// allowlist, timestamps, IDs, archived_*) are intentionally absent: because
// they are not declared here, any occurrence surfaces as an unknown field and
// is rejected by Validate. That is the mechanism that enforces the
// "non-portable fields forbidden" rule.
type AgentSpec struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Instructions string `json:"instructions,omitempty"`

	// Model may be empty to mean "target runtime default".
	Model              string `json:"model,omitempty"`
	ThinkingLevel      string `json:"thinking_level,omitempty"`
	ServiceTier        string `json:"service_tier,omitempty"`
	MaxConcurrentTasks int    `json:"max_concurrent_tasks,omitempty"`

	// PermissionMode is empty (use default) or one of the Permission* consts.
	PermissionMode    string `json:"permission_mode,omitempty"`
	PublicToWorkspace bool   `json:"public_to_workspace,omitempty"`

	CustomArgs []string `json:"custom_args,omitempty"`

	Skills        []SkillRef     `json:"skills,omitempty"`
	CustomEnvKeys []CustomEnvKey `json:"custom_env_keys,omitempty"`
	MCPServers    []MCPServer    `json:"mcp_servers,omitempty"`
}

// SkillRef references a skill the agent should be bound to. SourceURL is the
// only auto-materialisable handle; a skill without SourceURL is resolved by
// Name in the target workspace (find-or-create), never auto-installed.
type SkillRef struct {
	Name      string `json:"name"`
	SourceURL string `json:"source_url,omitempty"`
	Enabled   bool   `json:"enabled,omitempty"`
}

// CustomEnvKey records that an agent expects an environment variable named
// Key, without ever carrying the variable's value. Apply collects missing
// required keys into required_inputs so the caller can supply them through the
// secure agent-env channel.
type CustomEnvKey struct {
	Key         string `json:"key"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// MCPServer is the portable skeleton of one MCP server entry. ConfigSkeleton
// is the non-secret structural shape (command/args/transport hints); auth,
// token, headers and env values are stripped on export and never carried here.
type MCPServer struct {
	Name           string          `json:"name"`
	Transport      string          `json:"transport"`
	RequiresAuth   bool            `json:"requires_auth,omitempty"`
	ConfigSkeleton json.RawMessage `json:"config_skeleton,omitempty"`
}

// SquadSpec is the portable subset of a squad definition. Members reference
// each other and the leader by Ref, a template-internal symbol (not a UUID).
type SquadSpec struct {
	Name         string      `json:"name"`
	Description  string      `json:"description,omitempty"`
	Instructions string      `json:"instructions,omitempty"`
	MembersMode  string      `json:"members_mode"`
	LeaderRef    string      `json:"leader_ref"`
	Members      []MemberRef `json:"members"`
}

// MemberRef is one squad member. Ref is unique within the template. In
// embedded mode Agent must be present; in references mode it must be omitted.
type MemberRef struct {
	Ref   string     `json:"ref"`
	Role  string     `json:"role"`
	Agent *AgentSpec `json:"agent,omitempty"`
}

// ErrorCode is the stable, machine-readable validation outcome. Frontend and
// CLI depend on these literals; do not rename without a coordinated change.
type ErrorCode string

const (
	// CodeTemplateInvalid covers malformed JSON, missing required fields,
	// unknown fields, bad enums and kind/spec mismatches.
	CodeTemplateInvalid ErrorCode = "TEMPLATE_INVALID"
	// CodeTemplateVersionUnsupported is emitted when schema_version's major
	// number is not supported. No downgrade or guess is attempted.
	CodeTemplateVersionUnsupported ErrorCode = "TEMPLATE_VERSION_UNSUPPORTED"
	// CodeSecretDetected is emitted when a plaintext credential is found. The
	// template is rejected on both export and import; secrets are never
	// silently stripped.
	CodeSecretDetected ErrorCode = "SECRET_DETECTED"
)

// Error is one collected validation problem. Path is a dotted/indices location
// such as "spec.agent.mcp_servers[0].config_skeleton.token".
type Error struct {
	Code    ErrorCode `json:"code"`
	Path    string    `json:"path,omitempty"`
	Message string    `json:"message"`
}

// Warning is a non-blocking observation (e.g. a config that is portable but
// behaves differently in a fresh workspace). Warnings never flip Valid.
type Warning struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// Report aggregates every problem found in a single validation pass. Validate
// never fails fast: it collects all errors and warnings so the frontend / CLI
// can render them together.
type Report struct {
	Errors   []Error   `json:"errors,omitempty"`
	Warnings []Warning `json:"warnings,omitempty"`
}

// AddError appends a structured error.
func (r *Report) AddError(code ErrorCode, path, message string) {
	r.Errors = append(r.Errors, Error{Code: code, Path: path, Message: message})
}

// AddWarning appends a non-blocking warning.
func (r *Report) AddWarning(code, path, message string) {
	r.Warnings = append(r.Warnings, Warning{Code: code, Path: path, Message: message})
}

// HasErrors reports whether the pass collected any blocking error.
func (r *Report) HasErrors() bool { return len(r.Errors) > 0 }
