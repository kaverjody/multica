"use client";

import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";
import type { IssueDocumentSummary } from "@multica/core/types";
import { Badge } from "@multica/ui/components/ui/badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@multica/ui/components/ui/table";
import { useT, useTimeAgo } from "../../i18n";

export type DocumentSortField = "updated_at" | "title" | "type" | "version";
export type DocumentSortDirection = "asc" | "desc";

export interface DocumentSort {
  field: DocumentSortField;
  direction: DocumentSortDirection;
}

export const DEFAULT_DOCUMENT_SORT: DocumentSort = {
  field: "type",
  direction: "asc",
};

function SortIcon({ sort, field }: { sort: DocumentSort; field: DocumentSortField }) {
  if (sort.field !== field) return <ArrowUpDown className="size-3 text-faint-foreground" />;
  return sort.direction === "asc" ? (
    <ArrowUp className="size-3 text-muted-foreground" />
  ) : (
    <ArrowDown className="size-3 text-muted-foreground" />
  );
}

interface SortableHeadProps {
  sort: DocumentSort;
  field: DocumentSortField;
  label: string;
  onSort: (field: DocumentSortField) => void;
  className?: string;
}

function SortableHead({ sort, field, label, onSort, className }: SortableHeadProps) {
  return (
    <TableHead className={className}>
      <button
        type="button"
        onClick={() => onSort(field)}
        className="inline-flex items-center gap-1 hover:text-foreground"
      >
        {label}
        <SortIcon sort={sort} field={field} />
      </button>
    </TableHead>
  );
}

interface DocumentListProps {
  items: IssueDocumentSummary[];
  sort: DocumentSort;
  onSort: (field: DocumentSortField) => void;
  onSelect: (document: IssueDocumentSummary) => void;
}

export function DocumentList({ items, sort, onSort, onSelect }: DocumentListProps) {
  const { t } = useT("issue-documents");
  const timeAgo = useTimeAgo();

  return (
    <div className="min-h-0 flex-1 overflow-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <SortableHead sort={sort} field="title" label={t(($) => $.table.title)} onSort={onSort} />
            <SortableHead sort={sort} field="type" label={t(($) => $.table.type)} onSort={onSort} />
            <TableHead>{t(($) => $.table.issue)}</TableHead>
            <SortableHead sort={sort} field="version" label={t(($) => $.table.version)} onSort={onSort} />
            <TableHead>{t(($) => $.table.author)}</TableHead>
            <SortableHead sort={sort} field="updated_at" label={t(($) => $.table.updated)} onSort={onSort} />
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((doc) => (
            <TableRow
              key={doc.id}
              className="cursor-pointer"
              onClick={() => onSelect(doc)}
              tabIndex={0}
              role="button"
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  onSelect(doc);
                }
              }}
            >
              <TableCell className="max-w-64">
                <span className="block truncate font-medium">{doc.title}</span>
              </TableCell>
              <TableCell>
                <Badge variant="secondary">{t(($) => $.types[doc.type])}</Badge>
              </TableCell>
              <TableCell>
                <span className="block max-w-56 truncate">
                  {doc.issue_identifier}
                  {doc.issue_title ? ` · ${doc.issue_title}` : ""}
                </span>
              </TableCell>
              <TableCell className="tabular-nums">v{doc.version}</TableCell>
              <TableCell className="max-w-32">
                <span className="block truncate">{doc.author_name || "—"}</span>
              </TableCell>
              <TableCell className="whitespace-nowrap text-xs text-muted-foreground tabular-nums">
                {timeAgo(doc.updated_at)}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
