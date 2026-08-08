package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/resourcetmpl"
)

// TestCreateSquad_SavesAllMembers verifies CLO-419: a squad created with
// several members persists every one of them (the frontend create-squad dialog
// submits the whole selection in one shot; the handler must not drop any).
func TestCreateSquad_SavesAllMembers(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leaderID := createHandlerTestAgent(t, "multi-leader", nil)
	workerAID := createHandlerTestAgent(t, "multi-worker-a", nil)
	workerBID := createHandlerTestAgent(t, "multi-worker-b", nil)

	w := httptest.NewRecorder()
	r := squadScopeReq("", "POST", "/api/squads", map[string]any{
		"name":      uniqueName("multi-squad"),
		"leader_id": leaderID,
		"members": []map[string]any{
			{"member_type": "agent", "member_id": workerAID, "role": "core-dev"},
			{"member_type": "agent", "member_id": workerBID, "role": "reviewer"},
		},
	}, nil)
	testHandler.CreateSquad(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("CreateSquad: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp SquadResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode squad: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad_member WHERE squad_id = $1`, resp.ID)
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, resp.ID)
	})

	rows, err := testPool.Query(context.Background(),
		`SELECT member_type, member_id::text, role FROM squad_member WHERE squad_id = $1 ORDER BY created_at`,
		resp.ID)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	defer rows.Close()
	var got []map[string]string
	for rows.Next() {
		var mt, mid, role string
		if err := rows.Scan(&mt, &mid, &role); err != nil {
			t.Fatalf("scan member: %v", err)
		}
		got = append(got, map[string]string{"member_type": mt, "member_id": mid, "role": role})
	}
	if len(got) != 3 {
		t.Fatalf("member rows = %d, want 3 (leader + 2 workers)", len(got))
	}
	roles := map[string]int{}
	for _, m := range got {
		roles[m["role"]]++
	}
	if roles["leader"] != 1 {
		t.Errorf("leader role count = %d, want 1 (roles: %v)", roles["leader"], got)
	}
	if roles["core-dev"] != 1 || roles["reviewer"] != 1 {
		t.Errorf("worker roles = %v, want core-dev + reviewer", roles)
	}
}

// TestCreateSquad_InvalidMemberFailsWhole verifies CLO-419 atomicity: when any
// requested member is invalid, the whole create is rejected with failed_members
// and no squad is created.
func TestCreateSquad_InvalidMemberFailsWhole(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leaderID := createHandlerTestAgent(t, "atomic-leader", nil)
	// A member without a role must fail the whole request (CLO-418).
	w := httptest.NewRecorder()
	r := squadScopeReq("", "POST", "/api/squads", map[string]any{
		"name":      uniqueName("atomic-squad"),
		"leader_id": leaderID,
		"members": []map[string]any{
			{"member_type": "agent", "member_id": createHandlerTestAgent(t, "atomic-ok", nil)},
		},
	}, nil)
	testHandler.CreateSquad(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("CreateSquad with role-less member: expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	failed, ok := body["failed_members"].([]any)
	if !ok || len(failed) != 1 {
		t.Fatalf("failed_members = %v, want 1 entry", body["failed_members"])
	}

	// No squad must exist with that name (all-or-nothing).
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM squad WHERE name = $1`, "atomic-squad").Scan(&n); err != nil {
		t.Fatalf("count squads: %v", err)
	}
}

// TestAddSquadMember_RoleRequired verifies CLO-418: AddSquadMember rejects a
// member without a role.
func TestAddSquadMember_RoleRequired(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	leaderID := createHandlerTestAgent(t, "role-leader", nil)
	squad := createSquadAs(t, "", uniqueName("role-squad"), leaderID)

	w := httptest.NewRecorder()
	testHandler.AddSquadMember(w, squadScopeReq("", "POST", "/api/squads/members", map[string]any{
		"member_type": "agent",
		"member_id":   createHandlerTestAgent(t, "role-worker", nil),
		"role":        "",
	}, map[string]string{"id": squad.ID}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("AddSquadMember with empty role: expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestExportSquadTemplate_DefaultEmptyMemberRole verifies CLO-418: a legacy
// member row with an empty role exports with a defaulted role + warning, so the
// template stays valid.
func TestExportSquadTemplate_DefaultEmptyMemberRole(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	squadName := uniqueName("export-empty-role")
	leaderID := createHandlerTestAgent(t, "empty-role-leader", nil)
	workerID := createHandlerTestAgent(t, "empty-role-worker", nil)

	// Seed a squad whose worker member has an EMPTY role (legacy dirty data).
	var squadID string
	if err := testPool.QueryRow(context.Background(), `
		INSERT INTO squad (workspace_id, name, leader_id, creator_id)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, testWorkspaceID, squadName, leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
	})
	for _, m := range [][3]string{{"agent", leaderID, "leader"}, {"agent", workerID, ""}} {
		if _, err := testPool.Exec(context.Background(), `
			INSERT INTO squad_member (squad_id, member_type, member_id, role)
			VALUES ($1, $2, $3, $4)
		`, squadID, m[0], m[1], m[2]); err != nil {
			t.Fatalf("add squad member: %v", err)
		}
	}

	w := httptest.NewRecorder()
	testHandler.ExportResourceTemplate(w, newRequest("POST", "/api/templates/export?workspace_id="+testWorkspaceID, map[string]any{
		"kind": "squad", "resource_id": squadID,
	}))
	resp := decodeExport(t, w)

	// Every member role must be non-empty and valid.
	var defaulted bool
	for _, mem := range resp.Template.Spec.Squad.Members {
		if mem.Role == "" {
			t.Errorf("member %q exported with empty role", mem.Ref)
		}
		if mem.Role == resourcetmpl.RoleMember {
			defaulted = true
		}
	}
	if !defaulted {
		t.Errorf("expected an empty-role member to be defaulted to %q; members: %+v",
			resourcetmpl.RoleMember, resp.Template.Spec.Squad.Members)
	}
	foundWarn := false
	for _, warn := range resp.Warnings {
		if warn.Code == "MEMBER_ROLE_DEFAULTED" {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected a MEMBER_ROLE_DEFAULTED warning, got %+v", resp.Warnings)
	}

	// Round trip: the exported template must pass the package validator.
	raw, err := json.Marshal(resp.Template)
	if err != nil {
		t.Fatalf("marshal template: %v", err)
	}
	if report := resourcetmpl.Validate(raw); report.HasErrors() {
		t.Fatalf("exported template invalid: %v", report.Errors)
	}
}

// TestApplyResourceTemplate_DefaultConflictPolicyFail verifies CLO-417: apply
// with no conflict_policy and a pre-existing same-name resource returns 409
// NAME_CONFLICT (fail is the default; rename is opt-in).
func TestApplyResourceTemplate_DefaultConflictPolicyFail(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	name := uniqueName("apply-default-fail")
	code, resp, body := doApply(t, map[string]any{
		"template":          agentTemplate(name, nil),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if code != http.StatusOK {
		t.Fatalf("first apply failed: %d body %s", code, body)
	}
	cleanupAgentByID(t, resp.Created.Agents[0].ID)

	// No conflict_policy: the same name now collides → must be 409 + NAME_CONFLICT.
	code, resp, body = doApply(t, map[string]any{
		"template":          agentTemplate(name, nil),
		"target_runtime_id": handlerTestRuntimeID(t),
	})
	if code != http.StatusConflict {
		t.Fatalf("default-policy apply on conflict: status = %d, want 409 (body %s)", code, body)
	}
	if len(resp.Errors) == 0 || resp.Errors[0].Code != resourcetmpl.ErrorCode(CodeNameConflict) {
		t.Fatalf("errors = %+v, want NAME_CONFLICT", resp.Errors)
	}
	if countAgentsNamed(t, name) != 1 {
		t.Fatal("fail policy must not create a second agent")
	}
}

var _ = chi.RouteCtxKey
