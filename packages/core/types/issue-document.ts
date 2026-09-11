export type IssueDocumentType =
  | "requirements"
  | "architecture"
  | "development"
  | "testing"
  | "code_review"
  | "security"
  | "documentation"
  | "deployment"
  | "other";

export type IssueDocumentStatus =
  | "draft"
  | "submitted"
  | "approved"
  | "rejected"
  | "superseded";

export type IssueDocumentContentType = "markdown" | "json" | "text" | "file";

export type IssueDocumentAuthorType = "member" | "agent";

export interface IssueDocumentSummary {
  id: string;
  workspace_id: string;
  issue_id: string;
  issue_identifier: string;
  issue_title: string;
  type: IssueDocumentType;
  title: string;
  version: number;
  status: IssueDocumentStatus;
  content_type: IssueDocumentContentType;
  file_attachment_id: string | null;
  author_type: IssueDocumentAuthorType;
  author_id: string;
  author_name: string;
  created_at: string;
  updated_at: string;
}

export interface IssueDocumentDetail extends IssueDocumentSummary {
  content?: string | null;
}

export interface IssueDocumentVersion {
  id: string;
  version: number;
  status: IssueDocumentStatus;
  title: string;
  content_type: IssueDocumentContentType;
  file_attachment_id: string | null;
  created_at: string;
  updated_at: string;
}

export interface IssueDocumentListResponse {
  items: IssueDocumentSummary[];
  total: number;
}

export interface IssueDocumentGroup {
  issue_id: string;
  issue_identifier: string;
  issue_title: string;
  items: IssueDocumentSummary[];
  total: number;
}

export interface IssueDocumentGroupListResponse {
  groups: IssueDocumentGroup[];
  total: number;
}

export interface ListIssueDocumentsParams {
  type?: IssueDocumentType;
  status?: IssueDocumentStatus;
  issue_id?: string;
  q?: string;
  sort?: "updated_at" | "title" | "type" | "status" | "version";
  order?: "asc" | "desc";
  limit?: number;
  offset?: number;
}

export interface ListIssueDocumentGroupsParams {
  type?: IssueDocumentType;
  status?: IssueDocumentStatus;
  issue_id?: string;
  q?: string;
  sort?: "updated_at" | "title" | "type" | "status" | "version";
  order?: "asc" | "desc";
}

export interface IssueDocumentVersionsResponse {
  issue_id: string;
  type: IssueDocumentType;
  items: IssueDocumentVersion[];
}

export interface CreateIssueDocumentRequest {
  issue_id: string;
  type: IssueDocumentType;
  title: string;
  content?: string;
  content_type?: IssueDocumentContentType;
  file_attachment_id?: string;
  status?: IssueDocumentStatus;
}
