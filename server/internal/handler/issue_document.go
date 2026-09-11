package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/logger"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type IssueDocumentResponse struct {
	ID               string  `json:"id"`
	WorkspaceID      string  `json:"workspace_id"`
	IssueID          string  `json:"issue_id"`
	IssueIdentifier  string  `json:"issue_identifier"`
	IssueTitle       string  `json:"issue_title"`
	Type             string  `json:"type"`
	Title            string  `json:"title"`
	Version          int32   `json:"version"`
	Status           string  `json:"status"`
	ContentType      string  `json:"content_type"`
	FileAttachmentID *string `json:"file_attachment_id"`
	AuthorType       string  `json:"author_type"`
	AuthorID         string  `json:"author_id"`
	AuthorName       string  `json:"author_name"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

type IssueDocumentDetailResponse struct {
	IssueDocumentResponse
	Content *string `json:"content"`
}

type IssueDocumentVersionResponse struct {
	ID               string  `json:"id"`
	Version          int32   `json:"version"`
	Status           string  `json:"status"`
	Title            string  `json:"title"`
	ContentType      string  `json:"content_type"`
	FileAttachmentID *string `json:"file_attachment_id"`
	CreatedAt        string  `json:"created_at"`
	UpdatedAt        string  `json:"updated_at"`
}

var validIssueDocumentTypes = map[string]bool{
	"requirements":  true,
	"architecture":  true,
	"development":   true,
	"testing":       true,
	"code_review":   true,
	"security":      true,
	"documentation": true,
	"deployment":    true,
	"other":         true,
}

var validIssueDocumentStatuses = map[string]bool{
	"draft":      true,
	"submitted":  true,
	"approved":   true,
	"rejected":   true,
	"superseded": true,
}

var validIssueDocumentContentTypes = map[string]bool{
	"markdown": true,
	"json":     true,
	"text":     true,
	"file":     true,
}

var issueDocumentGroupSortColumns = map[string]string{
	"updated_at": "d.updated_at",
	"title":      "LOWER(d.title)",
	"type": `CASE d.type
		   WHEN 'requirements' THEN 1
		   WHEN 'architecture' THEN 2
		   WHEN 'development' THEN 3
		   WHEN 'testing' THEN 4
		   WHEN 'code_review' THEN 5
		   WHEN 'security' THEN 6
		   WHEN 'documentation' THEN 7
		   WHEN 'deployment' THEN 8
		   ELSE 9 END`,
	"status":  "d.status",
	"version": "d.version",
}

type IssueDocumentGroupResponse struct {
	IssueID         string                  `json:"issue_id"`
	IssueIdentifier string                  `json:"issue_identifier"`
	IssueTitle      string                  `json:"issue_title"`
	Items           []IssueDocumentResponse `json:"items"`
	Total           int                     `json:"total"`
}

func issueDocumentRowToResponse(row db.ListIssueDocumentsRow, issuePrefix string) IssueDocumentResponse {
	authorName := ""
	if row.AuthorType == "member" {
		authorName = row.MemberAuthorName.String
	} else if row.AuthorType == "agent" {
		authorName = row.AgentAuthorName.String
	}
	return IssueDocumentResponse{
		ID:               uuidToString(row.ID),
		WorkspaceID:      uuidToString(row.WorkspaceID),
		IssueID:          uuidToString(row.IssueID),
		IssueIdentifier:  issuePrefix + "-" + strconv.Itoa(int(row.IssueNumber)),
		IssueTitle:       row.IssueTitle,
		Type:             row.Type,
		Title:            row.Title,
		Version:          row.Version,
		Status:           row.Status,
		ContentType:      row.ContentType,
		FileAttachmentID: uuidToPtr(row.FileAttachmentID),
		AuthorType:       row.AuthorType,
		AuthorID:         uuidToString(row.AuthorID),
		AuthorName:       authorName,
		CreatedAt:        timestampToString(row.CreatedAt),
		UpdatedAt:        timestampToString(row.UpdatedAt),
	}
}

func issueDocumentDetailToResponse(row db.GetIssueDocumentDetailRow, issuePrefix string) IssueDocumentDetailResponse {
	summary := IssueDocumentResponse{
		ID:               uuidToString(row.ID),
		WorkspaceID:      uuidToString(row.WorkspaceID),
		IssueID:          uuidToString(row.IssueID),
		IssueIdentifier:  issuePrefix + "-" + strconv.Itoa(int(row.IssueNumber)),
		IssueTitle:       row.IssueTitle,
		Type:             row.Type,
		Title:            row.Title,
		Version:          row.Version,
		Status:           row.Status,
		ContentType:      row.ContentType,
		FileAttachmentID: uuidToPtr(row.FileAttachmentID),
		AuthorType:       row.AuthorType,
		AuthorID:         uuidToString(row.AuthorID),
		CreatedAt:        timestampToString(row.CreatedAt),
		UpdatedAt:        timestampToString(row.UpdatedAt),
	}
	if row.AuthorType == "member" {
		summary.AuthorName = row.MemberAuthorName.String
	} else if row.AuthorType == "agent" {
		summary.AuthorName = row.AgentAuthorName.String
	}
	return IssueDocumentDetailResponse{
		IssueDocumentResponse: summary,
		Content:               textToPtr(row.Content),
	}
}

func issueDocumentVersionToResponse(row db.ListIssueDocumentVersionsRow) IssueDocumentVersionResponse {
	return IssueDocumentVersionResponse{
		ID:               uuidToString(row.ID),
		Version:          row.Version,
		Status:           row.Status,
		Title:            row.Title,
		ContentType:      row.ContentType,
		FileAttachmentID: uuidToPtr(row.FileAttachmentID),
		CreatedAt:        timestampToString(row.CreatedAt),
		UpdatedAt:        timestampToString(row.UpdatedAt),
	}
}

const (
	defaultIssueDocumentLimit = 50
	maxIssueDocumentLimit     = 100
)

var issueDocumentSortColumns = map[string]string{
	"updated_at": "d.updated_at",
	"title":      "LOWER(d.title)",
	"type":       "d.type",
	"status":     "d.status",
	"version":    "d.version",
}

type issueDocumentListFilter struct {
	Type    pgtype.Text
	Status  pgtype.Text
	IssueID pgtype.UUID
	Q       pgtype.Text
}

func parseIssueDocumentListFilter(w http.ResponseWriter, r *http.Request) (issueDocumentListFilter, bool) {
	var f issueDocumentListFilter
	if raw := strings.TrimSpace(r.URL.Query().Get("type")); raw != "" {
		if !validIssueDocumentTypes[raw] {
			writeError(w, http.StatusBadRequest, "invalid document type")
			return f, false
		}
		f.Type = strToText(raw)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		if !validIssueDocumentStatuses[raw] {
			writeError(w, http.StatusBadRequest, "invalid document status")
			return f, false
		}
		f.Status = strToText(raw)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("issue_id")); raw != "" {
		u, err := util.ParseUUID(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid issue_id")
			return f, false
		}
		f.IssueID = u
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("q")); raw != "" {
		f.Q = strToText(escapeLike(raw))
	}
	return f, true
}

const listIssueDocumentSelectBody = `FROM issue_document d
JOIN issue i ON i.id = d.issue_id
JOIN workspace w ON w.id = d.workspace_id
LEFT JOIN member m ON m.id = d.author_id AND d.author_type = 'member'
LEFT JOIN "user" u ON u.id = m.user_id
LEFT JOIN agent a ON a.id = d.author_id AND d.author_type = 'agent'
WHERE %s`

const listIssueDocumentColumns = `SELECT d.id, d.workspace_id, d.issue_id, d.type, d.title, d.content_type,
       d.file_attachment_id, d.version, d.status, d.author_type, d.author_id,
       d.created_at, d.updated_at,
       i.number AS issue_number, i.title AS issue_title,
       u.name AS member_author_name, a.name AS agent_author_name`

const issueRootsCTE = `WITH RECURSIVE issue_roots AS (
    SELECT id, id AS root_id, number AS root_number, title AS root_title
    FROM issue
    WHERE workspace_id = $1 AND parent_issue_id IS NULL
    UNION ALL
    SELECT c.id, r.root_id, r.root_number, r.root_title
    FROM issue c
    JOIN issue_roots r ON c.parent_issue_id = r.id
)`

const listIssueDocumentGroupColumns = listIssueDocumentColumns + `,
       r.root_id, r.root_number, r.root_title`

const listIssueDocumentGroupSelectBody = `FROM issue_document d
JOIN issue i ON i.id = d.issue_id
JOIN workspace w ON w.id = d.workspace_id
JOIN issue_roots r ON r.id = d.issue_id
LEFT JOIN member m ON m.id = d.author_id AND d.author_type = 'member'
LEFT JOIN "user" u ON u.id = m.user_id
LEFT JOIN agent a ON a.id = d.author_id AND d.author_type = 'agent'
WHERE %s`

func buildIssueDocumentWhere(workspaceID pgtype.UUID, f issueDocumentListFilter) (string, []any) {
	where := []string{"d.workspace_id = $1"}
	args := []any{workspaceID}
	addArg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.Type.Valid {
		where = append(where, "d.type = "+addArg(f.Type))
	}
	if f.Status.Valid {
		where = append(where, "d.status = "+addArg(f.Status))
	}
	if f.IssueID.Valid {
		where = append(where, "d.issue_id = "+addArg(f.IssueID))
	}
	if f.Q.Valid {
		qRef := addArg(f.Q)
		where = append(where, fmt.Sprintf(`(
    LOWER(d.title) LIKE '%%' || LOWER(%s) || '%%'
 OR LOWER(i.title) LIKE '%%' || LOWER(%s) || '%%'
 OR CAST(i.number AS TEXT) LIKE LOWER(%s) || '%%'
 OR LOWER(w.issue_prefix || '-' || CAST(i.number AS TEXT)) LIKE '%%' || LOWER(%s) || '%%'
 OR LOWER(COALESCE(u.name, a.name)) LIKE '%%' || LOWER(%s) || '%%'
)`, qRef, qRef, qRef, qRef, qRef))
	}
	return strings.Join(where, " AND "), args
}

func scanListIssueDocumentRow(rows pgx.Rows) (db.ListIssueDocumentsRow, error) {
	var row db.ListIssueDocumentsRow
	err := rows.Scan(
		&row.ID, &row.WorkspaceID, &row.IssueID, &row.Type, &row.Title,
		&row.ContentType, &row.FileAttachmentID, &row.Version, &row.Status,
		&row.AuthorType, &row.AuthorID, &row.CreatedAt, &row.UpdatedAt,
		&row.IssueNumber, &row.IssueTitle, &row.MemberAuthorName, &row.AgentAuthorName,
	)
	return row, err
}

type listIssueDocumentGroupedRow struct {
	db.ListIssueDocumentsRow
	RootID     pgtype.UUID
	RootNumber int32
	RootTitle  string
}

func scanListIssueDocumentGroupedRow(rows pgx.Rows) (listIssueDocumentGroupedRow, error) {
	var row listIssueDocumentGroupedRow
	err := rows.Scan(
		&row.ID, &row.WorkspaceID, &row.IssueID, &row.Type, &row.Title,
		&row.ContentType, &row.FileAttachmentID, &row.Version, &row.Status,
		&row.AuthorType, &row.AuthorID, &row.CreatedAt, &row.UpdatedAt,
		&row.IssueNumber, &row.IssueTitle, &row.MemberAuthorName, &row.AgentAuthorName,
		&row.RootID, &row.RootNumber, &row.RootTitle,
	)
	return row, err
}

func (h *Handler) ListIssueDocuments(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	f, ok := parseIssueDocumentListFilter(w, r)
	if !ok {
		return
	}

	if strings.TrimSpace(r.URL.Query().Get("group")) == "issue" {
		h.listIssueDocumentGroups(w, r, wsUUID, f)
		return
	}

	limit := defaultIssueDocumentLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		if n > maxIssueDocumentLimit {
			n = maxIssueDocumentLimit
		}
		limit = n
	}
	offset := 0
	if raw := r.URL.Query().Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = n
	}

	sortExpr := issueDocumentSortColumns["updated_at"]
	if raw := strings.TrimSpace(r.URL.Query().Get("sort")); raw != "" {
		expr, ok := issueDocumentSortColumns[raw]
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid sort value")
			return
		}
		sortExpr = expr
	}
	sortDir := "DESC"
	if raw := strings.TrimSpace(r.URL.Query().Get("order")); raw != "" {
		switch strings.ToLower(raw) {
		case "asc":
			sortDir = "ASC"
		case "desc":
			sortDir = "DESC"
		default:
			writeError(w, http.StatusBadRequest, "invalid order value")
			return
		}
	}

	whereSql, args := buildIssueDocumentWhere(wsUUID, f)
	addArg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	offsetRef := addArg(int64(offset))
	limitRef := addArg(int64(limit))

	orderBy := sortExpr + " " + sortDir + ", d.id DESC"

	query := fmt.Sprintf(`%s
`+listIssueDocumentSelectBody+`
ORDER BY %s
LIMIT %s OFFSET %s`, listIssueDocumentColumns, whereSql, orderBy, limitRef, offsetRef)

	rows, err := h.DB.Query(r.Context(), query, args...)
	if err != nil {
		slog.Warn("ListIssueDocuments query failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list issue documents")
		return
	}
	defer rows.Close()

	var items []db.ListIssueDocumentsRow
	for rows.Next() {
		row, err := scanListIssueDocumentRow(rows)
		if err != nil {
			slog.Warn("ListIssueDocuments scan failed", append(logger.RequestAttrs(r), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to list issue documents")
			return
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		slog.Warn("ListIssueDocuments rows failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list issue documents")
		return
	}

	countQuery := fmt.Sprintf(`SELECT COUNT(*) `+listIssueDocumentSelectBody, whereSql)
	var total int64
	if err := h.DB.QueryRow(r.Context(), countQuery, args[:len(args)-2]...).Scan(&total); err != nil {
		total = int64(len(items))
	}

	issuePrefix := h.getIssuePrefix(r.Context(), wsUUID)
	resp := make([]IssueDocumentResponse, len(items))
	for i, row := range items {
		resp[i] = issueDocumentRowToResponse(row, issuePrefix)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": resp, "total": total})
}

func (h *Handler) listIssueDocumentGroups(w http.ResponseWriter, r *http.Request, wsUUID pgtype.UUID, f issueDocumentListFilter) {
	sortExpr := issueDocumentGroupSortColumns["type"]
	sortDir := "ASC"
	if raw := strings.TrimSpace(r.URL.Query().Get("sort")); raw != "" {
		expr, ok := issueDocumentGroupSortColumns[raw]
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid sort value")
			return
		}
		sortExpr = expr
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("order")); raw != "" {
		switch strings.ToLower(raw) {
		case "asc":
			sortDir = "ASC"
		case "desc":
			sortDir = "DESC"
		default:
			writeError(w, http.StatusBadRequest, "invalid order value")
			return
		}
	}

	whereSql, args := buildIssueDocumentWhere(wsUUID, f)

	query := fmt.Sprintf(issueRootsCTE+`
`+listIssueDocumentGroupColumns+`
`+listIssueDocumentGroupSelectBody+`
ORDER BY r.root_number ASC, %s %s, d.id DESC`, whereSql, sortExpr, sortDir)

	rows, err := h.DB.Query(r.Context(), query, args...)
	if err != nil {
		slog.Warn("listIssueDocumentGroups query failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list issue documents")
		return
	}
	defer rows.Close()

	issuePrefix := h.getIssuePrefix(r.Context(), wsUUID)
	var groups []*IssueDocumentGroupResponse
	byIssue := map[string]*IssueDocumentGroupResponse{}
	total := 0
	for rows.Next() {
		row, err := scanListIssueDocumentGroupedRow(rows)
		if err != nil {
			slog.Warn("listIssueDocumentGroups scan failed", append(logger.RequestAttrs(r), "error", err)...)
			writeError(w, http.StatusInternalServerError, "failed to list issue documents")
			return
		}
		rootID := uuidToString(row.RootID)
		group := byIssue[rootID]
		if group == nil {
			group = &IssueDocumentGroupResponse{
				IssueID:         rootID,
				IssueIdentifier: issuePrefix + "-" + strconv.Itoa(int(row.RootNumber)),
				IssueTitle:      row.RootTitle,
			}
			byIssue[rootID] = group
			groups = append(groups, group)
		}
		group.Items = append(group.Items, issueDocumentRowToResponse(row.ListIssueDocumentsRow, issuePrefix))
		group.Total = len(group.Items)
		total++
	}
	if err := rows.Err(); err != nil {
		slog.Warn("listIssueDocumentGroups rows failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list issue documents")
		return
	}

	resp := make([]IssueDocumentGroupResponse, len(groups))
	for i, g := range groups {
		resp[i] = *g
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": resp, "total": total})
}

func (h *Handler) GetIssueDocument(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentId")
	workspaceID := h.resolveWorkspaceID(r)
	docUUID, ok := parseUUIDOrBadRequest(w, documentID, "document id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	row, err := h.Queries.GetIssueDocumentDetail(r.Context(), db.GetIssueDocumentDetailParams{
		ID:          docUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "issue document not found")
			return
		}
		slog.Warn("GetIssueDocument failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to get issue document")
		return
	}
	issuePrefix := h.getIssuePrefix(r.Context(), wsUUID)
	writeJSON(w, http.StatusOK, issueDocumentDetailToResponse(row, issuePrefix))
}

func (h *Handler) ListIssueDocumentVersions(w http.ResponseWriter, r *http.Request) {
	documentID := chi.URLParam(r, "documentId")
	workspaceID := h.resolveWorkspaceID(r)
	docUUID, ok := parseUUIDOrBadRequest(w, documentID, "document id")
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}
	doc, err := h.Queries.GetIssueDocument(r.Context(), db.GetIssueDocumentParams{
		ID:          docUUID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "issue document not found")
			return
		}
		slog.Warn("ListIssueDocumentVersions failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list issue document versions")
		return
	}
	rows, err := h.Queries.ListIssueDocumentVersions(r.Context(), db.ListIssueDocumentVersionsParams{
		WorkspaceID: wsUUID,
		IssueID:     doc.IssueID,
		Type:        doc.Type,
	})
	if err != nil {
		slog.Warn("ListIssueDocumentVersions failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to list issue document versions")
		return
	}
	items := make([]IssueDocumentVersionResponse, len(rows))
	for i, row := range rows {
		items[i] = issueDocumentVersionToResponse(row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issue_id": uuidToString(doc.IssueID),
		"type":     doc.Type,
		"items":    items,
	})
}

type CreateIssueDocumentRequest struct {
	IssueID          string  `json:"issue_id"`
	Type             string  `json:"type"`
	Title            string  `json:"title"`
	Content          string  `json:"content"`
	ContentType      string  `json:"content_type"`
	FileAttachmentID *string `json:"file_attachment_id"`
	Status           *string `json:"status"`
}

const (
	maxIssueDocumentInlineContentBytes = 5 * 1024 * 1024
	maxIssueDocumentRequestBytes       = 6 * 1024 * 1024
	maxIssueDocumentTitleRunes         = 500
)

func (h *Handler) requireIssueDocumentWriteAccess(w http.ResponseWriter, r *http.Request, workspaceID string) (authorType string, authorID pgtype.UUID, canSetReviewStatus bool, ok bool) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return "", pgtype.UUID{}, false, false
	}
	actorType, actorID := h.resolveActor(r, userID, workspaceID)
	if actorType == "agent" {
		agentUUID, err := util.ParseUUID(actorID)
		if err != nil {
			writeError(w, http.StatusForbidden, "invalid agent identity")
			return "", pgtype.UUID{}, false, false
		}
		return "agent", agentUUID, false, true
	}
	member, roleOK := h.requireWorkspaceRole(w, r, workspaceID, "workspace not found", "owner", "admin")
	if !roleOK {
		return "", pgtype.UUID{}, false, false
	}
	return "member", member.ID, true, true
}

func (h *Handler) CreateIssueDocument(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace id")
	if !ok {
		return
	}

	authorType, authorID, canSetReviewStatus, ok := h.requireIssueDocumentWriteAccess(w, r, workspaceID)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxIssueDocumentRequestBytes)
	var req CreateIssueDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Content = sanitizeNullBytes(req.Content)
	if strings.TrimSpace(req.IssueID) == "" {
		writeError(w, http.StatusBadRequest, "issue_id is required")
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if len([]rune(title)) > maxIssueDocumentTitleRunes {
		writeError(w, http.StatusBadRequest, "title is too long")
		return
	}
	docType := strings.TrimSpace(req.Type)
	if !validIssueDocumentTypes[docType] {
		writeError(w, http.StatusBadRequest, "invalid document type")
		return
	}
	contentType := strings.TrimSpace(req.ContentType)
	if contentType == "" {
		contentType = "markdown"
	}
	if !validIssueDocumentContentTypes[contentType] {
		writeError(w, http.StatusBadRequest, "invalid content_type")
		return
	}
	status := "submitted"
	if req.Status != nil {
		status = strings.TrimSpace(*req.Status)
		if !validIssueDocumentStatuses[status] {
			writeError(w, http.StatusBadRequest, "invalid status")
			return
		}
		switch status {
		case "superseded":
			writeError(w, http.StatusBadRequest, "superseded is a server-managed status")
			return
		case "approved", "rejected":
			if !canSetReviewStatus {
				writeError(w, http.StatusForbidden, "only workspace owner/admin can set a review status")
				return
			}
		}
	}
	if contentType == "file" {
		if req.FileAttachmentID == nil || strings.TrimSpace(*req.FileAttachmentID) == "" {
			writeError(w, http.StatusBadRequest, "file_attachment_id is required for file documents")
			return
		}
	} else {
		if len(req.Content) > maxIssueDocumentInlineContentBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "document content is too large")
			return
		}
	}

	issue, ok := h.loadIssueForUser(w, r, req.IssueID)
	if !ok {
		return
	}

	var fileAttachmentID pgtype.UUID
	if req.FileAttachmentID != nil && strings.TrimSpace(*req.FileAttachmentID) != "" {
		u, err := util.ParseUUID(*req.FileAttachmentID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid file_attachment_id")
			return
		}
		att, err := h.Queries.GetAttachmentByIDOnly(r.Context(), u)
		if err != nil || uuidToString(att.WorkspaceID) != workspaceID {
			writeError(w, http.StatusBadRequest, "file_attachment_id must reference an attachment in the current workspace")
			return
		}
		fileAttachmentID = u
	}

	doc, err := h.IssueDocumentService.Submit(r.Context(), service.SubmitIssueDocumentParams{
		WorkspaceID:      wsUUID,
		IssueID:          issue.ID,
		Type:             docType,
		Title:            strings.TrimSpace(req.Title),
		Content:          req.Content,
		ContentType:      contentType,
		FileAttachmentID: fileAttachmentID,
		Status:           status,
		AuthorType:       authorType,
		AuthorID:         authorID,
	})
	if err != nil {
		if errors.Is(err, service.ErrIssueDocumentVersionConflict) {
			writeError(w, http.StatusConflict, "issue document version conflict; retry the submission")
			return
		}
		slog.Warn("CreateIssueDocument failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create issue document")
		return
	}

	detail, err := h.Queries.GetIssueDocumentDetail(r.Context(), db.GetIssueDocumentDetailParams{
		ID:          doc.ID,
		WorkspaceID: wsUUID,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "issue document not found")
			return
		}
		slog.Warn("CreateIssueDocument: detail readback failed", append(logger.RequestAttrs(r), "error", err)...)
		writeError(w, http.StatusInternalServerError, "failed to create issue document")
		return
	}
	issuePrefix := h.getIssuePrefix(r.Context(), wsUUID)
	writeJSON(w, http.StatusCreated, issueDocumentDetailToResponse(detail, issuePrefix))
}
