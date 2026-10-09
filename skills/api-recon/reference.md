# api-recon — Reference Guide

Grep recipes, `config.json` templates, and troubleshooting. Run all grep commands against the `js/` directory. If a bundle is minified to one line, first try `js-beautify` or `sed 's/}/}\n/g'`; raw grep with a context window is usually sufficient.

## Script Notes

All files in `scripts/` are **reference templates** and must be adapted to the target site before running. Common adjustments include:

| Script | Common adjustments |
|---|---|
| `harvest_static.py` | Endpoint regex, webpack/Vite manifest parsing, microfrontend `publicPath`, retries/concurrency |
| `runtime_harvest.js` | neutralize field names and success values, stub match rules/body structure, route source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether to enable L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | Destructive links to exclude with `--exclude`, cookie, depth/max, same-origin filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, hierarchy heuristics for `ROOTS`/`PREFIX_PARENT`, outer stub field names |
| `config.json` | Central entry point for all site-specific parameters above |

Put adapted files in the task working directory (e.g. `recon/`) and document in the report how they differ from the reference scripts.

---

## A. Reverse-Engineering the Three Gates

### A1. Render Gate — "How Does the App Determine the User Is Logged In?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Trace `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` and determine the **storage key**, **container** (Cookie vs. localStorage), and **encoding**:

| Encoding | How to forge it in config |
|---|---|
| Plain string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | Unsigned / `alg:none` JWTs, or signing with a key found in the bundle |
| Encrypted (SM2/AES/RSA) | Look for hard-coded keys; if the render gate only needs a decodable blob, it may be forgeable; otherwise fall back to static analysis |

→ Set in `cookies` / `localStorage`.

### A2. Interceptor Gate — "What Triggers a Redirect to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine the **field name**, **success value** (usually `0` or `200`), and **failure values that trigger redirects**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ Set `neutralize.fields` + `neutralize.success`.

### A3. Content Gate — "Where Do Menus/Permissions Come From?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two data layers** (common in enterprise dashboards):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | Route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | Sidebar menu rendering |
| `userRouteAuth` in bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` in bundle | `{ KEY: { name, link } }` | Alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm how `getResultTree(tree, permissions)` filters and which fields `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): build a permissive payload → `stubs`.

**Full permission-tree reconstruction** (large sites where the sidebar/submodules are still blank): see **section I**.

---

## B. `config.json` Template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",

  "cookies": [
    { "name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"func\":{},\"permissions\":[\"*\"]}" }
  ],
  "localStorage": { "token": "faketoken", "isLogin": "1" },

  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": { "success": true, "message": "ok" }
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",

  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {
    "storageReads": false,
    "cookieReads": false,
    "xhrHeaders": true
  },
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0, "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            { "name": "dashboard", "path": "/dashboard", "show": true, "children": [] },
            { "name": "alert", "path": "/alert", "show": true, "children": [] }
          ]
        }
      }
    }
  ],

  "explore": {
    "clickTabs": true,
    "clickTables": true,
    "pushStateFallback": true,
    "maxMenuItems": 50
  },

  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500, "perRouteMs": 900, "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",

  "captureResponses": true, "recordWs": true, "respMax": 600
}
```

Field descriptions:
- `runtimeMode`：`depth`（Puppeteer）、`coverage`（browser MCP）、`both`
- `cookies[].value` prefixes: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal value
- `forward: true` forwards real requests and rewrites status fields; `false` uses fully offline stubs
- `mockTier`: layers enabled by preload in coverage mode, e.g. `L1+L2`, `L1+L2+L3`
- `routes` comes from `routes.txt`; after forging the menu, the harness automatically adds `<a href>` links
- `captureResponses` / `recordWs` work only in depth mode
- `waitUntil`: use `domcontentloaded` for large SPAs to avoid `networkidle2` hanging
- `routeTimeout`: per-route `page.goto` timeout (milliseconds)
- `proxy`: Puppeteer `--proxy-server`; `HTTP_PROXY` / `HTTPS_PROXY` can also be set

### B1. Dual-Stub Template (`role_permissions` + `permissions/all`)

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {
      "response_code": 0,
      "data": {
        "permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"],
        "role_type": "SUPER_ADMIN"
      }
    }
  },
  {
    "match": "permissions/all",
    "body": {
      "response_code": 0,
      "data": [
        {
          "code": "MONITOR",
          "position": 1,
          "children": [
            { "code": "MONITOR_ALERT", "position": 1, "children": [] }
          ]
        }
      ]
    }
  }
]
```

The outer field name (`response_code` / `code` / `data`) must match the A2 interceptor gate; `permissions` must include every leaf code in the tree.

---

## C. coverage Mode: preload Configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before injection through CDP:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [ /* same as config.json stubs */ ],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Verify that `window.__API_RECON_PRELOAD__ === true` and the pathname remains stable.

Export recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime Hook Capabilities

Built-in browser Hook capabilities and coverage in preload (coverage) and runtime_harvest (depth):

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | Record request URLs/methods | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Find Authorization and other headers | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | Confirm session key names | ⚠️ Optional `observe.storageReads/cookieReads` |
| Read Vue routes | Complete `frontendRoutes` | ✅ `__API_RECON_ROUTES__` (loaded routes) |
| Neutralize Vue route guards / block login redirects | Expose modules to trigger APIs | ✅ `neutralizeVueRouter` + native redirect neutralization |
| Read React routes | Add routes | ⚠️ Static analysis + clicks; no dedicated Hook |
| Block page navigation (login path) | Stay on page for analysis | ⚠️ Blocks only login paths to avoid interfering with business navigation |
| Hook crypto libraries (CryptoJS/SM, etc.) | Encrypted parameters → plaintext API body | ❌ Manually Hook encryption function arguments; record conclusions in config |
| Bypass anti-debugging | Otherwise runtime may not record APIs | ❌ Must be handled manually; static analysis remains available |

---

## E. Endpoint Extraction Regex (When Static Results Are Sparse)

Broaden the `extract_endpoints` regex in `harvest_static.py`, or extract manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → Resolution |
|---|---|
| Few static APIs | Endpoint dialect mismatch → broaden the regex (section E) |
| Chunk count ≪ manifest | CSS-only or undeployed chunks; 404s have been retried |
| Runtime still shows login page | Render gate is wrong → recheck A1: key, container, encoding, domain |
| App shell loads but module is blank | Content gate → forge the menu (A3); `routes` path may be wrong |
| Only bootstrap/locale calls on each route | Permission codes are incomplete → reconstruct the permission tree (section I); check both `role_permissions` + `permissions/all` stubs |
| Sidebar has items but subpages are blank | Tree is missing intermediate nodes or codes do not match `userRouteAuth` |
| Every API redirects to login | Interceptor gate → verify `neutralize`; extend walk logic for nested fields |
| Zero WS frames | Subscription may require user interaction; increase `perRouteMs` |
| Empty response body | Real responses are available only with `forward: true` |
| Chromium missing | Install chromium or set `config.chromium` / `CHROMIUM` |
| Many mocks but still redirected to login | Hook ran too late or lacks a `location.href` setter → inject preload at document-start |
| All lists are empty | Empty L3 arrays are normal; continue by clicking tabs/settings/details |
| Redux action mistaken for a route | Filter internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload was not injected at document-start → change injection timing; or manually clear guards when `neutralizeVueRouter: false` |
| URL appears in response but not in log | Enable `extractUrlsFromResponse` or extract manually from `__API_RECON_DETAIL__` |
| Authorization header name unknown | Enable `observe.xhrHeaders` or inspect request headers in DevTools |
| Runtime is very slow / times out | Set `waitUntil: domcontentloaded`, lower `routeTimeout`, and avoid `networkidle2` |
| Proxy connection fails | Check `proxy` / environment variables; ensure Puppeteer and curl use the same proxy port |

---

## G. Hardened Targets

When the server progressively validates sessions (e.g. unforgeable signed cookies or server-rendered menus that cannot be stubbed), runtime analysis may stall at the shell. Expected behavior:

- **Static analysis is sufficient for endpoint enumeration** — module paths are in the code.
- If authorized, run the same harness with a **real session**: `forward: true`, no neutralization, and capture real methods/params/responses.

---

## H. Per-Task Checklist

1. Confirm authorization scope.
2. **Read** `scripts/harvest_static.py` → adapt to the target → run it → review `api_static.txt` and `routes.txt`.
3. **Phase 1b**: anchor-window expansion on paths + binding layer → `param_candidates.json` (section J).
4. Reverse-engineer A1/A2/A3 → write a site-specific `config.json`.
5. **Read and adapt** `runtime_harvest.js` / `preload.js` before running.
6. `runtimeMode=depth`: `npm install` → run the adapted harvest script.
7. `runtimeMode=coverage/both`: inject the adapted preload at document-start → dynamic enumeration with browser MCP + **parameter-trigger matrix**.
8. If modules do not render → reconstruct the permission tree (**section I**) → patch stubs → rerun.
9. Multi-sample parameter diff + error inference → `params_merged.json`.
10. Merge into `site_map.json` + `api_merged.txt`; accurately document coverage, gaps, and script changes.

---

## I. Permission-Tree Reconstruction (Phase 4 Deep Dive)

Use this when a simple forged `menus: [{ path, show: true }]` does not work and submodules still do not mount.

### I1. Locate the Auth Module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record the **permission API path**, **response field names**, and **consumer chunk filename**.

### I2. Extract `routeMap`

```bash
python3 scripts/extract_route_map.py recon/js recon/
# Output: recon/route_map.json
```

If `[!] no routeMap pattern found`: broaden the regex in `extract_route_map.py` or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build the Permission Tree + Stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases such as `He=o.DASHBOARD`).
2. Resolve aliases to real paths using `route_map.json`.
3. Infer parents from code prefixes (`MONITOR_ALERT` → `MONITOR`).
4. Output `permissions_tree.json`, `permissions_all_stub.json`, and `role_permissions_stub.json`.
5. With `--config`, automatically write `stubs` to `config.json` and extend `routes`.

**Adapt for the target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes without prefix-based relationships

### I4. Validate Stub Consistency

```bash
# The number of permissions should be approximately equal to the number of userRouteAuth entries.
wc -l recon/perm_codes_all.txt
# routes should cover every link in route_map.
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun Runtime and Compare

```bash
node recon/runtime_harvest.js recon/config.json
# Compare runtime_api.json entry counts before/after forging; check whether module APIs such as /attack and /asset appear.
```

| Before forging | After forging (success) |
|---|---|
| Same 3–5 bootstrap calls on every route | Different routes trigger different module APIs |
| Only `/api/locale/language` | Module endpoints such as `/api/web/...` appear |
| Only a handful of routes in `routes.txt` | `routes` contains 80–110+ routes from route_map |

### I6. If It Still Fails

- **coverage mode**: click the sidebar + tabs; permission gating may not be requested until interaction.
- **Stub fields**: compare nesting in the real API (curl + real session) with the stub.
- **Additional guards**: grep for button-level checks such as `hasPermission|checkRole|func.` and extend `role_permissions.permissions`.
- **Static fallback**: module API paths remain in `api_static.txt`; runtime only adds METHOD/body. Retain parameters from `param_candidates.json` + recorded samples.

---

## J. Parameter Analysis (Phases 1b / 5b / 5c)

**Methodology, not a generic script.** Find paths with regex; find parameters using anchor-window expansion + UI binding chains + multi-sample diffs + error inference.

### J1. Anchor-Window Expansion — Find Request-Building Objects from Paths

```bash
# Use paths found in Phase 1 as anchors.
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrappers and Transport Types

```bash
# axios / shared request wrapper
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# Path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation Gate — Required Fields / Format / Enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding Layer — Form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Runtime follow-up: in DevTools → Network → request → **Initiator** (call stack), trace upward from `fetch`/`send` to the request-building function.

### J5. Encrypted Parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Do not guess fields from ciphertext** — Hook the encryption function's **arguments** and record the plaintext payload before encryption; put findings in `config.json` / `param_candidates.json`.

### J6. Parameter-Trigger Matrix (Required in Phase 3)

Record each operation type once for every module and diff request bodies/queries:

| Operation | Inspect |
|---|---|
| Initial list load | Default pagination values |
| Search | keyword, filters |
| Advanced filters | Optional fields |
| Create/edit | Full entity |
| Bulk/export | `ids[]`, `exportType` |
| Sort/page | `sortField`, `order` |

Output `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence Levels

| Confidence | Criteria |
|---|---|
| **High** | Static callsite + at least 2 consistent runtime samples |
| **Medium** | Static analysis only, or only 1 runtime sample |
| **Low** | Inferred from response/error without secondary verification |
| **Not yet triggered** | Field known statically, but UI/permissions have not reached it |

### J8. Quick Recipes by Scenario

| Scenario | Sequence |
|---|---|
| REST list page | J1 request-building object → J6 four diffs → J3 rules |
| Create/edit form | J3 Form name → J4 submit chain → submit at runtime + intentionally leave blank to inspect 400 |
| GraphQL | J2 variables declarations → record variables for each runtime operation |
| Encrypted body | J5 Hook arguments → fields before encryption are the actual params |

### J9. Mapping to api-recon Phases

| api-recon | Parameter reconnaissance |
|---|---|
| Phase 1 static | J1 anchor-window expansion |
| Phase 2 A2 interceptor | Globally injected fields (`tenantId`, `sign`) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | Different forms per module → all fields trigger only with sufficient permissions |
| Phase 5 merge | `params_merged.json` + confidence; do not infer required fields from a single sample |

### J10. Troubleshooting

| Symptom | Resolution |
|---|---|
| Field appears statically but never at runtime | Mark as "not yet triggered"; complete the permission tree / click advanced filters / try each linked select option |
| Same path has different body shapes | Normal — record separate entries by `action`; do not force them into one schema |
| Stub response is fake but params are needed | **Inspect outbound request** body/headers; do not infer from the stub response |
| 400 reports a nested field | Check outer wrappers such as `data`/`bizData`/`variables` |
| GraphQL shows only operation names | Expand `variables` JSON; search statically for `$var: Type` |

---
