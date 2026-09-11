"use client";

import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useWorkspaceId } from "@multica/core/hooks";
import { issueDocumentGroupListOptions } from "@multica/core/issue-documents/queries";
import type { IssueDocumentSummary } from "@multica/core/types";
import {
  DocumentDetailDrawer,
} from "./components/document-detail-drawer";
import {
  DEFAULT_DOCUMENT_SORT,
  type DocumentSort,
  type DocumentSortField,
} from "./components/document-list";
import { DocumentGroupList } from "./components/document-groups";
import {
  DocumentListEmpty,
  DocumentListError,
  DocumentListNoMatches,
  DocumentListSkeleton,
} from "./components/document-states";
import {
  DocumentToolbar,
  type DocumentTypeFilter,
} from "./components/document-toolbar";

function useDebouncedValue(value: string, delay = 300): string {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delay);
    return () => clearTimeout(timer);
  }, [value, delay]);
  return debounced;
}

export function IssueDocumentsPage() {
  const wsId = useWorkspaceId();

  const [typeFilter, setTypeFilter] = useState<DocumentTypeFilter>("all");
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<DocumentSort>(DEFAULT_DOCUMENT_SORT);
  const [selected, setSelected] = useState<IssueDocumentSummary | null>(null);

  const debouncedSearch = useDebouncedValue(search);

  const hasActiveFilters =
    typeFilter !== "all" || debouncedSearch.trim() !== "";

  const query = useQuery(
    issueDocumentGroupListOptions(wsId, {
      type: typeFilter === "all" ? undefined : typeFilter,
      q: debouncedSearch.trim() || undefined,
      sort: sort.field,
      order: sort.direction,
    }),
  );

  const groups = useMemo(() => query.data?.groups ?? [], [query.data]);
  const total = query.data?.total ?? 0;

  const handleSortFieldChange = (field: DocumentSortField) => {
    setSort((prev) =>
      prev.field === field
        ? prev
        : { field, direction: DEFAULT_DIRECTIONS[field] },
    );
  };

  const handleSortDirectionChange = () => {
    setSort((prev) => ({
      field: prev.field,
      direction: prev.direction === "asc" ? "desc" : "asc",
    }));
  };

  if (query.isError) {
    return (
      <div className="flex min-h-0 flex-1 flex-col">
        <DocumentToolbar
          totalCount={0}
          typeFilter={typeFilter}
          search={search}
          sort={sort}
          onTypeFilterChange={setTypeFilter}
          onSearchChange={setSearch}
          onSortFieldChange={handleSortFieldChange}
          onSortDirectionChange={handleSortDirectionChange}
        />
        <DocumentListError onRetry={() => query.refetch()} />
      </div>
    );
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <DocumentToolbar
        totalCount={total}
        typeFilter={typeFilter}
        search={search}
        sort={sort}
        onTypeFilterChange={setTypeFilter}
        onSearchChange={setSearch}
        onSortFieldChange={handleSortFieldChange}
        onSortDirectionChange={handleSortDirectionChange}
      />

      {query.isLoading ? (
        <DocumentListSkeleton />
      ) : groups.length === 0 ? (
        hasActiveFilters ? (
          <DocumentListNoMatches />
        ) : (
          <DocumentListEmpty />
        )
      ) : (
        <DocumentGroupList groups={groups} onSelect={setSelected} />
      )}

      <DocumentDetailDrawer document={selected} onClose={() => setSelected(null)} />
    </div>
  );
}

const DEFAULT_DIRECTIONS: Record<DocumentSortField, DocumentSort["direction"]> = {
  type: "asc",
  title: "asc",
  updated_at: "desc",
  version: "desc",
};
