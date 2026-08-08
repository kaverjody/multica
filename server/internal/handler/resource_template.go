package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/multica-ai/multica/server/internal/agenttmpl"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/resourcetmpl"
	"github.com/multica-ai/multica/server/internal/util"
	"github.com/multica-ai/multica/server/pkg/agent"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// resource_template.go implements the CLO-245 template capability's three
// HTTP endpoints:
//
//	POST /api/templates/export   — export an agent/squad the caller may read
//	                              as a portable, redacted JSON template.
//	POST /api/templates/validate — validate a template against a target
//	                              workspace/runtime, collecting every problem.
//	POST /api/templates/apply    — materialise a validated template as new
//	                              agent(s)/squad, atomically and idempotently.
//
// The wire types, structural validation and secret detection live in
// internal/resourcetmpl (CLO-246); this file supplies the HTTP/DB layer only.

// Stable error codes beyond the structural set defined by resourcetmpl.
// These literals are part of the API contract (see CLO-248 issue body).
const (
	CodeRuntimeNotFound      = "RUNTIME_NOT_FOUND"
	CodeForbidden            = "FORBIDDEN"
	CodeConfigNotAllowed     = "CONFIG_NOT_ALLOWED"
	CodeDependencyNotFound   = "DEPENDENCY_NOT_FOUND"
	CodeNameConflict         = "NAME_CONFLICT"
	CodeApplyRolledBack      = "APPLY_ROLLED_BACK"
	CodeRequiredInputMissing = "REQUIRED_INPUT_MISSING"
	CodeSecretDetectedHTTP   = "SECRET_DETECTED"
)

// templateIDNamespace is the fixed UUID namespace used to derive a
// deterministic template_id from (workspace, kind, resource). Re-exporting
// the same resource always yields the same template_id (PMO ruling on
// CLO-253 Q5), while remaining unique per workspace/kind/resource.
var templateIDNamespace = uuid.MustParse("c0a7b6a1-3f8e-4f1a-9c2d-5b6e7a8f9a01")

// maxInstallableSkillSources bounds how many distinct skill URLs one apply
// may fetch, mirroring the import-size hygiene of ImportSkill.
const maxInstallableSkillSources = 16

// ---------------------------------------------------------------------------
// Export (CLO-247)
// ---------------------------------------------------------------------------

// ExportResourceTemplateRequest is the POST /api/templates/export body.
// resource_id may be a UUID or the resource's name within the workspace.
type ExportResourceTemplateRequest struct {
	Kind        string               `json:"kind"`
	ResourceID  string               `json:"resource_id"`
	MembersMode string               `json:"members_mode"`
	Metadata    *ExportMetadataInput `json:"metadata,omitempty"`
}

// ExportMetadataInput lets the caller override the exported metadata.
// Defaults: version "1.0.0", author = current user, source_workspace =
// current workspace, created_at = server time.
type ExportMetadataInput struct {
	Version     string   `json:"version,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Description string   `json:"description,omitempty"`
}

// ExportResourceTemplateResponse is the 200 body.
type ExportResourceTemplateResponse struct {
	Template resourcetmpl.Template  `json:"template"`
	Warnings []resourcetmpl.Warning `json:"warnings,omitempty"`
}

// ExportResourceTemplate handles POST /api/templates/export. Permission
// model (see CLO-247): the caller must be a workspace member and a human
// actor; for agents they must be able to read the config (owner/admin, the
// same semantics as canViewAgentSecrets); for squads additionally the
// leader and every agent member must be readable — otherwise 403 with the
// missing dependencies listed and no partial template produced.
func (h *Handler) ExportResourceTemplate(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	// Agent-actor tokens may never read resource configs through this
	// endpoint, mirroring the agent_env.go actor gate.
	actorType, _ := h.resolveActor(r, userID, workspaceID)
	if actorType == "agent" {
		writeError(w, http.StatusForbidden, "agents may not export resource templates")
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	var req ExportResourceTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Kind != resourcetmpl.KindAgent && req.Kind != resourcetmpl.KindSquad {
		writeError(w, http.StatusBadRequest, `kind must be "agent" or "squad"`)
		return
	}
	if req.ResourceID == "" {
		writeError(w, http.StatusBadRequest, "resource_id is required")
		return
	}
	membersMode := req.MembersMode
	if membersMode == "" {
		membersMode = resourcetmpl.MembersEmbedded
	}
	if membersMode != resourcetmpl.MembersEmbedded && membersMode != resourcetmpl.MembersReferences {
		writeError(w, http.StatusBadRequest, `members_mode must be "embedded" or "references"`)
		return
	}

	var tmpl resourcetmpl.Template
	var warnings []resourcetmpl.Warning

	switch req.Kind {
	case resourcetmpl.KindAgent:
		agentRow, err := h.loadAgentByIDOrName(r.Context(), wsUUID, req.ResourceID)
		if err != nil {
			writeError(w, http.StatusNotFound, "resource not found")
			return
		}
		if !canViewAgentSecrets(agentRow, userID, member.Role) {
			writeError(w, http.StatusForbidden, "you do not have permission to export this resource")
			return
		}
		spec, err := h.buildAgentSpec(r.Context(), agentRow)
		if err != nil {
			slog.Error("template export: build agent spec failed",
				append(logger.RequestAttrs(r), "agent_id", uuidToString(agentRow.ID), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to export resource")
			return
		}
		tmpl = resourcetmpl.Template{
			SchemaVersion: resourcetmpl.SchemaVersion,
			TemplateID:    deterministicTemplateID(uuidToString(wsUUID), req.Kind, uuidToString(agentRow.ID)),
			Kind:          resourcetmpl.KindAgent,
			Spec:          resourcetmpl.Spec{Agent: &spec},
		}
	case resourcetmpl.KindSquad:
		squad, err := h.loadSquadByIDOrName(r.Context(), wsUUID, req.ResourceID)
		if err != nil {
			writeError(w, http.StatusNotFound, "resource not found")
			return
		}
		spec, missing, warns, err := h.buildSquadSpec(r.Context(), wsUUID, squad, membersMode)
		if err != nil {
			slog.Error("template export: build squad spec failed",
				append(logger.RequestAttrs(r), "squad_id", uuidToString(squad.ID), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to export resource")
			return
		}
		// Read-permission on the leader AND every agent member: any single
		// unreadable dependency rejects the whole export (never partial).
		var unreadable []string
		for _, dep := range missing {
			if !canViewAgentSecrets(dep.agent, userID, member.Role) {
				unreadable = append(unreadable, dep.name)
			}
		}
		if len(unreadable) > 0 {
			sort.Strings(unreadable)
			writeJSON(w, http.StatusForbidden, map[string]any{
				"error":                "you do not have permission to export this squad",
				"missing_dependencies": unreadable,
			})
			return
		}
		warnings = warns
		tmpl = resourcetmpl.Template{
			SchemaVersion: resourcetmpl.SchemaVersion,
			TemplateID:    deterministicTemplateID(uuidToString(wsUUID), req.Kind, uuidToString(squad.ID)),
			Kind:          resourcetmpl.KindSquad,
			Spec:          resourcetmpl.Spec{Squad: &spec},
		}
	}

	// Metadata defaults: version from the request or 1.0.0, author = caller,
	// source_workspace = current workspace, created_at = server time.
	version := "1.0.0"
	if req.Metadata != nil && req.Metadata.Version != "" {
		version = req.Metadata.Version
	}
	desc := ""
	if req.Metadata != nil {
		desc = req.Metadata.Description
	}
	if desc == "" {
		switch req.Kind {
		case resourcetmpl.KindAgent:
			desc = tmpl.Spec.Agent.Description
		case resourcetmpl.KindSquad:
			desc = tmpl.Spec.Squad.Description
		}
	}
	authorDisplayName := userID
	if user, err := h.Queries.GetUser(r.Context(), parseUUID(userID)); err == nil {
		authorDisplayName = user.Name
	}
	tmpl.Metadata = resourcetmpl.Metadata{
		Name:            tmplName(tmpl),
		Description:     desc,
		Author:          resourcetmpl.Author{ID: userID, DisplayName: authorDisplayName},
		Version:         version,
		Visibility:      resourcetmpl.VisibilityWorkspace,
		SourceWorkspace: uuidToString(wsUUID),
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
	}
	if req.Metadata != nil {
		tmpl.Metadata.Tags = req.Metadata.Tags
	}

	// Belt-and-suspenders: run the secret scanner over the finished template
	// (instructions can carry credentials even after structural redaction).
	raw, err := json.Marshal(tmpl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode template")
		return
	}
	if secrets := resourcetmpl.DetectSecrets(raw); len(secrets) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":   CodeSecretDetectedHTTP,
			"message": "template contains plaintext secrets: " + secrets[0].Message,
		})
		return
	}

	// Audit trail: actor, kind, resource, template_id, version — never
	// instructions or env values. Export is read-only and carries no
	// plaintext secrets, so an audit-write failure is logged, not fatal.
	details, _ := json.Marshal(map[string]any{
		"kind":        req.Kind,
		"resource_id": req.ResourceID,
		"template_id": tmpl.TemplateID,
		"version":     tmpl.Metadata.Version,
	})
	if _, err := h.Queries.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: wsUUID,
		IssueID:     pgtype.UUID{},
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     parseUUID(userID),
		Action:      "template_exported",
		Details:     details,
	}); err != nil {
		slog.Warn("template export: activity_log write failed",
			append(logger.RequestAttrs(r), "error", err)...)
	}

	writeJSON(w, http.StatusOK, ExportResourceTemplateResponse{Template: tmpl, Warnings: warnings})
}

// tmplName returns the metadata name for a finished template.
func tmplName(t resourcetmpl.Template) string {
	if t.Spec.Agent != nil {
		return t.Spec.Agent.Name
	}
	if t.Spec.Squad != nil {
		return t.Spec.Squad.Name
	}
	return ""
}

// deterministicTemplateID derives a stable template_id for a resource so
// re-exports of the same resource produce the same template document id.
func deterministicTemplateID(workspaceID, kind, resourceID string) string {
	return uuid.NewSHA1(templateIDNamespace, []byte(workspaceID+"|"+kind+"|"+resourceID)).String()
}

// loadAgentByIDOrName resolves a resource identifier that may be a pure UUID
// or an agent name within the workspace.
func (h *Handler) loadAgentByIDOrName(ctx context.Context, wsUUID pgtype.UUID, id string) (db.Agent, error) {
	if agUUID, err := util.ParseUUID(id); err == nil {
		return h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agUUID, WorkspaceID: wsUUID})
	}
	agents, err := h.Queries.ListAllAgents(ctx, wsUUID)
	if err != nil {
		return db.Agent{}, err
	}
	for _, a := range agents {
		if a.Name == id {
			return a, nil
		}
	}
	return db.Agent{}, pgx.ErrNoRows
}

// loadSquadByIDOrName resolves a squad identifier that may be a pure UUID or
// a squad name within the workspace.
func (h *Handler) loadSquadByIDOrName(ctx context.Context, wsUUID pgtype.UUID, id string) (db.Squad, error) {
	if sqUUID, err := util.ParseUUID(id); err == nil {
		return h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{ID: sqUUID, WorkspaceID: wsUUID})
	}
	squads, err := h.Queries.ListAllSquads(ctx, wsUUID)
	if err != nil {
		return db.Squad{}, err
	}
	for _, s := range squads {
		if s.Name == id {
			return s, nil
		}
	}
	return db.Squad{}, pgx.ErrNoRows
}

// buildAgentSpec converts a stored agent row into its portable AgentSpec:
// values are never exported for custom_env (only key names), mcp_config is
// reduced to a stripped skeleton, runtime/owner/invocation details are
// compressed to portable fields only.
func (h *Handler) buildAgentSpec(ctx context.Context, a db.Agent) (resourcetmpl.AgentSpec, error) {
	spec := resourcetmpl.AgentSpec{
		Name:               a.Name,
		Description:        a.Description,
		Instructions:       a.Instructions,
		Model:              a.Model.String,
		ThinkingLevel:      a.ThinkingLevel.String,
		ServiceTier:        a.ServiceTier.String,
		MaxConcurrentTasks: int(a.MaxConcurrentTasks),
		PermissionMode:     a.PermissionMode,
	}
	if a.CustomArgs != nil {
		if err := json.Unmarshal(a.CustomArgs, &spec.CustomArgs); err != nil {
			slog.Warn("template export: unmarshal custom_args failed",
				"agent_id", uuidToString(a.ID), "error", err)
		}
	}
	// custom_env: key names only, sorted for stable output, values never.
	if a.CustomEnv != nil {
		var env map[string]string
		if err := json.Unmarshal(a.CustomEnv, &env); err == nil {
			keys := make([]string, 0, len(env))
			for k := range env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				spec.CustomEnvKeys = append(spec.CustomEnvKeys, resourcetmpl.CustomEnvKey{Key: k})
			}
		}
	}
	// invocation_targets compress to permission_mode + public_to_workspace;
	// member-class targets are not portable and are dropped.
	if a.PermissionMode == resourcetmpl.PermissionPublicTo {
		if targets, err := h.Queries.ListAgentInvocationTargets(ctx, a.ID); err == nil {
			for _, t := range targets {
				if t.TargetType == invocationTargetWorkspace {
					spec.PublicToWorkspace = true
					break
				}
			}
		}
	}
	// Skills: name + enabled only (no portable URL exists in the DB model).
	if rows, err := h.Queries.ListAgentSkillSummaries(ctx, a.ID); err == nil {
		for _, s := range rows {
			spec.Skills = append(spec.Skills, resourcetmpl.SkillRef{Name: s.Name, Enabled: s.Enabled})
		}
	}
	// mcp_config: structural skeleton only, auth material stripped.
	spec.MCPServers = mcpServersFromConfig(a.McpConfig)
	return spec, nil
}

// mcpAuthishKeys are the keys stripped from an MCP server entry when
// building the portable skeleton. Values under these keys are credentials or
// per-host state and must never cross into a template.
var mcpAuthishKeys = map[string]bool{
	"token": true, "tokens": true, "auth": true, "authorization": true,
	"headers": true, "env": true, "api_key": true, "apikey": true,
	"access_token": true, "refresh_token": true, "client_secret": true,
	"credential": true, "credentials": true, "password": true, "passwd": true,
	"private_key": true, "bearer": true, "secret": true, "secrets": true,
}

// mcpServersFromConfig parses the free-form agent.mcp_config JSONB into the
// portable mcp_servers skeleton. It tolerates the shapes observed in the
// wild: {"servers": {name: cfg}}, {"mcpServers": {name: cfg}}, array forms,
// and a bare map of server name → cfg. Non-parseable configs yield no
// servers (the caller's own DetectSecrets pass still covers instructions).
func mcpServersFromConfig(raw []byte) []resourcetmpl.MCPServer {
	if len(raw) == 0 {
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil
	}
	build := func(name string, cfg map[string]json.RawMessage) resourcetmpl.MCPServer {
		out := resourcetmpl.MCPServer{Name: name}
		transport := rawString(cfg["type"])
		if transport == "" {
			transport = rawString(cfg["transport"])
		}
		if transport == "" {
			transport = "stdio"
		}
		out.Transport = transport
		skeleton := make(map[string]json.RawMessage, len(cfg))
		for k, v := range cfg {
			if mcpAuthishKeys[k] {
				out.RequiresAuth = true
				continue
			}
			skeleton[k] = v
		}
		if len(skeleton) > 0 {
			if enc, err := json.Marshal(skeleton); err == nil {
				out.ConfigSkeleton = enc
			}
		}
		return out
	}
	servers := []resourcetmpl.MCPServer{}
	fromMap := func(m map[string]json.RawMessage) {
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			var cfg map[string]json.RawMessage
			if err := json.Unmarshal(m[name], &cfg); err != nil {
				continue
			}
			servers = append(servers, build(name, cfg))
		}
	}
	for _, wrapper := range []string{"servers", "mcpServers"} {
		entry, ok := root[wrapper]
		if !ok {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(entry, &obj); err == nil {
			fromMap(obj)
			return servers
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(entry, &arr); err == nil {
			for _, el := range arr {
				var cfg map[string]json.RawMessage
				if err := json.Unmarshal(el, &cfg); err != nil {
					continue
				}
				name := rawString(cfg["name"])
				if name == "" {
					continue
				}
				delete(cfg, "name")
				servers = append(servers, build(name, cfg))
			}
			return servers
		}
	}
	// Bare map of server name → cfg.
	if !mapContainsAny(root, []string{"servers", "mcpServers", "name", "transport", "command"}) {
		fromMap(root)
	}
	return servers
}

func mapContainsAny(m map[string]json.RawMessage, keys []string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

func rawString(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return ""
	}
	return s
}

// squadDependency pairs a member agent with a human-readable name for
// permission-error reporting.
type squadDependency struct {
	agent db.Agent
	name  string
}

// buildSquadSpec converts a stored squad into its portable SquadSpec.
// Human members are dropped with MEMBER_HUMAN_DROPPED warnings; agent
// members are emitted as embedded AgentSpecs or ref-only entries per
// membersMode. The leader always appears with role "leader" and its ref
// becomes leader_ref.
func (h *Handler) buildSquadSpec(ctx context.Context, wsUUID pgtype.UUID, squad db.Squad, membersMode string) (resourcetmpl.SquadSpec, []squadDependency, []resourcetmpl.Warning, error) {
	spec := resourcetmpl.SquadSpec{
		Name:         squad.Name,
		Description:  squad.Description,
		Instructions: squad.Instructions,
		MembersMode:  membersMode,
	}
	var deps []squadDependency
	var warnings []resourcetmpl.Warning

	memberRows, err := h.Queries.ListSquadMembers(ctx, squad.ID)
	if err != nil {
		return spec, nil, nil, err
	}

	// Load every agent member; the leader is included even if a member row
	// is missing (data anomaly), because a squad template must always carry
	// its leader (leader_ref validity).
	type memberEntry struct {
		role  string
		agent db.Agent
	}
	entries := []memberEntry{}
	seenAgents := map[string]bool{}
	for i, row := range memberRows {
		path := fmt.Sprintf("spec.squad.members[%d]", i)
		if row.MemberType != "agent" {
			warnings = append(warnings, resourcetmpl.Warning{
				Code:    "MEMBER_HUMAN_DROPPED",
				Path:    path,
				Message: "human members are not portable and were dropped",
			})
			continue
		}
		memberAgent, err := h.Queries.GetAgent(ctx, row.MemberID)
		if err != nil {
			return spec, nil, nil, err
		}
		seenAgents[uuidToString(row.MemberID)] = true
		role := row.Role
		if uuidToString(row.MemberID) == uuidToString(squad.LeaderID) {
			role = resourcetmpl.RoleLeader
		}
		// CLO-418: legacy rows may carry an empty role (members added before
		// role validation). A template with an empty member role would fail
		// validation on import, so fall back to a default and warn instead of
		// emitting an invalid template.
		if strings.TrimSpace(role) == "" {
			role = resourcetmpl.RoleMember
			warnings = append(warnings, resourcetmpl.Warning{
				Code:    "MEMBER_ROLE_DEFAULTED",
				Path:    path,
				Message: "member role was empty and was defaulted to " + resourcetmpl.RoleMember,
			})
		}
		entries = append(entries, memberEntry{role: role, agent: memberAgent})
		deps = append(deps, squadDependency{agent: memberAgent, name: memberAgent.Name})
	}
	if !seenAgents[uuidToString(squad.LeaderID)] {
		leader, err := h.Queries.GetAgent(ctx, squad.LeaderID)
		if err != nil {
			return spec, nil, nil, err
		}
		entries = append(entries, memberEntry{role: resourcetmpl.RoleLeader, agent: leader})
		deps = append(deps, squadDependency{agent: leader, name: leader.Name})
		warnings = append(warnings, resourcetmpl.Warning{
			Code:    "SQUAD_LEADER_NOT_IN_MEMBERS",
			Path:    "spec.squad.leader_ref",
			Message: "the squad leader was not recorded as a member; it was added with role leader",
		})
	}

	// Template-internal refs: slugified agent names, deduped with numeric
	// suffixes so every ref is unique within the template.
	refs := map[string]int{}
	refFor := func(name string) string {
		base := slugifyRef(name)
		if base == "" {
			base = "member"
		}
		n := refs[base]
		refs[base] = n + 1
		if n == 0 {
			return base
		}
		return fmt.Sprintf("%s-%d", base, n+1)
	}

	leaderRef := ""
	for _, entry := range entries {
		ref := refFor(entry.agent.Name)
		member := resourcetmpl.MemberRef{Ref: ref, Role: entry.role}
		if entry.role == resourcetmpl.RoleLeader && leaderRef == "" {
			leaderRef = ref
		}
		if membersMode == resourcetmpl.MembersEmbedded {
			as, err := h.buildAgentSpec(ctx, entry.agent)
			if err != nil {
				return spec, nil, nil, err
			}
			member.Agent = &as
		}
		spec.Members = append(spec.Members, member)
	}
	spec.LeaderRef = leaderRef
	return spec, deps, warnings, nil
}

// slugifyRef converts an agent name into a template-internal ref symbol:
// lowercase ASCII/digits, runs of other characters become single dashes,
// trimmed. Non-portable characters never appear in a ref.
func slugifyRef(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case ok:
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// ---------------------------------------------------------------------------
// Shared validation (CLO-248)
// ---------------------------------------------------------------------------

// RequiredInputs lists everything the caller must supply before an apply
// can materialise the template.
type RequiredInputs struct {
	EnvKeys       []EnvKeyInput       `json:"env_keys,omitempty"`
	MissingSkills []MissingSkillInput `json:"missing_skills,omitempty"`
	MCPServers    []MCPServerInput    `json:"mcp_servers,omitempty"`
	MissingAgents []MissingAgentInput `json:"missing_agents,omitempty"`
}

type EnvKeyInput struct {
	AgentRef string `json:"agent_ref"`
	Key      string `json:"key"`
}

type MissingSkillInput struct {
	Name        string `json:"name"`
	SourceURL   string `json:"source_url"`
	Installable bool   `json:"installable"`
}

type MCPServerInput struct {
	AgentRef string `json:"agent_ref"`
	Name     string `json:"name"`
}

type MissingAgentInput struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

// ApplyPlan previews what an apply would create and which names collide.
type ApplyPlan struct {
	AgentsToCreate []string       `json:"agents_to_create,omitempty"`
	SquadsToCreate []string       `json:"squads_to_create,omitempty"`
	Conflicts      []NameConflict `json:"conflicts,omitempty"`
}

type NameConflict struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	ExistingID string `json:"existing_id"`
}

// ValidateResourceTemplateRequest is the POST /api/templates/validate body.
type ValidateResourceTemplateRequest struct {
	Template        json.RawMessage `json:"template"`
	TargetRuntimeID string          `json:"target_runtime_id"`
	MembersMode     string          `json:"members_mode"`
}

// ValidateResourceTemplateResponse is the 200 body — validation failures are
// reported in the body, not as HTTP errors (only a broken request body 4xx).
type ValidateResourceTemplateResponse struct {
	Valid          bool                   `json:"valid"`
	Errors         []resourcetmpl.Error   `json:"errors,omitempty"`
	Warnings       []resourcetmpl.Warning `json:"warnings,omitempty"`
	RequiredInputs RequiredInputs         `json:"required_inputs"`
	Plan           ApplyPlan              `json:"plan"`
}

// templateValidation accumulates the outcome of one validate pass.
type templateValidation struct {
	tmpl     resourcetmpl.Template
	runtime  *db.AgentRuntime
	report   *resourcetmpl.Report
	required RequiredInputs
	plan     ApplyPlan
}

func (v *templateValidation) addError(code resourcetmpl.ErrorCode, path, message string) {
	v.report.AddError(code, path, message)
}

func (v *templateValidation) addWarning(code, path, message string) {
	v.report.AddWarning(code, path, message)
}

func (v *templateValidation) hasErrors() bool { return v.report.HasErrors() }

// ValidateResourceTemplate handles POST /api/templates/validate. It always
// answers 200 unless the request body itself is unreadable; all validation
// problems — structural, runtime, permission, dependency, conflict — are
// collected in one pass (no fail-fast).
func (h *Handler) ValidateResourceTemplate(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	var req ValidateResourceTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Template) == 0 {
		writeError(w, http.StatusBadRequest, "template is required")
		return
	}

	v := h.validateResourceTemplate(r.Context(), wsUUID, member, req.Template, req.TargetRuntimeID, req.MembersMode)
	writeJSON(w, http.StatusOK, ValidateResourceTemplateResponse{
		Valid:          !v.hasErrors(),
		Errors:         v.report.Errors,
		Warnings:       v.report.Warnings,
		RequiredInputs: v.required,
		Plan:           v.plan,
	})
}

// validateResourceTemplate runs the shared validation pipeline used by both
// the validate endpoint and the apply endpoint (apply re-runs it as its
// TOCTOU guard; the only extra checks in apply are env/skill-input related).
func (h *Handler) validateResourceTemplate(ctx context.Context, wsUUID pgtype.UUID, member db.Member, raw []byte, targetRuntimeID, membersMode string) *templateValidation {
	v := &templateValidation{
		report:   resourcetmpl.Validate(raw),
		required: RequiredInputs{},
		plan:     ApplyPlan{},
	}
	if err := json.Unmarshal(raw, &v.tmpl); err != nil {
		// Structural errors already reported by resourcetmpl.Validate.
		return v
	}

	// Target runtime: required, must exist in the workspace and be usable by
	// the caller (Q3 ruling: missing or inaccessible → RUNTIME_NOT_FOUND).
	if strings.TrimSpace(targetRuntimeID) == "" {
		v.addError(resourcetmpl.CodeTemplateInvalid, "target_runtime_id", "target_runtime_id is required")
	} else {
		rtUUID, err := util.ParseUUID(targetRuntimeID)
		if err != nil {
			v.addError(resourcetmpl.ErrorCode(CodeRuntimeNotFound), "target_runtime_id", "target_runtime_id must be a uuid")
		} else {
			rt, err := h.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
				ID: rtUUID, WorkspaceID: wsUUID,
			})
			if err != nil {
				v.addError(resourcetmpl.ErrorCode(CodeRuntimeNotFound), "target_runtime_id", "target runtime not found in this workspace")
			} else {
				v.runtime = &rt
				if !canUseRuntimeForAgent(member, rt) {
					v.addError(resourcetmpl.ErrorCode(CodeForbidden), "target_runtime_id", "you cannot use this runtime for creating agents")
				}
			}
		}
	}

	// Semantic checks need a structurally valid template.
	if v.hasErrors() {
		return v
	}

	switch v.tmpl.Kind {
	case resourcetmpl.KindAgent:
		spec := v.tmpl.Spec.Agent
		if spec == nil {
			return v
		}
		h.validateAgentSemantic(ctx, v, wsUUID, member, spec, "spec.agent", spec.Name, true)
		v.plan.AgentsToCreate = append(v.plan.AgentsToCreate, spec.Name)
	case resourcetmpl.KindSquad:
		spec := v.tmpl.Spec.Squad
		if spec == nil {
			return v
		}
		effective, err := effectiveMembersMode(spec.MembersMode, membersMode)
		if err != nil {
			v.addError(resourcetmpl.CodeTemplateInvalid, "spec.squad.members_mode", err.Error())
			return v
		}
		h.validateSquadSemantic(ctx, v, wsUUID, member, spec, effective)
	default:
		// Unknown kind already reported by resourcetmpl.Validate.
	}
	return v
}

// effectiveMembersMode applies the request-level members_mode override to
// the template's own members_mode (PMO ruling on CLO-253 Q3): an embedded
// template may be applied in references mode, but a references template may
// never be applied in embedded mode.
func effectiveMembersMode(templateMode, requestMode string) (string, error) {
	if requestMode == "" || requestMode == templateMode {
		return templateMode, nil
	}
	if templateMode == resourcetmpl.MembersEmbedded && requestMode == resourcetmpl.MembersReferences {
		return resourcetmpl.MembersReferences, nil
	}
	if templateMode == resourcetmpl.MembersReferences && requestMode == resourcetmpl.MembersEmbedded {
		return "", errors.New("references templates cannot be applied in embedded mode; re-export with members_mode=embedded")
	}
	return requestMode, nil
}

// validateAgentSemantic runs the workspace-aware checks for one AgentSpec:
// runtime-level thinking_level/service_tier re-validation, name-conflict
// detection, skill resolution, required env keys and MCP allowlist checks.
func (h *Handler) validateAgentSemantic(ctx context.Context, v *templateValidation, wsUUID pgtype.UUID, member db.Member, spec *resourcetmpl.AgentSpec, base, agentRef string, checkConflict bool) {
	if v.runtime != nil {
		if spec.ThinkingLevel != "" && !agent.IsKnownThinkingValue(v.runtime.Provider, spec.ThinkingLevel) {
			v.addError(resourcetmpl.CodeTemplateInvalid, base+".thinking_level",
				fmt.Sprintf("thinking_level %q is not a recognised value for runtime %q; set it empty to use the runtime default",
					spec.ThinkingLevel, v.runtime.Provider))
		}
		if spec.ServiceTier != "" && !agent.IsKnownServiceTier(v.runtime.Provider, spec.ServiceTier) {
			v.addError(resourcetmpl.CodeTemplateInvalid, base+".service_tier",
				fmt.Sprintf("service_tier %q is not a recognised value for runtime %q; set it empty to use the runtime default",
					spec.ServiceTier, v.runtime.Provider))
		}
	}
	if checkConflict {
		if existing, err := h.findAgentByName(ctx, wsUUID, spec.Name); err == nil {
			v.plan.Conflicts = append(v.plan.Conflicts, NameConflict{
				Kind: "agent", Name: spec.Name, ExistingID: uuidToString(existing.ID),
			})
		}
	}
	for _, sk := range spec.Skills {
		if _, err := h.Queries.GetSkillByWorkspaceAndName(ctx, db.GetSkillByWorkspaceAndNameParams{
			WorkspaceID: wsUUID, Name: sk.Name,
		}); errors.Is(err, pgx.ErrNoRows) {
			v.required.MissingSkills = append(v.required.MissingSkills, MissingSkillInput{
				Name: sk.Name, SourceURL: sk.SourceURL, Installable: sk.SourceURL != "",
			})
		}
	}
	for _, ek := range spec.CustomEnvKeys {
		if ek.Required {
			v.required.EnvKeys = append(v.required.EnvKeys, EnvKeyInput{AgentRef: agentRef, Key: ek.Key})
		}
	}
	for i, m := range spec.MCPServers {
		mp := fmt.Sprintf("%s.mcp_servers[%d]", base, i)
		if m.Transport != "" && m.Transport != "stdio" {
			v.addError(resourcetmpl.ErrorCode(CodeConfigNotAllowed), mp,
				fmt.Sprintf("mcp server transport %q is not allowed; only stdio servers are portable", m.Transport))
		}
		if len(m.ConfigSkeleton) > 0 {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(m.ConfigSkeleton, &obj); err != nil {
				v.addError(resourcetmpl.ErrorCode(CodeConfigNotAllowed), mp+".config_skeleton",
					"config_skeleton must be a JSON object")
			}
		}
		v.required.MCPServers = append(v.required.MCPServers, MCPServerInput{AgentRef: agentRef, Name: m.Name})
	}
}

// validateSquadSemantic runs the workspace-aware checks for a SquadSpec in
// the effective members mode.
func (h *Handler) validateSquadSemantic(ctx context.Context, v *templateValidation, wsUUID pgtype.UUID, member db.Member, spec *resourcetmpl.SquadSpec, effective string) {
	if existing, err := h.findSquadByName(ctx, wsUUID, spec.Name); err == nil {
		v.plan.Conflicts = append(v.plan.Conflicts, NameConflict{
			Kind: "squad", Name: spec.Name, ExistingID: uuidToString(existing.ID),
		})
	}
	v.plan.SquadsToCreate = append(v.plan.SquadsToCreate, spec.Name)

	switch effective {
	case resourcetmpl.MembersEmbedded:
		seenNames := map[string]string{}
		for i, mem := range spec.Members {
			mp := fmt.Sprintf("spec.squad.members[%d]", i)
			name := mem.Agent.Name
			if prevRef, dup := seenNames[name]; dup {
				v.addError(resourcetmpl.CodeTemplateInvalid, mp+".agent.name",
					fmt.Sprintf("duplicate member agent name %q (also used by member ref %q); names must be unique within a template", name, prevRef))
				continue
			}
			seenNames[name] = mem.Ref
			h.validateAgentSemantic(ctx, v, wsUUID, member, mem.Agent, mp+".agent", mem.Ref, true)
			v.plan.AgentsToCreate = append(v.plan.AgentsToCreate, name)
		}
	case resourcetmpl.MembersReferences:
		for i, mem := range spec.Members {
			mp := fmt.Sprintf("spec.squad.members[%d]", i)
			refName := mem.Ref
			resolved, err := h.findAgentByName(ctx, wsUUID, refName)
			if err != nil {
				v.addError(resourcetmpl.ErrorCode(CodeDependencyNotFound), mp+".ref",
					fmt.Sprintf("references-mode member %q does not match any agent in the target workspace", refName))
				v.required.MissingAgents = append(v.required.MissingAgents, MissingAgentInput{Ref: refName, Name: refName})
				continue
			}
			if !h.memberCanWireAgent(ctx, member, resolved, uuidToString(wsUUID)) {
				v.addError(resourcetmpl.ErrorCode(CodeForbidden), mp+".ref",
					fmt.Sprintf("you cannot wire member %q into a squad", refName))
			}
		}
	}
}

// findAgentByName returns the first agent with the given name in the
// workspace (kind=user rows only, matching the agent create surface).
func (h *Handler) findAgentByName(ctx context.Context, wsUUID pgtype.UUID, name string) (db.Agent, error) {
	agents, err := h.Queries.ListAllAgents(ctx, wsUUID)
	if err != nil {
		return db.Agent{}, err
	}
	for _, a := range agents {
		if a.Name == name {
			return a, nil
		}
	}
	return db.Agent{}, pgx.ErrNoRows
}

// findSquadByName returns the squad with the given name in the workspace.
func (h *Handler) findSquadByName(ctx context.Context, wsUUID pgtype.UUID, name string) (db.Squad, error) {
	squads, err := h.Queries.ListAllSquads(ctx, wsUUID)
	if err != nil {
		return db.Squad{}, err
	}
	for _, s := range squads {
		if s.Name == name {
			return s, nil
		}
	}
	return db.Squad{}, pgx.ErrNoRows
}

// ---------------------------------------------------------------------------
// Apply (CLO-248)
// ---------------------------------------------------------------------------

// ApplyResourceTemplateRequest is the POST /api/templates/apply body.
type ApplyResourceTemplateRequest struct {
	Template             json.RawMessage              `json:"template"`
	TargetRuntimeID      string                       `json:"target_runtime_id"`
	MembersMode          string                       `json:"members_mode"`
	Overrides            *ApplyOverrides              `json:"overrides,omitempty"`
	Env                  map[string]map[string]string `json:"env,omitempty"`
	InstallMissingSkills []string                     `json:"install_missing_skills,omitempty"`
	ConflictPolicy       string                       `json:"conflict_policy,omitempty"`
	IdempotencyKey       string                       `json:"idempotency_key,omitempty"`
	DryRun               bool                         `json:"dry_run,omitempty"`
}

// ApplyOverrides tweaks the template before materialisation. Agents is keyed
// by the template's member ref (for kind=agent templates, by the agent name
// — the same single shape per the Q10 ruling).
type ApplyOverrides struct {
	Name           *string                   `json:"name,omitempty"`
	Description    *string                   `json:"description,omitempty"`
	Instructions   *string                   `json:"instructions,omitempty"`
	Model          *string                   `json:"model,omitempty"`
	PermissionMode *string                   `json:"permission_mode,omitempty"`
	Agents         map[string]AgentOverrides `json:"agents,omitempty"`
}

// AgentOverrides is the per-ref override block.
type AgentOverrides struct {
	Name           *string `json:"name,omitempty"`
	Description    *string `json:"description,omitempty"`
	Instructions   *string `json:"instructions,omitempty"`
	Model          *string `json:"model,omitempty"`
	PermissionMode *string `json:"permission_mode,omitempty"`
}

// ApplyResourceTemplateResponse is the 200 body for a successful apply and
// for validate-failure replies (applied=false, errors populated).
type ApplyResourceTemplateResponse struct {
	Applied          bool                   `json:"applied"`
	Valid            bool                   `json:"valid,omitempty"`
	DryRun           bool                   `json:"dry_run"`
	Errors           []resourcetmpl.Error   `json:"errors,omitempty"`
	Warnings         []resourcetmpl.Warning `json:"warnings,omitempty"`
	RequiredInputs   *RequiredInputs        `json:"required_inputs,omitempty"`
	Plan             *ApplyPlan             `json:"plan,omitempty"`
	Created          ApplyCreated           `json:"created"`
	ResourceMapping  map[string]string      `json:"resource_mapping"`
	Skipped          []SkippedResource      `json:"skipped,omitempty"`
	RolledBack       bool                   `json:"rolled_back"`
	IdempotentReplay bool                   `json:"idempotent_replay"`
}

// ApplyCreated lists the resources materialised by an apply (or planned, for
// dry runs).
type ApplyCreated struct {
	Agents []CreatedAgent `json:"agents,omitempty"`
	Squads []CreatedSquad `json:"squads,omitempty"`
	Skills []CreatedSkill `json:"skills,omitempty"`
}

type CreatedAgent struct {
	Ref  string `json:"ref"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

type CreatedSquad struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
}

type CreatedSkill struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Reused bool   `json:"reused"`
}

// SkippedResource records a resource the caller asked to skip due to a name
// conflict (conflict_policy=skip).
type SkippedResource struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// conflict policies (overwrite is deliberately unsupported).
const (
	conflictPolicyFail   = "fail"
	conflictPolicyRename = "rename"
	conflictPolicySkip   = "skip"
)

// plannedAgent is one agent an apply intends to create (embedded members) or
// reference (references mode / skip resolution).
type plannedAgent struct {
	ref       string
	baseName  string
	finalName string
	action    string // "create" | "skip"
	existing  *db.Agent
}

// applyDecision is the outcome of the conflict-resolution pass.
type applyDecision struct {
	agents  []plannedAgent
	squad   *plannedResource
	mapping map[string]string
	skipped []SkippedResource
}

// plannedResource is a squad an apply intends to create.
type plannedResource struct {
	baseName  string
	finalName string
	action    string // "create" | "skip"
	existing  *db.Squad
}

// ApplyResourceTemplate handles POST /api/templates/apply.
//
// Hard rules enforced here (see CLO-248 issue body):
//   - the template is fully validated first; any error aborts before writes;
//   - dry_run=true returns the plan and never writes;
//   - conflict_policy defaults to fail; overwrite is rejected outright;
//   - skill network fetches happen OUTSIDE the DB transaction, only for URLs
//     the caller explicitly listed in install_missing_skills;
//   - agents + squad + squad_member + skill bindings commit in one pgx
//     transaction; any failure rolls back and reports APPLY_ROLLED_BACK;
//   - idempotency_key replays the first successful result.
func (h *Handler) ApplyResourceTemplate(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	if workspaceID == "" {
		writeError(w, http.StatusBadRequest, "workspace_id is required")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	member, ok := h.workspaceMember(w, r, workspaceID)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	var req ApplyResourceTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Template) == 0 {
		writeError(w, http.StatusBadRequest, "template is required")
		return
	}
	policy := req.ConflictPolicy
	if policy == "" {
		policy = conflictPolicyFail
	}
	switch policy {
	case conflictPolicyFail, conflictPolicyRename, conflictPolicySkip:
	case "overwrite":
		writeError(w, http.StatusBadRequest, "conflict_policy \"overwrite\" is not supported")
		return
	default:
		writeError(w, http.StatusBadRequest, `conflict_policy must be "fail", "rename" or "skip"`)
		return
	}

	// Idempotent replay: same (workspace, key) → replay the stored first
	// result, before any validation or writes.
	if req.IdempotencyKey != "" {
		if replay, err := h.lookupApplyLog(r.Context(), wsUUID, req.IdempotencyKey); err == nil {
			replay.IdempotentReplay = true
			writeJSON(w, http.StatusOK, replay)
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("template apply: idempotency lookup failed",
				append(logger.RequestAttrs(r), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to check idempotency")
			return
		}
	}

	// Full validation pass (shared with /validate).
	v := h.validateResourceTemplate(r.Context(), wsUUID, member, req.Template, req.TargetRuntimeID, req.MembersMode)

	// Apply-only checks: required env keys must be supplied; missing skills
	// must be explicitly opted into via install_missing_skills.
	if !v.hasErrors() {
		for _, ek := range v.required.EnvKeys {
			if strings.TrimSpace(req.Env[ek.AgentRef][ek.Key]) == "" {
				v.addError(resourcetmpl.ErrorCode(CodeRequiredInputMissing), "env."+ek.AgentRef+"."+ek.Key,
					fmt.Sprintf("required env key %q is missing for agent ref %q", ek.Key, ek.AgentRef))
			}
		}
		installSet := stringSet(req.InstallMissingSkills)
		for _, ms := range v.required.MissingSkills {
			switch {
			case ms.Installable && !installSet[ms.SourceURL]:
				v.addError(resourcetmpl.ErrorCode(CodeDependencyNotFound), "spec.skills",
					fmt.Sprintf("skill %q is not present in the workspace; add %q to install_missing_skills to install it", ms.Name, ms.SourceURL))
			case !ms.Installable:
				v.addError(resourcetmpl.ErrorCode(CodeDependencyNotFound), "spec.skills",
					fmt.Sprintf("skill %q is not present in the workspace and has no source_url to install", ms.Name))
			}
		}
	}

	// Unknown env refs, unknown override refs and unknown install URLs are
	// warnings, not errors. Override permission modes are validated here so a
	// dry run reports them instead of a post-tx rollback.
	var applyWarnings []resourcetmpl.Warning
	if !v.hasErrors() {
		knownRefs := templateRefs(v.tmpl)
		for ref := range req.Env {
			if _, ok := knownRefs[ref]; !ok {
				applyWarnings = append(applyWarnings, resourcetmpl.Warning{
					Code: "ENV_REF_UNKNOWN", Path: "env",
					Message: fmt.Sprintf("env provided for unknown agent ref %q is ignored", ref),
				})
			}
		}
		if req.Overrides != nil {
			for ref := range req.Overrides.Agents {
				if _, ok := knownRefs[ref]; !ok {
					applyWarnings = append(applyWarnings, resourcetmpl.Warning{
						Code: "OVERRIDE_REF_UNKNOWN", Path: "overrides.agents",
						Message: fmt.Sprintf("overrides for unknown agent ref %q are ignored", ref),
					})
					continue
				}
				if pm := req.Overrides.Agents[ref].PermissionMode; pm != nil && *pm != "" &&
					*pm != resourcetmpl.PermissionPrivate && *pm != resourcetmpl.PermissionPublicTo {
					v.addError(resourcetmpl.CodeTemplateInvalid, "overrides.agents."+ref+".permission_mode",
						fmt.Sprintf("permission_mode %q must be %q or %q", *pm, resourcetmpl.PermissionPrivate, resourcetmpl.PermissionPublicTo))
				}
			}
			if req.Overrides.PermissionMode != nil && *req.Overrides.PermissionMode != "" {
				if v.tmpl.Spec.Squad != nil {
					// Architect ruling (CLO-245): model / permission_mode are
					// agent-level concepts. A squad has no runtime and is not a
					// permission subject — its members are. Rejecting here
					// (instead of silently ignoring or writing dead columns)
					// is what makes the top-level override contract honest:
					// CONFIG_NOT_ALLOWED is the same code the validate pass
					// uses for non-portable template config.
					v.addError(resourcetmpl.ErrorCode(CodeConfigNotAllowed), "overrides.permission_mode",
						"permission_mode is not allowed for squad templates; a squad's visibility is determined by its member agents")
				} else if *req.Overrides.PermissionMode != resourcetmpl.PermissionPrivate && *req.Overrides.PermissionMode != resourcetmpl.PermissionPublicTo {
					v.addError(resourcetmpl.CodeTemplateInvalid, "overrides.permission_mode",
						fmt.Sprintf("permission_mode %q must be %q or %q", *req.Overrides.PermissionMode, resourcetmpl.PermissionPrivate, resourcetmpl.PermissionPublicTo))
				}
			}
			if v.tmpl.Spec.Squad != nil && req.Overrides.Model != nil && *req.Overrides.Model != "" {
				v.addError(resourcetmpl.ErrorCode(CodeConfigNotAllowed), "overrides.model",
					"model is not allowed for squad templates; a squad has no runtime of its own (model lives on the member agents)")
			}
		}
		for _, url := range req.InstallMissingSkills {
			found := false
			for _, ms := range v.required.MissingSkills {
				if ms.SourceURL == url {
					found = true
					break
				}
			}
			if !found {
				applyWarnings = append(applyWarnings, resourcetmpl.Warning{
					Code: "INSTALL_URL_UNUSED", Path: "install_missing_skills",
					Message: fmt.Sprintf("source_url %q does not match any missing skill; nothing was installed from it", url),
				})
			}
		}
	}

	if v.hasErrors() {
		writeJSON(w, http.StatusOK, ApplyResourceTemplateResponse{
			Applied:         false,
			Valid:           false,
			DryRun:          req.DryRun,
			Errors:          v.report.Errors,
			Warnings:        append(v.report.Warnings, applyWarnings...),
			RequiredInputs:  &v.required,
			Plan:            &v.plan,
			Created:         ApplyCreated{},
			ResourceMapping: map[string]string{},
		})
		return
	}

	// Conflict resolution pass (fail → 409; rename/skip → adjusted plan).
	decision, err := h.resolveApplyConflicts(r.Context(), wsUUID, v, &req, policy)
	if err != nil {
		writeJSON(w, http.StatusConflict, ApplyResourceTemplateResponse{
			Applied:         false,
			Valid:           false,
			Errors:          []resourcetmpl.Error{{Code: resourcetmpl.ErrorCode(CodeNameConflict), Path: "spec", Message: err.Error()}},
			Plan:            &v.plan,
			Created:         ApplyCreated{},
			ResourceMapping: map[string]string{},
		})
		return
	}

	// Everything below here is inside the "valid + conflicts resolved" path.
	if req.DryRun {
		writeJSON(w, http.StatusOK, dryRunResponse(v, decision, append(v.report.Warnings, applyWarnings...)))
		return
	}

	// --- Skill fetch phase, strictly OUTSIDE the DB transaction ---
	// Only URLs the caller explicitly opted into are fetched; nothing is
	// executed, only imported as skill documents (same pipeline as
	// CreateAgentFromTemplate).
	toFetch := collectSkillsToInstall(v, &req)
	fetched := map[string]*importedSkill{}
	if len(toFetch) > 0 {
		if len(toFetch) > maxInstallableSkillSources {
			writeError(w, http.StatusBadRequest, "too many install_missing_skills sources")
			return
		}
		httpClient := &http.Client{Timeout: 30 * time.Second}
		fetchCtx, cancel := context.WithTimeout(r.Context(), importFetchTimeout)
		defer cancel()
		imports, failed := fetchTemplateSkillsParallel(fetchCtx, httpClient, toFetch)
		if len(failed) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, fetchFailureResponse{
				Error:      "one or more skill sources are unavailable",
				FailedURLs: failed,
			})
			return
		}
		for j, imp := range imports {
			if imp != nil {
				fetched[toFetch[j].SourceURL] = imp
			}
		}
	}

	// --- Transaction: skills + agents + squad + members + log + audit ---
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to begin tx: "+err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	result := ApplyResourceTemplateResponse{
		Applied:         true,
		Valid:           true,
		DryRun:          false,
		Warnings:        append(v.report.Warnings, applyWarnings...),
		Created:         ApplyCreated{},
		ResourceMapping: decision.mapping,
		Skipped:         decision.skipped,
	}

	creatorUUID := parseUUID(userID)

	// Skill materialisation inside the tx: fetched skills are created
	// (find-or-reuse by frontmatter name), already-present skills are reused.
	skillIDsByName := map[string]pgtype.UUID{}
	var createdSkills []CreatedSkill
	for _, src := range toFetch {
		imp := fetched[src.SourceURL]
		if imp == nil {
			continue
		}
		if id, ok := skillIDsByName[imp.name]; ok {
			_ = id
			continue
		}
		existing, err := qtx.GetSkillByWorkspaceAndName(r.Context(), db.GetSkillByWorkspaceAndNameParams{
			WorkspaceID: wsUUID, Name: imp.name,
		})
		if err == nil {
			skillIDsByName[imp.name] = existing.ID
			createdSkills = append(createdSkills, CreatedSkill{ID: uuidToString(existing.ID), Name: imp.name, Reused: true})
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
			return
		}
		files := make([]CreateSkillFileRequest, 0, len(imp.files))
		for _, f := range imp.files {
			if !validateFilePath(f.path) {
				continue
			}
			files = append(files, CreateSkillFileRequest{Path: f.path, Content: f.content})
		}
		origin := map[string]any{"type": "resource_template", "source_url": src.SourceURL}
		if imp.origin != nil {
			for k, val := range imp.origin {
				if _, exists := origin[k]; !exists {
					origin[k] = val
				}
			}
		}
		created, err := createSkillWithFilesInTx(r.Context(), qtx, skillCreateInput{
			WorkspaceID: wsUUID,
			CreatorID:   creatorUUID,
			Name:        imp.name,
			Description: imp.description,
			Content:     imp.content,
			Config:      map[string]any{"origin": origin},
			Files:       files,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
			return
		}
		skillIDsByName[imp.name] = parseUUID(created.ID)
		createdSkills = append(createdSkills, CreatedSkill{ID: created.ID, Name: imp.name, Reused: false})
	}

	// Reused workspace skills are reported on the created agent's binding
	// below; created.skills lists the skills this apply materialised.
	result.Created.Skills = createdSkills

	// Agents first (embedded members in template order), then the squad.
	agentIDsByRef := map[string]pgtype.UUID{}
	var createdAgents []CreatedAgent
	for i := range decision.agents {
		pa := &decision.agents[i]
		if pa.action == "skip" {
			agentIDsByRef[pa.ref] = pa.existing.ID
			result.ResourceMapping[pa.ref] = uuidToString(pa.existing.ID)
			continue
		}
		spec := agentSpecForRef(v, pa.ref)
		agentRow, err := h.createAgentFromSpecInTx(r.Context(), qtx, wsUUID, creatorUUID, v, pa.finalName, spec, &req, pa.ref)
		if err != nil {
			slog.Error("template apply: create agent failed",
				append(logger.RequestAttrs(r), "ref", pa.ref, "name", pa.finalName, "error", err)...)
			writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
			return
		}
		agentIDsByRef[pa.ref] = agentRow.ID
		// Rename mappings (ref → finalName) take precedence over the
		// ref → id mapping when both would sit under the same key; the id
		// is still reported in created.agents.
		if _, renamed := result.ResourceMapping[pa.ref]; !renamed {
			result.ResourceMapping[pa.ref] = uuidToString(agentRow.ID)
		}
		createdAgents = append(createdAgents, CreatedAgent{Ref: pa.ref, ID: uuidToString(agentRow.ID), Name: pa.finalName})
	}
	result.Created.Agents = createdAgents
	result.Created.Skills = createdSkills

	// Squad (agent templates have no squad step).
	if decision.squad != nil && decision.squad.action == "create" {
		spec := v.tmpl.Spec.Squad
		leaderRef := spec.LeaderRef
		leaderID, ok := agentIDsByRef[leaderRef]
		if !ok || leaderID == (pgtype.UUID{}) {
			writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(fmt.Errorf("leader ref %q did not resolve to a created agent", leaderRef)))
			return
		}
		// Squad overrides (CLO-250 DEF-4, architect ruling): top-level
		// overrides.{Description, Instructions} are materialised on the squad
		// row; name is consumed earlier by the conflict-resolution pass.
		// model / permission_mode were rejected for squad templates by the
		// apply-only checks above (CONFIG_NOT_ALLOWED), so no dead columns
		// are written here — the squad keeps its members' semantics.
		squadDescription := spec.Description
		squadInstructions := spec.Instructions
		if req.Overrides != nil {
			if req.Overrides.Description != nil {
				squadDescription = *req.Overrides.Description
			}
			if req.Overrides.Instructions != nil {
				squadInstructions = *req.Overrides.Instructions
			}
		}
		squad, err := qtx.CreateSquad(r.Context(), db.CreateSquadParams{
			WorkspaceID:  wsUUID,
			Name:         decision.squad.finalName,
			Description:  squadDescription,
			LeaderID:     leaderID,
			CreatorID:    member.UserID,
			AvatarUrl:    pgtype.Text{},
			Instructions: squadInstructions,
		})
		if err != nil {
			slog.Error("template apply: create squad failed",
				append(logger.RequestAttrs(r), "squad_name", decision.squad.finalName, "error", err)...)
			writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
			return
		}
		for _, mem := range spec.Members {
			memberID, ok := agentIDsByRef[mem.Ref]
			if !ok || memberID == (pgtype.UUID{}) {
				writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(fmt.Errorf("member ref %q did not resolve to an agent", mem.Ref)))
				return
			}
			if _, err := qtx.AddSquadMember(r.Context(), db.AddSquadMemberParams{
				SquadID:    squad.ID,
				MemberType: "agent",
				MemberID:   memberID,
				Role:       mem.Role,
			}); err != nil {
				slog.Error("template apply: add squad member failed",
					append(logger.RequestAttrs(r), "squad_id", uuidToString(squad.ID), "member_ref", mem.Ref, "error", err)...)
				writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
				return
			}
		}
		result.Created.Squads = append(result.Created.Squads, CreatedSquad{ID: uuidToString(squad.ID), Name: decision.squad.finalName})
	}

	// Idempotency log + audit inside the same tx.
	if req.IdempotencyKey != "" {
		resultJSON, err := json.Marshal(result)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
			return
		}
		if _, err := tx.Exec(r.Context(),
			`INSERT INTO template_apply_log (workspace_id, idempotency_key, template_id, template_version, result) VALUES ($1, $2, $3, $4, $5)`,
			wsUUID, req.IdempotencyKey, v.tmpl.TemplateID, v.tmpl.Metadata.Version, resultJSON); err != nil {
			slog.Error("template apply: idempotency log write failed; rolling back",
				append(logger.RequestAttrs(r), "error", err)...)
			writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
			return
		}
	}
	auditDetails, _ := json.Marshal(map[string]any{
		"template_id":        v.tmpl.TemplateID,
		"version":            v.tmpl.Metadata.Version,
		"kind":               v.tmpl.Kind,
		"idempotency_key":    req.IdempotencyKey,
		"created_agent_ids":  createdAgentIDs(result.Created.Agents),
		"created_squad_ids":  createdSquadIDs(result.Created.Squads),
		"reused_skill_count": len(createdSkills),
	})
	if _, err := qtx.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: wsUUID,
		IssueID:     pgtype.UUID{},
		ActorType:   pgtype.Text{String: "member", Valid: true},
		ActorID:     creatorUUID,
		Action:      "template_applied",
		Details:     auditDetails,
	}); err != nil {
		slog.Error("template apply: activity_log write failed; rolling back",
			append(logger.RequestAttrs(r), "error", err)...)
		writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		slog.Error("template apply: commit failed",
			append(logger.RequestAttrs(r), "error", err)...)
		writeJSON(w, http.StatusInternalServerError, applyRolledBackResponse(err))
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// createdAgentIDs extracts ids for audit details.
func createdAgentIDs(agents []CreatedAgent) []string {
	ids := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.ID != "" {
			ids = append(ids, a.ID)
		}
	}
	return ids
}

// createdSquadIDs extracts ids for audit details.
func createdSquadIDs(squads []CreatedSquad) []string {
	ids := make([]string, 0, len(squads))
	for _, s := range squads {
		if s.ID != "" {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// applyRolledBackResponse builds the 500 body after a mid-transaction
// failure: the tx has been rolled back, no orphan resources remain.
func applyRolledBackResponse(cause error) ApplyResourceTemplateResponse {
	return ApplyResourceTemplateResponse{
		Applied:         false,
		Valid:           true,
		Errors:          []resourcetmpl.Error{{Code: resourcetmpl.ErrorCode(CodeApplyRolledBack), Path: "spec", Message: cause.Error()}},
		Created:         ApplyCreated{},
		ResourceMapping: map[string]string{},
		RolledBack:      true,
	}
}

// lookupApplyLog replays a previously stored successful apply.
func (h *Handler) lookupApplyLog(ctx context.Context, wsUUID pgtype.UUID, key string) (*ApplyResourceTemplateResponse, error) {
	var result []byte
	err := h.DB.QueryRow(ctx,
		`SELECT result FROM template_apply_log WHERE workspace_id = $1 AND idempotency_key = $2`,
		wsUUID, key).Scan(&result)
	if err != nil {
		return nil, err
	}
	var resp ApplyResourceTemplateResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// templateRefs returns the set of valid refs for env/override lookups:
// for agent templates the spec name, for squads the member refs.
func templateRefs(t resourcetmpl.Template) map[string]bool {
	out := map[string]bool{}
	if t.Spec.Agent != nil {
		out[t.Spec.Agent.Name] = true
	}
	if t.Spec.Squad != nil {
		for _, m := range t.Spec.Squad.Members {
			out[m.Ref] = true
		}
	}
	return out
}

// agentSpecForRef returns the AgentSpec behind a ref: for agent templates
// the top-level spec; for squad templates the embedded member.
func agentSpecForRef(v *templateValidation, ref string) *resourcetmpl.AgentSpec {
	if v.tmpl.Spec.Agent != nil {
		return v.tmpl.Spec.Agent
	}
	for _, m := range v.tmpl.Spec.Squad.Members {
		if m.Ref == ref {
			return m.Agent
		}
	}
	return nil
}

// resolveApplyConflicts computes final names per conflict_policy and
// resolves references-mode members against existing agents. Returns an error
// (→ 409) when the policy is fail and any name collides.
func (h *Handler) resolveApplyConflicts(ctx context.Context, wsUUID pgtype.UUID, v *templateValidation, req *ApplyResourceTemplateRequest, policy string) (*applyDecision, error) {
	d := &applyDecision{mapping: map[string]string{}}

	switch v.tmpl.Kind {
	case resourcetmpl.KindAgent:
		spec := v.tmpl.Spec.Agent
		name := spec.Name
		if req.Overrides != nil && req.Overrides.Name != nil && strings.TrimSpace(*req.Overrides.Name) != "" {
			name = strings.TrimSpace(*req.Overrides.Name)
		}
		pa := plannedAgent{ref: spec.Name, baseName: name, finalName: name, action: "create"}
		if existing, err := h.findAgentByName(ctx, wsUUID, name); err == nil {
			switch policy {
			case conflictPolicyFail:
				return nil, fmt.Errorf("an agent named %q already exists in this workspace", name)
			case conflictPolicyRename:
				pa.finalName = renamedName(name)
				d.mapping[spec.Name] = pa.finalName
			case conflictPolicySkip:
				pa.action = "skip"
				pa.existing = &existing
				d.skipped = append(d.skipped, SkippedResource{Kind: "agent", Name: name, Reason: "name_conflict"})
			}
		}
		d.agents = append(d.agents, pa)

	case resourcetmpl.KindSquad:
		spec := v.tmpl.Spec.Squad
		effective, err := effectiveMembersMode(spec.MembersMode, req.MembersMode)
		if err != nil {
			return nil, err
		}
		// References mode: members map to existing same-name agents; the
		// validation pass already verified they exist and are wireable.
		if effective == resourcetmpl.MembersReferences {
			for _, mem := range spec.Members {
				existing, err := h.findAgentByName(ctx, wsUUID, mem.Ref)
				if err != nil {
					return nil, fmt.Errorf("references-mode member %q not found in workspace", mem.Ref)
				}
				d.agents = append(d.agents, plannedAgent{ref: mem.Ref, baseName: mem.Ref, action: "skip", existing: &existing})
			}
		} else {
			// Embedded mode: create each member agent (skip policy resolves
			// name conflicts to the existing same-name agent).
			for _, mem := range spec.Members {
				name := mem.Agent.Name
				if req.Overrides != nil {
					if ao, ok := req.Overrides.Agents[mem.Ref]; ok && ao.Name != nil && strings.TrimSpace(*ao.Name) != "" {
						name = strings.TrimSpace(*ao.Name)
					}
				}
				pa := plannedAgent{ref: mem.Ref, baseName: name, finalName: name, action: "create"}
				if existing, err := h.findAgentByName(ctx, wsUUID, name); err == nil {
					switch policy {
					case conflictPolicyFail:
						return nil, fmt.Errorf("an agent named %q already exists in this workspace", name)
					case conflictPolicyRename:
						pa.finalName = renamedName(name)
						d.mapping[mem.Ref] = pa.finalName
					case conflictPolicySkip:
						pa.action = "skip"
						pa.existing = &existing
						d.skipped = append(d.skipped, SkippedResource{Kind: "agent", Name: name, Reason: "name_conflict"})
					}
				}
				d.agents = append(d.agents, pa)
			}
		}

		squadName := spec.Name
		if req.Overrides != nil && req.Overrides.Name != nil && strings.TrimSpace(*req.Overrides.Name) != "" {
			squadName = strings.TrimSpace(*req.Overrides.Name)
		}
		ps := &plannedResource{baseName: squadName, finalName: squadName, action: "create"}
		if existing, err := h.findSquadByName(ctx, wsUUID, squadName); err == nil {
			switch policy {
			case conflictPolicyFail:
				return nil, fmt.Errorf("a squad named %q already exists in this workspace", squadName)
			case conflictPolicyRename:
				ps.finalName = renamedName(squadName)
				d.mapping[squadName] = ps.finalName
			case conflictPolicySkip:
				ps.action = "skip"
				ps.existing = &existing
				d.skipped = append(d.skipped, SkippedResource{Kind: "squad", Name: squadName, Reason: "name_conflict"})
			}
		}
		d.squad = ps
	}
	return d, nil
}

// renamedName appends a short lowercase hex suffix (Q5 ruling): "<name>-<8hex>".
func renamedName(name string) string {
	return name + "-" + uuid.NewString()[:8]
}

// stringSet builds a lookup set.
func stringSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[it] = true
	}
	return out
}

// collectSkillsToInstall returns the deduped list of skill sources to fetch
// (only URLs the caller explicitly listed).
func collectSkillsToInstall(v *templateValidation, req *ApplyResourceTemplateRequest) []agenttmpl.TemplateSkillRef {
	installSet := stringSet(req.InstallMissingSkills)
	seen := map[string]bool{}
	refs := []agenttmpl.TemplateSkillRef{}
	for _, ms := range v.required.MissingSkills {
		if !ms.Installable || !installSet[ms.SourceURL] || seen[ms.SourceURL] {
			continue
		}
		seen[ms.SourceURL] = true
		refs = append(refs, agenttmpl.TemplateSkillRef{
			SourceURL:  ms.SourceURL,
			CachedName: ms.Name,
		})
	}
	return refs
}

// createAgentFromSpecInTx creates one agent from an AgentSpec inside the
// apply transaction: row + invocation targets + skill bindings + env.
func (h *Handler) createAgentFromSpecInTx(ctx context.Context, qtx *db.Queries, wsUUID, creatorUUID pgtype.UUID, v *templateValidation, name string, spec *resourcetmpl.AgentSpec, req *ApplyResourceTemplateRequest, ref string) (db.Agent, error) {
	rc, _ := json.Marshal(map[string]any{})
	ca, err := json.Marshal(spec.CustomArgs)
	if err != nil {
		ca = []byte("[]")
	}
	envBytes := []byte("{}")
	if vals, ok := req.Env[ref]; ok && len(vals) > 0 {
		envBytes, err = json.Marshal(vals)
		if err != nil {
			return db.Agent{}, fmt.Errorf("encode env: %w", err)
		}
	}

	// Permission-mode resolution order (CLO-250 DEF-5, architect ruling):
	// per-ref override > top-level override > spec value > private default.
	// Both override sources are validated against the private/public_to
	// allowlist in the apply-only checks before this point, so this
	// resolution reuses that same enforcement path and cannot bypass
	// CONFIG_NOT_ALLOWED.
	mode := spec.PermissionMode
	if req.Overrides != nil {
		if ao, ok := req.Overrides.Agents[ref]; ok && ao.PermissionMode != nil && *ao.PermissionMode != "" {
			mode = *ao.PermissionMode
		} else if req.Overrides.PermissionMode != nil && *req.Overrides.PermissionMode != "" {
			mode = *req.Overrides.PermissionMode
		}
	}
	if mode == "" {
		mode = permissionModePrivate
	}
	if mode != permissionModePrivate && mode != permissionModePublicTo {
		return db.Agent{}, fmt.Errorf("permission_mode %q must be private or public_to", mode)
	}
	perm := resolvedPermission{mode: mode}
	if mode == permissionModePublicTo && spec.PublicToWorkspace {
		perm.targets = []targetSpec{{targetType: invocationTargetWorkspace, targetID: wsUUID}}
	}

	// Top-level overrides act on the top-level resource of this apply, so
	// for kind=agent templates they also feed the agent itself; per-ref
	// overrides win over top-level ones (architect ruling).
	description := spec.Description
	instructions := spec.Instructions
	model := spec.Model
	if req.Overrides != nil {
		perRef := req.Overrides.Agents[ref]
		if perRef.Description != nil {
			description = *perRef.Description
		} else if req.Overrides.Description != nil {
			description = *req.Overrides.Description
		}
		if perRef.Instructions != nil {
			instructions = *perRef.Instructions
		} else if req.Overrides.Instructions != nil {
			instructions = *req.Overrides.Instructions
		}
		if perRef.Model != nil {
			model = *perRef.Model
		} else if req.Overrides.Model != nil {
			model = *req.Overrides.Model
		}
	}

	maxTasks := spec.MaxConcurrentTasks
	if maxTasks <= 0 {
		maxTasks = 6
	}

	agentRow, err := qtx.CreateAgent(ctx, db.CreateAgentParams{
		WorkspaceID:        wsUUID,
		Name:               name,
		Description:        description,
		Instructions:       instructions,
		AvatarUrl:          newAgentAvatar(nil),
		RuntimeMode:        v.runtime.RuntimeMode,
		RuntimeConfig:      rc,
		RuntimeID:          v.runtime.ID,
		Visibility:         perm.legacyVisibility(),
		PermissionMode:     perm.mode,
		MaxConcurrentTasks: int32(maxTasks),
		OwnerID:            creatorUUID,
		CustomEnv:          envBytes,
		CustomArgs:         ca,
		McpConfig:          nil,
		Model:              pgtype.Text{String: model, Valid: model != ""},
		ThinkingLevel:      pgtype.Text{String: spec.ThinkingLevel, Valid: spec.ThinkingLevel != ""},
		ServiceTier:        pgtype.Text{String: spec.ServiceTier, Valid: spec.ServiceTier != ""},
	})
	if err != nil {
		return db.Agent{}, err
	}

	if err := replaceInvocationTargetsWithQueries(ctx, qtx, agentRow.ID, creatorUUID, perm.targets); err != nil {
		return db.Agent{}, fmt.Errorf("persist invocation targets: %w", err)
	}

	// Bind skills: reused workspace skills by name; fetched skills were
	// materialised earlier in the same tx.
	if err := h.bindAgentSkillsInTx(ctx, qtx, wsUUID, agentRow.ID, spec); err != nil {
		return db.Agent{}, err
	}

	return agentRow, nil
}

// bindAgentSkillsInTx binds an agent to its spec skills, resolving each by
// name in the workspace (reuse-only; installs happen earlier in the tx via
// createSkillWithFilesInTx).
func (h *Handler) bindAgentSkillsInTx(ctx context.Context, qtx *db.Queries, wsUUID, agentID pgtype.UUID, spec *resourcetmpl.AgentSpec) error {
	for _, sk := range spec.Skills {
		existing, err := qtx.GetSkillByWorkspaceAndName(ctx, db.GetSkillByWorkspaceAndNameParams{
			WorkspaceID: wsUUID, Name: sk.Name,
		})
		if err != nil {
			// The validation pass guarantees presence (or explicit install
			// of the same tx); a vanished skill is a hard dependency error.
			return fmt.Errorf("skill %q unavailable: %w", sk.Name, err)
		}
		if err := qtx.AddAgentSkill(ctx, db.AddAgentSkillParams{
			AgentID: agentID,
			SkillID: existing.ID,
		}); err != nil {
			return fmt.Errorf("attach skill %q: %w", sk.Name, err)
		}
	}
	return nil
}

// dryRunResponse builds the plan-shaped response for dry_run=true: created
// lists carry the planned names without ids, nothing is written.
func dryRunResponse(v *templateValidation, d *applyDecision, warnings []resourcetmpl.Warning) ApplyResourceTemplateResponse {
	resp := ApplyResourceTemplateResponse{
		Applied:         true,
		Valid:           true,
		DryRun:          true,
		Warnings:        warnings,
		ResourceMapping: d.mapping,
		Skipped:         d.skipped,
		Created:         ApplyCreated{},
	}
	for _, pa := range d.agents {
		if pa.action == "skip" {
			continue
		}
		resp.Created.Agents = append(resp.Created.Agents, CreatedAgent{Ref: pa.ref, Name: pa.finalName})
	}
	if d.squad != nil && d.squad.action == "create" {
		resp.Created.Squads = append(resp.Created.Squads, CreatedSquad{Name: d.squad.finalName})
	}
	for _, ms := range v.required.MissingSkills {
		resp.Created.Skills = append(resp.Created.Skills, CreatedSkill{Name: ms.Name, Reused: false})
	}
	return resp
}
