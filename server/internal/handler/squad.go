package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/analytics"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// ── Response types ──────────────────────────────────────────────────────────

type SquadResponse struct {
	ID                     string                       `json:"id"`
	WorkspaceID            string                       `json:"workspace_id"`
	Name                   string                       `json:"name"`
	Description            string                       `json:"description"`
	Instructions           string                       `json:"instructions"`
	AvatarURL              *string                      `json:"avatar_url"`
	LeaderID               string                       `json:"leader_id"`
	CreatorID              string                       `json:"creator_id"`
	CreatedAt              string                       `json:"created_at"`
	UpdatedAt              string                       `json:"updated_at"`
	ArchivedAt             *string                      `json:"archived_at"`
	ArchivedBy             *string                      `json:"archived_by"`
	ParentSquadID          *string                      `json:"parent_squad_id"`
	UpgradeOnMemberMention bool                         `json:"upgrade_on_member_mention"`
	MemberCount            int                          `json:"member_count"`
	MemberPreview          []SquadMemberPreviewResponse `json:"member_preview"`
	ChildSquads            []SquadChildResponse         `json:"child_squads"`
}

type SquadMemberPreviewResponse struct {
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
	Role       string `json:"role"`
}

// SquadChildResponse is the lightweight child-squad summary embedded in a
// parent squad's GET/list payload (LIU-8 squad nesting).
type SquadChildResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	MemberCount int    `json:"member_count"`
}

type squadMemberSummary struct {
	count   int
	preview []SquadMemberPreviewResponse
}

type SquadMemberResponse struct {
	ID         string `json:"id"`
	SquadID    string `json:"squad_id"`
	MemberType string `json:"member_type"`
	MemberID   string `json:"member_id"`
	Role       string `json:"role"`
	CreatedAt  string `json:"created_at"`
}

// ── Converters ──────────────────────────────────────────────────────────────

func squadToResponse(s db.Squad) SquadResponse {
	return SquadResponse{
		ID:                     uuidToString(s.ID),
		WorkspaceID:            uuidToString(s.WorkspaceID),
		Name:                   s.Name,
		Description:            s.Description,
		Instructions:           s.Instructions,
		AvatarURL:              textToPtr(s.AvatarUrl),
		LeaderID:               uuidToString(s.LeaderID),
		CreatorID:              uuidToString(s.CreatorID),
		CreatedAt:              timestampToString(s.CreatedAt),
		UpdatedAt:              timestampToString(s.UpdatedAt),
		ArchivedAt:             timestampToPtr(s.ArchivedAt),
		ArchivedBy:             uuidToPtr(s.ArchivedBy),
		ParentSquadID:          uuidToPtr(s.ParentSquadID),
		UpgradeOnMemberMention: s.UpgradeOnMemberMention,
		MemberPreview:          []SquadMemberPreviewResponse{},
		ChildSquads:            []SquadChildResponse{},
	}
}

func squadMemberToResponse(m db.SquadMember) SquadMemberResponse {
	return SquadMemberResponse{
		ID:         uuidToString(m.ID),
		SquadID:    uuidToString(m.SquadID),
		MemberType: m.MemberType,
		MemberID:   uuidToString(m.MemberID),
		Role:       m.Role,
		CreatedAt:  timestampToString(m.CreatedAt),
	}
}

func addSquadMemberPreview(summary *squadMemberSummary, memberType string, memberID pgtype.UUID, role string) {
	summary.count++
	if len(summary.preview) >= 3 {
		return
	}
	summary.preview = append(summary.preview, SquadMemberPreviewResponse{
		MemberType: memberType,
		MemberID:   uuidToString(memberID),
		Role:       role,
	})
}

func applySquadMemberSummary(resp *SquadResponse, summary *squadMemberSummary) {
	if summary == nil {
		return
	}
	resp.MemberCount = summary.count
	resp.MemberPreview = summary.preview
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// canManageSquad reports whether the member may mutate the squad. Workspace
// owner/admin manage every squad; a regular member manages only the squads
// they created. Squads stay creator-scoped for management while remaining
// visible workspace-wide (ListSquads is unfiltered). Mirrors the front-end
// per-squad `canManage` gate so the UI and API agree on who can rename / add
// members / archive (MUL-4223).
func canManageSquad(member db.Member, squad db.Squad) bool {
	if roleAllowed(member.Role, "owner", "admin") {
		return true
	}
	return uuidToString(squad.CreatorID) == uuidToString(member.UserID)
}

// memberCanWireAgent reports whether the acting member may attach the given
// agent to a squad (as leader or worker). Workspace owner/admin may wire any
// workspace agent — their management surface is unchanged. A regular member
// (a creator managing their own squad) may only wire agents they can
// @-trigger: canInvokeAgent judged as the member themselves, so public_to
// agents on their allow-list and their own private agents pass, while other
// members' private / non-allow-listed agents are rejected. This stops a
// creator from smuggling an agent they cannot invoke into a squad and reaching
// it through squad routing (MUL-4223).
func (h *Handler) memberCanWireAgent(ctx context.Context, member db.Member, agent db.Agent, workspaceID string) bool {
	if roleAllowed(member.Role, "owner", "admin") {
		return true
	}
	uid := uuidToString(member.UserID)
	return h.canInvokeAgent(ctx, agent, "member", uid, uid, workspaceID)
}

// squadMemberRoleLeader is the role the squad leader carries in squad_member
// (kept in sync with resourcetmpl.RoleLeader used by template validation).
const squadMemberRoleLeader = "leader"

// normalizeSquadMemberRole validates a squad-member role. Roles are
// free-form labels in the product (frontend AddMemberDialog / create-squad
// accept arbitrary text such as "Reviewer" or "Frontend Lead"); the only
// hard constraints here are CLO-418: the role must be present and
// non-whitespace, and no longer than a sane bound so a typo can never
// produce an empty role that later breaks template export/validation. The
// canonical "leader" role is reserved for the squad leader.
func normalizeSquadMemberRole(role string) (string, bool) {
	r := strings.TrimSpace(role)
	if r == "" || len(r) > 200 {
		return "", false
	}
	return r, true
}

// loadSquadInWorkspace loads a squad scoped to the current workspace.
func (h *Handler) loadSquadInWorkspace(w http.ResponseWriter, r *http.Request) (db.Squad, string, bool) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	squadID := chi.URLParam(r, "id")
	squadUUID, ok := parseUUIDOrBadRequest(w, squadID, "squad id")
	if !ok {
		return db.Squad{}, "", false
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return db.Squad{}, "", false
	}
	squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{
		ID:          squadUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "squad not found")
		return db.Squad{}, "", false
	}
	return squad, workspaceID, true
}

func (h *Handler) loadSquadMemberSummary(ctx context.Context, squadID pgtype.UUID) (*squadMemberSummary, error) {
	rows, err := h.Queries.ListSquadMemberPreviewRowsBySquad(ctx, squadID)
	if err != nil {
		return nil, err
	}
	summary := &squadMemberSummary{}
	for _, row := range rows {
		addSquadMemberPreview(summary, row.MemberType, row.MemberID, row.Role)
	}
	return summary, nil
}

// loadChildSquadSummaries returns the child-squad summaries (id, name, member
// count) for a squad. v1 nesting is one level deep, so a squad's children
// never have children of their own.
func (h *Handler) loadChildSquadSummaries(ctx context.Context, squadID pgtype.UUID) ([]SquadChildResponse, error) {
	rows, err := h.Queries.ListChildSquadSummaries(ctx, squadID)
	if err != nil {
		return nil, err
	}
	out := make([]SquadChildResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, SquadChildResponse{
			ID:          uuidToString(row.ID),
			Name:        row.Name,
			MemberCount: int(row.MemberCount),
		})
	}
	return out, nil
}

// loadChildSquadSummariesByWorkspace batches child summaries for every squad
// in a workspace, keyed by the parent squad id. Used by ListSquads so the
// whole list payload carries child_squads in one pass.
func (h *Handler) loadChildSquadSummariesByWorkspace(ctx context.Context, workspaceID pgtype.UUID) (map[string][]SquadChildResponse, error) {
	rows, err := h.Queries.ListChildSquadSummariesByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	byParent := make(map[string][]SquadChildResponse, len(rows))
	for _, row := range rows {
		parentID := uuidToString(row.ParentSquadID)
		byParent[parentID] = append(byParent[parentID], SquadChildResponse{
			ID:          uuidToString(row.ID),
			Name:        row.Name,
			MemberCount: int(row.MemberCount),
		})
	}
	return byParent, nil
}

func (h *Handler) squadToResponseWithPreview(ctx context.Context, squad db.Squad) (SquadResponse, error) {
	resp := squadToResponse(squad)
	summary, err := h.loadSquadMemberSummary(ctx, squad.ID)
	if err != nil {
		return resp, err
	}
	applySquadMemberSummary(&resp, summary)
	children, err := h.loadChildSquadSummaries(ctx, squad.ID)
	if err != nil {
		return resp, err
	}
	resp.ChildSquads = children
	return resp, nil
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (h *Handler) ListSquads(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	squads, err := h.Queries.ListSquads(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list squads")
		return
	}

	previewRows, err := h.Queries.ListSquadMemberPreviewRows(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list squad member preview")
		return
	}
	summaries := make(map[string]*squadMemberSummary, len(squads))
	for _, row := range previewRows {
		squadID := uuidToString(row.SquadID)
		summary := summaries[squadID]
		if summary == nil {
			summary = &squadMemberSummary{}
			summaries[squadID] = summary
		}
		addSquadMemberPreview(summary, row.MemberType, row.MemberID, row.Role)
	}

	// Child-squad summaries (LIU-8 nesting) batched across the whole list so
	// every payload carries child_squads without an N+1.
	childByParent, err := h.loadChildSquadSummariesByWorkspace(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list child squad summaries")
		return
	}

	resp := make([]SquadResponse, len(squads))
	for i, s := range squads {
		resp[i] = squadToResponse(s)
		applySquadMemberSummary(&resp[i], summaries[uuidToString(s.ID)])
		resp[i].ChildSquads = childByParent[uuidToString(s.ID)]
		if resp[i].ChildSquads == nil {
			resp[i].ChildSquads = []SquadChildResponse{}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) CreateSquad(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	// Any workspace member can create a squad and becomes its creator
	// (CreatorID below). This aligns squads with agents/projects, which are
	// also member-creatable; management stays creator-scoped (MUL-4223).
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	var req struct {
		Name                   string   `json:"name"`
		Description            string   `json:"description"`
		LeaderID               string   `json:"leader_id"`
		AvatarURL              *string  `json:"avatar_url"`
		IncludedSquadIDs       []string `json:"included_squad_ids"`
		// F1 (LIU-9 子任务A): optional members added atomically with the create.
		// Each entry is validated before anything is written; any invalid entry
		// fails the whole request and the response carries failed_members (B07).
		Members []struct {
			MemberType string `json:"member_type"`
			MemberID   string `json:"member_id"`
			Role       string `json:"role"`
		} `json:"members"`
		// Per-squad safety valve for the F3 member-mention upgrade (default true).
		UpgradeOnMemberMention *bool `json:"upgrade_on_member_mention"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.LeaderID == "" {
		writeError(w, http.StatusBadRequest, "leader_id is required")
		return
	}

	leaderUUID, ok := parseUUIDOrBadRequest(w, req.LeaderID, "leader_id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	// Validate leader is an agent in this workspace.
	leaderAgent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
		ID:          leaderUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "leader must be a valid agent in this workspace")
		return
	}
	// A non-admin creator may only lead their squad with an agent they can
	// @-trigger; admins may wire any workspace agent (MUL-4223).
	if !h.memberCanWireAgent(r.Context(), member, leaderAgent, workspaceID) {
		writeError(w, http.StatusForbidden, "you can only use an agent you have access to as leader")
		return
	}

	// F1 (LIU-9 子任务A): validate every requested member BEFORE any write.
	// All-or-nothing semantics (AC-1.3): any invalid member fails the whole
	// create and the 400 response carries the failed_members list so the
	// front-end can highlight exactly which entries were rejected (B07).
	type validatedMember struct {
		memberType string
		memberID   pgtype.UUID
		role       string
	}
	var (
		failedMembers []map[string]string
		members       = make([]validatedMember, 0, len(req.Members))
	)
	seenMembers := make(map[string]struct{}, len(req.Members))
	for _, m := range req.Members {
		fail := func(reason string) {
			failedMembers = append(failedMembers, map[string]string{
				"member_type": m.MemberType,
				"member_id":   m.MemberID,
				"reason":      reason,
			})
		}
		if m.MemberType != "agent" && m.MemberType != "member" {
			fail("member_type must be 'agent' or 'member'")
			continue
		}
		memberUUID, err := util.ParseUUID(m.MemberID)
		if err != nil {
			fail("invalid member_id")
			continue
		}
		// AC-1.5: duplicates (including re-listing the leader) are conflicts,
		// never silently merged.
		key := m.MemberType + ":" + uuidToString(memberUUID)
		if _, dup := seenMembers[key]; dup {
			fail("duplicate member")
			continue
		}
		if m.MemberType == "agent" && uuidToString(memberUUID) == uuidToString(leaderUUID) {
			fail("leader is already a member")
			continue
		}
		// Role is required for every member (CLO-418): an empty role would
		// otherwise be persisted and later break template export/validation.
		role, ok := normalizeSquadMemberRole(m.Role)
		if !ok {
			fail("role is required")
			continue
		}
		if m.MemberType == "agent" {
			agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
				ID: memberUUID, WorkspaceID: wsUUID,
			})
			if err != nil {
				fail("agent not found in this workspace")
				continue
			}
			if !h.memberCanWireAgent(r.Context(), member, agent, workspaceID) {
				fail("you can only add an agent you have access to")
				continue
			}
		} else {
			if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
				UserID: memberUUID, WorkspaceID: wsUUID,
			}); err != nil {
				fail("member not found in this workspace")
				continue
			}
		}
		seenMembers[key] = struct{}{}
		members = append(members, validatedMember{
			memberType: m.MemberType,
			memberID:   memberUUID,
			role:       role,
		})
	}
	if len(failedMembers) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":          "one or more members are invalid; no squad was created",
			"failed_members": failedMembers,
		})
		return
	}

	// Validate included_squad_ids (LIU-8 squad nesting, v1 one level): every
	// included squad must exist in this workspace, be unarchived, and not
	// already belong to another parent. Any invalid id fails the whole
	// request with a 400 before anything is written.
	childUUIDs, err := h.validateIncludedSquads(r, wsUUID, req.IncludedSquadIDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	avatarURL := pgtype.Text{}
	if req.AvatarURL != nil {
		avatarURL = pgtype.Text{String: *req.AvatarURL, Valid: true}
	}

	// Create the parent squad and attach every included child in a single
	// transaction so a failure mid-way cannot leave a half-nested squad.
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start squad create transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	upgradeOnMemberMention := pgtype.Bool{Valid: true, Bool: true}
	if req.UpgradeOnMemberMention != nil {
		upgradeOnMemberMention = pgtype.Bool{Valid: true, Bool: *req.UpgradeOnMemberMention}
	}
	squad, err := qtx.CreateSquad(r.Context(), db.CreateSquadParams{
		WorkspaceID:            wsUUID,
		Name:                   req.Name,
		Description:            req.Description,
		LeaderID:               leaderUUID,
		CreatorID:              member.UserID,
		AvatarUrl:              avatarURL,
		UpgradeOnMemberMention: upgradeOnMemberMention,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create squad")
		return
	}

	// Auto-add leader as a member with role "leader".
	if _, err := qtx.AddSquadMember(r.Context(), db.AddSquadMemberParams{
		SquadID:    squad.ID,
		MemberType: "agent",
		MemberID:   leaderUUID,
		Role:       "leader",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to add squad leader")
		return
	}

	// F1: add every validated member in the same transaction (F2: each may
	// carry an optional role). Duplicates were rejected above, so a unique
	// violation here would indicate a race — fail the whole create, never
	// produce a half-built squad (AC-1.3/1.5).
	for _, m := range members {
		if _, err := qtx.AddSquadMember(r.Context(), db.AddSquadMemberParams{
			SquadID:    squad.ID,
			MemberType: m.memberType,
			MemberID:   m.memberID,
			Role:       m.role,
		}); err != nil {
			if isUniqueViolation(err) {
				writeError(w, http.StatusConflict, "member already in squad")
				return
			}
			writeError(w, http.StatusInternalServerError, "failed to add squad member")
			return
		}
	}

	for _, childID := range childUUIDs {
		if _, err := qtx.SetSquadParent(r.Context(), db.SetSquadParentParams{
			ID:            childID,
			ParentSquadID: squad.ID,
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to attach included squad")
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit squad create transaction")
		return
	}

	resp, err := h.squadToResponseWithPreview(r.Context(), squad)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load squad member preview")
		return
	}
	h.publish(protocol.EventSquadCreated, workspaceID, "member", uuidToString(member.UserID), map[string]any{"squad": resp})
	obsmetrics.RecordEvent(h.Analytics, h.Metrics, analytics.SquadCreated(
		uuidToString(member.UserID),
		workspaceID,
		uuidToString(squad.ID),
		1,
	))
	writeJSON(w, http.StatusCreated, resp)
}

// validateIncludedSquads validates the included_squad_ids payload for squad
// nesting (LIU-8, v1 one level). It returns the child squad UUIDs in request
// order (duplicates removed), or an error describing the first invalid id.
// Rules, per the acceptance criteria:
//   - every id must parse as a UUID and resolve to a squad in this workspace;
//   - included squads must not be archived;
//   - included squads must not already belong to another parent (a squad can
//     only be merged into one parent, and nesting is one level deep).
func (h *Handler) validateIncludedSquads(r *http.Request, wsUUID pgtype.UUID, ids []string) ([]pgtype.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, raw := range ids {
		if _, dup := seen[raw]; dup {
			continue
		}
		seen[raw] = struct{}{}
		unique = append(unique, raw)
	}

	childUUIDs := make([]pgtype.UUID, 0, len(unique))
	byID := make(map[string]pgtype.UUID, len(unique))
	for _, raw := range unique {
		u, err := util.ParseUUID(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid included_squad_ids entry %q", raw)
		}
		childUUIDs = append(childUUIDs, u)
		byID[raw] = u
	}

	rows, err := h.Queries.ListSquadsByIdsInWorkspace(r.Context(), db.ListSquadsByIdsInWorkspaceParams{
		ID:          childUUIDs,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to validate included squads")
	}
	found := make(map[string]db.Squad, len(rows))
	for _, row := range rows {
		found[uuidToString(row.ID)] = row
	}
	for _, raw := range unique {
		squad, ok := found[raw]
		if !ok {
			return nil, fmt.Errorf("included squad %q does not exist in this workspace", raw)
		}
		if squad.ArchivedAt.Valid {
			return nil, fmt.Errorf("included squad %q is archived", raw)
		}
		if squad.ParentSquadID.Valid {
			return nil, fmt.Errorf("included squad %q already belongs to another squad", raw)
		}
	}
	return childUUIDs, nil
}

func (h *Handler) GetSquad(w http.ResponseWriter, r *http.Request) {
	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	resp, err := h.squadToResponseWithPreview(r.Context(), squad)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load squad member preview")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) UpdateSquad(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageSquad(member, squad) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	var req struct {
		Name                   *string `json:"name"`
		Description            *string `json:"description"`
		Instructions           *string `json:"instructions"`
		LeaderID               *string `json:"leader_id"`
		AvatarURL              *string `json:"avatar_url"`
		UpgradeOnMemberMention *bool   `json:"upgrade_on_member_mention"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	params := db.UpdateSquadParams{ID: squad.ID}
	if req.Name != nil {
		params.Name = pgtype.Text{String: *req.Name, Valid: true}
	}
	if req.Description != nil {
		params.Description = pgtype.Text{String: *req.Description, Valid: true}
	}
	if req.Instructions != nil {
		params.Instructions = pgtype.Text{String: *req.Instructions, Valid: true}
	}
	if req.AvatarURL != nil {
		params.AvatarUrl = pgtype.Text{String: *req.AvatarURL, Valid: true}
	}
	if req.UpgradeOnMemberMention != nil {
		params.UpgradeOnMemberMention = pgtype.Bool{Valid: true, Bool: *req.UpgradeOnMemberMention}
	}
	if req.LeaderID != nil {
		lid, ok := parseUUIDOrBadRequest(w, *req.LeaderID, "leader_id")
		if !ok {
			return
		}
		// Validate new leader is an agent in workspace.
		newLeader, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
			ID: lid, WorkspaceID: wsUUID,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "leader must be a valid agent in this workspace")
			return
		}
		// A non-admin creator may only promote an agent they can @-trigger.
		if !h.memberCanWireAgent(r.Context(), member, newLeader, workspaceID) {
			writeError(w, http.StatusForbidden, "you can only use an agent you have access to as leader")
			return
		}
		// Ensure new leader is a squad member; auto-add if not.
		isMember, _ := h.Queries.IsSquadMember(r.Context(), db.IsSquadMemberParams{
			SquadID: squad.ID, MemberType: "agent", MemberID: lid,
		})
		if !isMember {
			h.Queries.AddSquadMember(r.Context(), db.AddSquadMemberParams{
				SquadID: squad.ID, MemberType: "agent", MemberID: lid, Role: "leader",
			})
		}
		params.LeaderID = lid
	}

	updated, err := h.Queries.UpdateSquad(r.Context(), params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update squad")
		return
	}

	resp, err := h.squadToResponseWithPreview(r.Context(), updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load squad member preview")
		return
	}
	h.publish(protocol.EventSquadUpdated, workspaceID, "member", requestUserID(r), map[string]any{"squad": resp})
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) DeleteSquad(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageSquad(member, squad) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}

	if squad.ArchivedAt.Valid {
		writeError(w, http.StatusBadRequest, "squad is already archived")
		return
	}

	// Transfer issues assigned to this squad to the leader agent.
	if err := h.Queries.TransferSquadAssignees(r.Context(), db.TransferSquadAssigneesParams{
		AssigneeID:   squad.ID,
		AssigneeID_2: squad.LeaderID,
	}); err != nil {
		slog.Warn("transfer squad assignees failed", "squad_id", uuidToString(squad.ID), "error", err)
	}

	// Mirror the issue-assignee transfer for autopilots that target this
	// squad. Without this, autopilot.assignee_id would still point at the
	// archived squad row and every subsequent dispatch would skip with
	// "assignee squad is archived" — visible to ops but useless to the
	// owner. Rewriting to the leader keeps the autopilot semantics
	// unchanged (Path A from MUL-2429 is leader-only execution anyway).
	if err := h.Queries.TransferSquadAutopilotsToLeader(r.Context(), db.TransferSquadAutopilotsToLeaderParams{
		AssigneeID:   squad.ID,
		AssigneeID_2: squad.LeaderID,
	}); err != nil {
		slog.Warn("transfer squad autopilots failed", "squad_id", uuidToString(squad.ID), "error", err)
	}

	userID := requestUserID(r)
	userUUID, _ := parseUUIDOrBadRequest(w, userID, "user_id")

	// Archive the squad and detach its children in one transaction: per the
	// lifecycle acceptance criteria, archiving a parent must clear its
	// children's parent_squad_id so they become independent squads again
	// (they are NOT archived along with the parent).
	tx, err := h.TxStarter.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to start squad archive transaction")
		return
	}
	defer tx.Rollback(r.Context())
	qtx := h.Queries.WithTx(tx)

	if _, err := qtx.ArchiveSquad(r.Context(), db.ArchiveSquadParams{
		ID:         squad.ID,
		ArchivedBy: userUUID,
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to archive squad")
		return
	}
	if _, err := qtx.ClearChildSquadParents(r.Context(), squad.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to detach child squads")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to commit squad archive transaction")
		return
	}

	h.publish(protocol.EventSquadDeleted, workspaceID, "member", userID, map[string]any{
		"squad_id":  uuidToString(squad.ID),
		"leader_id": uuidToString(squad.LeaderID),
	})
	w.WriteHeader(http.StatusNoContent)
}

// ── Squad Members ───────────────────────────────────────────────────────────

func (h *Handler) ListSquadMembers(w http.ResponseWriter, r *http.Request) {
	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	members, err := h.Queries.ListSquadMembers(r.Context(), squad.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list squad members")
		return
	}
	resp := make([]SquadMemberResponse, len(members))
	for i, m := range members {
		resp[i] = squadMemberToResponse(m)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ── Squad Member Status ────────────────────────────────────────────────────

// SquadMemberStatus is the per-member entry in the squad member status
// response. Agent members carry a derived working/idle/offline/unstable
// status plus any active issues; human members are returned with member_type
// only so the front-end can render them in the same list without
// reordering.
type SquadMemberStatusResponse struct {
	MemberType   string                  `json:"member_type"`
	MemberID     string                  `json:"member_id"`
	Status       *string                 `json:"status"`
	ActiveIssues []SquadActiveIssueBrief `json:"active_issues"`
	LastActiveAt *string                 `json:"last_active_at"`
}

type SquadActiveIssueBrief struct {
	IssueID     string `json:"issue_id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	IssueStatus string `json:"issue_status"`
}

type SquadMemberStatusListResponse struct {
	Members []SquadMemberStatusResponse `json:"members"`
}

// deriveSquadMemberStatus collapses runtime + task signals into the five
// status buckets used by the squad UI. Mirrors the workload+availability
// split in packages/core/agents/derive-presence.ts: working wins over
// runtime health (an agent that is in the middle of dispatched/running
// work counts as working even if the runtime briefly drops), then
// availability buckets decide between idle / unstable / offline.
//
// Thresholds match deriveRuntimeHealth: any offline runtime whose
// last_seen_at is within the last 5 minutes is reported as "unstable" so
// the squad UI surfaces transient drops the same way the agent dot does.
//
// Archived agents always report `archived` regardless of any leftover
// runtime row or task — they should appear in the list but never look
// like they're still working or merely offline (a leftover online
// runtime row would otherwise read as "offline" and hide the fact that
// the agent has been archived). Per the RFC decision (see MUL-2319), we
// surface archived agents in this endpoint rather than filtering them
// out in the SQL.
func deriveSquadMemberStatus(
	archived bool,
	runtimeStatus pgtype.Text,
	lastSeen pgtype.Timestamptz,
	hasActiveTask bool,
	now time.Time,
) string {
	if archived {
		return "archived"
	}
	if hasActiveTask {
		return "working"
	}
	if !runtimeStatus.Valid {
		return "offline"
	}
	if runtimeStatus.String == "online" {
		return "idle"
	}
	if !lastSeen.Valid {
		return "offline"
	}
	if now.Sub(lastSeen.Time) < 5*time.Minute {
		return "unstable"
	}
	return "offline"
}

// ListSquadMemberStatus returns one entry per squad member with derived
// status, the issues each agent member is currently running, and the last
// observed runtime activity. The endpoint is read-only and inherits the
// workspace-membership guard from the route middleware — any member of the
// workspace can read it.
func (h *Handler) ListSquadMemberStatus(w http.ResponseWriter, r *http.Request) {
	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}

	rows, err := h.Queries.ListSquadMemberStatusRows(r.Context(), squad.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list squad member status")
		return
	}

	prefix := h.getIssuePrefix(r.Context(), squad.WorkspaceID)
	now := time.Now()

	// Group rows by member_id while preserving the SQL ORDER BY (squad_member
	// insertion order). One member may appear in multiple rows when they have
	// more than one active task.
	type memberAcc struct {
		response       SquadMemberStatusResponse
		archived       bool
		hasActiveTask  bool
		runtimeStatus  pgtype.Text
		runtimeSeenAt  pgtype.Timestamptz
		latestActiveAt pgtype.Timestamptz
	}
	order := make([]string, 0, len(rows))
	acc := make(map[string]*memberAcc, len(rows))

	for _, row := range rows {
		memberID := uuidToString(row.MemberID)
		entry, exists := acc[memberID]
		if !exists {
			entry = &memberAcc{
				response: SquadMemberStatusResponse{
					MemberType:   row.MemberType,
					MemberID:     memberID,
					ActiveIssues: []SquadActiveIssueBrief{},
				},
				archived:      row.AgentArchivedAt.Valid,
				runtimeStatus: row.RuntimeStatus,
				runtimeSeenAt: row.RuntimeLastSeenAt,
			}
			acc[memberID] = entry
			order = append(order, memberID)
		}

		if row.MemberType != "agent" {
			continue
		}

		// A dispatched/running task occupies an agent slot even when it
		// has no associated issue (chat / quick-create tasks set
		// agent_task_queue.issue_id = NULL). The `working` bucket is
		// defined by task presence, not by whether we can render an
		// issue link, so flag the agent here regardless of issue_id.
		if row.TaskID.Valid {
			entry.hasActiveTask = true

			if row.TaskIssueID.Valid {
				brief := SquadActiveIssueBrief{
					IssueID:    uuidToString(row.TaskIssueID),
					Identifier: prefix + "-" + strconv.Itoa(int(row.IssueNumber.Int32)),
					Title:      row.IssueTitle.String,
					IssueStatus: func() string {
						if row.IssueStatus.Valid {
							return row.IssueStatus.String
						}
						return ""
					}(),
				}
				entry.response.ActiveIssues = append(entry.response.ActiveIssues, brief)
			}

			if row.TaskDispatchedAt.Valid && (!entry.latestActiveAt.Valid ||
				row.TaskDispatchedAt.Time.After(entry.latestActiveAt.Time)) {
				entry.latestActiveAt = row.TaskDispatchedAt
			}
		}
	}

	resp := SquadMemberStatusListResponse{
		Members: make([]SquadMemberStatusResponse, 0, len(order)),
	}
	for _, id := range order {
		entry := acc[id]
		if entry.response.MemberType == "agent" {
			status := deriveSquadMemberStatus(
				entry.archived,
				entry.runtimeStatus,
				entry.runtimeSeenAt,
				entry.hasActiveTask,
				now,
			)
			entry.response.Status = &status
			// last_active_at prefers the freshest active-task dispatch
			// over the runtime heartbeat: a working agent should not
			// look stale because the runtime heartbeat is a few seconds
			// behind. Falls back to runtime last_seen_at otherwise.
			if entry.latestActiveAt.Valid {
				entry.response.LastActiveAt = timestampToPtr(entry.latestActiveAt)
			} else if entry.runtimeSeenAt.Valid {
				entry.response.LastActiveAt = timestampToPtr(entry.runtimeSeenAt)
			}
		}
		resp.Members = append(resp.Members, entry.response)
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) AddSquadMember(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageSquad(member, squad) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}

	var req struct {
		MemberType string `json:"member_type"`
		MemberID   string `json:"member_id"`
		Role       string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MemberType != "agent" && req.MemberType != "member" {
		writeError(w, http.StatusBadRequest, "member_type must be 'agent' or 'member'")
		return
	}
	if req.MemberID == "" {
		writeError(w, http.StatusBadRequest, "member_id is required")
		return
	}

	memberUUID, ok := parseUUIDOrBadRequest(w, req.MemberID, "member_id")
	if !ok {
		return
	}

	// Validate the member belongs to this workspace.
	if req.MemberType == "agent" {
		agent, err := h.Queries.GetAgentInWorkspace(r.Context(), db.GetAgentInWorkspaceParams{
			ID: memberUUID, WorkspaceID: wsUUID,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "agent not found in this workspace")
			return
		}
		// A non-admin creator may only add agents they can @-trigger (public
		// or their own / allow-listed agents); admins may add any workspace
		// agent (MUL-4223).
		if !h.memberCanWireAgent(r.Context(), member, agent, workspaceID) {
			writeError(w, http.StatusForbidden, "you can only add an agent you have access to")
			return
		}
	} else {
		if _, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
			UserID: memberUUID, WorkspaceID: wsUUID,
		}); err != nil {
			writeError(w, http.StatusBadRequest, "member not found in this workspace")
			return
		}
	}

	sm, err := h.Queries.AddSquadMember(r.Context(), db.AddSquadMemberParams{
		SquadID:    squad.ID,
		MemberType: req.MemberType,
		MemberID:   memberUUID,
		Role:       req.Role,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "member already in squad")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to add squad member")
		return
	}

	writeJSON(w, http.StatusCreated, squadMemberToResponse(sm))
	h.publish(protocol.EventSquadUpdated, workspaceID, "member", requestUserID(r), map[string]any{
		"squad_id": uuidToString(squad.ID),
	})
}

func (h *Handler) RemoveSquadMember(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageSquad(member, squad) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}

	var req struct {
		MemberType string `json:"member_type"`
		MemberID   string `json:"member_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	memberUUID, ok := parseUUIDOrBadRequest(w, req.MemberID, "member_id")
	if !ok {
		return
	}

	// Prevent removing the leader.
	if req.MemberType == "agent" && uuidToString(squad.LeaderID) == req.MemberID {
		writeError(w, http.StatusBadRequest, "cannot remove the squad leader; change leader first")
		return
	}

	rows, err := h.Queries.RemoveSquadMember(r.Context(), db.RemoveSquadMemberParams{
		SquadID:    squad.ID,
		MemberType: req.MemberType,
		MemberID:   memberUUID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to remove squad member")
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "squad member not found")
		return
	}

	h.publish(protocol.EventSquadUpdated, workspaceID, "member", requestUserID(r), map[string]any{
		"squad_id": uuidToString(squad.ID),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) UpdateSquadMemberRole(w http.ResponseWriter, r *http.Request) {
	workspaceID := workspaceIDFromURL(r, "workspaceId")
	member, ok := h.requireWorkspaceMember(w, r, workspaceID, "workspace not found")
	if !ok {
		return
	}

	squad, _, ok := h.loadSquadInWorkspace(w, r)
	if !ok {
		return
	}
	if !canManageSquad(member, squad) {
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}

	var req struct {
		MemberType string `json:"member_type"`
		MemberID   string `json:"member_id"`
		Role       string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	memberUUID, ok := parseUUIDOrBadRequest(w, req.MemberID, "member_id")
	if !ok {
		return
	}

	sm, err := h.Queries.UpdateSquadMemberRole(r.Context(), db.UpdateSquadMemberRoleParams{
		SquadID:    squad.ID,
		MemberType: req.MemberType,
		MemberID:   memberUUID,
		Role:       req.Role,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "squad member not found")
		return
	}

	h.publish(protocol.EventSquadUpdated, workspaceID, "member", requestUserID(r), map[string]any{
		"squad_id": uuidToString(squad.ID),
	})
	writeJSON(w, http.StatusOK, squadMemberToResponse(sm))
}

// ── Squad Leader Evaluation ──────────────────────────────────────────────────

// RecordSquadLeaderEvaluation records a squad leader's evaluation decision
// into the unified activity_log. Called by the leader agent via CLI after
// each trigger to record whether it took action, stayed silent, or failed.
func (h *Handler) RecordSquadLeaderEvaluation(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}

	var req struct {
		Outcome string `json:"outcome"` // action | no_action | failed
		Reason  string `json:"reason"`  // short explanation from leader
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Outcome != "action" && req.Outcome != "no_action" && req.Outcome != "failed" {
		writeError(w, http.StatusBadRequest, "outcome must be 'action', 'no_action', or 'failed'")
		return
	}

	// The issue must be assigned to a squad.
	if !issue.AssigneeType.Valid || issue.AssigneeType.String != "squad" || !issue.AssigneeID.Valid {
		writeError(w, http.StatusBadRequest, "issue is not assigned to a squad")
		return
	}

	squad, err := h.Queries.GetSquadInWorkspace(r.Context(), db.GetSquadInWorkspaceParams{
		ID:          issue.AssigneeID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "squad not found")
		return
	}

	// Security: only the squad leader agent can record evaluations.
	workspaceID := uuidToString(issue.WorkspaceID)
	userID := requestUserID(r)
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if actorType != "agent" || actorID != uuidToString(squad.LeaderID) {
		writeError(w, http.StatusForbidden, "only the squad leader agent can record evaluations")
		return
	}

	taskID := r.Header.Get("X-Task-ID")
	taskUUID, ok := parseUUIDOrBadRequest(w, taskID, "task id")
	if !ok {
		return
	}
	task, err := h.Queries.GetAgentTask(r.Context(), taskUUID)
	if err != nil || !task.IssueID.Valid || uuidToString(task.IssueID) != uuidToString(issue.ID) {
		writeError(w, http.StatusBadRequest, "task does not belong to issue")
		return
	}

	details, _ := json.Marshal(map[string]string{
		"squad_id": uuidToString(squad.ID),
		"task_id":  util.UUIDToString(taskUUID),
		"outcome":  req.Outcome,
		"reason":   req.Reason,
	})

	activity, err := h.Queries.CreateActivity(r.Context(), db.CreateActivityParams{
		WorkspaceID: issue.WorkspaceID,
		IssueID:     issue.ID,
		ActorType:   pgtype.Text{String: "agent", Valid: true},
		ActorID:     squad.LeaderID,
		Action:      "squad_leader_evaluated",
		Details:     details,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record evaluation")
		return
	}

	h.publish(protocol.EventActivityCreated, uuidToString(issue.WorkspaceID), "agent", actorID, map[string]any{
		"issue_id": uuidToString(issue.ID),
		"entry": map[string]any{
			"type":       "activity",
			"id":         uuidToString(activity.ID),
			"actor_type": "agent",
			"actor_id":   actorID,
			"action":     activity.Action,
			"details":    json.RawMessage(details),
			"created_at": timestampToString(activity.CreatedAt),
		},
	})

	writeJSON(w, http.StatusCreated, map[string]string{
		"id":         uuidToString(activity.ID),
		"action":     activity.Action,
		"created_at": timestampToString(activity.CreatedAt),
	})
}

// ── Squad Trigger Logic ─────────────────────────────────────────────────────

// shouldSuppressSquadLeaderSelfTrigger reports whether a squad leader's own
// comment should be blocked from re-enqueuing that same leader. The only
// leader-authored non-leader task allowed to wake the assigned leader is a
// same-squad worker task; generic agent tasks such as direct mentions and
// thread-parent replies are not worker-role proof and must not self-trigger.
func (h *Handler) shouldSuppressSquadLeaderSelfTrigger(ctx context.Context, issueID, leaderID, squadID pgtype.UUID) bool {
	latest, err := h.Queries.GetLatestTaskRoleForIssueAndAgent(ctx, db.GetLatestTaskRoleForIssueAndAgentParams{
		IssueID: issueID,
		AgentID: leaderID,
	})
	if err != nil {
		return false
	}
	if latest.IsLeaderTask {
		return true
	}
	return !latest.SquadID.Valid || uuidToString(latest.SquadID) != uuidToString(squadID)
}

// commentMentionsAnyone returns true when the comment body contains at least
// one routing-style mention — [@Name](mention://agent|member|squad|all/<id>).
// Issue cross-references (mention://issue/...) are ignored because they are
// not directed at a participant. Only the current comment is inspected —
// parent (thread root) mentions are NOT inherited here.
func commentMentionsAnyone(content string) bool {
	for _, m := range util.ParseMentions(content) {
		switch m.Type {
		case "agent", "member", "squad", "all":
			return true
		}
	}
	return false
}

// The squad-leader assign/promotion readiness decision now lives in the single
// service.IssueService.WillEnqueueRun predicate (MUL-3375), shared by the issue
// write paths and the preview endpoint. The former handler-local mirrors
// (shouldEnqueueSquadLeaderOnAssign / isSquadLeaderReady) were removed to stop
// the four-entry-point drift. The squad enqueue side effect still flows through
// enqueueSquadLeaderTask below, which keeps the leader access gate and pending
// dedup in one place.

// enqueueSquadLeaderTask triggers the squad leader agent for an issue assigned
// to a squad. Assign and backlog-promotion paths use this directly; comment
// paths go through computeCommentAgentTriggers so preview and create share the
// same trigger set.
// enqueueSquadLeaderTask returns true when it actually enqueued a leader task
// (so the caller can record a handoff trace only on a real run start).
func (h *Handler) enqueueSquadLeaderTask(ctx context.Context, issue db.Issue, triggerCommentID pgtype.UUID, authorType, authorID, handoffNote string) bool {
	squad, err := h.Queries.GetSquadInWorkspace(ctx, db.GetSquadInWorkspaceParams{
		ID:          issue.AssigneeID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return false
	}

	// The gate must judge the SAME top-of-chain human the enqueue path will
	// persist on the leader task row, or it drifts: an agent-created issue that
	// correctly inherits its originator (MUL-4305) would still be denied here
	// if the gate used an empty originator. Member authors are their own
	// originator; for agent/system-triggered assigns we resolve the originator
	// exactly like EnqueueTaskForSquadLeader* does (via the issue's origin
	// link). triggerCommentID is always empty on the assign/promote path, so we
	// pass an invalid UUID to match. A still-unresolved originator leaves
	// leaderOriginator empty, which correctly fails closed for member/team
	// targets while a workspace target still admits the agent principal.
	leaderOriginator := ""
	if authorType == "member" {
		leaderOriginator = authorID
	} else {
		leaderOriginator = uuidToString(h.TaskService.OriginatorForIssueTask(ctx, issue, pgtype.UUID{}))
	}
	if !h.canEnqueueSquadLeader(ctx, squad.LeaderID, authorType, authorID, leaderOriginator, uuidToString(issue.WorkspaceID)) {
		return false
	}

	hasPending, err := h.Queries.HasPendingTaskForIssueAndAgent(ctx, db.HasPendingTaskForIssueAndAgentParams{
		IssueID: issue.ID,
		AgentID: squad.LeaderID,
		// Key dedup on the reviewed head (TEN-356).
		HeadSha: h.TaskService.ResolveIssueReviewSHAParam(ctx, issue.ID),
	})
	if err != nil || hasPending {
		return false
	}

	// triggerCommentID is always empty on the assign/promote path; the handoff
	// note rides its own task column, never trigger_comment_id.
	_ = triggerCommentID
	// The member who performed the assign/promote is the accountable human for the
	// leader run (MUL-4302 §4) — the same principal the gate above judged. An agent
	// author is not a human, so only a member actor is threaded.
	if _, err := h.TaskService.EnqueueTaskForSquadLeaderWithHandoff(ctx, issue, squad.LeaderID, squad.ID, handoffNote, memberActorUserID(authorType, authorID)); err != nil {
		slog.Warn("enqueue squad leader task failed",
			"issue_id", uuidToString(issue.ID),
			"squad_id", uuidToString(squad.ID),
			"leader_id", uuidToString(squad.LeaderID),
			"error", err)
		return false
	}
	return true
}
