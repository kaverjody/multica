"use client";

import { useQuery } from "@tanstack/react-query";
import { Download } from "lucide-react";
import { useWorkspaceId } from "@multica/core/hooks";
import {
  attachmentDownloadPath,
  type IssueDocumentSummary,
} from "@multica/core/types";
import { buttonVariants } from "@multica/ui/components/ui/button";
import { Markdown } from "@multica/ui/markdown";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@multica/ui/components/ui/sheet";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import {
  issueDocumentDetailOptions,
  issueDocumentVersionsOptions,
} from "@multica/core/issue-documents/queries";
import { useT, useTimeAgo } from "../../i18n";

interface DocumentDetailDrawerProps {
  document: IssueDocumentSummary | null;
  onClose: () => void;
}

export function DocumentDetailDrawer({ document, onClose }: DocumentDetailDrawerProps) {
  const { t } = useT("issue-documents");
  const timeAgo = useTimeAgo();
  const wsId = useWorkspaceId();

  const detailQuery = useQuery({
    ...issueDocumentDetailOptions(wsId, document?.id ?? ""),
    enabled: !!document,
  });
  const versionsQuery = useQuery({
    ...issueDocumentVersionsOptions(wsId, document?.id ?? ""),
    enabled: !!document,
  });

  const detail = detailQuery.data;
  const isFile = detail?.content_type === "file";

  return (
    <Sheet open={!!document} onOpenChange={(open) => !open && onClose()}>
      <SheetContent side="right" className="flex w-full flex-col gap-4 sm:max-w-lg">
        <SheetHeader>
          <SheetTitle className="truncate">{document?.title}</SheetTitle>
          <SheetDescription className="line-clamp-2">
            {document
              ? `${document.issue_identifier} · ${t(($) => $.types[document.type])} · v${document.version}`
              : ""}
          </SheetDescription>
        </SheetHeader>

        <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <span>{document?.author_name || "—"}</span>
          {detail ? <span>· {timeAgo(detail.updated_at)}</span> : null}
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto">
          {detailQuery.isPending ? (
            <div className="space-y-2">
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-4 w-5/6" />
              <Skeleton className="h-4 w-4/6" />
            </div>
          ) : isFile && detail?.file_attachment_id ? (
            <div className="flex flex-col items-center gap-3 py-10">
              <p className="text-sm text-muted-foreground">{document?.title}</p>
              <a
                href={attachmentDownloadPath(detail.file_attachment_id)}
                className={buttonVariants({ variant: "outline", size: "sm" })}
              >
                <Download aria-hidden="true" className="size-3.5" />
                {t(($) => $.detail.download)}
              </a>
            </div>
          ) : detail?.content ? (
            detail.content_type === "markdown" ? (
              <Markdown mode="full" className="text-sm">
                {detail.content}
              </Markdown>
            ) : (
              <pre className="whitespace-pre-wrap break-words text-sm">{detail.content}</pre>
            )
          ) : (
            <p className="py-10 text-center text-sm text-muted-foreground">
              {t(($) => $.detail.no_content)}
            </p>
          )}
        </div>

        {versionsQuery.data && versionsQuery.data.items.length > 0 ? (
          <div className="shrink-0 border-t pt-3">
            <h3 className="mb-2 text-xs font-medium text-muted-foreground">
              {t(($) => $.detail.versions)}
            </h3>
            <ul className="space-y-1.5">
              {versionsQuery.data.items.map((v) => (
                <li
                  key={v.id}
                  className="flex items-center gap-2 text-xs text-muted-foreground"
                >
                  <span className="tabular-nums">v{v.version}</span>
                  <span className="ml-auto tabular-nums">{timeAgo(v.updated_at)}</span>
                </li>
              ))}
            </ul>
          </div>
        ) : null}
      </SheetContent>
    </Sheet>
  );
}
