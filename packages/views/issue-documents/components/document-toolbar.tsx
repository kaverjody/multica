"use client";

import { FileStack, Search } from "lucide-react";
import { Input } from "@multica/ui/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@multica/ui/components/ui/select";
import type { IssueDocumentType } from "@multica/core/types";
import { useT } from "../../i18n";
import { CollectionPageHeader } from "../../layout/collection-page";
import type { DocumentSort, DocumentSortField } from "./document-list";

export const ISSUE_DOCUMENT_TYPES: IssueDocumentType[] = [
  "requirements",
  "architecture",
  "development",
  "testing",
  "code_review",
  "security",
  "documentation",
  "deployment",
  "other",
];

export const ISSUE_DOCUMENT_SORT_FIELDS: DocumentSortField[] = [
  "type",
  "updated_at",
  "title",
  "version",
];

export type DocumentTypeFilter = IssueDocumentType | "all";

interface DocumentToolbarProps {
  totalCount: number;
  typeFilter: DocumentTypeFilter;
  search: string;
  sort: DocumentSort;
  onTypeFilterChange: (value: DocumentTypeFilter) => void;
  onSearchChange: (value: string) => void;
  onSortFieldChange: (field: DocumentSortField) => void;
  onSortDirectionChange: () => void;
}

export function DocumentToolbar({
  totalCount,
  typeFilter,
  search,
  sort,
  onTypeFilterChange,
  onSearchChange,
  onSortFieldChange,
  onSortDirectionChange,
}: DocumentToolbarProps) {
  const { t } = useT("issue-documents");

  const typeItems = [
    { value: "all" as const, label: t(($) => $.filters.all_types) },
    ...ISSUE_DOCUMENT_TYPES.map((type) => ({
      value: type,
      label: t(($) => $.types[type]),
    })),
  ];
  const sortItems = ISSUE_DOCUMENT_SORT_FIELDS.map((field) => ({
    value: field,
    label: t(($) => $.filters.sort[field]),
  }));

  return (
    <>
      <CollectionPageHeader
        icon={FileStack}
        title={t(($) => $.page.title)}
        count={totalCount}
        description={t(($) => $.page.tagline)}
      />
      <div className="flex flex-wrap items-center gap-2 border-b px-5 py-2">
        <Select
          items={typeItems}
          value={typeFilter}
          onValueChange={(v) => onTypeFilterChange((v as DocumentTypeFilter) ?? "all")}
        >
          <SelectTrigger className="h-8 w-40">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {typeItems.map((item) => (
              <SelectItem key={item.value} value={item.value}>
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          items={sortItems}
          value={sort.field}
          onValueChange={(v) => onSortFieldChange((v as DocumentSortField) ?? "type")}
        >
          <SelectTrigger
            className="h-8 w-40"
            aria-label={t(($) => $.filters.sort_by)}
            title={t(($) => $.filters.sort_by)}
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {sortItems.map((item) => (
              <SelectItem key={item.value} value={item.value}>
                {item.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <button
          type="button"
          onClick={onSortDirectionChange}
          className="inline-flex h-8 items-center gap-1 rounded-md border border-border px-2.5 text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
          aria-label={t(($) => $.filters.sort_direction)}
          title={t(($) => $.filters.sort_direction)}
        >
          {sort.direction === "asc" ? "↑" : "↓"}
          {t(($) => $.filters.sort[sort.field])}
        </button>
        <div className="relative min-w-0 flex-1 md:max-w-xs">
          <Search
            aria-hidden="true"
            className="absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground"
          />
          <Input
            value={search}
            onChange={(e) => onSearchChange(e.target.value)}
            placeholder={t(($) => $.filters.search)}
            className="h-8 pl-8"
          />
        </div>
      </div>
    </>
  );
}
