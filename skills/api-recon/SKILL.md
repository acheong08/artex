---
name: api-recon
description: Use this skill to enumerate a website's API endpoints.
---

# API Recon (Frontend API Reconnaissance)

With **proper authorization**, discover as comprehensively as possible: **backend APIs** (paths, methods, parameters, response bodies), **frontend routes**, and **UI feature triggers** (tabs, dialogs, table actions, etc.).

---

## Scope and Prohibitions (Agents Must Read · Violations Are Out of Scope)

This skill is for **API and parameter-surface reconnaissance only**, not vulnerability discovery or exploitation.

### Task Boundaries

| Scope | Allowed | Prohibited |
|---|---|---|
| **Target** | Enumerate paths, methods, parameters, routes, and UI triggers | SQLi/XSS/authorization bypass/brute-force/fuzzing vulnerabilities, request-tampering attacks, destructive actions |
| **Authentication** | Use hooks + stubs/mocks to bypass the **client-side** login gate | Ask for or guess credentials; attempt to submit a real login form |
| **Runtime** | Hook interfaces without credentials and use mock responses to render the SPA's authenticated shell | Flows that require a real backend session to continue |

### Dynamic Analysis Without Credentials (Phase 3 Default)

1. Use `preload.js` / `runtime_harvest.js` to **intercept and stub** bootstrap interfaces for login, permissions, menus, etc.
2. Return mock bodies for business-query interfaces with a **correct structure, successful business status code, and optionally empty data**.
3. Let the frontend render its authenticated pages without a backend or in a 401 environment, triggering additional XHR/fetch/WebSocket requests.
4. **Empty data, blank tables, and placeholder UI are expected**—do not switch to real login or vulnerability testing for this reason.

**In short:** use mocks to expose frontend routes and mount components, and **record outbound requests only**. Backend responses are irrelevant; what matters is **which interfaces the frontend continues to call**.

### Strict Process Prohibitions

| Prohibited | Alternative |
|---|---|
| Before Phase 1 is complete, grep/curl/Read the main `index-*.js` entry to extract API paths | Run `OUTDIR/harvest_static.py` |
| Write a replacement harvest script such as `extract_apis.py` | Modify and rerun `OUTDIR/harvest_static.py` |
| Repeat the same failed grep/command two or more times | Change approach: inspect tool_logs, modify harvest, or consult the reference |
| Skip gates A/B and run the original `scripts/` directly | Copy to OUTDIR and adapt for the target |
| Authenticate with real usernames/passwords, OTP, OAuth, etc. | Use stubs/mocks (see above) |
| Skip stubs to "get real data" and test authorization bypass/injection | Record outbound traffic only; that is the recon boundary |
| Perform irreversible actions such as deleting/exporting sensitive data or bulk writes | The same applies to clicks in coverage mode |
| Claim all pages and interfaces have been found without runtime + dynamic enumeration | See "Definition of Done" or document limitations |
| Claim all parameters are known without the parameter-trigger matrix + diff | Use the Phase 3b matrix + Phase 5 diff |
| Infer required/optional parameters from one runtime sample | Use multi-sample diffs or infer from validation rules/errors |

---

## Two-Layer Model and Run Modes

| Layer | Output | Limitations |
|---|---|---|
| **Static** (JS bundle) | All endpoint paths, route candidates, and candidate fields from request-building code | No HTTP methods; parameters require Phase 1b; misses URLs assembled at runtime |
| **Runtime** (live session) | Method + body + response + dynamic URL + WS/SSE; multi-sample diffs fill in parameters | A page must render to issue requests; one sample is insufficient to determine required/optional fields |

| Run mode | Engine | Use |
|---|---|---|
| **depth** | `runtime_harvest.js` (Puppeteer) | API inventory, methods/parameters/response bodies, WS/SSE, reproducible batch runs |
| **coverage** | browser + `preload.js` | Click tabs/dialogs/tables for deeper feature coverage |
| **both** | depth followed by coverage | Most complete, takes the longest |

**Parameter methodology** (no generic script): find paths with harvest/regex; find parameters using **anchor-window expansion + UI binding chains + multi-sample diffs + error inference** (grep recipes in section J of [reference.md](reference.md)).

---

## Definition of Done

Recon is complete only when all of the following are satisfied:

- [ ] **Static**: Phase 1 harvest produced `api_static.txt`, `routes.txt`, and `js/`.
- [ ] **Runtime**: At least one of depth or coverage was run; coverage/both requires an **active Hook + dynamic enumeration loop**.
- [ ] **Reach the application shell**: Visiting business paths does not redirect to `/login` (watch for hash-based routes).
- [ ] **Parameters**: For coverage/both, complete the parameter-trigger matrix and generate `param_samples.json`; merge into `params_merged.json` in Phase 5.
- [ ] **Depth** (if module pages are blank): Restore the permission tree in Phase 4 and rerun until **module-level APIs** appear (not just locale/bootstrap).
- [ ] **Deliverables**: All Phase 5 outputs are present (see the Phase 5 output table); `insert_assets` writes service and endpoint assets.

---

## Scripts and Gates

Files in `scripts/` are **reference templates only**; do not run an unmodified original script as the final result.

**Rule:** read first → adapt to the target → write to `OUTDIR` (e.g. `recon/`) → record changes in `CHANGES.md`. If a script does not fit, rewrite it using the methodology and reuse only its structure.

| Gate | When | Reference script → OUTDIR copy | Common required changes |
|---|---|---|---|
| **A (static)** | After Phase 0, before the **first** harvest/spider run | `harvest_static.py` / `spider_mpa.py` | **Default regex works on most sites**; change endpoint regex, webpack/Vite `publicPath`, or MPA exclude/cookie only when the manifest/dialect differs |
| **B (runtime)** | After Phase 2, before running depth/coverage | `runtime_harvest.js` / `preload.js` + `config.json` | Cookie/localStorage keys, neutralized success values, stubs, login regex, API prefixes, hash/history |

**Required SPA order** (not interchangeable; follow the phase numbers rather than "explore first, script later"):

| Step | Required | Prohibited |
|---|---|---|
| After Phase 0 | The next Bash command is `python3 OUTDIR/harvest_static.py <URL> OUTDIR` | curl/grep/Read the main `index-*.js` entry (usually >500KB) |
| Gate A | Copy script → make small changes as needed → **run immediately** | Manually extract APIs before deciding whether to harvest |
| Before Phase 1 is complete | Verify outputs with `wc -l`; fix harvest and retry on 404 | Write a custom extract script; repeatedly grep URLs that were not downloaded |
| From Phase 1b onward | grep only `OUTDIR/js/*.js` | Use the main bundle instead of harvest |

- ✅ Copy `harvest_static.py` → optionally adjust regex → **run immediately**
- ❌ curl the main bundle → grep repeatedly → write a temporary extractor → harvest only at the end
- **MPA**: the next Bash command after Phase 0 is `python3 OUTDIR/spider_mpa.py ...`

---

## Tool and Output Constraints

| Constraint | Description |
|---|---|
| Large files | Do **not** Read/grep `index-*.js` files >100KB into context; process them with OUTDIR scripts |
| grep output | Must use `\| head -20` or `-m 5`; keep only path summaries in chat, never paste bundle fragments |
| Verification | Use `wc -l`, `ls \| wc -l`; do not Read entire directories |
| Initial regex check | Optional, at most once, and only on a small chunk ≤50KB or HTML; use harvest as the authoritative static result |
| Reference | Recipes/templates/troubleshooting are in [reference.md](reference.md); do not inline the full text again |

---

## Execution Roadmap

```
Phase 0 classify + OUTDIR
  → Gate A → Phase 1 harvest (★ run immediately ★)
  → Phase 1b parameter analysis
  → Phase 2 authentication gates → config.json
  → Gate B → Phase 3 runtime + parameter matrix
  → Phase 4 permission tree (if needed) → rerun Phase 3
  → Phase 5 merge report + use insert_assets to bulk insert all discovered service and endpoint API assets; never omit a discovered asset
```

Check off items in order; **do not proceed to the next phase until the previous one is complete**.

1. [ ] **Phase 0**: Initial SPA/MPA check; create `OUTDIR` → [Phase 0](#phase-0--classification)
2. [ ] **Gate A + Phase 1**: Copy script → harvest **immediately** → verify with `wc -l` → [Phase 1](#phase-1--static)
3. [ ] **Phase 1b**: Anchor-window expansion + binding layer → `param_candidates.json` → [Phase 1b](#phase-1b--parameter-analysis)
4. [ ] **Phase 2**: Authentication gates → `config.json` → [Phase 2](#phase-2--three-authentication-gates)
5. [ ] **Gate B**: Adapt runtime scripts → [Phase 3](#phase-3--runtime)
6. [ ] **Phase 3**: depth / coverage / both; verify the app shell; parameter-trigger matrix → `param_samples.json`
7. [ ] **Phase 4** (if needed): Permission tree → patch stubs → rerun Phase 3 → [Phase 4](#phase-4--permission-tree-reconstruction)
8. [ ] **Phase 5**: Merge outputs + report + `insert_assets` → [Phase 5](#phase-5--merge-and-report)

---

## Phase 0 — Classification

Fetch the entry HTML and **create `OUTDIR`** (do not modify the skill's `scripts/`):

- **SPA**: Empty shell + `<div id=app>` + chunks → Phases 1–5
- **MPA**: SSR + `<form>`, no endpoint bundle → after Gate A:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Outputs: `forms.txt`, `links.txt`, `api_inline.txt`. If an SPA has approximately zero forms, proceed to Phase 1.

---

## Phase 1 — Static Analysis

Follow [Scripts and Gates](#scripts-and-gates) and [Tool and Output Constraints](#tool-and-output-constraints).

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
```

Harvest parses HTML scripts → webpack/Vite manifest → downloads all lazy chunks → outputs `js/`, `api_static.txt`, `routes.txt`, and `chunkmap.txt`.

```bash
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

- Chunk count vs. manifest: if chunks return 404, modify harvest and retry; do not curl each chunk manually.
- If `api_static.txt` has too few entries, broaden the endpoint regex in OUTDIR and rerun (see the reference).

### Phase 1b — Parameter Analysis

Paths come from Phase 1; parameter fields require separate reconnaissance. See [Tool and Output Constraints](#tool-and-output-constraints) for grep rules.

**Completion criteria:** for important interfaces, determine field names, transport location, inferred types, whether fields are required, sample values, and confidence.

#### 1b.0 — Transport Type

| Transport | Where parameters are | Static analysis starts with |
|---|---|---|
| REST JSON | body + query | `(params\|data\|body)\s*:\s*\{` near the path anchor |
| GraphQL | `variables` | gql templates, `$page: Int` |
| Traditional form | urlencoded | `<form>`, `FormData` |
| File upload | multipart | `FormData.append` |
| Path parameters | `/user/:id` | Route table + `useParams` / `$route.params` |
| Encryption/signing | Wrapped in `sign`/`data` | Hook the encryption function arguments (reference section D) |

Output: annotate each interface with `transport: query|json|form|graphql|encrypted`.

#### 1b.1 — Anchor-Window Expansion

Use a known path as an anchor and expand the search window to find the request-building object:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

| Wrapper layer | Parameter clues |
|---|---|
| axios instance | `data` / `params` |
| Shared request wrapper | Interceptor-injected global fields |
| OpenAPI client | Generated method signature |
| React Query / SWR | Hook's second argument |
| Vue composable | Composable arguments |

Look for type information in `yup`/`zod`/rules, `Form.Item name=`, or embedded Swagger.

→ `param_candidates.json`: `{ path, fields[], source: "static-callsite", confidence }`

#### 1b.2 — Binding Layer

```
Form field → onFinish/handleSubmit → transform → API payload
```

| Binding source | Technique |
|---|---|
| Form submit | Trace submit → transform → API |
| Table search | `getFieldsValue()` → `params` |
| Route | `:id` / `?tab=` |
| Interceptor | Global `tenantId`, pagination, sign |
| Enum select | `options` → API enum value |

Use the DevTools call stack to trace upward from `fetch`/`XHR.send` to the request-building function.

#### 1b.3 — Three Request-Building Questions (Not the Phase 2 Authentication Gates)

| Question | What to determine |
|---|---|
| **Assembly** | Where the payload is built; evidence of transformations |
| **Validation** | required, pattern, enum |
| **Transport** | path / query / body / multipart / headers |

The interceptor gate (Phase 2) also reveals globally injected fields (Authorization, `X-Tenant-Id`, sign).

#### 1b.4 — Handoff to Phase 3

Candidate fields come from static analysis/binding layers; determine **required/optional/conditional dependencies** through the Phase 3 parameter matrix + diff + Phase 5 error inference.

---

## Phase 2 — Three Authentication Gates

Grep `OUTDIR/js/` (with `head`) and write findings to `config.json` (see the reference for recipes):

| Gate | Question | Keywords |
|---|---|---|
| **Render gate** | How is the user recognized as logged in? | `isLogin`, `getToken`, Cookie/localStorage |
| **Interceptor gate** | What triggers a redirect to `/login`? | `response_code`, `errno`, axios interceptor |
| **Content gate** | Where do menus/permissions come from? | `menu`, `permission`, `role`, `acl`, `routes` |

Do not treat a localStorage key name as a credential; confirm it from the chunk/request chain.

**Exit = Gate B**: record findings in `config.json` and adapt `OUTDIR/runtime_harvest.js` / `preload.js`.

### Phase 2b — API Observation (Optional)

Use `preload.js` in OUTDIR to confirm session key names, Authorization, and nested API URLs:

| Configuration | Output |
|---|---|
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | Observe headers |
| `extractUrlsFromResponse: true` | Nested APIs in responses |
| `observe.storageReads/cookieReads: true` | Populate config |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

Export these after each coverage run: `__API_RECON_LOG__`, `__API_RECON_DETAIL__`, `__API_RECON_ROUTES__`, and `__API_RECON_OBSERVE__`.

---

## Phase 3 — Runtime

Gate B must be complete. Follow [Scope and Prohibitions](#scope-and-prohibitions-agents-must-read--violations-are-out-of-scope) and the no-credential mock strategy.

Set `"runtimeMode": "depth" | "coverage" | "both"` in `config.json` (see the reference for a template).

### Hook and Stub (Shared by depth + coverage)

| Layer | Scope | Purpose |
|---|---|---|
| L1 exact | auth/permission/bootstrap stubs | Pass initial authentication |
| L2 negative correction | All JSON responses | Convert unauthenticated codes to success |
| L3 fallback | `/api` etc. not matched by L1 | Empty success body to render the UI |

- **depth**: fake auth + `forward` to rewrite business status codes + `stubs`; traverse `routes` (hash/history); output `runtime_api.json`
- **coverage**: inject `preload.js` at **document-start** (CDP `addScriptToEvaluateOnNewDocument` or Userscript)

Verify that `window.__API_RECON_PRELOAD__` exists and business paths do not redirect to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

### 3b — Dynamic Enumeration in coverage Mode (Required)

1. Main navigation/sidebar — click each item and wait 1–3 seconds for network activity.
2. Tabs — `role=tab`, `.ant-tabs-tab`.
3. Tables — inspect/view, edit, and open details for the first row.
4. Toolbars — export, filter, create (**avoid irreversible deletion**).
5. On entering each module — merge APIs/routes.
6. SPA — use controlled `pushState` for paths not covered in `routes.txt` (not allowed for MPA).

**Parameter-trigger matrix** (required): record each operation type in every module and **diff multiple samples**:

| Operation | Parameters commonly added |
|---|---|
| Initial list load | Pagination + default filters |
| Search | keyword, filter |
| Advanced filters | Additional optional fields |
| Create/edit | Full entity |
| Bulk/export/sort | `ids[]`, `exportType`, `sortField` |

**Outbound bodies/headers remain real when using stubs**—use the requests as the source of truth. Save recordings to `scan_raw.json`, `param_samples.json`, and `api_detail.json`.

- **Vue**: `neutralizeVueRouter: true` + document-start preload.
- **React**: `routes.txt` + sidebar clicks + `pushState`.
- **both**: run 3a depth, then 3b coverage.

---

## Phase 4 — Permission Tree Reconstruction

**Trigger:** module pages are blank / each route shows only bootstrap calls (such as locale) → the content gate has not passed.

| Symptom | Meaning |
|---|---|
| App shell loads | Render and interceptor gates passed |
| Sidebar items missing/clicks show blank pages | Stub shape is wrong or permission codes are incomplete |
| Same few APIs on every route | `v-if permission` did not pass |
| `routes.txt` contains far fewer routes than the bundle | Add routes from the auth module |

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
```

Typical chain: `role_permissions` (flat codes) + `permissions/all` (tree) → `getResultTree` → `userRouteAuth[CODE].url`.

```bash
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Intermediate outputs: `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, `perm_codes_all.txt`.

Stub checks: outer `response_code` matches the interceptor gate; flat codes align with the tree; `routes` covers every link in `route_map`.

After updating `config.json`, **rerun Phase 3**. For large SPAs, adjust `waitUntil`, `routeTimeout`, and `perRouteMs` (see sections A3/I in the reference).

---

## Phase 5 — Merge and Report

### Output Table

| File | Phase | Contents |
|---|---|---|
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundles and paths |
| `param_candidates.json` | 1b | Candidate parameter fields from static analysis |
| `config.json` | 2 | Three gates + runtime configuration |
| `runtime_api.json` | 3a | Detailed depth recording (including WS/SSE) |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multi-sample data, click logs, details |
| `route_map.json`, etc. | 4 | Intermediate permission-tree files (if run) |
| `params_merged.json` | 5 | Merged parameter fields + confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static\|runtime\|both]` |
| `site_map.json` | 5 | Routes, APIs, params, features, limitations |
| **insert_assets** | 5 | Write all service and endpoint assets to the asset library |

### 5b — Parameter Merge

Diff `param_samples.json`; there is **no generic merge script**. See reference J7 for confidence levels (high/medium/low/not yet triggered).

### 5c — Error Inference

Within the authorized scope, incomplete requests may be sent to inspect 400 responses (**parameter reconnaissance, not vulnerability testing**), such as `field 'x' is required` or enum errors. Watch for `data` wrappers, `variables`, and pre-encryption `bizData`.

The report must include: runtimeMode, static/runtime API counts, parameter confidence, uncovered modules, and a summary of `CHANGES.md` relative to the reference scripts.

Suggested `site_map.json` structure:

```json
{
  "site": "https://example.com",
  "runtimeMode": "both",
  "appType": "vue-spa",
  "routeGuardStrategy": ["nav-neutralize", "L1-auth", "L2-patch", "forward"],
  "apisFromStatic": [],
  "apisFromRuntime": [],
  "apis": [],
  "params": [{ "method": "POST", "path": "/api/user/list", "transport": "json", "fields": [] }],
  "frontendRoutes": [],
  "routesVerifiedByClick": [],
  "featuresTriggered": [],
  "limitations": ""
}
```

See [reference.md](reference.md) for additional fields and grep recipes.

---

## General Notes

- **Framework-agnostic**: webpack/Vite/Angular lazy-loading methods are similar.
- **Transport**: REST/JSON, GraphQL, WebSocket, SSE; gRPC-web is out of scope.
- **SSR**: Client-side fetches can be recorded; RSC/Server Actions cannot be fully enumerated.
- **Blind spots**: JSVMP, WASM, strong HMAC/mTLS validation → use static analysis and document limitations.
- **Parameter blind spots**: Conditional dependencies, hidden params, WASM request building → mark as "not yet triggered" / "unreachable".
- **Static analysis is a safety net**: endpoints can still be enumerated statically when runtime analysis is blocked.

---

## Additional Resources

- Grep recipes, `config.json` template, troubleshooting, Hooks, parameter analysis section J, and site_map template: **[reference.md](reference.md)**
- See the [Scripts and Gates](#scripts-and-gates) table for reference script paths.
