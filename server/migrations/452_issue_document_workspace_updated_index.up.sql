CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_document_workspace_updated ON issue_document (workspace_id, updated_at DESC);
