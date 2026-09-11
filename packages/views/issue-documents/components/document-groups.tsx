"use client";

import type { IssueDocumentGroup, IssueDocumentSummary } from "@multica/core/types";
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

interface DocumentGroupListProps {
  groups: IssueDocumentGroup[];
  onSelect: (document: IssueDocumentSummary) => void;
}

export function DocumentGroupList({ groups, onSelect }: DocumentGroupListProps) {
  const { t } = useT("issue-documents");
  const timeAgo = useTimeAgo();

  return (
    <div className="min-h-0 flex-1 overflow-auto">
      {groups.map((group) => (
        <section
          key={group.issue_id}
          aria-label={group.issue_identifier}
          className="border-b last:border-b-0"
        >
          <header className="flex items-center gap-2 border-b bg-muted/30 px-5 py-2">
            <span className="shrink-0 font-mono text-xs font-medium text-foreground">
              {group.issue_identifier}
            </span>
            <span className="min-w-0 truncate text-sm font-medium">
              {group.issue_title}
            </span>
            <span className="ml-auto shrink-0 font-mono text-xs tabular-nums text-faint-foreground">
              {group.total}
            </span>
          </header>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t(($) => $.table.type)}</TableHead>
                <TableHead>{t(($) => $.table.title)}</TableHead>
                <TableHead>{t(($) => $.table.version)}</TableHead>
                <TableHead>{t(($) => $.table.author)}</TableHead>
                <TableHead>{t(($) => $.table.updated)}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {group.items.map((doc) => (
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
                  <TableCell>
                    <Badge variant="secondary">{t(($) => $.types[doc.type])}</Badge>
                  </TableCell>
                  <TableCell className="max-w-64">
                    <span className="block truncate font-medium">{doc.title}</span>
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
        </section>
      ))}
    </div>
  );
}
