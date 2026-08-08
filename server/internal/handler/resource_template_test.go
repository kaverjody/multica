package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/multica-ai/multica/server/internal/resourcetmpl"
)

// resource_template_test.go covers CLO-247 (POST /api/templates/export) and
// CLO-248 (POST /api/templates/validate + /apply). The suite skips itself
// when no test database is available (see handler_test.go TestMain).

func uniqueName(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// createRegularTestMember inserts a second workspace member with the plain
// "member" role (the fixture user is the workspace owner).
func createRegularTestMember(t *testing.T) string {
	t.Helper()
	var userID string
	email := fmt.Sprintf("member-%s@multica.ai", uuid.NewString()[:8])
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"Regular Test Member", email).Scan(&userID); err != nil {
		t.Fatalf("create member user: %v", err)
	}
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'member')`,
		testWorkspaceID, userID); err != nil {
		t.Fatalf("create member row: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, userID)
	})
	return userID
}

// createTestSquad inserts a squad row plus member rows directly (the squad
// leader is auto-added as a role=leader member, mirroring CreateSquad).
func createTestSquad(t *testing.T, name, leaderID string, members [][3]string) string {
	t.Helper()
	var squadID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, $2, '', $3, $4) RETURNING id
	`, testWorkspaceID, name, leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	rows := append([][3]string{{"agent", leaderID, "leader"}}, members...)
	for _, m := range rows {
		if _, err := testPool.Exec(context.Background(), `
			INSERT INTO squad_member (squad_id, member_type, member_id, role)
			VALUES ($1, $2, $3, $4)
		`, squadID, m[0], m[1], m[2]); err != nil {
			t.Fatalf("add squad member: %v", err)
		}
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})
	return squadID
}

func templateFixture(kind, name string, spec map[string]any) map[string]any {
	return map[string]any{
		"schema_version": "1.0",
		"template_id":    uuid.NewString(),
		"kind":           kind,
		"metadata": map[string]any{
			"name":       name,
			"version":    "1.0.0",
			"visibility": "workspace",
			"author":     map[string]any{"id": testUserID, "display_name": "tester"},
		},
		"spec": spec,
	}
}

func agentTemplate(name string, extra map[string]any) map[string]any {
	spec := map[string]any{
		"name":                 name,
		"description":          "",
		"instructions":         "",
		"model":                "",
		"thinking_level":       "",
		"service_tier":         "",
		"max_concurrent_tasks": 6,
		"permission_mode":      "private",
		"custom_args":          []string{},
		"skills":               []any{},
		"custom_env_keys":      []any{},
		"mcp_servers":          []any{},
	}
	for k, v := range extra {
		spec[k] = v
	}
	return templateFixture("agent", name, map[string]any{"agent": spec})
}

func squadTemplate(name string, membersMode, leaderRef string, members []map[string]any) map[string]any {
	return templateFixture("squad", name, map[string]any{"squad": map[string]any{
		"name":         name,
		"description":  "",
		"instructions": "",
		"members_mode": membersMode,
		"leader_ref":   leaderRef,
		"members":      members,
	}})
}

func memberRef(ref, role string, agent map[string]any) map[string]any {
	m := map[string]any{"ref": ref, "role": role}
	if agent != nil {
		m["agent"] = agent
	}
	return m
}

func decodeExport(t *testing.T, w *httptest.ResponseRecorder) ExportResourceTemplateResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("export: status %d, body %s", w.Code, w.Body.String())
	}
	var resp ExportResourceTemplateResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode export response: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// CLO-247: export
// ---------------------------------------------------------------------------

func TestExportResourceTemplate_AgentRoundTrip(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("export-agent")
	agentID := createHandlerTestAgent(t, name, nil)
	if _, err := testPool.Exec(context.Background(), `
		UPDATE agent SET custom_env = $1, instructions = $2 WHERE id = $3
	`, `{"API_KEY":"supersecretvalue123","NON_SECRET":"x"}`, "You are the export agent.", agentID); err != nil {
		t.Fatalf("set agent env: %v", err)
	}

	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "agent", "resource_id": agentID,
	}))
	resp := decodeExport(t, w)

	tmpl := resp.Template
	if tmpl.Kind != resourcetmpl.KindAgent || tmpl.Spec.Agent == nil {
		t.Fatalf("template kind = %q, agent spec nil = %v", tmpl.Kind, tmpl.Spec.Agent == nil)
	}
	if tmpl.Spec.Agent.Name != name {
		t.Errorf("spec agent name = %q, want %q", tmpl.Spec.Agent.Name, name)
	}
	// Round trip: the exported template must pass the package validator.
	raw, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("marshal template: %v", err)
	}
	if report := resourcetmpl.Validate(raw); report.HasErrors() {
		t.Errorf("exported template fails validation: %+v", report.Errors)
	}
	// custom_env: keys only, values never.
	var envKeys []string
	for _, k := range tmpl.Spec.Agent.CustomEnvKeys {
		envKeys = append(envKeys, k.Key)
	}
	if len(envKeys) != 2 {
		t.Fatalf("custom_env_keys = %v, want 2 keys", envKeys)
	}
	if strings.Contains(w.Body.String(), "supersecretvalue123") {
		t.Error("export leaked custom_env value")
	}
	// Non-portable fields must not appear anywhere in the spec.
	for _, forbidden := range []string{`"runtime_id"`, `"runtime_mode"`, `"owner_id"`, `"avatar_url"`, `"created_at"`} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Errorf("exported template contains forbidden field %s", forbidden)
		}
	}
	// Deterministic template_id: same resource re-exported → same id, and
	// resolving by name yields the same id as resolving by UUID.
	w2 := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w2, newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "agent", "resource_id": name,
	}))
	resp2 := decodeExport(t, w2)
	if resp2.Template.TemplateID != resp.Template.TemplateID {
		t.Errorf("template_id not deterministic: %q vs %q", resp.Template.TemplateID, resp2.Template.TemplateID)
	}
	if resp2.Template.Spec.Agent.Name != name {
		t.Errorf("name-based export resolved wrong agent: %q", resp2.Template.Spec.Agent.Name)
	}
	// Metadata defaults.
	if tmpl.Metadata.Version != "1.0.0" || tmpl.Metadata.Visibility != "workspace" {
		t.Errorf("metadata defaults wrong: %+v", tmpl.Metadata)
	}
}

func TestExportResourceTemplate_McpRedacted(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("export-mcp")
	agentID := createHandlerTestAgent(t, name, nil)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent SET mcp_config = $1::jsonb WHERE id = $2`,
		`{"servers": {"github": {"type": "stdio", "command": "gh-mcp", "token": "ghp_secretToken12345", "env": {"KEY": "value"}}}}`,
		agentID); err != nil {
		t.Fatalf("set mcp_config: %v", err)
	}

	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "agent", "resource_id": agentID,
	}))
	resp := decodeExport(t, w)

	servers := resp.Template.Spec.Agent.MCPServers
	if len(servers) != 1 {
		t.Fatalf("mcp_servers = %d entries, want 1", len(servers))
	}
	if servers[0].Name != "github" || servers[0].Transport != "stdio" {
		t.Errorf("mcp server = %+v", servers[0])
	}
	if !servers[0].RequiresAuth {
		t.Error("mcp server requires_auth should be true (token present)")
	}
	if strings.Contains(w.Body.String(), "ghp_secretToken12345") {
		t.Error("export leaked mcp token")
	}
	if strings.Contains(string(servers[0].ConfigSkeleton), "token") {
		t.Errorf("config_skeleton still carries auth material: %s", servers[0].ConfigSkeleton)
	}
}

func TestExportResourceTemplate_SquadEmbedded(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leader := uniqueName("sq-lead")
	worker := uniqueName("sq-worker")
	leaderID := createHandlerTestAgent(t, leader, nil)
	workerID := createHandlerTestAgent(t, worker, nil)
	squadID := createTestSquad(t, uniqueName("export-squad"), leaderID, [][3]string{{"agent", workerID, "worker"}})

	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "squad", "resource_id": squadID, "members_mode": "embedded",
	}))
	resp := decodeExport(t, w)

	spec := resp.Template.Spec.Squad
	if spec == nil {
		t.Fatal("squad spec is nil")
	}
	if spec.MembersMode != resourcetmpl.MembersEmbedded {
		t.Errorf("members_mode = %q", spec.MembersMode)
	}
	if len(spec.Members) != 2 {
		t.Fatalf("members = %d, want 2", len(spec.Members))
	}
	// leader_ref must point at a role=leader member; every embedded member
	// carries a full AgentSpec.
	leaderRef := ""
	hasLeader := false
	for _, m := range spec.Members {
		if m.Agent == nil {
			t.Errorf("embedded member %q has no agent spec", m.Ref)
		}
		if m.Role == resourcetmpl.RoleLeader {
			hasLeader = true
			leaderRef = m.Ref
		}
	}
	if !hasLeader || spec.LeaderRef != leaderRef || leaderRef == "" {
		t.Errorf("leader_ref = %q, members with leader role: %v", spec.LeaderRef, hasLeader)
	}
	raw, _ := json.Marshal(resp.Template)
	if report := resourcetmpl.Validate(raw); report.HasErrors() {
		t.Errorf("exported squad template fails validation: %+v", report.Errors)
	}
}

func TestExportResourceTemplate_SquadReferences(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leaderID := createHandlerTestAgent(t, uniqueName("ref-lead"), nil)
	workerID := createHandlerTestAgent(t, uniqueName("ref-worker"), nil)
	squadID := createTestSquad(t, uniqueName("export-squad-refs"), leaderID, [][3]string{{"agent", workerID, "worker"}})

	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "squad", "resource_id": squadID, "members_mode": "references",
	}))
	resp := decodeExport(t, w)
	spec := resp.Template.Spec.Squad
	for _, m := range spec.Members {
		if m.Agent != nil {
			t.Errorf("references-mode member %q carries an agent payload", m.Ref)
		}
	}
}

func TestExportResourceTemplate_HumanMemberDropped(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leaderID := createHandlerTestAgent(t, uniqueName("hm-lead"), nil)
	squadID := createTestSquad(t, uniqueName("export-squad-human"), leaderID,
		[][3]string{{"member", testUserID, "worker"}})

	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "squad", "resource_id": squadID,
	}))
	resp := decodeExport(t, w)
	if len(resp.Template.Spec.Squad.Members) != 1 {
		t.Errorf("members = %d, want 1 (human member dropped)", len(resp.Template.Spec.Squad.Members))
	}
	found := false
	for _, warn := range resp.Warnings {
		if warn.Code == "MEMBER_HUMAN_DROPPED" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected MEMBER_HUMAN_DROPPED warning, got %+v", resp.Warnings)
	}
}

func TestExportResourceTemplate_Forbidden(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	agentID := createHandlerTestAgent(t, uniqueName("priv-agent"), nil)
	other := createRegularTestMember(t)

	req := newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "agent", "resource_id": agentID,
	})
	req.Header.Set("X-User-ID", other)
	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	// Never a partial template: the body must not contain the agent name.
	if strings.Contains(w.Body.String(), agentID) {
		t.Error("403 response leaked resource identifier")
	}

	// Agent-actor tokens are rejected too.
	req2 := newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "agent", "resource_id": agentID,
	})
	req2.Header.Set("X-Actor-Source", "task_token")
	req2.Header.Set("X-Agent-ID", agentID)
	w2 := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("agent-actor status = %d, want 403", w2.Code)
	}
}

func TestExportResourceTemplate_SquadDependencyForbidden(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leaderID := createHandlerTestAgent(t, uniqueName("sd-lead"), nil)
	workerID := createHandlerTestAgent(t, uniqueName("sd-worker"), nil)
	squadID := createTestSquad(t, uniqueName("export-squad-dep"), leaderID, [][3]string{{"agent", workerID, "worker"}})

	// A regular member (not the workspace owner) cannot read the squad's
	// leader/member configs (owned by the fixture user) → 403 with the
	// missing dependencies listed, never a partial template.
	other := createRegularTestMember(t)
	req := newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "squad", "resource_id": squadID,
	})
	req.Header.Set("X-User-ID", other)
	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", w.Code, w.Body.String())
	}
	var body struct {
		Error               string   `json:"error"`
		MissingDependencies []string `json:"missing_dependencies"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode 403 body: %v", err)
	}
	if len(body.MissingDependencies) == 0 {
		t.Error("403 body must list missing dependencies")
	}
}

func TestExportResourceTemplate_SecretDetected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("secret-agent")
	agentID := createHandlerTestAgent(t, name, nil)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE agent SET instructions = $1 WHERE id = $2`,
		"sk-ant-0123456789abcdef1234 is the key to use.", agentID); err != nil {
		t.Fatalf("set instructions: %v", err)
	}

	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "agent", "resource_id": agentID,
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SECRET_DETECTED") {
		t.Errorf("response should carry SECRET_DETECTED, got %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// CLO-248: validate
// ---------------------------------------------------------------------------

func doValidate(t *testing.T, body map[string]any) (int, ValidateResourceTemplateResponse, string) {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.ValidateResourceTemplate(w, newRequest("POST", "/api/templates/validate?workspace_id="+testWorkspaceID, body))
	var resp ValidateResourceTemplateResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return w.Code, resp, w.Body.String()
}

func TestValidateResourceTemplate_MultipleErrorsCollected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	body := templateFixture("agent", "bad", map[string]any{"agent": map[string]any{"name": ""}})
	body["schema_version"] = "9.0"
	body["unknown_top_level"] = true
	body["spec"].(map[string]any)["squad"] = map[string]any{"name": "x", "members_mode": "embedded", "leader_ref": "l", "members": []any{}}

	code, resp, _ := doValidate(t, map[string]any{
		"template": body, "target_runtime_id": uuid.NewString(),
	})
	if code != http.StatusOK {
		t.Fatalf("validate status = %d, want 200", code)
	}
	if resp.Valid {
		t.Error("valid should be false")
	}
	if len(resp.Errors) < 4 {
		t.Errorf("errors = %d, want >= 4 collected in one pass: %+v", len(resp.Errors), resp.Errors)
	}
}

func TestValidateResourceTemplate_RuntimeNotFound(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, resp, _ := doValidate(t, map[string]any{
		"template": agentTemplate(uniqueName("v-agent"), nil), "target_runtime_id": uuid.NewString(),
	})
	if resp.Valid {
		t.Error("valid should be false for a missing runtime")
	}
	if !hasErrorCode(resp.Errors, "RUNTIME_NOT_FOUND") {
		t.Errorf("expected RUNTIME_NOT_FOUND, got %+v", resp.Errors)
	}
}

func TestValidateResourceTemplate_RuntimeForbidden(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	other := createRegularTestMember(t)
	var privateRuntimeID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO agent_runtime (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, visibility)
		VALUES ($1, NULL, $2, 'cloud', 'codex', 'online', 'x', '{}'::jsonb, $3, 'private')
		RETURNING id
	`, testWorkspaceID, uniqueName("private-rt"), testUserID).Scan(&privateRuntimeID); err != nil {
		t.Fatalf("create private runtime: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, privateRuntimeID)
	})

	req := newRequest("POST", "/api/templates/validate?workspace_id="+testWorkspaceID, map[string]any{
		"template": agentTemplate(uniqueName("v-agent"), nil), "target_runtime_id": privateRuntimeID,
	})
	req.Header.Set("X-User-ID", other)
	w := httptest.NewRecorder()
	testHandler.ValidateResourceTemplate(w, req)
	var resp ValidateResourceTemplateResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp.Valid {
		t.Error("valid should be false: private runtime not usable by a regular member")
	}
	if !hasErrorCode(resp.Errors, "FORBIDDEN") {
		t.Errorf("expected FORBIDDEN, got %+v", resp.Errors)
	}
}

func TestValidateResourceTemplate_ThinkingLevelRejected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	// The fixture runtime has an unknown provider, so any non-empty
	// thinking_level must be rejected (never silently dropped).
	_, resp, _ := doValidate(t, map[string]any{
		"template":          agentTemplate(uniqueName("v-think"), map[string]any{"thinking_level": "high"}),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if resp.Valid {
		t.Error("valid should be false for an unrecognised thinking_level")
	}
	if !hasErrorCode(resp.Errors, "TEMPLATE_INVALID") {
		t.Errorf("expected TEMPLATE_INVALID, got %+v", resp.Errors)
	}
}

func TestValidateResourceTemplate_MissingSkillInputs(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, resp, _ := doValidate(t, map[string]any{
		"template": agentTemplate(uniqueName("v-skill"), map[string]any{"skills": []any{
			map[string]any{"name": "NoSuchSkillXYZ", "source_url": ""},
			map[string]any{"name": "AnotherMissingSkill", "source_url": "https://github.com/acme/skill"},
		}}),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if !resp.Valid {
		t.Errorf("missing skills are inputs, not errors: %+v", resp.Errors)
	}
	if len(resp.RequiredInputs.MissingSkills) != 2 {
		t.Fatalf("missing_skills = %+v, want 2 entries", resp.RequiredInputs.MissingSkills)
	}
	for _, ms := range resp.RequiredInputs.MissingSkills {
		if ms.Name == "NoSuchSkillXYZ" && ms.Installable {
			t.Error("skill without source_url must not be installable")
		}
		if ms.Name == "AnotherMissingSkill" && !ms.Installable {
			t.Error("skill with source_url must be installable")
		}
	}
}

func TestValidateResourceTemplate_ConflictsAndEnvKeys(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, resp, _ := doValidate(t, map[string]any{
		"template": agentTemplate("Handler Test Agent", map[string]any{"custom_env_keys": []any{
			map[string]any{"key": "API_KEY", "required": true},
		}}),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if !resp.Valid {
		t.Fatalf("conflict + env inputs are not errors: %+v", resp.Errors)
	}
	if len(resp.Plan.Conflicts) != 1 || resp.Plan.Conflicts[0].Name != "Handler Test Agent" {
		t.Errorf("plan.conflicts = %+v", resp.Plan.Conflicts)
	}
	if len(resp.RequiredInputs.EnvKeys) != 1 || resp.RequiredInputs.EnvKeys[0].Key != "API_KEY" {
		t.Errorf("env_keys = %+v", resp.RequiredInputs.EnvKeys)
	}
}

func TestValidateResourceTemplate_ReferencesDependencyNotFound(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	members := []map[string]any{
		memberRef("no-such-agent-xyz", "leader", nil),
	}
	_, resp, _ := doValidate(t, map[string]any{
		"template":          squadTemplate(uniqueName("v-squad"), "references", "no-such-agent-xyz", members),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if resp.Valid {
		t.Error("valid should be false: references member does not exist")
	}
	if !hasErrorCode(resp.Errors, "DEPENDENCY_NOT_FOUND") {
		t.Errorf("expected DEPENDENCY_NOT_FOUND, got %+v", resp.Errors)
	}
	if len(resp.RequiredInputs.MissingAgents) != 1 {
		t.Errorf("missing_agents = %+v", resp.RequiredInputs.MissingAgents)
	}
}

func TestValidateResourceTemplate_SecretDetected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	_, resp, _ := doValidate(t, map[string]any{
		"template":          agentTemplate(uniqueName("v-secret"), map[string]any{"instructions": "sk-ant-0123456789abcdef1234"}),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if resp.Valid {
		t.Error("valid should be false for a template with a plaintext secret")
	}
	if !hasErrorCode(resp.Errors, "SECRET_DETECTED") {
		t.Errorf("expected SECRET_DETECTED, got %+v", resp.Errors)
	}
}

func hasErrorCode(errs []resourcetmpl.Error, code string) bool {
	for _, e := range errs {
		if string(e.Code) == code {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// CLO-248: apply
// ---------------------------------------------------------------------------

func doApply(t *testing.T, body map[string]any) (int, ApplyResourceTemplateResponse, string) {
	t.Helper()
	w := httptest.NewRecorder()
	testHandler.ApplyResourceTemplate(w, newRequest("POST", "/api/templates/apply?workspace_id="+testWorkspaceID, body))
	var resp ApplyResourceTemplateResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return w.Code, resp, w.Body.String()
}

func countAgentsNamed(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent WHERE workspace_id = $1 AND name = $2`,
		testWorkspaceID, name).Scan(&n); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	return n
}

func cleanupAgentByID(t *testing.T, id string) {
	t.Helper()
	if id != "" {
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, id)
		})
	}
}

func TestApplyResourceTemplate_AgentCreated(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-agent")
	code, resp, body := doApply(t, map[string]any{
		"template":          agentTemplate(name, nil),
		"target_runtime_id": handlerTestRuntimeID(t),
		"conflict_policy":   "fail",
	})
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body %s", code, body)
	}
	if !resp.Applied || resp.DryRun || resp.RolledBack {
		t.Fatalf("unexpected apply flags: %+v", resp)
	}
	if len(resp.Created.Agents) != 1 || resp.Created.Agents[0].Name != name {
		t.Fatalf("created agents = %+v", resp.Created.Agents)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	if countAgentsNamed(t, name) != 1 {
		t.Fatal("agent was not persisted")
	}
	// owner = caller, default permission private.
	var ownerID, permissionMode string
	if err := testPool.QueryRow(context.Background(),
		`SELECT owner_id, permission_mode FROM agent WHERE id = $1`, resp.Created.Agents[0].ID).
		Scan(&ownerID, &permissionMode); err != nil {
		t.Fatalf("load created agent: %v", err)
	}
	if ownerID != testUserID {
		t.Errorf("agent owner = %s, want %s", ownerID, testUserID)
	}
	if permissionMode != "private" {
		t.Errorf("agent permission_mode = %q, want private (no auto-activation)", permissionMode)
	}
	// resource_mapping: ref → id.
	if resp.ResourceMapping[name] != resp.Created.Agents[0].ID {
		t.Errorf("resource_mapping = %+v", resp.ResourceMapping)
	}
}

func TestApplyResourceTemplate_DryRunWritesNothing(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-dry")
	code, resp, _ := doApply(t, map[string]any{
		"template":          agentTemplate(name, nil),
		"target_runtime_id": handlerTestRuntimeID(t),
		"dry_run":           true,
	})
	if code != http.StatusOK {
		t.Fatalf("dry-run status = %d", code)
	}
	if !resp.DryRun || !resp.Applied {
		t.Fatalf("dry-run flags wrong: %+v", resp)
	}
	if len(resp.Created.Agents) != 1 || resp.Created.Agents[0].ID != "" {
		t.Fatalf("dry-run created = %+v (no ids expected)", resp.Created.Agents)
	}
	if countAgentsNamed(t, name) != 0 {
		t.Fatal("dry-run must not write")
	}
}

func TestApplyResourceTemplate_ConflictPolicies(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-conflict")

	// fail (default): 409 NAME_CONFLICT, nothing created.
	code, resp, _ := doApply(t, map[string]any{
		"template": agentTemplate(name, nil), "target_runtime_id": handlerTestRuntimeID(t),
	})
	if code != http.StatusOK {
		t.Fatalf("first apply failed: %d", code)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	code, resp, _ = doApply(t, map[string]any{
		"template": agentTemplate(name, nil), "target_runtime_id": handlerTestRuntimeID(t),
		"conflict_policy": "fail",
	})
	if code != http.StatusConflict {
		t.Fatalf("fail policy status = %d, want 409", code)
	}
	if countAgentsNamed(t, name) != 1 {
		t.Fatal("fail policy must not create a second agent")
	}

	// rename: "<name>-<8hex>", mapping returned.
	code, resp, _ = doApply(t, map[string]any{
		"template": agentTemplate(name, nil), "target_runtime_id": handlerTestRuntimeID(t),
		"conflict_policy": "rename",
	})
	if code != http.StatusOK || len(resp.Created.Agents) != 1 {
		t.Fatalf("rename policy failed: code=%d resp=%+v", code, resp)
	}
	renamed := resp.Created.Agents[0].Name
	if !strings.HasPrefix(renamed, name+"-") {
		t.Errorf("renamed agent = %q, want prefix %q", renamed, name+"-")
	}
	if resp.ResourceMapping[name] != renamed {
		t.Errorf("rename mapping = %+v", resp.ResourceMapping)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)

	// skip: nothing created, skipped recorded.
	code, resp, _ = doApply(t, map[string]any{
		"template": agentTemplate(name, nil), "target_runtime_id": handlerTestRuntimeID(t),
		"conflict_policy": "skip",
	})
	if code != http.StatusOK {
		t.Fatalf("skip policy status = %d", code)
	}
	if len(resp.Created.Agents) != 0 {
		t.Errorf("skip policy created agents: %+v", resp.Created.Agents)
	}
	if len(resp.Skipped) != 1 || resp.Skipped[0].Kind != "agent" {
		t.Errorf("skip policy skipped = %+v", resp.Skipped)
	}
	if countAgentsNamed(t, name) != 1 {
		t.Fatal("skip policy must not create a second agent")
	}
}

func TestApplyResourceTemplate_OverwriteRejected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	w := httptest.NewRecorder()
	testHandler.ApplyResourceTemplate(w, newRequest("POST", "/api/templates/apply?workspace_id="+testWorkspaceID, map[string]any{
		"template": agentTemplate(uniqueName("apply-ow"), nil), "target_runtime_id": handlerTestRuntimeID(t),
		"conflict_policy": "overwrite",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestApplyResourceTemplate_SquadEmbeddedOrderAndLeader(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	squadName := uniqueName("apply-squad")
	leaderName := uniqueName("apply-lead")
	workerName := uniqueName("apply-worker")
	members := []map[string]any{
		memberRef("lead", "leader", map[string]any{
			"name": leaderName, "description": "l", "instructions": "lead it",
			"thinking_level": "", "service_tier": "", "permission_mode": "private",
			"custom_args": []string{}, "skills": []any{}, "custom_env_keys": []any{}, "mcp_servers": []any{},
		}),
		memberRef("worker", "worker", map[string]any{
			"name": workerName, "description": "w", "instructions": "",
			"thinking_level": "", "service_tier": "", "permission_mode": "private",
			"custom_args": []string{}, "skills": []any{}, "custom_env_keys": []any{}, "mcp_servers": []any{},
		}),
	}
	code, resp, body := doApply(t, map[string]any{
		"template":          squadTemplate(squadName, "embedded", "lead", members),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if code != http.StatusOK {
		t.Fatalf("apply status = %d, body %s", code, body)
	}
	if len(resp.Created.Agents) != 2 || len(resp.Created.Squads) != 1 {
		t.Fatalf("created = %+v", resp.Created)
	}
	squadID := resp.Created.Squads[0].ID
	leaderID := resp.Created.Agents[0].ID
	workerID := resp.Created.Agents[1].ID
	for _, a := range resp.Created.Agents {
		cleanupAgentByID(t, a.ID)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})

	// Dependency order: leader agent created first (agents[0]).
	if resp.Created.Agents[0].Ref != "lead" {
		t.Errorf("agents order = %+v, want leader first", resp.Created.Agents)
	}
	if resp.ResourceMapping["lead"] != leaderID || resp.ResourceMapping["worker"] != workerID {
		t.Errorf("resource_mapping = %+v", resp.ResourceMapping)
	}

	// Squad row: leader_id set; member rows include leader with role=leader.
	var dbLeaderID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT leader_id FROM squad WHERE id = $1`, squadID).Scan(&dbLeaderID); err != nil {
		t.Fatalf("load squad: %v", err)
	}
	if dbLeaderID != leaderID {
		t.Errorf("squad.leader_id = %s, want %s", dbLeaderID, leaderID)
	}
	rows, err := testPool.Query(context.Background(),
		`SELECT member_id, role FROM squad_member WHERE squad_id = $1 ORDER BY created_at`, squadID)
	if err != nil {
		t.Fatalf("list squad members: %v", err)
	}
	type memberRow struct {
		id   string
		role string
	}
	var got []memberRow
	for rows.Next() {
		var m memberRow
		if err := rows.Scan(&m.id, &m.role); err != nil {
			t.Fatal(err)
		}
		got = append(got, m)
	}
	rows.Close()
	if len(got) != 2 {
		t.Fatalf("squad_member rows = %+v, want 2", got)
	}
	if got[0].id != leaderID || got[0].role != "leader" {
		t.Errorf("leader member row = %+v, want (id=%s, role=leader)", got[0], leaderID)
	}
	if got[1].id != workerID || got[1].role != "worker" {
		t.Errorf("worker member row = %+v", got[1])
	}
}

func TestApplyResourceTemplate_RollbackNoOrphans(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	// A member description longer than the DB CHECK (255 chars) passes
	// template validation but fails inside the transaction → full rollback.
	leaderName := uniqueName("rb-lead")
	badName := uniqueName("rb-bad")
	members := []map[string]any{
		memberRef("lead", "leader", map[string]any{
			"name": leaderName, "description": "l", "instructions": "",
			"thinking_level": "", "service_tier": "", "permission_mode": "private",
			"custom_args": []string{}, "skills": []any{}, "custom_env_keys": []any{}, "mcp_servers": []any{},
		}),
		memberRef("bad", "worker", map[string]any{
			"name":         badName,
			"description":  strings.Repeat("x", 300),
			"instructions": "", "thinking_level": "", "service_tier": "",
			"permission_mode": "private", "custom_args": []string{},
			"skills": []any{}, "custom_env_keys": []any{}, "mcp_servers": []any{},
		}),
	}
	code, resp, body := doApply(t, map[string]any{
		"template":          squadTemplate(uniqueName("rb-squad"), "embedded", "lead", members),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", code, body)
	}
	if !resp.RolledBack || !hasErrorCode(resp.Errors, "APPLY_ROLLED_BACK") {
		t.Errorf("expected rolled_back + APPLY_ROLLED_BACK, got %+v", resp)
	}
	// No orphan resources: neither member agent nor the squad exists.
	for _, n := range []string{leaderName, badName} {
		if countAgentsNamed(t, n) != 0 {
			t.Errorf("orphan agent %q left behind", n)
		}
	}
}

func TestApplyResourceTemplate_RequiredEnvMissingAndApplied(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-env")
	tmpl := agentTemplate(name, map[string]any{"custom_env_keys": []any{
		map[string]any{"key": "API_KEY", "required": true},
	}})

	// Without env → REQUIRED_INPUT_MISSING, no write.
	code, resp, _ := doApply(t, map[string]any{
		"template": tmpl, "target_runtime_id": handlerTestRuntimeID(t),
	})
	if code != http.StatusOK || resp.Applied {
		t.Fatalf("missing required env: code=%d resp=%+v", code, resp)
	}
	if !hasErrorCode(resp.Errors, "REQUIRED_INPUT_MISSING") {
		t.Errorf("expected REQUIRED_INPUT_MISSING, got %+v", resp.Errors)
	}
	if countAgentsNamed(t, name) != 0 {
		t.Fatal("apply with missing required env must not create anything")
	}

	// With env → created, value persisted via the agent env channel.
	code, resp, _ = doApply(t, map[string]any{
		"template": tmpl, "target_runtime_id": handlerTestRuntimeID(t),
		"env": map[string]any{name: map[string]string{"API_KEY": "value123"}},
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("apply with env failed: code=%d resp=%+v", code, resp)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	var customEnv []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT custom_env FROM agent WHERE id = $1`, resp.Created.Agents[0].ID).Scan(&customEnv); err != nil {
		t.Fatalf("load custom_env: %v", err)
	}
	if !strings.Contains(string(customEnv), "API_KEY") || !strings.Contains(string(customEnv), "value123") {
		t.Errorf("custom_env = %s, want API_KEY=value123", customEnv)
	}
}

func TestApplyResourceTemplate_SkillDependency(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-skill")
	tmpl := agentTemplate(name, map[string]any{"skills": []any{
		map[string]any{"name": "NoSuchSkillXYZ", "source_url": ""},
	}})

	// A missing skill without a source_url cannot be installed → error.
	code, resp, _ := doApply(t, map[string]any{
		"template": tmpl, "target_runtime_id": handlerTestRuntimeID(t),
	})
	if resp.Applied {
		t.Fatal("apply must fail for an un-installable missing skill")
	}
	if !hasErrorCode(resp.Errors, "DEPENDENCY_NOT_FOUND") {
		t.Errorf("expected DEPENDENCY_NOT_FOUND, got %+v", resp.Errors)
	}
	_ = code

	// A skill that exists in the workspace is reused and bound.
	skillName := uniqueName("ws-skill")
	var skillID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO skill (workspace_id, name, description, content, created_by)
		VALUES ($1, $2, 'd', 'content', $3) RETURNING id
	`, testWorkspaceID, skillName, testUserID).Scan(&skillID); err != nil {
		t.Fatalf("create skill: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM skill WHERE id = $1`, skillID)
	})
	name2 := uniqueName("apply-skill2")
	code, resp, _ = doApply(t, map[string]any{
		"template": agentTemplate(name2, map[string]any{"skills": []any{
			map[string]any{"name": skillName, "source_url": ""},
		}}),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if !resp.Applied {
		t.Fatalf("apply with existing skill failed: %+v", resp.Errors)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	var bound int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_skill WHERE agent_id = $1`, resp.Created.Agents[0].ID).Scan(&bound); err != nil {
		t.Fatalf("count agent_skill: %v", err)
	}
	if bound != 1 {
		t.Errorf("agent_skill rows = %d, want 1", bound)
	}
}

func TestApplyResourceTemplate_ReferencesMode(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leaderID := createHandlerTestAgent(t, uniqueName("refs-lead"), nil)
	workerID := createHandlerTestAgent(t, uniqueName("refs-worker"), nil)
	var leaderName, workerName string
	if err := testPool.QueryRow(context.Background(), `SELECT name FROM agent WHERE id = $1`, leaderID).Scan(&leaderName); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(context.Background(), `SELECT name FROM agent WHERE id = $1`, workerID).Scan(&workerName); err != nil {
		t.Fatal(err)
	}

	squadName := uniqueName("apply-refs-squad")
	members := []map[string]any{
		memberRef(leaderName, "leader", nil),
		memberRef(workerName, "worker", nil),
	}
	before := countAgentsNamed(t, leaderName) + countAgentsNamed(t, workerName)
	code, resp, body := doApply(t, map[string]any{
		"template":          squadTemplate(squadName, "references", leaderName, members),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("references apply failed: code=%d resp=%+v body=%s", code, resp, body)
	}
	if len(resp.Created.Agents) != 0 || len(resp.Created.Squads) != 1 {
		t.Fatalf("references apply created = %+v (no new agents expected)", resp.Created)
	}
	t.Cleanup(func() {
		if len(resp.Created.Squads) > 0 {
			testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, resp.Created.Squads[0].ID)
		}
	})
	if countAgentsNamed(t, leaderName)+countAgentsNamed(t, workerName) != before {
		t.Fatal("references apply must not create agents")
	}
	// Squad wired to the existing agents.
	var dbLeaderID string
	if err := testPool.QueryRow(context.Background(),
		`SELECT leader_id FROM squad WHERE id = $1`, resp.Created.Squads[0].ID).Scan(&dbLeaderID); err != nil {
		t.Fatal(err)
	}
	if dbLeaderID != leaderID {
		t.Errorf("squad leader = %s, want %s", dbLeaderID, leaderID)
	}

	// Same template with members_mode=embedded override must be rejected.
	code, resp, _ = doApply(t, map[string]any{
		"template":          squadTemplate(squadName, "references", leaderName, members),
		"target_runtime_id": handlerTestRuntimeID(t),
		"members_mode":      "embedded",
	})
	if resp.Applied {
		t.Fatal("references template must not be applied in embedded mode")
	}
	if !hasErrorCode(resp.Errors, "TEMPLATE_INVALID") {
		t.Errorf("expected TEMPLATE_INVALID, got %+v", resp.Errors)
	}
}

func TestApplyResourceTemplate_IdempotentReplay(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-idem")
	key := uuid.NewString()
	body := map[string]any{
		"template": agentTemplate(name, nil), "target_runtime_id": handlerTestRuntimeID(t),
		"idempotency_key": key,
	}

	code, resp, _ := doApply(t, body)
	if code != http.StatusOK || !resp.Applied || resp.IdempotentReplay {
		t.Fatalf("first apply wrong: code=%d resp=%+v", code, resp)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)

	// Second apply with the same key: replay, no duplicate creation.
	code, resp2, _ := doApply(t, body)
	if code != http.StatusOK || !resp2.IdempotentReplay {
		t.Fatalf("replay wrong: code=%d resp=%+v", code, resp2)
	}
	if !resp2.Applied {
		t.Error("replayed response should report applied")
	}
	if countAgentsNamed(t, name) != 1 {
		t.Fatal("replayed apply must not create a second agent")
	}
}

// ---------------------------------------------------------------------------
// CLO-250 DEF-4 / DEF-5: top-level overrides consumption
// (architect ruling 2026-08-06: zero migration; squad model/permission_mode
// must be rejected with CONFIG_NOT_ALLOWED, not silently ignored)
// ---------------------------------------------------------------------------

// TestApplyResourceTemplate_SquadOverrides verifies the squad row consumes
// top-level overrides.{Description, Instructions} (CLO-250 DEF-4 regression).
func TestApplyResourceTemplate_SquadOverrides(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	squadName := uniqueName("apply-squad-ov")
	leaderName := uniqueName("apply-ov-lead")
	members := []map[string]any{
		memberRef("lead", "leader", map[string]any{
			"name": leaderName, "description": "l", "instructions": "",
			"thinking_level": "", "service_tier": "", "permission_mode": "private",
			"custom_args": []string{}, "skills": []any{}, "custom_env_keys": []any{}, "mcp_servers": []any{},
		}),
	}
	code, resp, body := doApply(t, map[string]any{
		"template":          squadTemplate(squadName, "embedded", "lead", members),
		"target_runtime_id": handlerTestRuntimeID(t),
		"overrides": map[string]any{
			"description":  "override-description",
			"instructions": "override-instructions",
		},
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("squad apply with overrides failed: code=%d resp=%+v body=%s", code, resp, body)
	}
	if len(resp.Created.Squads) != 1 {
		t.Fatalf("created squads = %+v", resp.Created.Squads)
	}
	squadID := resp.Created.Squads[0].ID
	for _, a := range resp.Created.Agents {
		cleanupAgentByID(t, a.ID)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})

	var description, instructions string
	if err := testPool.QueryRow(context.Background(),
		`SELECT description, instructions FROM squad WHERE id = $1`, squadID).
		Scan(&description, &instructions); err != nil {
		t.Fatalf("load squad: %v", err)
	}
	if description != "override-description" {
		t.Errorf("squad description = %q, want override-description", description)
	}
	if instructions != "override-instructions" {
		t.Errorf("squad instructions = %q, want override-instructions", instructions)
	}
}

// TestApplyResourceTemplate_SquadModelPermissionConfigNotAllowed verifies
// that model / permission_mode overrides on a squad template fail with
// CONFIG_NOT_ALLOWED instead of being silently ignored or written to dead
// columns (architect ruling).
func TestApplyResourceTemplate_SquadModelPermissionConfigNotAllowed(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	squadName := uniqueName("apply-squad-na")
	leaderName := uniqueName("apply-na-lead")
	members := []map[string]any{
		memberRef("lead", "leader", map[string]any{
			"name": leaderName, "description": "l", "instructions": "",
			"thinking_level": "", "service_tier": "", "permission_mode": "private",
			"custom_args": []string{}, "skills": []any{}, "custom_env_keys": []any{}, "mcp_servers": []any{},
		}),
	}
	code, resp, body := doApply(t, map[string]any{
		"template":          squadTemplate(squadName, "embedded", "lead", members),
		"target_runtime_id": handlerTestRuntimeID(t),
		"overrides": map[string]any{
			"model":           "gpt-4o",
			"permission_mode": "public_to",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("apply code = %d, want 200 (validation result in body): %s", code, body)
	}
	if resp.Applied {
		t.Fatal("apply must not succeed when squad overrides carry model/permission_mode")
	}
	if len(resp.Created.Agents) != 0 || len(resp.Created.Squads) != 0 {
		t.Fatalf("nothing must be created: %+v", resp.Created)
	}
	paths := map[string]bool{}
	for _, e := range resp.Errors {
		if e.Code == resourcetmpl.ErrorCode(CodeConfigNotAllowed) {
			paths[e.Path] = true
		}
	}
	if !paths["overrides.model"] {
		t.Errorf("missing CONFIG_NOT_ALLOWED for overrides.model; errors = %+v", resp.Errors)
	}
	if !paths["overrides.permission_mode"] {
		t.Errorf("missing CONFIG_NOT_ALLOWED for overrides.permission_mode; errors = %+v", resp.Errors)
	}
}

// TestApplyResourceTemplate_AgentTopLevelOverrides verifies top-level
// overrides.{Description, Instructions, Model, PermissionMode} are consumed
// for agent templates (the top-level resource of the apply).
func TestApplyResourceTemplate_AgentTopLevelOverrides(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-top-ov")
	code, resp, body := doApply(t, map[string]any{
		"template":          agentTemplate(name, nil),
		"target_runtime_id": handlerTestRuntimeID(t),
		"overrides": map[string]any{
			"description":     "top-desc",
			"instructions":    "top-inst",
			"model":           "top-model",
			"permission_mode": "public_to",
		},
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("apply failed: code=%d resp=%+v body=%s", code, resp, body)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	var description, instructions, model, permissionMode string
	if err := testPool.QueryRow(context.Background(),
		`SELECT description, instructions, COALESCE(model, ''), permission_mode FROM agent WHERE id = $1`, resp.Created.Agents[0].ID).
		Scan(&description, &instructions, &model, &permissionMode); err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if description != "top-desc" {
		t.Errorf("agent description = %q, want top-desc", description)
	}
	if instructions != "top-inst" {
		t.Errorf("agent instructions = %q, want top-inst", instructions)
	}
	if model != "top-model" {
		t.Errorf("agent model = %q, want top-model", model)
	}
	if permissionMode != "public_to" {
		t.Errorf("agent permission_mode = %q, want public_to", permissionMode)
	}
}

// TestApplyResourceTemplate_AgentTopLevelPermissionOverride verifies the
// top-level overrides.permission_mode is consumed for agent templates when
// the spec does not set one (CLO-250 DEF-5 regression).
func TestApplyResourceTemplate_AgentTopLevelPermissionOverride(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-top-pm")
	// Build a template whose spec omits permission_mode entirely so the
	// top-level override is the only source (the helper defaults to private).
	tmpl := agentTemplate(name, nil)
	delete(tmpl["spec"].(map[string]any)["agent"].(map[string]any), "permission_mode")
	code, resp, body := doApply(t, map[string]any{
		"template":          tmpl,
		"target_runtime_id": handlerTestRuntimeID(t),
		"overrides":         map[string]any{"permission_mode": "public_to"},
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("apply failed: code=%d resp=%+v body=%s", code, resp, body)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	var permissionMode string
	if err := testPool.QueryRow(context.Background(),
		`SELECT permission_mode FROM agent WHERE id = $1`, resp.Created.Agents[0].ID).Scan(&permissionMode); err != nil {
		t.Fatalf("load agent: %v", err)
	}
	if permissionMode != "public_to" {
		t.Errorf("agent permission_mode = %q, want public_to (top-level override consumed)", permissionMode)
	}
}

// TestApplyResourceTemplate_PermissionOverridePrecedence pins the fallback
// order per the architect ruling: per-ref override > top-level override >
// spec value > private default.
func TestApplyResourceTemplate_PermissionOverridePrecedence(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	// 1) Top-level override beats the spec value.
	name := uniqueName("apply-pm-top")
	code, resp, _ := doApply(t, map[string]any{
		"template":          agentTemplate(name, map[string]any{"permission_mode": "public_to"}),
		"target_runtime_id": handlerTestRuntimeID(t),
		"overrides":         map[string]any{"permission_mode": "private"},
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("top-level-wins apply failed: code=%d resp=%+v", code, resp)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	var pm string
	if err := testPool.QueryRow(context.Background(),
		`SELECT permission_mode FROM agent WHERE id = $1`, resp.Created.Agents[0].ID).Scan(&pm); err != nil {
		t.Fatal(err)
	}
	if pm != "private" {
		t.Errorf("top-level override should beat spec: permission_mode = %q, want private", pm)
	}

	// 2) Per-ref override beats the top-level override (kind=agent template;
	// per-ref is keyed by the agent name per the Q10 ruling).
	perRefName := uniqueName("apply-pm-perref")
	code, resp, _ = doApply(t, map[string]any{
		"template":          agentTemplate(perRefName, nil),
		"target_runtime_id": handlerTestRuntimeID(t),
		"overrides": map[string]any{
			"permission_mode": "private",
			"agents": map[string]any{
				perRefName: map[string]any{"permission_mode": "public_to"},
			},
		},
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("per-ref-wins apply failed: code=%d resp=%+v", code, resp)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	if err := testPool.QueryRow(context.Background(),
		`SELECT permission_mode FROM agent WHERE id = $1`, resp.Created.Agents[0].ID).Scan(&pm); err != nil {
		t.Fatal(err)
	}
	if pm != "public_to" {
		t.Errorf("per-ref override should beat top-level: permission_mode = %q, want public_to", pm)
	}
}

// TestApplyResourceTemplate_AgentTopLevelNameOverride covers the Web wizard's
// rename tweak (CLO-399): the frontend sends overrides.name for a single
// agent template and expects the created agent to carry that name instead of
// the spec name.
func TestApplyResourceTemplate_AgentTopLevelNameOverride(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-name-ov")
	renamed := uniqueName("apply-name-renamed")
	code, resp, body := doApply(t, map[string]any{
		"template":          agentTemplate(name, nil),
		"target_runtime_id": handlerTestRuntimeID(t),
		"overrides":         map[string]any{"name": renamed},
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("apply failed: code=%d resp=%+v body=%s", code, resp, body)
	}
	if len(resp.Created.Agents) != 1 || resp.Created.Agents[0].Name != renamed {
		t.Fatalf("created agents = %+v, want single agent named %q", resp.Created.Agents, renamed)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)
	if countAgentsNamed(t, name) != 0 {
		t.Fatal("spec name must not be used when overrides.name is set")
	}
}

// TestApplyResourceTemplate_SquadTopLevelNameOverride covers the same Web
// wizard rename tweak for squad templates (CLO-399): overrides.name renames
// the materialised squad.
func TestApplyResourceTemplate_SquadTopLevelNameOverride(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	squadName := uniqueName("apply-sq-name-ov")
	renamed := uniqueName("apply-sq-renamed")
	leaderName := uniqueName("apply-sq-name-lead")
	members := []map[string]any{
		memberRef("lead", "leader", map[string]any{
			"name": leaderName, "description": "", "instructions": "",
			"thinking_level": "", "service_tier": "", "permission_mode": "private",
			"custom_args": []string{}, "skills": []any{}, "custom_env_keys": []any{}, "mcp_servers": []any{},
		}),
	}
	code, resp, body := doApply(t, map[string]any{
		"template":          squadTemplate(squadName, "embedded", "lead", members),
		"target_runtime_id": handlerTestRuntimeID(t),
		"overrides":         map[string]any{"name": renamed},
	})
	if code != http.StatusOK || !resp.Applied {
		t.Fatalf("squad apply with name override failed: code=%d resp=%+v body=%s", code, resp, body)
	}
	if len(resp.Created.Squads) != 1 || resp.Created.Squads[0].Name != renamed {
		t.Fatalf("created squads = %+v, want single squad named %q", resp.Created.Squads, renamed)
	}
	squadID := resp.Created.Squads[0].ID
	for _, a := range resp.Created.Agents {
		cleanupAgentByID(t, a.ID)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})
	var got string
	if err := testPool.QueryRow(context.Background(),
		`SELECT name FROM squad WHERE id = $1`, squadID).Scan(&got); err != nil {
		t.Fatalf("load squad: %v", err)
	}
	if got != renamed {
		t.Errorf("squad name = %q, want %q", got, renamed)
	}
}
