CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_document_workspace_status ON issue_document (workspace_id, status);
