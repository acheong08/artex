"use client";

import * as React from "react";

import { CpuIcon, FlaskConicalIcon, KeyboardIcon, RadioTowerIcon, SearchIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import { CHAT_SEND_MODE_OPTIONS, type ChatSendMode, setChatSendMode, useChatSendMode } from "@/lib/chat-send-mode";
import type { Settings } from "@/lib/types";

import { UpdateCard } from "./_components/update-card";

export default function SystemSettingsPage() {
  const [trafficCapture, setTrafficCapture] = React.useState(false);
  const [agentTrafficBinding, setAgentTrafficBinding] = React.useState(false);
  const [webSearch, setWebSearch] = React.useState(false);
  const [backend, setBackend] = React.useState("ddgs");
  const [braveKeySet, setBraveKeySet] = React.useState(false);
  const [braveKeyInput, setBraveKeyInput] = React.useState("");
  const [tavilyKeySet, setTavilyKeySet] = React.useState(false);
  const [tavilyKeyInput, setTavilyKeyInput] = React.useState("");
  const [savingTavilyKey, setSavingTavilyKey] = React.useState(false);
  const [proxyInput, setProxyInput] = React.useState("");
  const [savingProxy, setSavingProxy] = React.useState(false);
  const [globalProxyInput, setGlobalProxyInput] = React.useState("");
  const [savingGlobalProxy, setSavingGlobalProxy] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [loaded, setLoaded] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [savingKey, setSavingKey] = React.useState(false);
  const [pyInterp, setPyInterp] = React.useState("");
  const [workers, setWorkers] = React.useState("3");
  const [savingWorkers, setSavingWorkers] = React.useState(false);
  // Scope for injecting operational constraints (all enabled by default).
  const [injectPlanner, setInjectPlanner] = React.useState(true);
  const [injectWorker, setInjectWorker] = React.useState(true);
  // Experimental feature: noa context compaction (disabled by default).
  const [noaCompaction, setNoaCompaction] = React.useState(false);
  // Frontend-only preferences: read and write localStorage directly, not /api/settings.
  const sendMode = useChatSendMode();

  const apply = React.useCallback((s: Settings) => {
    setTrafficCapture(!!s.traffic_capture);
    setAgentTrafficBinding(!!s.agent_traffic_binding);
    setWebSearch(!!s.web_search_enabled);
    setBackend(s.web_search_backend || "ddgs");
    setBraveKeySet(!!s.brave_key_set);
    setTavilyKeySet(!!s.tavily_key_set);
    setProxyInput(s.web_search_proxy ?? "");
    setGlobalProxyInput(s.global_proxy ?? "");
    setPyInterp(s.python_interpreter ?? "");
    setWorkers(String(s.workers ?? 3));
    setInjectPlanner(s.constraints_inject_planner !== false);
    setInjectWorker(s.constraints_inject_worker !== false);
    setNoaCompaction(!!s.noa_compaction);
  }, []);

  const saveWorkers = () => {
    const n = Number(workers);
    if (!Number.isInteger(n) || n <= 0) {
      toast.error("Concurrency must be a positive integer");
      return;
    }
    setSavingWorkers(true);
    api
      .setSettings({ workers: n })
      .then((s) => {
        apply(s);
        toast.success("Worker-agent concurrency saved (applies to tasks started from now on)");
      })
      .catch((e) => toast.error("Unable to save: " + (e as Error).message))
      .finally(() => setSavingWorkers(false));
  };

  const savePython = () => {
    setSaving(true);
    api
      .setSettings({ python_interpreter: pyInterp.trim() })
      .then((s) => {
        apply(s);
        toast.success("Python interpreter settings saved");
      })
      .catch((e) => toast.error("Unable to save: " + (e as Error).message))
      .finally(() => setSaving(false));
  };
  const detectPython = () => {
    setSaving(true);
    api
      .detectPython()
      .then((r) => setPyInterp(r.python_interpreter))
      .catch(() => undefined)
      .finally(() => setSaving(false));
  };

  React.useEffect(() => {
    api
      .settings()
      .then(apply)
      .catch(() => undefined)
      .finally(() => setLoaded(true));
  }, [apply]);

  const toggleTraffic = (v: boolean) => {
    setTrafficCapture(v); // optimistic
    setSaving(true);
    api
      .setSettings({ traffic_capture: v })
      .then(apply)
      .catch(() => setTrafficCapture(!v)) // revert on failure
      .finally(() => setSaving(false));
  };

  const toggleInjectPlanner = (v: boolean) => {
    setInjectPlanner(v); // optimistic
    api
      .setSettings({ constraints_inject_planner: v })
      .then(apply)
      .catch(() => setInjectPlanner(!v)); // revert on failure
  };

  const toggleAgentTrafficBinding = (v: boolean) => {
    setAgentTrafficBinding(v);
    setSaving(true);
    api
      .setSettings({ agent_traffic_binding: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "Automatic traffic linking enabled for agents" : "Automatic traffic linking disabled for agents");
      })
      .catch((e) => {
        setAgentTrafficBinding(!v);
        toast.error(`Unable to save: ${(e as Error).message}`);
      })
      .finally(() => setSaving(false));
  };

  const toggleInjectWorker = (v: boolean) => {
    setInjectWorker(v); // optimistic
    api
      .setSettings({ constraints_inject_worker: v })
      .then(apply)
      .catch(() => setInjectWorker(!v)); // revert on failure
  };

  const toggleNoaCompaction = (v: boolean) => {
    setNoaCompaction(v); // optimistic
    api
      .setSettings({ noa_compaction: v })
      .then((s) => {
        apply(s);
        toast.success(v ? "noa context compaction enabled (applies to runs started from now on)" : "noa context compaction disabled (built-in compaction restored)");
      })
      .catch((e) => {
        setNoaCompaction(!v); // revert on failure
        toast.error(`Unable to save: ${(e as Error).message}`);
      });
  };

  // Persist a web-search patch (enable and/or backend). Optimistic with refetch.
  const saveWebSearch = (patch: Partial<Settings>) => {
    setSaving(true);
    api
      .setSettings(patch)
      .then((s) => {
        apply(s);
        toast.success("Web search settings saved");
      })
      .catch((e) => {
        toast.error("Unable to save: " + (e as Error).message);
        api
          .settings()
          .then(apply)
          .catch(() => undefined);
      })
      .finally(() => setSaving(false));
  };

  const saveBraveKey = () => {
    setSavingKey(true);
    api
      .setSettings({ brave_search_api_key: braveKeyInput })
      .then((s) => {
        apply(s);
        setBraveKeyInput("");
        toast.success("Brave API key saved");
      })
      .catch((e) => toast.error("Unable to save: " + (e as Error).message))
      .finally(() => setSavingKey(false));
  };

  const saveTavilyKey = () => {
    setSavingTavilyKey(true);
    api
      .setSettings({ tavily_search_api_key: tavilyKeyInput })
      .then((s) => {
        apply(s);
        setTavilyKeyInput("");
        toast.success("Tavily API key saved");
      })
      .catch((e) => toast.error("Unable to save: " + (e as Error).message))
      .finally(() => setSavingTavilyKey(false));
  };

  const saveProxy = () => {
    setSavingProxy(true);
    api
      .setSettings({ web_search_proxy: proxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(proxyInput.trim() ? "Egress proxy saved" : "Egress proxy cleared (direct connection enabled)");
      })
      .catch((e) => toast.error("Unable to save: " + (e as Error).message))
      .finally(() => setSavingProxy(false));
  };

  const saveGlobalProxy = () => {
    setSavingGlobalProxy(true);
    api
      .setSettings({ global_proxy: globalProxyInput.trim() })
      .then((s) => {
        apply(s);
        toast.success(globalProxyInput.trim() ? "Global proxy saved" : "Global proxy cleared (direct connection enabled)");
      })
      .catch((e) => toast.error("Unable to save: " + (e as Error).message))
      .finally(() => setSavingGlobalProxy(false));
  };

  // Run a real "test" search ("test") against the CURRENT form values (backend +
  // proxy + entered key), falling back to saved values server-side. Toasts result.
  const runTest = () => {
    setTesting(true);
    api
      .testWebSearch({
        web_search_backend: backend,
        web_search_proxy: proxyInput.trim(),
        brave_search_api_key: braveKeyInput,
        tavily_search_api_key: tavilyKeyInput,
      })
      .then((r) => {
        if (r.ok) toast.success(`Search test succeeded · ${r.backend} returned ${r.count} results`);
        else toast.error("Search test failed: " + (r.error || "Unknown error"));
      })
      .catch((e) => toast.error("Search test failed: " + (e as Error).message))
      .finally(() => setTesting(false));
  };

  // brave-free selected but no key stored and none being entered → tool stays off.
  const braveNeedsKey = webSearch && backend === "brave-free" && !braveKeySet;

  return (
    <div className="flex flex-1 flex-col gap-4 md:gap-6">
      <div>
        <h1 className="text-xl font-semibold tracking-tight">Settings</h1>
        <p className="text-muted-foreground text-sm">Global runtime options</p>
      </div>

      {/* Use columns instead of a grid: the web-search card is several times taller
          than the others, and its height changes with the selected provider (Brave /
          Tavily keys are conditionally rendered). A grid sizes each row to its tallest
          card and leaves large gaps; columns balance cards by content height. Use mb
          rather than gap because column-gap controls only horizontal spacing. */}
      <div className="columns-1 gap-4 md:gap-6 lg:columns-2">
        <UpdateCard />

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Traffic capture
            </CardTitle>
            <CardDescription>
              When enabled, all agent HTTP traffic is recorded through the capture proxy, and the traffic_search / traffic_get
              tools and proxy settings are provided to agents (with proxy instructions in their prompts).
              <br />
              When disabled (the default), no traffic is recorded. Agents <b>do not</b> receive proxy settings or traffic tools, and their prompts <b>do not include</b> proxy instructions. Changes take effect immediately by rebuilding agents.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="traffic-capture" className="text-sm font-normal text-muted-foreground">
              {trafficCapture ? "Enabled · Recording traffic and providing proxy settings" : "Disabled · No traffic recording or proxy settings"}
            </Label>
            <Switch
              id="traffic-capture"
              checked={trafficCapture}
              disabled={!loaded || saving}
              onCheckedChange={toggleTraffic}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Automatic traffic linking for agents
            </CardTitle>
            <CardDescription id="agent-traffic-binding-description">
              Disabled by default. When enabled, the report agent checks existing HTTP requests/responses when a finding is saved, links relevant traffic, and then writes the report.
              <b>Reviewing packets and additional tool calls increase token usage.</b>
              <br />
              Findings can still be reported when traffic is TCP, was not captured, or has no match. This does not affect traffic capture, manual linking, or viewing saved evidence. Takes effect on the next agent run; disabling it immediately prevents new automatic links.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="agent-traffic-binding" className="text-sm font-normal text-muted-foreground">
              {agentTrafficBinding ? "Enabled · Increases token usage" : "Disabled · Manual linking remains available"}
            </Label>
            <Switch
              id="agent-traffic-binding"
              aria-describedby="agent-traffic-binding-description"
              checked={agentTrafficBinding}
              disabled={!loaded || saving}
              onCheckedChange={toggleAgentTrafficBinding}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Global proxy
            </CardTitle>
            <CardDescription>
              All agents’ <b>target traffic</b> uses this proxy for outbound connections (to hide the source IP or use a jump host). Supports <b>http / https / socks5</b> and optional{" "}
              <code>user:pass</code> authentication. Leave blank for a direct connection.
              <br />
              With <b>traffic capture</b> enabled, this is the capture proxy’s <b>upstream</b> (traffic is still fully recorded before being sent through this proxy). With capture disabled, it is provided directly to agent bash / WebFetch. This is separate from the web-search and LLM proxies.
              <br />
              <b>Note:</b> With capture <b>disabled</b>, socks5 depends on each command-line tool supporting <code>ALL_PROXY</code> (curl works; some tools may ignore it). If you primarily use socks5, enable traffic capture—the MITM proxy connects directly, so tools do not need to support it.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-2">
            <Label htmlFor="global-proxy" className="text-sm font-normal text-muted-foreground">
              Proxy URL
            </Label>
            <div className="flex items-center gap-2">
              <Input
                id="global-proxy"
                autoComplete="off"
                placeholder="******host:1080 or http://host:port (leave blank for a direct connection)"
                value={globalProxyInput}
                disabled={!loaded || savingGlobalProxy}
                onChange={(e) => setGlobalProxyInput(e.target.value)}
              />
              <Button type="button" onClick={saveGlobalProxy} disabled={!loaded || savingGlobalProxy}>
                Save
              </Button>
            </div>
            <p className="text-muted-foreground text-xs">
              {globalProxyInput.trim() ? "Configured · All target traffic uses this proxy" : "Not configured · Target traffic connects directly"}
            </p>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <ShieldAlertIcon className="size-4" />
              Operational constraints
            </CardTitle>
            <CardDescription>
              When enabled, each task’s <b>operational constraints</b> (allow/deny rules configured in the task overview) are added to the relevant agents’ system prompts to define exploration boundaries (for example, “test only the current port” or “no brute forcing”).
              <br />
              Control injection for the <b>planner</b> and <b>worker</b> separately. Both are enabled by default. Changes take effect on the next turn without rebuilding agents. Disabled agents will no longer receive these constraints.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-planner" className="text-sm font-normal text-muted-foreground">
                Include in planner prompt{injectPlanner ? " · Enabled" : " · Disabled"}
              </Label>
              <Switch
                id="inject-planner"
                checked={injectPlanner}
                disabled={!loaded}
                onCheckedChange={toggleInjectPlanner}
              />
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="inject-worker" className="text-sm font-normal text-muted-foreground">
                Include in worker prompt{injectWorker ? " · Enabled" : " · Disabled"}
              </Label>
              <Switch
                id="inject-worker"
                checked={injectWorker}
                disabled={!loaded}
                onCheckedChange={toggleInjectWorker}
              />
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <FlaskConicalIcon className="size-4" />
              Experimental features
            </CardTitle>
            <CardDescription>
              Features still under evaluation. Disabled by default; they may change agent behavior or affect stability. Review the impact before enabling.
              <br />
              <b>noa context compaction</b>: The model actively compresses long conversation histories (noa v0.4.0). When enabled, the four agent types integrated by the platform (<b>planner / worker / main agent / chat</b>) use noa to manage context instead of built-in compaction. Original compressed content is archived in the task workspace for reference. Changes take effect immediately for runs started from now on, without rebuilding agents; disabling restores built-in compaction.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="noa-compaction" className="text-sm font-normal text-muted-foreground">
              noa context compaction{noaCompaction ? " · Enabled" : " · Disabled"}
            </Label>
            <Switch
              id="noa-compaction"
              checked={noaCompaction}
              disabled={!loaded}
              onCheckedChange={toggleNoaCompaction}
            />
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <SearchIcon className="size-4" />
              Web search
            </CardTitle>
            <CardDescription>
              This is the <b>master switch and provider configuration</b> for web search. Once enabled, you can choose whether to enable
              <b>web_search</b> in each agent’s settings (returns titles, links, and snippets only; WebFetch retrieves page content). Web search does <b>not use</b>
              the capture proxy and is independent of traffic capture.
              <br />
              Providers: <b>DuckDuckGo (ddgs)</b> (no key required), <b>Brave (free tier)</b> (requires a Brave API key),{" "}
              <b>Tavily</b> (requires a Tavily API key), or <b>DeepSeek</b> (uses the current LLM profile). When the master switch is off, web search is disabled for all agents.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="web-search" className="text-sm font-normal text-muted-foreground">
                {webSearch ? "Enabled · Configure web search per agent" : "Disabled · Agents cannot use web search"}
              </Label>
              <Switch
                id="web-search"
                checked={webSearch}
                disabled={!loaded || saving}
                onCheckedChange={(v) => {
                  setWebSearch(v); // optimistic
                  saveWebSearch({ web_search_enabled: v });
                }}
              />
            </div>

            {webSearch && (
              <div className="flex items-center justify-between gap-4">
                <Label className="text-sm font-normal text-muted-foreground">Search provider</Label>
                <Select
                  value={backend}
                  disabled={!loaded || saving}
                  onValueChange={(v) => {
                    setBackend(v); // optimistic
                    saveWebSearch({ web_search_backend: v });
                  }}
                >
                  <SelectTrigger className="w-48 shrink-0">
                    <SelectValue placeholder="Select a provider" />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="ddgs">DuckDuckGo (ddgs · free, no key)</SelectItem>
                    <SelectItem value="brave-free">Brave (free tier · key required)</SelectItem>
                    <SelectItem value="tavily">Tavily (key required)</SelectItem>
                    <SelectItem value="deepseek">DeepSeek (official)</SelectItem>
                  </SelectContent>
                </Select>
              </div>
            )}

            {webSearch && backend === "deepseek" && (
              <div className="border-border/60 bg-muted/30 flex flex-col gap-2 rounded-md border p-3">
                <p className="text-sm font-medium">DeepSeek official web search</p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  This provider uses the <b>currently active LLM profile</b>. It therefore
                  <b>supports only official DeepSeek models</b>, and the profile <b>must use the anthropic protocol</b>
                  —DeepSeek’s OpenAI-compatible endpoint does not support server-side search. Changing the LLM profile may make this provider unavailable.
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  Unlike other providers, searches are performed by the <b>DeepSeek service</b>. Each search uses an additional model call (and incurs token
                  costs). Search requests <b>do not use the egress proxy above</b> and are <b>not recorded as traffic</b>. Results contain <b>titles and links only</b>
                  (no snippets); use WebFetch to retrieve page content.
                </p>
                <p className="text-muted-foreground text-xs leading-relaxed">
                  Confirm these requirements yourself; the system does not enforce them. Use the “Test search” button below to verify the configuration.
                </p>
              </div>
            )}

            {webSearch && backend === "brave-free" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="brave-key" className="text-sm font-normal text-muted-foreground">
                  Brave Search API Key
                  {braveKeySet && <span className="ml-2 text-xs text-emerald-500">Configured</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="brave-key"
                    type="password"
                    autoComplete="off"
                    placeholder={braveKeySet ? "Configured (leave blank to keep current key)" : "Enter Brave API key"}
                    value={braveKeyInput}
                    disabled={!loaded || savingKey}
                    onChange={(e) => setBraveKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveBraveKey}
                    disabled={!loaded || savingKey || braveKeyInput.trim() === ""}
                  >
                    Save
                  </Button>
                </div>
                {braveNeedsKey && (
                  <p className="text-xs text-amber-500">
                    Brave is selected but no key is configured. Web search will not be enabled until a key is saved.
                  </p>
                )}
                <p className="text-muted-foreground text-xs">
                  The free tier includes about 2,000 searches/month. Get a key at https://brave.com/search/api/.
                </p>
              </div>
            )}

            {webSearch && backend === "tavily" && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="tavily-key" className="text-sm font-normal text-muted-foreground">
                  Tavily Search API Key
                  {tavilyKeySet && <span className="ml-2 text-xs text-emerald-500">Configured</span>}
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="tavily-key"
                    type="password"
                    autoComplete="off"
                    placeholder={tavilyKeySet ? "Configured (leave blank to keep current key)" : "Enter Tavily API key (tvly-…)"}
                    value={tavilyKeyInput}
                    disabled={!loaded || savingTavilyKey}
                    onChange={(e) => setTavilyKeyInput(e.target.value)}
                  />
                  <Button
                    type="button"
                    onClick={saveTavilyKey}
                    disabled={!loaded || savingTavilyKey || tavilyKeyInput.trim() === ""}
                  >
                    Save
                  </Button>
                </div>
                {webSearch && backend === "tavily" && !tavilyKeySet && (
                  <p className="text-xs text-amber-500">
                    Tavily is selected but no key is configured. Web search will not be enabled until a key is saved.
                  </p>
                )}
                <p className="text-muted-foreground text-xs">Sign up and get an API key at https://tavily.com.</p>
              </div>
            )}

            {webSearch && (
              <div className="flex flex-col gap-2">
                <Label htmlFor="ws-proxy" className="text-sm font-normal text-muted-foreground">
                  Egress proxy (optional)
                </Label>
                <div className="flex items-center gap-2">
                  <Input
                    id="ws-proxy"
                    autoComplete="off"
                    placeholder="http://host:port or socks5://host:port (blank = direct)"
                    value={proxyInput}
                    disabled={!loaded || savingProxy}
                    onChange={(e) => setProxyInput(e.target.value)}
                  />
                  <Button type="button" onClick={saveProxy} disabled={!loaded || savingProxy}>
                    Save
                  </Button>
                </div>
                <p className="text-muted-foreground text-xs">
                  Separate egress proxy used only to access the search provider (VPN/SOCKS, etc.). It is independent of the MITM traffic-capture proxy; searches use this proxy when direct access is unavailable.
                </p>
              </div>
            )}

            {webSearch && (
              <div className="flex items-center justify-between gap-4 border-t pt-4">
                <p className="text-muted-foreground text-xs">
                  Run an actual search for “test” using the current provider, proxy, and key to verify that it works.
                </p>
                <Button
                  type="button"
                  variant="outline"
                  onClick={runTest}
                  disabled={!loaded || testing}
                  className="shrink-0"
                >
                  {testing ? "Testing…" : "Test search"}
                </Button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <RadioTowerIcon className="size-4" />
              Custom scripts · Python interpreter
            </CardTitle>
            <CardDescription>
              Custom <b>script</b> tools use this interpreter to run Python. It is detected automatically on startup (python3 preferred); enter an absolute path to a virtual environment or specific version, or leave blank for runtime detection.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                className="font-mono text-sm"
                placeholder="/usr/bin/python3 (blank = auto-detect)"
                value={pyInterp}
                disabled={!loaded || saving}
                onChange={(e) => setPyInterp(e.target.value)}
              />
              <Button variant="outline" onClick={detectPython} disabled={!loaded || saving}>
                Detect again
              </Button>
              <Button onClick={savePython} disabled={!loaded || saving}>
                Save
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <CpuIcon className="size-4" />
              Worker concurrency
            </CardTitle>
            <CardDescription>
              Number of worker agents running concurrently per task (default: 3). Higher values allow more parallel exploration and consume more resources. Changes
              <b>apply to tasks started from now on</b>; running tasks are unaffected.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Input
                type="number"
                min={1}
                className="w-32 font-mono text-sm"
                placeholder="3"
                value={workers}
                disabled={!loaded || savingWorkers}
                onChange={(e) => setWorkers(e.target.value)}
              />
              <Button onClick={saveWorkers} disabled={!loaded || savingWorkers}>
                Save
              </Button>
            </div>
          </CardContent>
        </Card>

        <Card className="mb-4 break-inside-avoid md:mb-6">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <KeyboardIcon className="size-4" />
              Chat input send shortcut
            </CardTitle>
            <CardDescription>
              Shared by the chat page and the main-agent conversation in task details. Changes take effect immediately; no need to save.
              <br />
              This preference is <b>stored only in this browser</b> and is not synced with your account. Set it again if you switch browsers or clear site data.
            </CardDescription>
          </CardHeader>
          <CardContent className="flex items-center justify-between gap-4">
            <Label htmlFor="chat-send-mode" className="text-sm font-normal text-muted-foreground">
              Send behavior
            </Label>
            <Select value={sendMode} onValueChange={(v) => setChatSendMode(v as ChatSendMode)}>
              <SelectTrigger id="chat-send-mode" className="w-72">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {CHAT_SEND_MODE_OPTIONS.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
