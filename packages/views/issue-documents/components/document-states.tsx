"use client";

import { AlertCircle, FileStack } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import { Skeleton } from "@multica/ui/components/ui/skeleton";
import { useT } from "../../i18n";
import { CollectionPageState } from "../../layout/collection-page";

export function DocumentListSkeleton() {
  return (
    <div className="space-y-2 p-4">
      {Array.from({ length: 6 }).map((_, i) => (
        <Skeleton key={i} className="h-12 w-full" />
      ))}
    </div>
  );
}

export function DocumentListEmpty() {
  const { t } = useT("issue-documents");
  return (
    <div className="flex flex-1 items-center justify-center">
      <CollectionPageState
        icon={FileStack}
        title={t(($) => $.page.empty.title)}
        description={t(($) => $.page.empty.description)}
      />
    </div>
  );
}

export function DocumentListNoMatches() {
  const { t } = useT("issue-documents");
  return (
    <div className="flex flex-1 items-center justify-center">
      <CollectionPageState
        icon={FileStack}
        title={t(($) => $.page.no_matches.title)}
        description={t(($) => $.page.no_matches.description)}
      />
    </div>
  );
}

export function DocumentListError({ onRetry }: { onRetry: () => void }) {
  const { t } = useT("issue-documents");
  return (
    <div className="flex flex-1 items-center justify-center">
      <CollectionPageState
        role="alert"
        tone="destructive"
        icon={AlertCircle}
        title={t(($) => $.page.list_error.title)}
        description={t(($) => $.page.list_error.fallback)}
        actions={
          <Button type="button" variant="outline" size="sm" onClick={onRetry}>
            {t(($) => $.page.list_error.retry)}
          </Button>
        }
      />
    </div>
  );
}
