"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * "New version" indicator in the top bar: check once per page load and show it next
 * to the version number when an update is available. Clicking it opens the
 * "Version and updates" card in system settings.
 *
 * The backend caches GitHub query results for 30 minutes, so checking on each mount
 * is safe. Unauthenticated GitHub API requests are limited to 60 per hour per IP;
 * without that cache, opening a few tabs could exhaust the quota before an update.
 *
 * Silently ignore failures here: the top bar is not the place for errors. Users can
 * check for updates in settings to see the reason.
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already checks that the version numbers are comparable, so
        // development builds will not show this indicator.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silently ignore network errors and GitHub rate limits in the top bar.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`Version ${latest} is available. Click to update.`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* The animated pulse stands out among the many top-bar elements. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">New version {latest}</span>
      <span className="sm:hidden">New version</span>
    </Link>
  );
}
