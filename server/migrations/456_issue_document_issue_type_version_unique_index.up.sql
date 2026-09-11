CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_document_issue_type_version ON issue_document (issue_id, type, version);
