package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Worker is an LLM work agent (docs §4.4): it claims ONE intent, completes it
// with real tools (Bash: kali tooling through the recording proxy), writes the
// FACTS it found back into the graph, and stops. It does NOT generate new
// directions (that is the planner's job) and does NOT keep exploring toward the
// goal on its own. Multiple workers run concurrently as goroutines.
// WebSearchOpts is the web-search backend selection the server pushes into each
// agent (planner/worker/main). Enabled=false leaves the web_search tool off.
// Backend is "ddgs" (no key), "brave-free" (BraveKey required), "tavily"
// (TavilyKey required), or "deepseek" (DeepSeek* required, filled from the
// active LLM profile). It maps directly onto agentcore.Options.
// Proxy is a dedicated egress proxy for the search request (http/https/socks5),
// independent of the traffic-recording MITM proxy — set it when the search endpoint
// is only reachable via a VPN/SOCKS proxy. Empty = direct.
//
// The DeepSeek backend differs from the other three: it has no directly callable
// search API. Search is available only through the web_search_20250305 server tool
// in its Anthropic-compatible messages interface, so each search uses a model call
// and is issued by DeepSeek's servers. It does not pass through the local proxy or
// appear in recorded traffic.
type WebSearchOpts struct {
	Enabled   bool
	Backend   string
	BraveKey  string
	TavilyKey string
	Proxy     string
	// DeepSeek* values come from the active LLM configuration (only for official
	// DeepSeek endpoints using the Anthropic format); they are not configured
	// separately and change with the selected LLM configuration.
	DeepSeekBaseURL string
	DeepSeekAPIKey  string
	DeepSeekModel   string
}

type Worker struct {
	findingRecorder FindingRecorder
	prov            llm.Provider
	model           string
	workDir         string
	proxyAddr       string
	proxyCACert     string            // recording proxy's CA cert path (for WebFetch HTTPS verify)
	webSearch       WebSearchOpts     // web_search tool backend selection (off by default)
	tx              *transcript.Store // raw LLM conversation persistence (nil = off)
	window          int               // context window in tokens (for compaction)
	windowFn        func() int        // optional dynamic task-chain minimum
	maxTurns        int               // max agent turns per run (0 = unlimited)
	// runTimeout is the wall-clock budget for the main exploration of one intent
	// (0 = unlimited). When it fires, the run is cut and a settlement round is
	// forced so already-identified facts get written back instead of being lost.
	runTimeout time.Duration
	// extraTools are host-provided tools (e.g. traffic query, oast) appended to
	// the worker's graph write-back tools.
	extraTools []actool.CoreTool
	// injectConstraints resolves whether this task's operation constraints get
	// injected into the worker system prompt. Read per run so the settings toggle
	// takes effect without rebuilding the agent. nil = inject (default).
	injectConstraints func() bool
	// nonStreamingFn resolves whether this run uses the non-streaming (Complete)
	// path. Read per run so a profile/task toggle takes effect without rebuilding
	// the agent. nil = streaming (default).
	nonStreamingFn func() bool
	// noaEnabledFn resolves whether this run uses the experimental noa context-
	// compression mechanism. Read per run, like nonStreaming. nil = off (built-in
	// compaction).
	noaEnabledFn func() bool
	// maxTokensFn resolves the per-reply output cap in tokens, on the same
	// per-run basis. nil or 0 = send no cap and let the endpoint decide.
	maxTokensFn func() int
}

// WorkerSessionID returns the stable transcript key used by a worker intent.
// Worker slots are reusable, so the intent id (rather than work#N) is the
// session identity. Keep this helper public so the Worker message API and UI
// can refer to exactly the conversation that will be resumed.
func WorkerSessionID(explorationID, intentID int64) string {
	return fmt.Sprintf("exp%d-worker-i%d", explorationID, intentID)
}

const workerChatMarkerPrefix = "<!-- ARTEX_WORKER_CHAT:"

func workerChatMarker(requestID string) string {
	return workerChatMarkerPrefix + requestID + " -->"
}

func hasWorkerChatMessage(messages []llm.Message, requestID string) bool {
	marker := workerChatMarker(requestID)
	for _, message := range messages {
		if message.Role == llm.RoleUser && strings.Contains(message.Text(), marker) {
			return true
		}
	}
	return false
}

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default). Read per
// run so a profile or task-chain toggle takes effect without rebuilding.
func (w *Worker) SetNonStreaming(fn func() bool) { w.nonStreamingFn = fn }

func (w *Worker) nonStreaming() bool { return w.nonStreamingFn != nil && w.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (w *Worker) SetNoaEnabled(fn func() bool) { w.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (w *Worker) SetMaxTokens(fn func() int) { w.maxTokensFn = fn }

func (w *Worker) maxTokens() int {
	if w.maxTokensFn == nil {
		return 0
	}
	return w.maxTokensFn()
}

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the worker system prompt. nil = inject (default).
func (w *Worker) SetConstraintInject(fn func() bool) { w.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (w *Worker) wantConstraints() bool { return w.injectConstraints == nil || w.injectConstraints() }

// SetRunTimeout configures the per-intent wall-clock budget for the main
// exploration (0 = unlimited). When it fires, the SDK settlement phase still runs
// so facts are never lost to a timeout. Safe to call before Execute.
func (w *Worker) SetRunTimeout(run time.Duration) {
	w.runTimeout = run
}

// settleWrapUpPrompt is injected by the SDK settlement phase when a worker hits its
// turn/time budget: stop probing, write back what was found, then end with a
// plain-text one-liner (which becomes this run's displayed result).
const settleWrapUpPrompt = "You are about to be terminated because your budget is exhausted. Do not run any more commands or probes. Do the following in order: (1) Write back each item you identified above but have not yet persisted — use insert_assets for new assets, record_fact for exploration conclusions/facts, and report_finding for confirmed vulnerabilities; (2) **Finish with one standalone plain-text sentence** summarizing what you did and the key conclusions you reached (this sentence will be shown as the result of this run, so be sure to output it)."

func NewWorker(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int, extra ...actool.CoreTool) *Worker {
	return &Worker{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, extraTools: extra}
}

// defaultToolsExcept returns actool.DefaultTools() minus the named tools (by
// CoreTool.Name()). Used to trim SDK default tools an agent shouldn't have.
func defaultToolsExcept(exclude ...string) []actool.CoreTool {
	drop := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		drop[n] = true
	}
	all := actool.DefaultTools()
	out := make([]actool.CoreTool, 0, len(all))
	for _, t := range all {
		if !drop[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

func (w *Worker) SetCompactionWindowResolver(fn func() int) { w.windowFn = fn }

func (w *Worker) compactionWindow() int {
	if w.windowFn != nil {
		return w.windowFn()
	}
	return w.window
}

// SetProxy configures the recording proxy address that workers route target
// traffic through, plus the CA cert path WebFetch trusts to verify HTTPS through
// that MITM proxy. Empty addr disables the hint.
func (w *Worker) SetProxy(addr, caCert string) { w.proxyAddr, w.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for this worker (off by default).
func (w *Worker) SetWebSearch(o WebSearchOpts) { w.webSearch = o }

// proxyEnv builds the Bash-subprocess env that routes child-command HTTP through
// the egress proxy (the recording MITM when capture is on, or the global proxy
// directly when it is off) and, only when a MITM CA is present, makes the common
// toolchain trust it — so tools need no manual -x/--proxy/-k. Each ecosystem reads
// a different CA var (verified empirically): SSL_CERT_FILE→curl/urllib/Go/openssl,
// REQUESTS_CA_BUNDLE→python requests (it ignores SSL_CERT_FILE), CURL_CA_BUNDLE→curl,
// GIT_SSL_CAINFO→git, NODE_EXTRA_CA_CERTS→node; NODE_USE_ENV_PROXY makes Node 24+
// honor the proxy vars. ALL_PROXY is set too so a socks5 egress proxy (which curl
// only reads from ALL_PROXY, not HTTP(S)_PROXY) works in the capture-off path.
// Empty proxyAddr → nil (direct, unchanged env).
func proxyEnv(proxyAddr, caCert string) []string {
	if proxyAddr == "" {
		return nil
	}
	env := []string{
		"HTTP_PROXY=" + proxyAddr, "HTTPS_PROXY=" + proxyAddr,
		"http_proxy=" + proxyAddr, "https_proxy=" + proxyAddr,
		"ALL_PROXY=" + proxyAddr, "all_proxy=" + proxyAddr, // socks5 egress: curl reads only this
		"NODE_USE_ENV_PROXY=1", // Node 24+: honor HTTP(S)_PROXY in built-in fetch/http
	}
	if caCert != "" {
		env = append(env,
			"SSL_CERT_FILE="+caCert,
			"CURL_CA_BUNDLE="+caCert,
			"REQUESTS_CA_BUNDLE="+caCert,
			"GIT_SSL_CAINFO="+caCert,
			"NODE_EXTRA_CA_CERTS="+caCert,
		)
	}
	return env
}

// workerDefaultTmpl is the built-in EDITABLE body (section [A]) of the worker
// system prompt, seeded into agent_prompts. The trafficTool block and intermediate
// artifact output rules are NOT here — they are code-owned and appended by
// workerSystem after rendering (sections [B]/[C]), so editing the DB body can never
// drop them.
const workerDefaultTmpl = `You are the "executor" (work agent) in an authorized penetration testing system. You receive **one intent** (a one-sentence exploration direction). Your sole responsibility is to **complete that intent, write findings back to the knowledge graph, and then stop and return**.

**Boundaries (hard rules):**
1. **Work only on the one intent assigned to you.** If, while investigating it, you notice a lead worth exploring beyond its scope (a path exposed by an error, a possible connection to another asset, or an apparent entry point for another exploit chain), **mention it briefly in a fact summary for the planner**.
2. An initial setback (a filtered payload / 404 / injection with no visible response) does not mean you have exhausted the path. Try all reasonable bypasses for this intent before reporting your conclusion.
3. Operate only within the authorized scope. If the system prompt includes **operation constraints** at the top, they are the highest-priority hard rules: check each command/probe against them before execution and do not perform anything that violates them, even if it is part of your assigned intent.

**Write back as you discover things** (only information written to the graph counts; information in your head or text does not. Persist each result immediately so it is not lost when you run out of turns). Use the right graph for each of these three write-back types:
- **New asset/resource → insert_assets (asset graph):** the asset itself, such as a subdomain, service, endpoint, fingerprint, or credential. **Register assets here only; use record_fact for exploration conclusions or judgments.**
- **Exploration conclusion/fact → record_fact (exploration graph; pass intent_id):** use this for conclusions. **Combine multiple observations into one fact** (a one-sentence summary plus detail that expands on it, based on the actual execution). Do not create one fact per attribute; an intent normally produces one fact. Fragmenting facts causes unbounded graph growth, so **default to one fact and put related details in detail**. Use the facts array for separate entries only when conclusions are genuinely independent and cannot be combined; this should be rare. **Record only deltas**: write only what is **new** from this run; do not restate existing facts in different words (if you only confirmed an existing conclusion without adding anything, no new fact is needed). **Record only what you actually observed**: provide evidence (one concise line with the command and the one or two most probative output lines; put details in detail) and set confidence (observed = directly seen / inferred = inferred from indications).
- **Confirmed vulnerability → report_finding (exploration graph; include a PoC and pass intent_id):** **use this only if you actually triggered it during this run and obtained reproducible evidence (request/response or command output)**. Never treat a version/fingerprint matching a CVE, a parameter that merely looks injectable, or an inference from an external vulnerability database/release notes/code diff as confirmed. Do not use CVE lookups or patch-version comparisons as substitutes for triggering the issue. If you cannot trigger a suspicious issue, use record_fact to record an inferred fact (the lead and why it could not be triggered) for the planner; do not force it into a finding.

When the intent is complete, summarize in one sentence what you did and which facts you wrote back.`

// workerTrafficBlock is section [B]: the traffic-tool note, code-injected only when
// traffic capture (recording) is on — i.e. the traffic_* tools actually exist.
// Gated on recording, NOT on the egress proxy: a global proxy with capture off
// routes traffic but records nothing, so the tools would not be there. Not stored,
// not editable.
func workerTrafficBlock(recording bool) string {
	if !recording {
		return ""
	}
	return "\n\n**Traffic tools**:\n- traffic_search / traffic_get / traffic_blob: review responses and find resources already visited. **Search recorded traffic first; do not curl the same URL again.** traffic_search **requires host** and returns only 3 lightweight index entries by default (id/method/url/status/resp_len, without response content); increase limit explicitly for more. Use body_contains to search request/response bodies (at least 3 characters; supports substrings and Chinese, for example passwords/keys/errors/internal addresses). To view a record's content, call traffic_get(id). Very large bodies appear as @blob sha256:<hash>; use traffic_blob(hash) to retrieve the full content in chunks."
}

// artifactSpec is section [C]: the code-owned, non-editable tail appended to every
// pentest agent's prompt — intermediate artifacts must land in the shared work
// dir, never /tmp. Guaranteed present regardless of how the DB body is edited.
func artifactSpec(dir string) string {
	return "\n\n**Intermediate artifact output rules**: Write all intermediate artifacts, including scripts, payloads, captured response bodies, and temporary data, **only to this task's working directory " + dir + "** (relative paths are written here; you may also use this absolute path). **Do not write to /tmp or any other absolute path.**"
}

// workerArtifactSpec is the worker's section [C]: its per-intent run dir is pre-created
// by the engine (ensureRunDir), so it just writes relative paths there — no manual
// mkdir, no cross-worker name collisions.
func workerArtifactSpec(runDir string) string {
	return "\n\n**Intermediate artifact output rules**: Write all intermediate artifacts, including scripts, payloads, captured response bodies, and temporary data, **only to this intent's dedicated working directory " + runDir + "** (it has already been created; write relative paths here without creating a directory manually). **Do not write to /tmp or any other absolute path.**"
}

// ensureRunDir builds and creates an agent's working directory under base:
// <base>/tasks/<taskID> for planner/main; <base>/tasks/<taskID>/i<intentID> for a
// worker (intentID<=0 → task dir only). The "tasks/" segment groups per-task dirs
// symmetrically with the chat agent's "sessions/<sessionID>". Best-effort mkdir — on
// failure, writes fail the same way an unwritable CWD would.
func ensureRunDir(base string, taskID, intentID int64) string {
	dir := filepath.Join(base, "tasks", strconv.FormatInt(taskID, 10))
	if intentID > 0 {
		dir = filepath.Join(dir, "i"+strconv.FormatInt(intentID, 10))
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// cmdOutDir is the SDK large-tool-output spill dir under an agent's run dir.
func cmdOutDir(dir string) string { return filepath.Join(dir, "cmd-output") }

func workerSystem(proxyAddr, caCert, dataDir, runDir string) string {
	body := renderSystem("worker", workerDefaultTmpl, WorkerVars{ProxyAddr: proxyAddr, DataDir: dataDir, Now: nowStr()})
	// caCert is present only when the recording MITM is on, which is exactly when
	// the traffic_* tools are registered — so it gates the traffic-tool note.
	// Optional finding guidance is added for every role after tool resolution.
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// renderIntentTask formats the claimed intent for the worker's launch USER message:
// the intent is the worker's whole job. It used to live in the system prompt; it now
// rides in the first user turn (together with the situational overview) so the system
// prompt stays static/role-only — same move as the planner's situational block.
// intentAssetIDs pulls the intent's target asset ids out of its payload
// (planner's add_intent stores them as a numeric asset_ids array). nil on absence
// or malformed payload.
func intentAssetIDs(intent *db.Node) []int64 {
	if intent == nil {
		return nil
	}
	var p struct {
		AssetIDs []int64 `json:"asset_ids"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return nil
	}
	return p.AssetIDs
}

func renderIntentTask(intent *db.Node) string {
	return fmt.Sprintf("\n\n[Your assigned intent (your only task for this run: work only on this intent, record only facts, and stop when done)]:\n%s\nIntent ID: %d (pass this when writing back with record_fact / report_finding)", string(intent.Payload), intent.ID)
}

// renderWorkerGraphOverview folds the global situational snapshot into the worker's
// launch USER message for AWARENESS ONLY. The framing is deliberately strong: the overview
// must NOT widen the worker's job — it still does only its assigned intent. Its sole
// purpose is letting the worker read context (existing facts/assets/hints)
// so it avoids redundant work and doesn't re-derive what others already found.
func renderWorkerGraphOverview(data map[string]any) string {
	// Coverage helps the planner identify under-tested areas and decide whether to
	// expand scope. That conflicts with the worker's boundary of handling only its
	// assigned intent, not pursuing uncovered areas, so omit it from the worker view.
	// data is a new map dedicated to this worker; deleting the key does not affect the planner.
	delete(data, "coverage")
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back silently: the worker just won't have the global context
	}
	return "\n\n[Global exploration overview (read-only; use it to put your intent in context)]:\n" +
		"The following is an overview of the task's current exploration. Use it to see what others have already discovered and avoid duplication, and to consider how your intent relates to the overall task.\n" +
		"**Think broadly** while exploring your intent. The only boundary is that you must not execute other intents; those belong to other workers and are scheduled by the planner. If you think of a valuable lead (cross-asset connection, a possible entry point for another exploit chain, or a suspicious global pattern), **record it as a fact for the planner**. This is an important deliverable, not optional. Report leads for the planner to assess rather than keeping them to yourself.\n" +
		string(b)
}

// Execute runs one intent. hooks (the per-task Guard) gates every tool call; may
// be nil. emit, if non-nil, receives one ActivityRecord per execution step.
// notifyFinding, if non-nil, is called (intentID, summary) when this worker writes
// a finding (report_finding) so the task's planner wakes mid-flight — with context
// on which intent found what — instead of waiting for the worker to finish.
// Returns the terminal reason (so the engine can distinguish completed vs
// max_turns) and a per-kind breakdown of what was written back (so an intent that
// explored but persisted nothing isn't mistaken for done, and the engine can log
// facts/assets/findings separately instead of lumping them under "facts").
func (w *Worker) Execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string)) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, "", "")
}

// ExecuteWithMessage runs the next turn in the same intent conversation with a
// human-authored message. The HTTP handler does not edit the transcript;
// agentcore records the message as a normal user turn when this Worker starts.
// This keeps Worker continuation identical to the regular agent chat flow.
func (w *Worker) ExecuteWithMessage(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	return w.execute(ctx, name, taskID, as, ts, intent, hooks, emit, enr, notifyFinding, strings.TrimSpace(requestID), strings.TrimSpace(message))
}

func (w *Worker) execute(ctx context.Context, name string, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, intent *db.Node, hooks harness.HookRunner, emit func(db.Activity), enr EnrichTrigger, notifyFinding func(int64, string), requestID, message string) (harness.TerminalReason, WriteCounts, error) {
	tsx := NewToolSet(ts, name)
	tsx.SetFindingRecorder(w.findingRecorder)
	tsx.SetTaskID(taskID)
	coverageEnabled := as == nil || as.CoverageEnabled(taskID)
	tsx.SetCoverageEnabled(coverageEnabled)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetOwnerNode(intent.ID)         // assets this worker discovers anchor to its intent → visible to the task
	tsx.SetEnrich(enr)                  // async DNS/HTTP auto-completion for assets this worker writes
	tsx.SetNotifyFinding(notifyFinding) // Wake the planner as soon as report_finding is saved, identifying the intent and finding.
	// base = built-in worker tools ∪ host tools (traffic) ∪ default tools (incl. Bash);
	// then augment with the agent's visible skills/MCP. During the SDK settlement
	// phase, Bash is hidden via Settlement.DisabledTools (no local gating needed).
	base := append(tsx.WorkerTools(), w.extraTools...)
	// Deliberately withhold MultiEdit/Glob/Grep from the worker: use Edit for precise
	// file changes and Bash(grep/find) for searches, narrowing the toolset and
	// reducing low-value calls. Other SDK defaults (Read/Write/Edit/LS/Bash/Sleep) remain.
	base = append(base, defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts), IntentID: intent.ID})
	tools, def, cleanup := AugmentTools(ctx, "worker", base)
	defer cleanup()

	// The intent is the worker's sole responsibility and an invariant throughout the
	// run. Put it, the launch instruction, and the raw data for intent-linked target
	// assets in the system prompt. It is rebuilt for each run and cannot be removed by
	// compaction, so it remains available during long runs and continuations do not
	// depend on whether the transcript retained the initial message. The tradeoff is
	// that per-intent data in the system prompt prevents cache reuse across intents;
	// this is deliberate because losing the intent is worse than saving tokens.
	// Unlike the planner, which has no single mandate because it creates intents, the
	// worker does have one. Only the global situational overview stays in the launch
	// user message; it is optional and can be stale, so dropping it is harmless.
	// The engine creates this intent's dedicated directory at
	// <workDir>/tasks/<taskID>/i<intentID>.
	runDir := ensureRunDir(w.workDir, taskID, intent.ID)
	// The run-wide intent is not the current tool action. Do not forward it or
	// inherit a parent run's background into the action reviewer.
	ctx = intercept.WithReviewContext(ctx, runDir, intercept.ReviewBackground{})
	overview := renderWorkerGraphOverview(tsx.graphOverviewData())
	sysBody := workerSystem(w.proxyAddr, w.proxyCACert, w.workDir, runDir)
	if w.wantConstraints() {
		sysBody += constraintBlock(ts) // Inject any operation constraints into the system prompt; the worker must follow them.
	}
	// Append the intent block, intent-linked asset block, and launch instruction to
	// the end of the system prompt, using the same approach as constraintBlock.
	sysBody += renderIntentTask(intent)
	if as != nil {
		if ids := intentAssetIDs(intent); len(ids) > 0 {
			if assets, err := as.GetByIDs(ids); err == nil && len(assets) > 0 {
				if b, err := json.Marshal(assets); err == nil {
					sysBody += "\n\nTarget assets associated with this intent's asset_ids:\n" + string(b)
				}
				// Automatically add assets explicitly targeted by this intent to the
				// task's test scope, using the same conservative granularity as
				// insertAssets. ON CONFLICT DO NOTHING and the uq_task_scope unique
				// index prevent duplicates, making reruns and retries idempotent.
				// When asset coverage is disabled, do not accumulate test scope (the denominator).
				if coverageEnabled {
					for _, a := range assets {
						_ = as.AddAutoScope(taskID, a.Type, a.Domain, a.URL, a.IP)
					}
				}
			}
		}
	}
	sysBody += "\n\nBegin executing the intent above: work only on it, record only facts, assets, and findings, and stop when done."
	system, boundary := deferredSystem(sysBody, def)
	// The task-level deadline (injected through ctx) clamps this run's wall-clock
	// budget and determines its wrap-up prompt (see taskclock.go).
	tc := taskClockFrom(ctx)
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, w.runTimeout)
	settle := wrapupSettlement("worker", []string{"Bash"})
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("worker", []string{"Bash"}, clamped)
	}
	opts := agentcore.Options{
		Provider:        w.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		// WebFetch uses the recording proxy, so its HTTP traffic is recorded like curl.
		// Load the proxy CA so HTTPS certificates re-signed by the MITM proxy validate
		// normally instead of disabling certificate verification. An empty proxy means direct access.
		EnableWebFetch: true,
		WebFetchProxy:  w.proxyAddr,
		WebFetchCACert: w.proxyCACert,
		// Optional web search. ddgs needs no key; brave-free requires BraveKey; tavily requires TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), unrelated to the traffic-recording MITM proxy; empty means direct access.
		EnableWebSearch:       w.webSearch.Enabled,
		WebSearchBackend:      w.webSearch.Backend,
		BraveSearchAPIKey:     w.webSearch.BraveKey,
		TavilySearchAPIKey:    w.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: w.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  w.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   w.webSearch.DeepSeekModel,
		WebSearchProxy:        w.webSearch.Proxy,
		// HTTP requests from Bash commands use the recording proxy and trust its CA by default (no -x/-k needed).
		BashEnv:    proxyEnv(w.proxyAddr, w.proxyCACert),
		WorkingDir: runDir,
		MaxTurns:   w.maxTurns, // 0 = unlimited (configurable in agent management)
		// Enforce the wall-clock budget at turn boundaries without interrupting a turn;
		// 0 means unlimited. With a task-level deadline, clamp to min(run budget,
		// time remaining to the deadline) so this run naturally wraps up when the task expires (see taskclock.go).
		MaxDuration: maxDur,
		// When either budget is reached (turns OR duration), the SDK runs one wrap-up
		// turn with Bash hidden so recognized results are saved rather than left unfinished.
		// When clamped by the task deadline, PromptByReason uses the task-timeout prompt
		// if time expires, or falls back to the per-run prompt if the turn limit is hit
		// first. Without clamping, use only the per-run prompt.
		Settlement: settle,
		// large tool output spills to cmd-output/ with a head + pointer (SDK tool.Capture);
		// Full output is preserved on disk. The truncation limit uses the SDK default (30,000 characters).
		ToolOutputDir: cmdOutDir(runDir),
		Compaction:    compactionConfig(w.compactionWindow()), // long tool-heavy runs stay within the window
		Todos:         actool.NewTodoStore(),                  // Session-scoped temporary todos (TodoWrite), for planning only; discarded on exit.
		NonStreaming:  w.nonStreaming(),                       // Use Provider.Complete when this profile selects non-streaming mode.
		MaxTokens:     w.maxTokens(),                          // 0 means no limit is sent; the server default applies.
	}
	if hooks != nil { // typed-nil guard: only set when concrete (avoids harness panic)
		opts.Hooks = hooks
	}
	if w.tx != nil { // persist raw LLM conversation; one file per worked intent
		opts.Transcript = w.tx
		opts.SessionID = WorkerSessionID(ts.ID(), intent.ID)
	}
	intentID := intent.ID
	emitWrap := func(r db.Activity) {
		if emit != nil {
			r.NodeID, r.Worker = &intentID, name
			emit(r)
		}
	}
	// The intent, launch instruction, and intent-linked assets are already in the
	// system prompt (assembled in sysBody above).
	// This launch user message carries only the global situational overview, which is
	// optional context and can be dropped without harm.
	// If marshaling the overview rarely fails and returns empty, use a fallback launch
	// message to avoid an empty user message on the first turn.
	input := overview
	if strings.TrimSpace(input) == "" {
		input = "Begin working on the intent assigned in the system prompt: do only that work, record only facts, assets, and findings, and stop when done."
	}

	// Experimental feature: when enabled, noa handles context compaction and stores
	// persistent archives under <workDir>/noa/<SessionID>.
	noaSession := WorkerSessionID(ts.ID(), intent.ID)
	enableNoa(&opts, w.noaEnabledFn, w.workDir, noaSession, noaWarn(noaSession))
	ctx = attachSideCapture(ctx, &opts)
	s := agentcore.NewSession(opts)
	defer s.Close() // release the session's background-task manager (temp dir + processes)

	// Resume prior conversation if this intent was paused/blocked/exhausted and is
	// being re-run. The transcript ID is deterministic per intent, so if a prior
	// session exists the worker continues from where it left off instead of
	// restarting from scratch.
	alreadyRecorded := false
	if w.tx != nil {
		_ = s.Resume(opts.SessionID)
		alreadyRecorded = requestID != "" && hasWorkerChatMessage(s.Messages(), requestID)
		if len(s.Messages()) > 0 && message == "" {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
			input = "Continue."
		} else if len(s.Messages()) > 0 {
			seedUnlockFromHistory(s.Messages(), def.UnlockSkill)
		}
	}
	if message != "" {
		if alreadyRecorded {
			input = "Continue working on the new intent provided in the previous human chat message. Do not repeat actions that are already complete."
		} else if len(s.Messages()) > 0 {
			input = workerChatMarker(requestID) + "\n[New intent from human chat]\n" + message +
				"\n\nAct on this human input immediately. When finished, use the context to decide whether the original task should continue."
		} else {
			input += "\n\n" + workerChatMarker(requestID) + "\n[New intent from human chat]\n" + message +
				"\n\nPrioritize this human input."
		}
	}

	// Budgets + settlement are owned by the SDK (MaxTurns/MaxDuration + Settlement):
	// on hit it runs a wrap-up turn and finishes with ReasonMaxTurns/ReasonTimeout.
	// MaxDuration now interrupts an in-flight tool at the wall-clock deadline and
	// enters the wrap-up phase on the live ctx, so a run whose tool overran the budget
	// still settles (no external hard-timeout backstop needed). ctx itself carries only
	// pause / planner kill / shutdown, which the engine distinguishes and re-queues/stops.
	_, reason, err := captureRunSession(ctx, s, input, emitWrap)
	return reason, tsx.Writes(), err
}
