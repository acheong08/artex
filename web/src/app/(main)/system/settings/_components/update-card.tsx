"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress } from "@/lib/types";

/** Maximum wait for a new version to come online. An update starts the process three
 * times (staging → replacement → new version), each taking seconds; three minutes
 * allows for slow disks and Docker container recreation. */
const RESTART_TIMEOUT_MS = 180_000;

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // Separate from progress: the process exits after staging, so SSE disconnects and
  // we must switch to polling /api/health.
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);

  // quiet also determines whether to bypass the backend cache. Automatic checks on
  // page entry use the cache (the top bar just checked); manual "Check for updates"
  // bypasses it so newly released versions appear immediately.
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        if (!quiet) {
          if (r.error) toast.error("Update check failed: " + r.error);
          else if (r.has_update) toast.success(`New version ${r.latest} available`);
          else if (r.comparable) toast.success("You're already using the latest version");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error("Update check failed: " + (e as Error).message);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // Poll /api/health until the version changes.
  //
  // Check that the version changed, not merely that the server is reachable: the old
  // version briefly restarts during replacement to install artex.new and then exits.
  // A connectivity check alone would incorrectly report success.
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        const r = await fetch("/api/health", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`Updated to ${j.version}. Reloading the page.`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // Being unreachable during restart is expected; keep polling.
      }
    }
    setRestarting(false);
    toast.error("Timed out waiting for the service to restart. Check the backend logs or make sure artex was started with start.sh / start.bat.");
  }, []);

  // Subscribe to update progress. SSE bypasses Next's /api rewrite, which buffers
  // responses and prevents events from streaming.
  const openStream = React.useCallback(
    (fromVersion: string) => {
      const es = new EventSource(sseUrl("/api/update/stream"));
      es.onmessage = (ev) => {
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          es.close();
          setBusy(false);
          toast.error("Update failed: " + (p.error || p.message));
          return;
        }
        if (p.phase === "staged") {
          es.close();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        // SSE disconnects when the process exits. If waiting for a restart, this is
        // expected; let /api/health polling determine when it is back.
        es.close();
      };
      return es;
    },
    [waitForNewVersion],
  );

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `Update to ${info.latest}?\n\n` +
        "The program will restart and interrupt any running tasks.\n" +
        (info.mode === "docker"
          ? "\nNote: An in-container update only replaces the program; it does not update tools such as playwright or nmap in the image. " +
            "If the new version requires new tools, use docker compose pull instead."
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "Preparing…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      setBusy(false);
      setProgress(null);
      toast.error("Failed to start update: " + (e as Error).message);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "Roll back to the previous version?\n\nThe program will restart and interrupt any running tasks.\nNote: The database schema will not be rolled back, and the older version may not recognize data written by the newer version.",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("Switched to the previous version. Restarting…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error("Rollback failed: " + (e as Error).message);
      });
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // Only the download phase has a real percentage (from Content-Length). Validation,
  // extraction, and restart durations are unknown, so fill the progress bar and pulse
  // it to indicate work is in progress without an ETA.
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;

  return (
    // The settings page uses a multi-column masonry layout; cards provide their own
    // vertical spacing and avoid splitting across columns (see page.tsx).
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          Version and Updates
        </CardTitle>
        <CardDescription>Check GitHub for new versions and install them. Updates restart the program and interrupt running tasks.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">Current version</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "Standalone"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">Latest version</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-xs text-muted-foreground underline-offset-4 hover:underline"
            >
              Release notes <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-xs text-amber-700 dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            Unable to connect to GitHub: {info.error}
            {" "}Configure a global proxy above and try again.
          </p>
        )}

        {info && !info.comparable && info.reason && <p className="text-xs text-muted-foreground">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            No release package is available for {info.os}/{info.arch} in {info.latest} (missing {info.asset}); automatic updates aren't available.
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-xs text-muted-foreground">
            Downloading <span className="font-mono">{info.asset}</span>
            {info.size ? ` (${humanSize(info.size)})` : ""}. It will be replaced only after SHA256 verification and a smoke test; the current version is preserved if either fails.
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            You're already using the latest version.
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-xs text-muted-foreground">
            Docker updates replace only the program, not tools such as playwright or nmap in the image. Running
            <span className="font-mono"> docker compose up -d </span>
            docker compose up -d rebuilds the container, which restores the version bundled in the image. To update the image as well, run
            <span className="font-mono"> docker compose pull artex &amp;&amp; docker compose up -d artex</span>.
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-xs text-muted-foreground">
              {restarting ? "Restarting to apply the update. Please wait; the page will refresh automatically…" : progress?.message}
            </p>
          </div>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            Check for updates
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `Update to ${info.latest}` : "Update now"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              Roll back to previous version
            </Button>
          )}
        </div>

        <p className="text-xs text-muted-foreground">
          One-click updates rely on the service supervisor to restart the program. Start ARTEX with <span className="font-mono">start.sh</span> (or
          <span className="font-mono"> start.bat</span> on Windows); running the artex binary directly won't restart it automatically after it exits.
        </p>
      </CardContent>
    </Card>
  );
}
