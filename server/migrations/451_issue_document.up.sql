-- issue_document: the single source of truth for issue-flow intermediate
-- documents ("Issue Documents" tab, CLO-278). One row per version per
-- (workspace_id, issue_id, type); older versions are kept and marked
-- `superseded` so version history stays available.
--
-- The type / status / content_type enums are aligned with the workflow-branch
-- `artifact` model so a future workflow integration can map cleanly. No FOREIGN
-- KEY constraints (repo convention): relationships are validated in the
-- application layer.
CREATE TABLE issue_document (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id       UUID NOT NULL,
    issue_id           UUID NOT NULL,
    type               TEXT NOT NULL
        CHECK (type IN ('requirements', 'architecture', 'development', 'testing',
                        'code_review', 'security', 'documentation', 'deployment', 'other')),
    title              TEXT NOT NULL,
    content            TEXT,
    content_type       TEXT NOT NULL DEFAULT 'markdown'
        CHECK (content_type IN ('markdown', 'json', 'text', 'file')),
    file_attachment_id UUID,
    version            INT NOT NULL DEFAULT 1,
    status             TEXT NOT NULL DEFAULT 'submitted'
        CHECK (status IN ('draft', 'submitted', 'approved', 'rejected', 'superseded')),
    author_type        TEXT NOT NULL
        CHECK (author_type IN ('member', 'agent')),
    author_id          UUID NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
