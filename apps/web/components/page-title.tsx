"use client";

import { useLayoutEffect } from "react";

/**
 * Owns the browser tab title for client-resolved dashboard routes.
 *
 * Prefer React 19's hoisted `<title>` so Next's metadata manager and the
 * document stay on the same value. Also assign `document.title` in
 * `useLayoutEffect` so a first paint after a cold issue-link open cannot
 * linger on the root layout default ("Project workspace") until a manual
 * refresh — a plain `useEffect` loses that race when Next re-applies
 * metadata after hydration.
 */
export function PageTitle({ title }: { title: string }) {
  useLayoutEffect(() => {
    if (title && document.title !== title) {
      document.title = title;
    }
  }, [title]);

  return <title>{title}</title>;
}
