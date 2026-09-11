-- Issue-flow intermediate documents (CLO-278). No foreign keys (repo
-- convention); the application-layer IssueDocumentService owns consistency.

-- name: CreateIssueDocument :one
INSERT INTO issue_document (
    workspace_id, issue_id, type, title, content, content_type,
    file_attachment_id, version, status, author_type, author_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
) RETURNING *;

-- name: GetIssueDocument :one
SELECT * FROM issue_document
WHERE id = $1 AND workspace_id = $2;

-- name: GetIssueDocumentDetail :one
SELECT d.id, d.workspace_id, d.issue_id, d.type, d.title, d.content, d.content_type,
       d.file_attachment_id, d.version, d.status, d.author_type, d.author_id,
       d.created_at, d.updated_at,
       i.number AS issue_number, i.title AS issue_title,
       u.name AS member_author_name, a.name AS agent_author_name
FROM issue_document d
JOIN issue i ON i.id = d.issue_id
LEFT JOIN member m ON m.id = d.author_id AND d.author_type = 'member'
LEFT JOIN "user" u ON u.id = m.user_id
LEFT JOIN agent a ON a.id = d.author_id AND d.author_type = 'agent'
WHERE d.id = $1 AND d.workspace_id = $2;

-- name: GetIssueDocumentNextVersion :one
SELECT COALESCE(MAX(version), 0) + 1 AS next_version
FROM issue_document
WHERE workspace_id = $1 AND issue_id = $2 AND type = $3;

-- name: SupersedeIssueDocumentByIssueType :exec
-- Mark older versions superseded WITHOUT touching updated_at: that column is
-- the document's own "last written" timestamp, and the list sorts by it. A
-- superseded historical version must not jump to the top of "recently
-- updated" just because a newer version was submitted (CLO-283 R10).
UPDATE issue_document
SET status = 'superseded'
WHERE workspace_id = $1 AND issue_id = $2 AND type = $3
  AND status <> 'superseded';

-- name: ListIssueDocuments :many
SELECT d.id, d.workspace_id, d.issue_id, d.type, d.title, d.content_type,
       d.file_attachment_id, d.version, d.status, d.author_type, d.author_id,
       d.created_at, d.updated_at,
       i.number AS issue_number, i.title AS issue_title,
       u.name AS member_author_name, a.name AS agent_author_name
FROM issue_document d
JOIN issue i ON i.id = d.issue_id
JOIN workspace w ON w.id = d.workspace_id
LEFT JOIN member m ON m.id = d.author_id AND d.author_type = 'member'
LEFT JOIN "user" u ON u.id = m.user_id
LEFT JOIN agent a ON a.id = d.author_id AND d.author_type = 'agent'
WHERE d.workspace_id = $1
  AND (sqlc.narg('type')::text IS NULL OR d.type = sqlc.narg('type'))
  AND (sqlc.narg('status')::text IS NULL OR d.status = sqlc.narg('status'))
  AND (sqlc.narg('issue_id')::uuid IS NULL OR d.issue_id = sqlc.narg('issue_id'))
  AND (sqlc.narg('q')::text IS NULL OR (
        LOWER(d.title) LIKE '%' || LOWER(sqlc.narg('q')) || '%'
     OR LOWER(i.title) LIKE '%' || LOWER(sqlc.narg('q')) || '%'
     OR CAST(i.number AS TEXT) LIKE LOWER(sqlc.narg('q')) || '%'
     OR LOWER(w.issue_prefix || '-' || CAST(i.number AS TEXT)) LIKE '%' || LOWER(sqlc.narg('q')) || '%'
     OR LOWER(COALESCE(u.name, a.name)) LIKE '%' || LOWER(sqlc.narg('q')) || '%'
  ))
ORDER BY d.updated_at DESC, d.id DESC
LIMIT $2 OFFSET $3;

-- name: CountIssueDocuments :one
SELECT COUNT(*)
FROM issue_document d
JOIN issue i ON i.id = d.issue_id
JOIN workspace w ON w.id = d.workspace_id
LEFT JOIN member m ON m.id = d.author_id AND d.author_type = 'member'
LEFT JOIN "user" u ON u.id = m.user_id
LEFT JOIN agent a ON a.id = d.author_id AND d.author_type = 'agent'
WHERE d.workspace_id = $1
  AND (sqlc.narg('type')::text IS NULL OR d.type = sqlc.narg('type'))
  AND (sqlc.narg('status')::text IS NULL OR d.status = sqlc.narg('status'))
  AND (sqlc.narg('issue_id')::uuid IS NULL OR d.issue_id = sqlc.narg('issue_id'))
  AND (sqlc.narg('q')::text IS NULL OR (
        LOWER(d.title) LIKE '%' || LOWER(sqlc.narg('q')) || '%'
     OR LOWER(i.title) LIKE '%' || LOWER(sqlc.narg('q')) || '%'
     OR CAST(i.number AS TEXT) LIKE LOWER(sqlc.narg('q')) || '%'
     OR LOWER(w.issue_prefix || '-' || CAST(i.number AS TEXT)) LIKE '%' || LOWER(sqlc.narg('q')) || '%'
     OR LOWER(COALESCE(u.name, a.name)) LIKE '%' || LOWER(sqlc.narg('q')) || '%'
  ));

-- name: ListIssueDocumentVersions :many
SELECT id, version, status, title, content_type, file_attachment_id, created_at, updated_at
FROM issue_document
WHERE workspace_id = $1 AND issue_id = $2 AND type = $3
ORDER BY version DESC;
