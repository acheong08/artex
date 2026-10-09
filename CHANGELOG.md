# Changelog

This file records notable changes to the project and follows the format of [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

## [0.3.15] - 2026-10-07

### Interception

#### Added

- **Added a built-in interception rule for destructive API paths**: Previously, the built-in HTTP destructive-action rule recognized only the DELETE **method**, but most applications allow deletion through GET/POST endpoints, so calls such as `curl 'http://t/api/user/delete?id=1'` could actually delete target data. A new `deny` rule now covers `/delete /del /remove /unlink /erase /destroy` (the verb must be followed by a separator, so `/delivery`, `/details`, and `/delta` are not false positives). Existing installations receive it after upgrading; it can be disabled or deleted under **System → Command Interception**.
- **Model-fallback approval token usage can be viewed separately**: Approval calls previously had no separate usage attribution, so consumption could only be guessed. They are now metered independently. A usage card under **System → Command Interception → Model Fallback Approval** shows five cumulative metrics—calls, input, output, cache reads, and cache writes—plus a mini chart of daily usage over the last 30 days.

### Finding Notifications

#### Added

- **Findings can be sent to IM channels**: A new **System → Notifications** page can send scan findings through six channel types: **DingTalk, Feishu, WeCom, generic Webhook, Telegram, and email**. Any number of instances can be configured per type, each with independent enablement, rate limits, and filters.
- **Two delivery modes: real-time and digest**: Real-time mode sends each matching finding immediately. Digest mode combines findings into one message at a global interval (30 minutes by default), with new-finding counts and severity distribution at the top. To send high-severity findings in real time and digest the rest, configure two separate channels.
- **Filters support four dimensions**: minimum severity; task/asset scope; finding-type keyword inclusion and exclusion (exclusions take precedence); and whether to receive remediation-status changes (off by default). Messages include a **View Details** button whose destination comes from the global **Link-back URL** setting; leave it blank to omit the button.
- **Delivery history and manual resend**: Lists each delivery's status, attempt count, failure reason, and channel. Filter by channel and status; failed items can be resent with one click (resending resets the retry count).
- **Channel credentials are masked on display**: Webhook URLs, signing keys, Bot Tokens, SMTP passwords, etc. are shown only as masked values with a trailing hint. Submitting the unchanged value means **no change**; clearing the input deletes that field.

#### Fixed

> This section records a security audit after the initial feature implementation. Each issue was reproduced, fixed, and covered by a regression test.

- **Fixed a serious masked-credential bypass via "change destination, retain credentials"**: Configuration merging previously retained stored values for every unmentioned key. Changing only a destination while omitting credentials could therefore make the server send stored real credentials to any address, silently. Now, whenever the destination changes, every credential field must be explicitly addressed—provide a new value or leave it explicitly blank to indicate it is no longer needed.
- **Fixed missing escaping of untrusted content in Markdown-based channels**: Finding titles/summaries come from model output and asset URLs from scan results. A crafted title could render as a clickable external link in DingTalk / WeCom / Feishu, while image syntax could cause clients to fetch remote content and expose readers' IP addresses. Content is now normalized to one line and Markdown metacharacters are escaped at each channel's rendering boundary, rather than in a shared title function.
- **Fixed SSRF exposure in delivery destinations**: Loopback, internal-network, and cloud-metadata addresses were previously all reachable, and the first 200 bytes of failed response bodies were stored in `last_error` and exposed in delivery history, creating a semi-blind read primitive. Protection is now enforced at the **dialing stage**, covering DNS rebinding and rejecting cross-host redirects. Loopback and link-local addresses require `ARTEX_NOTIFY_ALLOW_LOCAL=1`; RFC1918 private networks remain allowed (self-hosted internal SMTP relays are common).
- **Fixed notification credentials leaking through error messages**: DingTalk `access_token`, WeCom `key`, and Telegram bot tokens are in URLs, and on delivery failure the full URL could flow into `last_error`, delivery history, server logs, and frontend messages. URLs are now redacted uniformly, retaining only `scheme://host` and the underlying cause.
- **Fixed silent loss when truncated digests were marked fully delivered**: Omitted findings were neither in the message nor the failure list, although delivery history reported success. Digests are now packed **message by message**; only included items are marked delivered, while the rest return to the queue for the next message without consuming retries. The message header also states how many items will continue in the next message.
- **Fixed filters silently failing on a misspelled minimum-severity threshold**: A typo such as `hgih` in `min_severity` produced an unknown severity ordinal of 0, making the condition always true; users expecting only high-severity findings instead received all findings. Values are now validated on write, with allowed values listed.
- **Fixed `rate_per_min = 0` (unlimited) being unreachable**: The database-write layer silently replaced an explicit zero with the default, so an operator who disabled rate limiting was still throttled. The default is now applied at the API layer only when the field is omitted.
- **Fixed digest channels bypassing the token bucket entirely**: `rate_per_min` previously had no effect in digest mode. Claimed item counts are now constrained by both the current allowance and an in-memory cap.
- **Fixed failure handling using the maximum attempt count for an entire batch, penalizing new deliveries for old ones**: A delivery that had already been retried twice could cause new deliveries in the same batch to be marked failed and permanently lost before their first retry. Failure decisions are now made per item.
- **Fixed unparseable snapshots in digest batches being silently marked successful**: Such items were skipped during rendering but counted as successful with the batch. They are now explicitly failed with a reason.
- **Fixed a channel's per-round delivery count exceeding the lease duration**: In multi-instance deployments, expired leases could be reclaimed and messages sent twice, incrementing attempt counts twice. The per-round limit is now derived from `lease / per-request timeout`, with an assertion locking down the relationship between these constants.
- **Fixed Telegram truncation splitting HTML entities**: Truncation previously avoided cutting a partial tag but not fragments such as `&amp`, which could cause the parser to reject the **entire** message. Both tags and entities are now preserved.
- **Fixed temporary SMTP failures being treated as permanent**: Graylisted 4xx responses such as `450` are temporary and should be retried, but were previously treated as terminal; servers using graylisting would fail every push on the first attempt. Responses are now classified by their first digit: 4xx are retryable and 5xx are permanent.
- **Fixed masked literals being stored when submitting nested structures**: Object fields such as `webhook.headers` can only be masked or submitted as a whole; placing a mask sentinel inside the object was stored as a real value, silently breaking authentication. Such submissions are now explicitly rejected.

#### Design Notes

- **The finding-write transaction performs a single blind INSERT**: `notification_events` and the finding are written in the same transaction, ensuring they are atomic on commit. This INSERT deliberately does not read channel tables or apply user filters, and is wrapped in a `SAVEPOINT`; otherwise, one invalid filter could abort the transaction and prevent a high-severity finding from being stored.
- **Deliveries are claimed with leases, not long transactions**: After `FOR UPDATE SKIP LOCKED` claims rows, they are marked `sending` and `next_attempt_at` is moved forward as a lease. No database lock is held during delivery. Rows left behind by a crashed process are reclaimed after the lease expires, allowing recovery without infinite retries.
- **Rate limiting does not consume retry budget**: Calculate the current allowance from the token bucket first, then claim that number of deliveries. Reversing the order would count rate-limited deliveries as attempts; waiting through all three attempts would exhaust the budget and mark them failed.

### Test Infrastructure

#### Fixed

- **Fixed permanent test-data residue caused by cleanup order in company ICP ownership tests**: `defer d.Close()` ran before `t.Cleanup` on function return, so cleanup statements used a closed connection; their errors were discarded, leaving test assets and companies in the database and causing unrelated asset-count assertions to fail. Connection closing now also uses `t.Cleanup` and is registered first, and cleanup failures are surfaced.

### Traffic

#### Added

- **Traffic list supports content search, path/status/length filters, and column sorting**: Filter rows can search **response content** (using the full-text index), **path**, **status code** (exact values or buckets such as `2xx`), and **response-length ranges**. Headers sort by time, status code, and response length; sort preferences are stored locally. Two indexes were added and are applied automatically to existing installations after upgrade. The Agent tool query contract is unchanged.
- **Added "Clear All" to the traffic list**: Deletes all traffic records in one operation (ignoring current filters), cleans up historical host directories no longer referenced by the index, then performs a full compaction and reports the space actually reclaimed. Traffic evidence linked to findings is stored in a separate evidence store and is unaffected.

#### Fixed

- **Fixed disk space not being reclaimed after deleting traffic**: The index database did not enable `auto_vacuum` at creation, so the file never shrank. The full-text index used `contentless_delete`, so `DELETE` wrote tombstones without reclaiming original postings—deleting traffic actually grew the index (a 6 MB body produced a 16 MB index, which remained 16 MB after all records were deleted). New index databases now enable `auto_vacuum=incremental`; after each deletion, full-text index merging and incremental vacuuming run in background batches, gradually returning space to the system (the same case shrank to 104 KB). The write lock is released between batches, so traffic recording is not blocked.

> Upgrade note: `auto_vacuum` can only be set when a database is created, so **existing index databases remain in the old mode** and incremental vacuuming is a no-op (a notice is printed at startup). Upgrading stops tombstone growth; reclaim existing space once with **Clear All**, which switches the database to incremental vacuuming so subsequent deletions reclaim space normally.

### LLM

#### Fixed

- **Fixed custom session headers not being applied to one-shot LLM calls**: Goal decomposition (round 0) and cold-node compaction are one-shot calls without a transcript store, so their contexts lacked a session ID and gateways could not read the header. On endpoints such as opencode zen that return 400 when it is missing, round 0 failed while later Planner rounds worked normally. Both paths now explicitly attach a session ID stable for the exploration, also allowing token usage to be attributed to the corresponding exploration (it was not recorded previously).

### Tools

#### Fixed

- **Fixed output truncation not applying to MCP and custom tools, allowing large results to overwhelm context**: MCP tool returns were not truncated at all. HTTP-style custom tools received an empty tool context and therefore always used a hard-coded 6,000-character limit instead of the session setting. Both paths now use `actool.Capture`: output is truncated according to the session's `MaxOutputChars` (default 30,000); if `ToolOutputDir` is configured, the full output is spilled to disk and only an opening excerpt plus a file pointer remains in the message.
- **Upgraded norma to v0.4.3**: the harness now consistently runs `CaptureOnce`, preventing any tool—including future additions—from overwhelming context because truncation was not wired up.

### Deployment

#### Fixed

- **Fixed the activity stream failing behind a reverse proxy**: The SSE URL was always constructed as `${hostname}:8787`. With a reverse proxy on port 443, the page loaded from `https://domain/` while SSE pointed to port 8787, which was not publicly exposed, causing repeated reconnects. Behavior now depends on `NODE_ENV`: production defaults to same-origin; only `next dev` retains `:8787` to bypass Next.js SSE buffering. The `NEXT_PUBLIC_SSE_BASE` override remains available. The README now documents reverse-proxy deployment.

### Findings

#### Fixed

- **Fixed the asset list in the "By Asset" view overflowing without a scrollbar**: The outer card had only `max-height`, not a definite height, so the scroll viewport's percentage height could not be resolved. With many assets, the list either broke out of the card or was clipped and could not scroll. The height limit is now applied directly to the asset tree's native scroll container and adapts to the window height.

### Database

#### Fixed

- **Capped the connection pool**: `database/sql` does not limit the number of connections an application can open by default. If the pool has no idle connection, it creates one without a limit until PostgreSQL rejects it at `max_connections` (100 by default). The pool is now capped at 32; excess queries wait for an idle connection, so the same load slows down instead of failing. Lower the cap as well if `max_connections` has been reduced.
- **Improved connection reuse**: `MaxIdleConns` defaults to 2, so connections above that limit were closed after use and the next request had to repeat the TCP and PostgreSQL authentication handshakes. The idle limit now matches the total limit, with a 30-minute connection lifetime and 5-minute idle timeout so the pool naturally replaces long-lived unhealthy connections.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.14] - 2026-09-24

### Task List

#### Added

- **Added a "Findings" column to the task list**: Counts are grouped by severity (critical / high / medium / low), with nonzero values colored by severity to make each task's finding volume and distribution easy to scan.

#### Changed

- **Narrowed the "Description / Objective" columns**: Long content is truncated and can be viewed in full on hover, leaving more horizontal space in the list.

### Exploration Graph and Planning Overview

#### Changed

- **Streamlined the planning overview (`graph_overview`) and capped list sizes**: Recent facts, completed intents, pending intents, cold-zone summaries, and confirmed finding details now use a "latest window + total count" format; omitted items can be queried as needed. This significantly reduces per-turn LLM context and prevents context growth in long tasks (cold-zone data in linked-task overviews is also capped).

#### Fixed

- **Removed duplicate reverse edges between digest and finding nodes in the exploration graph after cold-node compaction.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset Interception

#### Added

- **Added global asset interception rules (blocklist)**: Supports exact and fuzzy matching for domains / IPs / URLs and CIDR ranges, with create/read/update/delete and enable/disable controls. Manage them under **System → Asset Interception**. Fuzzy blocking for government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites is built in by default.
- **Integrated asset interception into the execution flow**: Before dispatching intents (`add_intent`) or inserting assets (`insert_assets`), the Agent checks target assets against interception rules. Blocked intents are not dispatched and blocked assets are not inserted; the Agent receives the asset information and reason for interception.
- **Added task-level block/allow (allowlist) rules**: These are independent of global rules and apply only to the current task. Evaluation order is "block first, then allow": a matching block denies the operation; if no block matches but the task has allow rules and none match, testing is not allowed; when no allow rules are configured, the allowlist is inactive. Rules can be entered when creating a task or managed in the task details **Overview**.
- **Task templates support preset categories and task-level block/allow rules**: Templates can save a task category and a set of task-level rules, which are carried into the new-task form when applied.

### Operation Review

#### Fixed

- **Tightened the model-judge output protocol to reduce false allows caused by truncation**: Reduced the decision comment limit from 500 to 120 Chinese characters and required output to be **JSON only, with no preface or code fences**, preventing truncated decisions from becoming unparseable and then being allowed by the model-failure policy (fail-open).

### Task Archives

#### Fixed

- **Archives now skip symbolic links instead of failing the entire package**: The archive format supports only regular files and directories, so any symlink in the working directory previously caused the whole task archive to fail. Symlinks are now skipped and logged while all other files are archived normally (links are not followed and cannot escape the directory tree).

### Accounts and Compliance

#### Added

- **Added a "Usage Notice and Disclaimer" dialog before login**: Users must check the agreement box before signing in.

### License and Dependencies

#### Changed

- **Adopted the AGPL-3.0 open-source license** and expanded the README's license and disclaimer section.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration Timeline

#### Added

- **Added an exploration timeline to task details** (#144): Displays exploration nodes (start/objective/intent/fact/finding/hint/compaction) as a chronological feed, with type filters, keyword search, ascending/descending order, pagination, and auto-refresh. The newest-first first page acts as a live view and refreshes every round. After leaving it, only the unread count accumulates, without interrupting reading; **N new updates · Return to latest** jumps back. Items are grouped by day, so long tasks remain easy to follow across pages.
- **Each timeline row displays the node ID** (#145), making it easier to cross-reference the exploration graph and locate specific nodes.
- **Expanded timeline node details show upstream/downstream relationships and anchored assets** (#147): Expanding an item shows its upstream (derived from) and downstream (produced) relationships. Hovering over a related item opens a node card (type/status/source/time/summary/payload excerpt); assets anchored to the node are also listed (type label + readable value). Data is sent with the timeline page, so expanding a row makes no extra request.
- **Timeline search supports filtering by node ID** (#150): In addition to content/source, the search box now matches node IDs. Enter a number or a displayed value such as `#41` to locate a node exactly.

### Intent Management

#### Added

- **Intent deletion supports soft-delete and hard-delete modes** (#149): Pending, running, and paused intents can be deleted; a reason is required, and the mode is selected in the confirmation dialog.
  - **Soft delete (default)**: Marks the intent as **Deleted**, records the reason in a separate field, and retains the intent node, all its outputs, and lineage.
  - **Hard delete**: Physically removes the intent and exclusive descendant nodes supported only by it (cascading along output/intent chains to the leaves), avoiding orphaned data. Token usage for the deleted intent is retained under its original date. Shared nodes (referenced by other intents), objectives, and task root facts are preserved. The hard-delete dialog shows the estimated number of affected nodes.

  Both modes notify the planner that the user deleted the intent and provide the reason, prompting replanning.

### Operation Review

#### Added

- **Approval records can be filtered by status and decision source** (#139), making it easier to locate specific records.

#### Fixed

- **Pending approval requests load independently of history pagination** (#133), so they remain fully visible while browsing paginated history.

### Agent

#### Changed

- **Changed the Worker role description to use general cybersecurity-platform wording** (#138).

#### Fixed

- **Fixed cold-node compaction never running**: Connected the task-scoped planner to the Compactor, restoring cold-node compaction (`cold-digest`), which had never run because it was not wired in.

### Chat

#### Added

- **Chat supports @-mentioning multiple record types and scrolling pagination** (#135): Multiple record types can be referenced in chat, and mention candidates load with scrolling pagination.

#### Fixed

- **Long message bubbles are constrained to the conversation panel** (#137) and no longer overflow it.
- **Fixed "missing uploaded file" errors when uploading before a new conversation was created**: The Composer now snapshots the selected `FileList` as an array before clearing the input and invoking the callback. Previously, draft mode asynchronously created the conversation first; by the time execution resumed, the `FileList` bound to the input had been cleared, so the upload lacked its file field and the backend returned 400.

### Traffic

#### Fixed

- **The traffic-recording proxy now listens only on 127.0.0.1 by default** (#129, #130), preventing the open proxy from being exposed on other network interfaces.

### Web

#### Fixed

- **Fixed the global header disappearing from the task list in static exports.**
- **Added the missing linked-traffic mock to the demo finding details page, fixing a blank screen.**

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation Review

#### Added

- **Approval records can navigate to their source** (#125): Clicking **Source** on an approval record jumps to the tool execution that triggered it. The original full conversation opens, pages load automatically up to the target, and the command and result expand in place, centered and highlighted in the scroll area while surrounding messages remain readable. Automatic repositioning stops when the user scrolls manually. Precise location across regular conversations, task Workers, Planners, and segmented Main Agent sessions uses persisted `tool_use_id` values and task mappings (with a new in-scope call ID index). Missing, duplicate, or ambiguous associations show an explicit notice rather than jumping to another execution. Clear notices are also shown if the session was deleted or the task was archived or its record is missing.
- **Approval records support pagination** (#115).

#### Changed

- **Streamlined model-review inputs** (#124, #125): The input to model review (the LLM judge used when no rule matches) is now versioned JSON containing only the current complete tool call, explicitly selected brief context, and the local working directory. Context includes only genuine user messages (the current user message in chat or the task's Main Agent); Workers no longer include intent summaries or inherited context from parent Agents, and Planner or automatically triggered sessions do not fabricate user messages. Task descriptions, goals, operational constraints, global exploration state, prior calls, and full Worker intent are no longer included. They continue to be used for Agent execution and independent session audits, but not for operation review. All verdicts must be JSON with `decision` and `comment`; explanations must cover the actual operation, its consequences if successful, and the matched rule. Instructions in arguments or context cannot change the review policy. The exact input sent to the model and its fingerprint are saved. Older snapshots are retained and labeled with their version; inputs are not reconstructed from current data.

#### Fixed

- **Model verdicts wrapped in code fences are no longer silently allowed** (#126): Previously, if the model returned verdict JSON inside a ``` code fence, parsing failed and the model failure policy could silently allow it. The fence is now stripped before parsing; only if parsing still fails is the configured failure policy applied.

### Agent

#### Added

- **Added experimental noa context compression** (norma upgraded to v0.4.0): This opt-in experimental feature is off by default and can be enabled in System Settings. When enabled, noa (model-driven context compression) handles compression for the Main Agent, Planner, Worker, and chat Agents, replacing the built-in compression. Original text is persisted in a centralized archive under `<workDir>/noa/<session-id>/` rather than scattered across task directories, with globally unique directories keyed by session ID. Integration failures automatically fall back to built-in compression without interrupting real tasks. The setting is read once per run, so changes affect only runs started afterward.

#### Fixed

- **Exploration graph tools now return an error instead of crashing on nil without task context**: Calls to exploration-graph tools outside a task context now return a clear error instead of crashing while dereferencing empty storage.

### MCP

#### Added

- **Support for legacy SSE MCP servers** (#117): Compatible with MCP servers that provide only the legacy SSE transport.

### Traffic

#### Fixed

- **Traffic search matches recorded hosts by port** (#114): Host matching in `traffic_search` now includes the port, preventing records for the same host on different ports from interfering with each other.
- **A failed `traffic_search` description migration no longer interrupts later reporter migrations**: Individual migration failures are isolated and do not affect subsequent migrations.

### Web

#### Changed

- **Added separators between vulnerability retest options** (#122) for clearer visual distinction.

#### Fixed

- **Fixed a full-page crash in demo task details**: In mock mode, the task details **Sessions** tab requested `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no route for it. The fallback treated any path ending in `s` as a collection and returned `[]`, leaving `data.items` undefined; the side-question hook's `merge()` then threw `TypeError: t is not iterable`. Because the exception occurred in the `setItems` updater, React rethrew it during rendering, beyond the caller's `catch`, and the error boundary took over the entire page with “This page couldn't load.” The mock handler now explicitly returns empty side-question history, and `sideAPI.history` normalizes the response as a defense in depth, treating any non-array `items` value as `[]`.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **Added DeepSeek as an official web-search provider**: It reuses the currently active LLM configuration. Unlike the other three providers, DeepSeek has no directly callable search API; search is available only as a server-side tool (`web_search_20250305`) in its Anthropic-compatible interface and is executed by DeepSeek's servers. Therefore, it **supports only DeepSeek's official endpoint + the anthropic protocol** (OpenAI-protocol endpoints reject server-side tools). Each search **uses an additional model call**, **does not use the search egress proxy**, and **is not included in traffic recording**. Results contain **only titles and links** (no summaries; use WebFetch to retrieve page content). The settings page explains these limitations but **does not validate or block** the configuration; users must verify compatibility themselves using **Test Search**.

### Agent

#### Added

- **Workers can review traces across work items**: Added `search_all_worker_traces` (globally searches matching steps across all work traces in the task by keyword, without requiring an `intent_id` first) and `get_worker_trace` (selects a work item, lists its step stream, searches it by keyword, and retrieves full content by `step_id`). These tools help reuse observations from other work that were not written to facts and avoid duplicated effort.
- **`node_detail` is now available to Workers**: Together with the trace-review tools, this lets a Worker look up full node details from an `intent_id` / node ID.
- **Planning rounds triggered by `add_hint` are explicitly announced**: Previously, new hints were only folded into the overview for the Planner to discover. Now each `add_hint` records a trigger such as "Human added N strategic hints: …" (one trigger for a batch, not one per hint), explicitly tells the Planner that the round was triggered by new hints, and includes their content.

#### Changed

- **Relaxed the tone of the global overview prompt**: Encourages divergent exploration and timely reporting of cross-intent clues rather than premature convergence.
- **Clarified Worker boundaries**: Initial resistance does not mean a direction has been fully explored; finish reasonable bypass methods within the intent before drawing conclusions.
- Removed the per-asset `related` parameter from `insert_assets`: its only purpose was to decide whether to add an asset to the task scope, but the value was not persisted (and was invalidated when the same asset was registered again, with no UI indication of which assets were deemed unrelated). In practice, it asked the model to make an ephemeral extra judgment.
- **Task scope (`task_scope`) is no longer affected by the asset-coverage toggle**: `insert_assets` always adds assets to scope automatically (`source='auto'`), whether coverage is enabled or not, and `add_task_scope` is always available to the Planner, task MainAgent, and goal-decomposition Agent. Scope is the task's authorization boundary and the basis for asset-query filtering; the coverage toggle controls only whether scope is used as the denominator for metrics, not whether scope is accumulated. Previously, disabling coverage disabled both `auto` and `agent` write paths, leaving only rows added manually in the UI. `list_untested_assets` remains hidden when coverage is disabled because it is purely a coverage view.

#### Fixed

- **Fixed assets leaking across tasks** (#59): `list_assets` previously hard-coded task ID 0 and queried the entire shared asset library; the model had no way to express scope filters, so it could treat assets from other tasks (especially IPs) as targets and go off track. It now returns only assets within the test scope (`task_scope`) of **the current task and directly linked tasks**, matching by **ownership** rather than literal value: a root domain in scope grants access to its subdomains/services/endpoints, and a network range grants access to hosts and services within it. Assets outside the scope cannot be fetched directly by ID either. Non-task contexts (Auto/pentest) have no scope to use and still fall back to the full library. Also fixed a scope-matching blind spot for IP-literal hosts (e.g. `http://1.2.3.4/api`) that could not be matched by network ranges. The UI's **Test Assets** view (filtered by task producer) is unaffected.
- **Fixed cross-work trace tools being removed from Workers**: After `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the default Worker toolset, an older migration that narrowed Worker tools unbound them again at startup, leaving Workers without access. They have been removed from that unbinding list and are restored by a one-time migration for databases that already ran the old migration.
- **Fixed intermittent persistence failures for `/btw` side questions**: Strips JSONB-unsupported NUL (`\u0000`) escapes before storing a side_question checkpoint, preventing write errors when content contains that character.

### Triggers

#### Fixed

- **Deduplicated task descriptions/objectives by task when merging trigger sessions**: When triggers from multiple tasks are merged, descriptions/objectives for the same task are no longer repeated in the message, preventing long objectives from being stacked until context overflows.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- Task MainAgent supports **multiple conversations**: create, switch between, and reset contexts independently; each conversation remains interactive.
- Added persistent `/btw` **side questions**: ask follow-up questions without interrupting the main flow; Q&A is stored and survives restarts.
- Added `source_task_ids` to `spawn_task`, allowing new tasks to **inherit assets and findings read-only** from source tasks.

#### Changed

- Disabled cross-engagement memory for Workers and narrowed their default tools: reading context and reviewing work across intents are Planner responsibilities; Workers handle execution and writeback for one intent only.
- Streamlined the default Worker prompt; removed `terminated` and `worker_name` fields from `get_worker_output`; shortened the descriptions of `insert_assets` / `list_assets`.

#### Fixed

- Fixed work stopping prematurely after an **idle turn**: Sometimes the model spent a full turn thinking without producing text or calling tools. The harness saw a normal end (`end_turn` with no `tool_use`) and marked the work `completed` with an empty summary, leaving an unfinished intent midway; users saw a task finish normally with no written summary. None of the five LLM retry layers applied: this was not an error; same-provider safe-window retry requires a stream failure; the circuit breaker treats `err == nil` as success; intent reruns apply only to `model_error`; and the SDK's empty-response retry checks whether any event was yielded, while thinking deltas count as events. The work Stop hook now detects this turn and injects a continuation instruction so the model can continue from its existing reasoning. It deliberately does not resend the same request, because this idle behavior is usually stable and caused by prompt/context shape, not a random transient.
- Reuses the LLM page's **empty-response retry count** (both address cases where the model finishes without substantive output, but use different criteria and techniques), defaulting to 2; `-1` disables it and restores the previous behavior of ending after an idle turn. This is a **total limit per intent**, not a consecutive count: the harness already allows only one consecutive idle-turn continuation, and the counter resets only after a genuine tool turn. The limit prevents loops such as "tool → idle → continue → tool → idle" from exhausting the intent budget. The log records `[work <worker> · #<intent>] idle turn… injecting continuation instruction (n/N)`. See `docs/LLM重试设计.md` §1.1.
- Fixed `/btw` context budgeting and input layout for long conversations; side-question request IDs now degrade gracefully in insecure contexts (non-HTTPS).

### Traffic

#### Added

- Findings support **linking multiple traffic evidence items**, with sorting, notes, and role labels (request/response/supporting).
- The report Agent **automatically links** relevant traffic evidence before writing a report.
- Added an Agent **traffic-binding toggle** and completed evidence handoff between Agents.

### Tasks

#### Added

- Added **session-level finding retests**: retests run in separate Agent sessions, with status displayed in lists and details.

### Interception

#### Added

- Approval records now include **details and execution audits**: inspect tool-request context, initial model/rule decisions, execution output, and parameter fingerprints.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed connection tests not including custom session headers, which caused opencode zen to return 400.

### Network

#### Added

- MCP's HTTP transport can now **skip TLS certificate verification**, making it easier to connect to services with self-signed certificates.

### UI

#### Added

- The session list is grouped by Agent, with expand/collapse and independent pinning; conversations can also be filtered by Agent.
- Attack-path graphs now render compacted (digest) nodes with their members collapsed.

#### Fixed

- Fixed a blank screen caused by unsynchronized login credentials.
- Interception notices now explicitly say **Platform control**, avoiding confusion with defenses on the target side.

### Deployment and Updates

#### Added

- **One-click updates from the UI**: Added a **Version and Updates** card to System Settings, and the top bar shows a notice when a new version is available. The process downloads the release package → verifies `SHA256SUMS` → runs a smoke test → stages the update → exits so the watchdog relaunches the app and completes the replacement; the page then refreshes automatically.
- Added watchdog startup scripts, `start.sh` / `start.bat`, as the official entry points (included in release packages and Docker images; `install.sh` is unchanged). They use the exit code to decide whether to relaunch and forward SIGTERM to artex. Verification and replacement logic lives in Go, keeping the scripts simple.
- **Automatic recovery on failure**: If verification or the smoke test fails, the update is discarded and the current version keeps running. If the new version fails to start three times in a row, the previous version is restored. Manual rollback is also available in Settings (database schema changes are not rolled back).
- Updates accept only GitHub domains and require HTTPS; the release source cannot be configured. One-click updates are disabled in development builds. GitHub query results are cached for 30 minutes to avoid exhausting the API quota with top-bar notices.

#### Known Limitations

- Under Docker, only the program is replaced, not the image: the toolchain is not upgraded, and recreating the container reverts to the version included in the image. Run `docker compose pull artex` when needed.
- The release package's `skills/` directory is not synchronized, so newly added built-in skills do not take effect automatically.
- Updating restarts the application and interrupts running tasks.

### Dependencies

#### Changed

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- Added a **Retries and Backoff** tab to the LLM page, where the **count** and **interval** can be configured for five retry layers. A failed model call passes from inner to outer layers: connection retries (SDK, for connection resets/timeouts/429/5xx before streaming begins), empty-response retries (SDK, normal completion with no content, OpenAI format only), same-provider safe-window retries (replay after interruption before any output is delivered), polling circuit breaker (cooldown after a failure threshold), and intent reruns (rerun the whole intent after a Worker ends with `model_error`). The outer layer is used only after inner retries are exhausted. Each layer has two controls with consistent semantics: blank = use the existing default count and exponential backoff; a count = use that count; an interval = replace exponential backoff with a fixed interval; `-1` = disable the layer. The first three layers are endpoint-scoped and can be overridden per field in each model profile (setting only the interval still inherits the global count); circuit breaking and intent reruns are process-level and have one global configuration each. Changes apply immediately without a restart. Leaving all fields blank preserves current behavior, byte-for-byte for existing databases (new columns default to 0; absent settings keys use all defaults). Connection tests deliberately omit these parameters: with a hard 30-second timeout, configured retries could make a working endpoint time out. See `docs/LLM重试设计.md` for the design.
- Each LLM profile supports a custom **session header** (`session_header_key`): when set, every LLM request includes the HTTP header with the current run's session ID (e.g. `conv-<id>` for chat, `exp<x>-worker-i<intent>` for workers). This supports gateways that use session-ID headers for prompt caching or sticky routing. The ID is injected from the request context without modifying norma, and a shared Provider can send different values per session. Existing databases are migrated, and the frontend LLM configuration dialog has a new input.

#### Fixed

- Fixed saving LLM profiles failing with `23502` when the session-header field was blank: the column is `NOT NULL DEFAULT ''`, but `NULLIF($n,'')` converted an empty header to NULL and violated the constraint. The value is now passed through using empty-string semantics.

### Agent

#### Added

- Wall-clock timeouts now **finish in place**: Based on norma v0.3.6, when `MaxDuration` expires, the running tool is interrupted and the active context continues for a configured number of wrap-up turns (writing recognized content and a summary, with terminal state `timeout`). This no longer relies on each Worker/Planner's external hard context of `maxDur+90s`, which could kill a stuck run as `aborted_tools`. Chat gets the same behavior automatically through the shared harness; MainAgent is unaffected because it has no `MaxDuration`.
- Added a stalled-planner fallback: if a heartbeat/no-change wake-up finds no open or running intent anywhere in the graph, the opening instruction becomes a stall warning stating that no Workers are running and no directions are queued. The round must produce one or more distinct new intents (not zero).

#### Changed

- Refactored Worker prompts: raw intent/startup-instruction/anchored-asset JSON is now placed in the system prompt and rebuilt each turn, so compaction cannot remove it and resumed runs do not depend on the transcript's first message. The startup user message is reduced to the global overview (which may be degraded or stale). This deliberately places per-intent data in the system prompt and sacrifices cross-intent cache reuse to ensure intents are not lost.
- Streamlined default Planner/Worker prompts and fixed several operational issues: the Planner now distinguishes `recent_done` by state (check traces before deciding about blocked/exhausted intents; neither treat them as dead ends nor rerun blindly), treats negative conclusions as observations rather than definitive claims and checks evidence before relying on them, must create work if objectives remain unmet and there are no open/running intents, and prioritizes depth over coverage. Workers record negative conclusions only as observations with tentative interpretations; the Planner makes decisions. Cross-intent clues go into fact summaries for the Planner instead of being pursued by the Worker. Reseeding appends the new default as a new version and activates it; user-customized/older versions remain in history and can be restored.
- Narrowed the default work-Agent toolset and asset-writeback instructions: Workers handle only their individual intent's execution and writeback; reading context and reviewing work across intents are Planner responsibilities. Removed `list_facts` / `node_detail` / `list_companies` and cross-work search (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) from the default Worker tools, retaining `list_findings` (deduplicate before reporting) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`. Removed stale prompt fields that conflicted with the `insert_assets` schema (`type=tech`/`on_url`/`props`). Existing databases receive a one-time migration that removes these bindings without changing same-named Planner/Main bindings.

### Exploration Graph

#### Added

- **Added exploration-graph cold-node compaction (`cold-digest`)**: Old, long-inactive intents/facts are folded into digest nodes for `graph_overview`; original nodes are retained permanently and can be fully restored by ID (lossless storage, presentation-only compaction, reversible folding). Hot/cold classification uses reverse reachability, with any active branch making a node hot, plus R=6-round debounce and connected-component grouping. Background compaction (minor folds uncovered cold regions; major rereads source data and merges fragments) rechecks activity and uses mutually exclusive cooldowns; it never runs on the hot path or overwrites reactivated nodes. The overview provides `cold_digests` and an asset-indexed `cold_index`; `expand_digest` / `expand_index` restore nodes. Linked/inherited task overviews also reuse their own folded views, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are available only to Planner and MainAgent, not Workers. Existing databases are migrated.
- `graph_overview` now outputs the complete `finding_list`: findings are the most valuable task output and are usually few per task, so all confirmed findings are included in the overview (unlike facts, which show only a recent window). Planner/Workers can see them all each turn without calling `list_findings`. Each item is concise: `{id, summary, evidence?, from_intent?, assets?}`, with affected assets shown as readable values (URL / domain / ip:port) rather than raw IDs.

#### Changed

- `graph_overview` no longer lists `hosts`: the coverage section removes the host list (large tasks could include up to 500 host strings per turn, with limited planning value), retaining only `host_count`; query specific hosts with `list_assets` as needed. Added top-level `done_intents_total` (total completed intents), alongside `recent_done_intents` truncated to ≤15, so the Planner can tell whether deduplication might be affected by omitted items.

### Tools

#### Changed

- Changed the default binding for `add_company_scope` from Worker to Planner: defining enterprise asset scope is the responsibility of planning / Main Agent / Auto, while Workers focus on exploration. New databases seed the binding for mainagent/planner/auto; existing databases receive a one-time migration.
- Bound `list_assets` to Planner by default: Planner now has both `list_assets` (DSL search across the full library) and `list_untested_assets` (untested assets within scope). Existing databases are backfilled once without overriding user-unbound tools.

### Tasks

#### Added

- Added a **Running Workers** column to the task list, counting intent nodes with `state='running'` for each task. It uses the same definition as the **Running Workers** overview on the task details page and displays 0 when none are running.

### Network

#### Added

- Added **global egress proxy** configuration: all target traffic can use a shared global proxy (http/https/socks5, supporting `user:pass`). When traffic capture is enabled, it serves as the upstream for the MITM recording proxy (traffic is recorded before egress; both intercepted and passed-through requests use the upstream, avoiding source IP exposure). When capture is disabled, it is injected directly into the agent's bash environment and WebFetch (`proxyEnv` now supports socks5 through `ALL_PROXY`). The configuration is stored in the settings KV table (no migration required). System Settings now has a **Global Proxy** card, independent of the web-search and LLM proxies.

### UI

#### Fixed

- Corrected intent status labels: `exhausted` changed from “Exhausted” to “Budget exhausted” (the step/time budget was cut short and only partial results were written; the direction was not fully explored); `blocked` changed from “Blocked” to “Execution error” (model/API/network retries were exhausted and the intent was barely explored; this does not mean the target/WAF blocked it). Also added the previously missing `stopped` status for work stopped manually by the user.

### Dependencies

#### Changed

- Upgraded norma to v0.3.4, adding truncation and file persistence for MCP tool output (see `651b961`); v0.3.6 later added in-place wall-clock timeout wrap-up.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Added an **output limit** to model configuration, with a choice of request field name. The limit caps the number of tokens generated in a single response and is sent with every request. `0` (default) means the field is omitted and the server default applies. This differs from the **context window**, which is the model's total capacity and is used locally to calculate the compaction threshold; it is not sent in requests. The field-name option applies only to the `openai` (Chat Completions) format: blank (default) sends `max_tokens`, which most compatible gateways recognize; official OpenAI reasoning models (o-series / GPT-5) accept only `max_completion_tokens` and return `unsupported_parameter` for `max_tokens`, so those endpoints must be switched to the new field manually. The Anthropic format always uses `max_tokens`, and the Responses API always uses `max_output_tokens`, so the option is disabled and cleared on save for non-openai formats. The output limit is now wired through in all five paths—planner, worker, chat, Main Agent, and goal decomposition—where it was previously unset (OpenAI sent no limit, while Anthropic used the SDK's 8192 fallback). Values are resolved from configuration each round, just like streaming, so a failover takes effect on the next round. Upgrading an existing database leaves behavior unchanged (new columns default to 0 and an empty field name).
- When **Test** is clicked in the admin UI, each HTTP attempt's status code and the gateway's raw response body (truncated to 4K) are now written to the server log. This helps diagnose 401s, quota messages, empty frames, and HTML responses, rather than exposing only the UI's collapsed `ok` / `err` result. This logging does not depend on the **LLM recording** setting.

#### Changed

- The task's LLM configuration chain can now be edited in any state, not only while **running**, **paused**, or **chain exhausted**. Main Agent conversations continue using this chain after a task ends (`done` / `failed` / `timeout`), but previously a broken model in the chain could not be changed, preventing further interaction. Terminal-state checks have been removed from both backend HTTP handling and DB transactions, and the frontend dialog now allows editing and saving for terminal tasks. Saving a terminal task no longer reopens quota-blocked intents (they would be moved to `open` without a Worker to run them and would no longer qualify for **Rerun intent**). To resume work, use **Rerun intent** or **Add goal**, which return the task to a running state.

### Agent

#### Changed

- “Message a running Worker to change direction” now reuses the existing pause/resume and transcript-resume mechanism, matching Main Agent conversations. Previously, it used a custom persistent intervention protocol that affected scheduler barriers, recovery, and more than ten activity-query filters. Messages are now injected as input for the next round, and the intent runs directly in a dedicated goroutine, independently of the three-slot Worker pool. The frontend keeps the message box and restores the **Continue directly** button; messages are sent over SSE. No DB schema changes. Trade-offs: messages are kept only in memory and are not recovered after a crash; sending one may briefly exceed the task's Worker concurrency limit by one (an acceptable, infrequent case).

### Tasks

#### Fixed

- Fixed out-of-memory (OOM) crashes when archiving large tasks: archives now stream snapshots instead of loading the entire task into memory at once. Also fixed three recovery gaps in the cold-archive path. In modern installations, traffic is stored only in SQLite; a crash between PostgreSQL and SQLite commits previously left traffic for archived tasks in hot storage with no recovery path. A staging journal is now written whether or not a historical directory exists, and a missing `journal.json` is treated as disposable.
- Fixed system slowdowns with many tasks, including delays when loading conversations and the loading screen. Queries for the task list, task context, and exploration records were optimized, and duplicate requests were reduced on the dashboard, conversation page, and task details page.

### Assets

#### Changed

- Removed the **256-rule maximum** for enterprise asset scopes. Enterprises that add IPs/domains individually could easily hit this limit and had to be split into multiple enterprises, even though a larger scope was not inherently slower. The limit was removed from the frontend, backend, and demo mock. Each rule remains limited to 1024 characters, and the 2 MiB request-body limit remains as a safeguard (approximately 40,000–50,000 rules).

### Skills

#### Fixed

- Fixed **“Upload failed: zip: unsupported compression”** errors when uploading skill archives. The Go standard library includes only Store and Deflate decompressors, so archives made with bzip2 or Zstandard (for example, using a non-default compression setting) could not be extracted. Both decompressors are now supported (pure Go, with no new external dependencies). For unsupported methods such as Deflate64, LZMA, XZ, and PPMd, and for encrypted archives, a clear notice is shown before extraction, identifying the file, compression method, and how to repackage it instead of exposing the underlying English error.
- Fixed filename validation rejecting non-ASCII names in skills. The path validator previously used the ASCII allowlist `[A-Za-z0-9-_./]`, so one non-ASCII filename (such as `référence/description.md`) caused the entire archive upload to fail with an invalid-path error. It now uses a Unicode denylist: filenames in any language and spaces are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (including RLO filename spoofing), `\ % # ? * : " < > |`, `..`, absolute paths, and empty path segments remain prohibited to prevent traversal. Skill names are also more permissive: ASCII names remain limited to lowercase letters, digits, and hyphens (the agentskills.io convention); non-ASCII letters are accepted, but spaces, dots, and path separators are not.
- Fixed garbled filenames or rejected archives when extracting Chinese-named ZIP files created by Windows archiving software. These ZIP files omit the UTF-8 flag and encode filenames as GBK; filenames are now decoded as GBK as a fallback before path validation. Quoted frontmatter such as `name: "my-skill"` is also parsed correctly.

### UI

#### Added

- Added an **All findings** flat view to the findings page and made it the default, with a header tab to switch to the existing **Grouped by task** view. The grouped view requires expanding tasks one by one to see findings, which is cumbersome when scanning across tasks. The flat view shows a cross-task table (10/20/50/100 items per page) with the same features as the grouped view: select for export; expand rows to see evidence and detailed reports; edit name, category, severity, and disposition inline; investigate; and delete. Both views share statistics cards and filters. Switching views preserves filters, and the current view and filters are saved locally. Polling applies only to the active view, while inline changes update both caches to prevent stale data after switching.
- Added an **By asset** view to the findings page. The left pane shows an asset tree (enterprise → root domain / IP → subdomain → service → endpoint; the enterprise level appears only when the asset belongs to one), and the right pane shows findings under the selected node's subtree. It shares the same table and filters as the other views. The tree includes only assets with findings and fills in ancestors as needed (so ancestors still appear when a finding is linked only to a deeply nested endpoint). Counts aggregate across each subtree and deduplicate findings. Findings for deleted assets or without linked assets appear in an **Unlinked assets** bucket. Unlike the other views, the asset view does not poll: queries run only when entering the view, changing filters, adding/editing/deleting findings on the page, or clicking the tree's refresh button. The tree is for navigation and does not need recalculation every five seconds. Each level shows only the difference from its parent (subdomains omit the root-domain suffix, services show `https :443`, and endpoints show only the path); the full value appears in hover text and breadcrumbs. If there are too many nodes, the service/endpoint levels are omitted for the entire level with a notice, while counts remain included in the parent.

#### Fixed

- Fixed conversation records being squeezed and cut off in mobile session details: on narrow screens, the session list now collapses to give the records more room.

### Dependencies

#### Changed

- Upgraded norma from v0.3.2 to v0.3.3, adding `Config.MaxTokensField` so the output limit for OpenAI Chat Completions can use `max_completion_tokens` (the key required by reasoning models). The two keys are mutually exclusive; only one is sent, and the default remains `max_tokens`.
- Upgraded `golang.org/x/mod` from v0.37.0 to v0.40.0, fixing two dependency security alerts.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the **non-streaming** setting reverting to **streaming** after reopening the configuration (#69): the configuration-list API DTO omitted the `streaming` field, so the response never included it and the frontend treated `undefined` as the default (streaming). Writes and DB storage were correct; the value simply could not be read back. The field has been restored to the DTO (without `omitempty`, since `false` must also appear in the response).
- Connection tests now verify that the model actually responds and use the configured request/response mode (#65). Previously, they checked only HTTP success, so a successful request with no model response (for example, exhausted reasoning budget, safety-filtered content, or a compatibility layer dropping `content`) still reported “Connection successful,” unlike the silent conversation experience. Tests also always used streaming, even for non-streaming profiles. Empty responses now fail, successful tests display the model response, and the current streaming setting is included so that a passing test means the conversation should work.

### Agent

#### Changed

- `list_facts` now supports pagination and keyword filtering to prevent large fact sets from overflowing the context (#74). By default, it returns the 20 most recent entries and supports `limit` (maximum 100), a `before` cursor, and a `q` summary keyword. The response is `{facts, total, has_more, next_before}`. Long summaries are truncated by character count (full content remains available through `node_detail`). Worker and Planner prompts now use the paginated interface. A one-time migration adds the new parameter schema to the tool catalog for existing databases (`SeedTool` only inserts on first use; without this migration, the tool management page showed “no parameters”).
### Tools

#### Added

- Added tool-call statistics to the **Tool Executions** page (#72). The **Statistics** toolbar button opens a dialog showing each tool's call count, share, and failures, sorted by count. It reuses the list's task/keyword filters and summarizes the entire result set, not just the current page. Data is fetched only when the dialog opens.
### Dependencies

#### Changed

- Upgraded norma from v0.3.1 to v0.3.2: fixed silently dropped reasoning/refusal in OpenAI responses (added `reasoning_text` for Responses), deduplicated the three reasoning field names, added bounded retries for empty responses (zero content blocks), and fixed gateway 400s caused by compaction splitting tool-call pairs (including existing orphaned pairs embedded in transcripts).
### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM profile can switch between streaming and non-streaming (streaming by default). Streaming uses SSE; non-streaming sends `stream:false` and returns the complete JSON response at once, avoiding poor SSE implementations at some gateways (empty frames or dropped reasoning fields) at the cost of live progress and Token counts. Worker, Planner, Main Agent, chat, and goals resolve the value dynamically from the active profile. All three Provider wrapper layers (`llmpool`, `llmrec`, and task runtime) support non-streaming. Added a `streaming` column to `llm_profiles` and migrated existing databases with `ALTER` (default `true`, so existing profiles are unchanged).
- Added LLM configuration support for the OpenAI Responses API format: each profile now supports a third format, `openai-responses` (calling `POST /v1/responses`), alongside Chat Completions and Anthropic. `BaseURL` normalization is supported, and the default model is `gpt-5`. Added `openai-responses` to the `format` constraint on `llm_profiles` and migrated existing databases idempotently. The frontend format selector now includes **OpenAI (Responses API)**. Upgraded norma to v0.3.1, including a fix for returning `reasoning_content`.
- Added recording of raw LLM request/response HTTP data: the HTTP transport layer captures the actual wire body, including tool schemas, `tool_use` blocks, and raw SSE frames not visible in the normalized view. Each retry attempt within norma is retained separately. Added `raw_request` and `raw_response` columns to `llm_records` and migrated existing databases with `ALTER`. The recording details panel now has a **Raw data** view toggle and copy buttons for requests and responses, with an `execCommand` fallback for non-secure contexts.
### Chat

#### Changed

- Multi-select in the conversation list is now controlled by a **Multi-select** mode toggle. Checkboxes are no longer always shown on each row, keeping the default list cleaner. The header shows the total count and a **Multi-select** button. Clicking it enters selection mode with checkboxes, select-all, and bulk delete; **Done** exits and clears the selection. Bulk deletion exits automatically if all items are deleted successfully; if any fail, selection mode remains active for retry. Individual rename, pin, and delete actions remain in each row's ⋯ menu.

### UI

#### Changed

- Improved task management, conversation actions, and traffic viewing (#57): task-list column sort preferences are now persisted, inline renaming is triggered directly by an icon rather than a menu, and sheet interactions and traffic-viewing details have been refined.

#### Fixed

- Worker asset tags now show only domains and IPs and correctly handle empty tags.

### Agent

#### Added

- Added **quantitative acceptance checks** when the Planner evaluates goals. If a goal has measurable criteria (for example, reaching X% asset-test coverage, obtaining N flags, or gaining a permission), the Planner must check actual values in `graph_overview` (such as `coverage.pct` and finding counts) before calling `prove_goal`. If the target is not met, `prove_goal` is prohibited and new intents must be assigned to close the gap; the goal cannot be marked met because “most” or “roughly” has been completed. This fixes goals requiring 100% coverage being marked complete when measured coverage was only 40%.

#### Changed

- Rewrote the Planner prompt's criteria for producing **zero intents this round**. Previously, zero intents were described as the “most common and important principle,” which could cause the Planner to stop too early when goals were unmet and untested scope remained. Now, zero intents are appropriate only in two cases: (1) all considered directions are already covered by `open` / `running` / `recent_done` intents; or (2) the next step depends on output from currently running work that is not yet available (wait for it to finish and plan on the next wake-up after the graph is updated). The prompt also states the converse: do not stop merely because zero intents are common when there is a new direction not covered by existing work, or when goals remain unmet and untested scope remains.
- Strengthened the evidence threshold for negative conclusions in the Worker prompt. For conclusions such as “cannot inject,” “port closed,” or “no login entry point” that could cause the Planner to abandon an entire direction, Workers must first exhaust reasonable methods within the intent (trying different encodings, parameters, paths, or methods). If these methods have not been exhausted or evidence is weak, the conclusion must be labeled `confidence=inferred`, preventing a premature `observed` negative from closing off a route (especially early in a task, when an incorrect conclusion can derail exploration and be difficult to recover from).
- These default Planner/Worker prompt changes are added as new versions and activated through a one-time migration (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag). Older versions remain in the version history, and users with custom prompts can restore them from the version records.

### Operations

#### Added

- Added `reset-password.sh` to reset the administrator password (the username is fixed as `ARTEX`). It supports local and Docker deployments; connection details can be specified explicitly or read automatically from `--dsn`, `$ARTEX_PG_DSN`, or `config.json`. The script uses `pgcrypto` in the database to generate a bcrypt hash compatible with backend login and writes it to `settings.auth.password_hash`; no service restart is required. The password is passed through an environment variable rather than process argv and is escaped to prevent injection.
### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After selecting multiple tasks in the task list, their categories can now be changed in bulk. The destination can be **Uncategorized**, removing tasks from their current category. The entire batch is written in a single transaction; the only failures are tasks deleted after selection.

#### Changed

- The **Task category** field when creating a task is now a searchable input instead of a dropdown. Typing filters existing categories; pressing Enter (or clicking **Create** in the dropdown) immediately creates and selects a category that does not exist. The selected category appears as a removable tag; only one category is still allowed.

### Chat

#### Fixed

- Fixed messages being blocked with **LLM not configured, cannot chat** when a conversation had a selected LLM profile but no globally active profile. The pre-send check previously looked only at the global profile, while execution preferred the conversation's selected profile. Both now use the same resolution logic (conversation/Agent binding first, global fallback).
- Chat-unavailable notices are now specific to the state: **Add a profile** when none exist, and **Activate a profile or select one for this conversation** when profiles exist but none is active, instead of always saying **Not configured**.

### Agent

#### Added

- In tasks where all goals are complete, Workers can now directly claim and run intents submitted by the Main Agent through `add_intent`, then stop when finished. The Planner does not run in this state (tasks without open goals are not planned), preventing it from reevaluating the goals and canceling the newly submitted intent. Once the frontier is exhausted, the task returns to **Completed**. Before submitting an intent, the Main Agent asks whether it should be registered as an official goal based on its content; registering it resumes normal autonomous planning.

#### Changed

- When `step_ids` passed to `get_worker_trace` / `get_task_worker_trace` exceeds the per-call limit (5), the tools no longer return an error. They return the full content for the first five steps and use `returned_step_ids`, `omitted_step_ids`, and `notice` to identify what was included and omitted, with a note that no further retrieval is needed if the results suffice. Duplicate and invalid IDs are removed before counting.

#### Fixed

- The task list is now sorted by task ID in descending order (newest first) instead of creation time. Tasks created at the same time can have identical timestamps, making their order unstable; combined with polling every 10 seconds and nondeterministic iteration of an in-memory map, this caused them to frequently swap places.

### Assets

#### Fixed

- Fixed enterprise ownership recalculation failing because of a single dirty row: if `assets.ip` contained a hostname (written by an Agent or the asset API), `a.ip::inet` raised `22P02`, causing enterprise-scope creation/editing and deletion to fail and roll back. It now uses the safe conversion `try_inet()`, skipping invalid values instead of aborting the whole statement.
- Unparseable `ip` values are no longer silently skipped. After saving an enterprise scope, the UI now reports how many assets have an invalid IP, including their IDs and values, and warns that they will not be matched by IP/CIDR rules. The same information is written to server logs, including for paths without a frontend response (enterprise deletion, ScopeSentry sync, and Agent writes).
- IP/CIDR matching for task test scopes also now uses `try_inet()`, replacing the previous regex guard that checked only the character set. Hostnames such as `abc.def`, made up entirely of hexadecimal letters, could previously bypass the check and trigger the same error.

#### Changed

- The `ip` field for `ip`, `service`, and `endpoint` assets no longer accepts hostnames. `insert_assets` and the asset API now return per-item errors with an `index` and a suggested correction (use `type=subdomain` and fill in `domain`, or resolve the A/AAAA record first). Agents can use this information to correct and resubmit the item; other assets in the batch are still stored normally.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- Running Workers can now be paused, resumed, or canceled individually. Pausing preserves intents, facts, and findings; canceling transactionally removes the current intent and its direct outputs after execution exits.
- Added the `paused` intent state, execution fences, and named termination reasons to prevent late blackboard writes after pausing, cancellation, or task deletion.
- Added an optional per-task concurrency limit. New work, resumes, finding investigations, and queue replenishment all use the same persistent FIFO admission path.

#### Changed

- The default wall-clock duration for a Worker run increased from 600 to 1200 seconds. A revived timed-out task starts a fresh timer when its next actual run begins.
- Task status badges are now driven consistently by the real execution state of Workers, Planners, and the Main Agent, fixing Workers appearing **Idle** while running.
- Worker sessions retain individual controls and a current-session Token summary; the model name is now displayed beside the current session title.

#### Removed

- Removed Worker multi-select, select-all, and bulk pause/resume UI, along with the Worker bulk-control API and Mock contract.
- Removed Token badges and tooltips from Worker list rows; the full underlying Token ledger and task aggregation API remain.

### LLM

#### Added

- Tasks now support an ordered LLM profile chain. Provider quota exhaustion is recognized explicitly and triggers automatic failover to the next profile; the current profile, exhausted state, and error summary are persisted.
- Running and paused tasks can edit the full profile chain, reorder it, and manually switch the active profile; changes take effect on the next LLM call.
- Automatic failover, manual switching, and exhaustion of the entire chain are recorded as structured system activities, with deduplicated notices in the task activity stream.
- Added a task-role model resolution API to consistently resolve the profile and model for the next Main Agent, Planner, or Worker call.
- Added an optional global LLM Pool with configurable call order, per-model failure fallback, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger aggregating input, output, and cached Tokens by task, conversation, model, and profile.

#### Changed

- Goal decomposition, Planner, Worker, and Main Agent now share the task-level LLM runtime (resolution priority below).
- Failover responds only to explicit quota, balance, or billing errors; ordinary rate-limit, authentication, network, server, and context errors do not trigger an incorrect switch.
- The LLM settings page now uses profile cards with editing in a side drawer, and provides Pool rotation order, priorities, exclusions, health status, and recovery controls.
- Exhausted-chain notices now wrap on narrow screens. The current model is shown as a text label beside the session title, with a full tooltip, instead of an icon in the session list.
- LLM resolution order for task roles is now **Agent binding → task profile chain → global**. A role with an explicit model binding always uses that model; unbound roles use the task chain, then fall back to the global profile if the chain is empty.
- An empty egress proxy setting now means a direct connection and does not fall back to `HTTP_PROXY`/`HTTPS_PROXY`  environment variables (explicitly setting `ARTEX_LLM_PROXY`  remains effective); proxy settings accept username/password credentials in `socks5://user:pass@host:port`。

#### Fixed

- Transient streaming failures before submission (before any output is produced) now retry safely with backoff on the same provider, greatly reducing cases where a run stops after one or two tools without a summary and ends in `model_error`. Quota exhaustion, oversized context, and deterministic 4xx errors are not retried; they continue to be handled by failover or compaction recovery, respectively.

#### Removed

- Removed the LLM icon from the session list to avoid confusion with Worker status icons.

### UI

#### Added

- Tasks now support creating, renaming, deleting, and filtering global categories; a category can be selected directly when creating a task.
- Added CRUD for global task templates and a management drawer. New tasks can load preset descriptions and goals, and current content can be saved as a template.
- New tasks can link multiple source tasks and enterprise asset scopes. A single multiline field automatically recognizes domains, URLs, IPs, CIDRs, ICP records, and enterprise keywords.
- Test assets can be added and removed from a task in real time, with sources recorded (manual, enterprise, inherited, or discovered by an Agent). Worker session titles show a summary of current test assets and their sources.
- The findings page is grouped by task with independent pagination per group; findings can include descriptions and spawn high-priority investigation intents.
- Conversations support rename, pin, unpin, and delete. The task list supports bulk pause/resume for the current page.
- Traffic details and tool execution details now use side drawers. HTTP messages include `Host` and syntax highlighting for request lines, status codes, headers, JSON, and markup bodies.
- Added **Details** and **Pause/Resume** buttons to task-list actions. The conversation send shortcut is configurable in System Settings (`Enter`, `Cmd+Enter`, etc.), and the web-search section now has a proxy input.
- The badge beside each conversation title now shows the LLM profile name; the model ID is in a hover tooltip. Tasks can have an optional name, with a searchable name column in the task list (falls back to the description when blank).
- The Skills page now reports usage count, last-used time, and missing dependencies to help identify skills that are inactive or not installed.

#### Changed

- Task titles are now focusable links to details. Task assets, enterprise assets, task groups on the findings page, and findings within each group use server-side pagination, stable sorting, and exact totals.
- Enterprise creation now uses a side drawer. Scope entry is standardized as multiline text with live recognition, validation, and type previews.
- The Main Agent input auto-grows, sends on `Enter`, and inserts a newline on `Shift+Enter`, while avoiding accidental sends during IME composition.
- Agent previews, task reports, and related details now share Markdown rendering. Delete confirmations, long errors, and mobile drawer widths have been fixed consistently.
- The task-template selector displays and searches by template name but submits the template ID. Restored the system's original font size, and standardized the displayed app version as `0.3.3`.
- Task totals and current-session Token figures now highlight only input, cached-read, and output values. The send button uses a simpler up-arrow icon.
- The task overview now shows enterprise names, rather than IDs, for the test scope.

#### Fixed

- Ordinary text containing a period is no longer misidentified as an ICP filing number.
- Fixed duplicate keys in the Agent editor's variable list when a variable-directory entry had the same name as a global runtime variable such as `{{.Now}}`.

#### Removed

- Removed the standalone **Open** button from task cards; click the task title to open details.
- Removed the dashboard's **New task** button, the Logo URL field when adding an enterprise, and finding-count badges from the top of the findings page and its task groups.
- Reverted the global 10% font-size increase and removed Worker checkboxes, model icons, and per-row Token counts from the conversation list.

### Agent

#### Added

- New tasks can link multiple existing tasks and inherit read-only data from each direct source in real time: goals, facts, findings, completed intents, asset scope, and blackboard context.
- Blackboard read tools can query source-task nodes, facts, findings, and execution traces on demand. Inherited nodes are marked with their source, and all write tools reject modifications to them.
- Enterprise-scope tools support domains, URLs, IPs, CIDRs, ICP records, and enterprise keywords. Keywords guide Agent scope but are not used for automatic asset ownership.
- Investigating a finding creates a high-priority manual Worker intent in the original task, with an asset anchor and a `derived_from` edge.
- Added a **Goal management** card to the overview for manually viewing, adding, editing, and deleting goals. Adding or editing notifies the Planner and revives the task; deletion permanently removes the goal node and cascades to its edges and anchors, without reviving the task.
- The Main Agent now gets the `steer_work` tool by default, allowing it to inject live corrective instructions into a running Worker without interrupting it or losing progress (intent ownership is still checked against the current task).

#### Changed

- Intents from source tasks are not added to a new task's frontier. The new task retains its own exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys its data. It is marked `stopped`, requires a deletion reason, and stores that reason as a fact attached to the intent and in its payload. The Planner is notified as if the intent were `cancelled`; the intent content and reason are retained.
- Main Agent conversations are now driven by the server activity stream, and input can be restored after a send failure. Opening a conversation scrolls to the bottom and keeps the last reply visible after details are lazy-loaded.
- Pausing a task terminates current Main Agent, Planner, and Worker calls, but does not prevent the user from starting a new Main Agent orchestration conversation while the task is paused.
- Cancellation, shutdown, and streaming interruption now consistently preserve the actual termination reason, generated content, run count, duration, Tokens, and unreturned tool calls.

#### Removed

- Removed optimistic client-side message echoes for the Main Agent to prevent duplicate messages and cross-conversation content during pauses, failures, or concurrent activity.

### Constraints

#### Added

- Goal decomposition now extracts operational constraints (allow/deny) before splitting goals; the Main Agent can add constraints at runtime. Constraints are injected into Planner and Worker system prompts at the highest priority, and debiasing, diversification, and broader exploration must explicitly follow them.
- Added a **Constraint management** card and CRUD endpoints to the overview. Constraint injection can be toggled separately for Planner and Worker (both enabled by default; read each round and effective immediately). The `task_constraints` table is created with `CREATE TABLE IF NOT EXISTS` and added automatically to existing databases.

### Interception

#### Added

- Added model-based fallback approval for command interception, in addition to regex/string rules. When no rule matches, the model makes a semantic `ALLOW` / `ASK` / `DENY` decision. The failure fallback and approval-timeout actions are configurable. The interception page now has separate **Interception rules** and **Model configuration** tabs; model decisions are marked with a `[model]` prefix and a reason.

#### Fixed

- Hardened verdict parsing so a valid `DENY` decision is not mistakenly treated as an allow.

### Traffic

#### Changed

- Traffic recording now stores each complete exchange in SQLite (large bodies spill to a hash-deduplicated blob store). Text bodies use a trigram full-text index, supporting arbitrary substring and Chinese-language searches. Deletion is now a single SQL transaction, reducing it from hours to milliseconds without pausing recording. Added streaming downloads for large bodies and full-text parameters to `traffic_search`. Existing file-tree data requires no migration and remains readable, searchable, and deletable.

### Tasks and Assets

#### Added

- Added an **Asset coverage** toggle when creating a task (on by default). When disabled, coverage is neither calculated nor displayed, the overview shows only assets, `task_scope` does not accumulate automatically, and related tools are removed from the Planner and Main Agent. Enterprise associations are unaffected.
- Added a `related` flag (default `true`) to each asset in `insert_assets`. It applies only when coverage is enabled; `false` adds the asset to the shared library but excludes it from this task's coverage (for example, a discovered adjacent site or unrelated asset).
- Enterprise scopes and task test assets now consistently recognize domains, URLs, IPs, CIDRs, ICP records, and keyword text. Domains/IPs can create or reuse global assets; CIDRs, ICP records, and keywords remain as task-scope context.

### Build

- `build.sh --release` can build Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one run, and creates a ZIP release package containing `skills/`, configuration examples, and the README.
- Go linker flags strip debug information when creating the ZIP release package. UPX is now an explicit opt-in to avoid startup segmentation faults from its self-extracting ELF on some Linux systems.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and includes `SHA256SUMS` in the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- New tasks can link multiple existing tasks and inherit facts, findings, completed intents, asset scope, and blackboard context from direct sources in real time and read-only. Each new task still has its own execution queue, working directory, and conversation history.
- Tasks support ordered LLM profile chains. Recognized provider quota exhaustion triggers automatic failover to the next profile, with the active profile, exhausted state, and structured audit activity persisted.
- Running and paused tasks can edit and reorder the LLM profile chain and manually switch the active profile. Automatic/manual switching and chain exhaustion are reported on the task details page.
- Running Workers can be paused, resumed, or canceled. Pausing preserves blackboard data; cancellation transactionally removes the intent and its direct facts, findings, and execution records after the Worker stops writing.
- Task deletion can optionally also remove associated assets, traffic, findings, and task files. Added deletion barriers, concurrency protection, and auditable deletion statistics.
- Added an optional per-task concurrency limit; new tasks beyond the limit are queued FIFO and start automatically when a slot becomes available.
- Added pagination for task assets and company assets, task-category statistics, and a paginated-intent Mock contract.
- Added the single-binary `build.sh` build script, supporting static frontend export, embedded assets, cross-platform targets, and build-version injection.

### Changed

- Explicit task-level LLM profile chains coexist with the existing global LLM Pool. Explicit chains retain strict quota-failover semantics; without one, Agent bindings and global configuration rules continue to apply.
- The Main Agent input supports auto-growing multiline text, sends on `Enter`, and inserts a newline on `Shift+Enter`, while avoiding accidental sends during IME composition.
- Planner, Worker, and Main Agent share the task-level LLM runtime. The task chain uses the smallest context window among candidate models as the safe compaction threshold.
- Task details show running, idle, and paused states based on actual LLM-call status. Facts, findings, intents, asset references, and graph nodes from source tasks are consistently marked and remain read-only.
- Agent previews, task reports, and related detail views now share a Markdown rendering component.
- LLM model configuration now uses cards and drawers, with independent scrolling for the model list inside the drawer.
- Pausing a task no longer blocks Main Agent conversations. Main Agent orchestration sessions are independent of task pause, so users can send new messages while paused (pausing terminates only the current round).
- The default wall-clock duration for a Worker run increased from 600 to 1200 seconds.

### Fixed

- Removed optimistic echoes from the Main Agent console in favor of server-rendered data, fixing duplicated or cross-session messages during pauses, send failures, and other cases.
- Main Agent sessions now open scrolled to the bottom with the full last reply visible. Once the full reply is expanded by lazy loading, the page automatically returns to the bottom instead of pushing it off screen.
- Fixed the Main Agent continuing to run after a task was paused, and the orchestration Agent failing to stop the Main Agent when pausing a task.
- Fixed task status badges and action buttons becoming inconsistent when a task was completed or a Planner, Worker, or Main Agent was actually running.
- Fixed late blackboard writes and leftover files that could result from races between Worker cancellation, task deletion, and concurrent writes.
- Fixed broken Markdown previews, long-text overflow in delete confirmations, mobile widths, and misaligned buttons on some task details pages.
- Added named termination reasons to every Agent cancellation path. Activity details can show who canceled, terminal state, run count, duration, Token usage, and unreturned tool calls.
- Fixed a race in which the parent context could overwrite the named `shutdown` reason during backend shutdown, and fixed garbled activity summaries caused by truncating Chinese text by byte length.
- Fixed cancellation events with partial streaming output losing the actual termination reason, while preserving content generated before cancellation.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

[Unreleased]: https://github.com/Autumn-27/ARTEX/compare/v0.3.10...HEAD
[0.3.10]: https://github.com/Autumn-27/ARTEX/compare/v0.3.9...v0.3.10
[0.3.9]: https://github.com/Autumn-27/ARTEX/compare/v0.3.8...v0.3.9
[0.3.8]: https://github.com/Autumn-27/ARTEX/compare/v0.3.7...v0.3.8
[0.3.7]: https://github.com/Autumn-27/ARTEX/compare/v0.3.6...v0.3.7
[0.3.6]: https://github.com/Autumn-27/ARTEX/compare/v0.3.5...v0.3.6
[0.3.5]: https://github.com/Autumn-27/ARTEX/compare/v0.3.4...v0.3.5
[0.3.4]: https://github.com/Autumn-27/ARTEX/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/Autumn-27/ARTEX/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/Autumn-27/ARTEX/compare/v0.3.1...v0.3.2
