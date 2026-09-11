import { infiniteQueryOptions, queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type {
  ListIssueDocumentGroupsParams,
  ListIssueDocumentsParams,
} from "../types";

export const issueDocumentKeys = {
  all: (wsId: string) => ["issue-documents", wsId] as const,
  list: (wsId: string, params: ListIssueDocumentsParams = {}) =>
    [...issueDocumentKeys.all(wsId), "list", params] as const,
  groups: (wsId: string, params: ListIssueDocumentGroupsParams = {}) =>
    [...issueDocumentKeys.all(wsId), "groups", params] as const,
  detail: (wsId: string, id: string) =>
    [...issueDocumentKeys.all(wsId), "detail", id] as const,
  versions: (wsId: string, id: string) =>
    [...issueDocumentKeys.all(wsId), "versions", id] as const,
};

export const ISSUE_DOCUMENT_PAGE_SIZE = 50;

export function issueDocumentInfiniteListOptions(
  wsId: string,
  params: ListIssueDocumentsParams = {},
) {
  return infiniteQueryOptions({
    queryKey: issueDocumentKeys.list(wsId, params),
    initialPageParam: 0,
    queryFn: ({ pageParam }) =>
      api.listIssueDocuments({
        ...params,
        limit: ISSUE_DOCUMENT_PAGE_SIZE,
        offset: pageParam,
      }),
    getNextPageParam: (lastPage, allPages) => {
      const loaded = allPages.reduce((n, page) => n + page.items.length, 0);
      return loaded < lastPage.total ? loaded : undefined;
    },
    staleTime: 60_000,
  });
}

export function issueDocumentGroupListOptions(
  wsId: string,
  params: ListIssueDocumentGroupsParams = {},
) {
  return queryOptions({
    queryKey: issueDocumentKeys.groups(wsId, params),
    queryFn: () => api.listIssueDocumentGroups(params),
    staleTime: 60_000,
  });
}

export function issueDocumentDetailOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: issueDocumentKeys.detail(wsId, id),
    queryFn: () => api.getIssueDocument(id),
    enabled: !!id,
  });
}

export function issueDocumentVersionsOptions(wsId: string, id: string) {
  return queryOptions({
    queryKey: issueDocumentKeys.versions(wsId, id),
    queryFn: () => api.getIssueDocumentVersions(id),
    enabled: !!id,
  });
}
