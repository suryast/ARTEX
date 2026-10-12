# api-recon — Reference Manual

Grep recipes, the `config.json` template, and troubleshooting. All greps are run against the `js/` directory. When a bundle is a single line, run `js-beautify` or `sed 's/}/}\n/g'` first; usually a raw grep with a context window is enough.

## Script notes

All files in `scripts/` are **reference templates** and must be adjusted for the target site before running. Typical adjustment points:

| Script | Common items to adjust |
|---|---|
| `harvest_static.py` | endpoint regex, webpack/Vite manifest parsing, micro-frontend publicPath, retry/concurrency |
| `runtime_harvest.js` | neutralize field names and success values, stub match rules and body structure, routes source, WS recording, `waitUntil`/`routeTimeout`/`proxy` |
| `preload.js` | `loginPathRe`, L1 stubs, `neutralize.fields`, `apiPattern`, whether to enable L3, `recordDetail`, `observe.*`, `neutralizeVueRouter` |
| `spider_mpa.py` | `--exclude` destructive links, cookie, depth/max, same-origin filtering |
| `extract_route_map.py` | `routeMap` / `routeLink` regex, KEY naming pattern |
| `build_perm_tree.py` | `userRouteAuth` parsing, `ROOTS`/`PREFIX_PARENT` hierarchy heuristics, stub outer field names |
| `config.json` | the single entry point for all the site-specific parameters above |

Keep the adjusted files in the task working directory (e.g. `recon/`), and note in the report the specific changes made relative to the reference scripts.

---

## A. Reversing the three gates

### A1. Render gate — "How is logged-in state determined?"

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Trace the chain `isLogin = f(getUser())` → `getUser = decode(storage.read(KEY))` to determine the **storage key**, the **container** (Cookie vs localStorage), and the **encoding**:

| Encoding | How to forge in config |
|---|---|
| plaintext string / `"1"` / token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | an unsigned/`alg:none` JWT, or sign with a key found in the bundle |
| encrypted (SM2/AES/RSA) | find a hardcoded key; forge when the render gate only needs a decodable blob; otherwise fall back to static |

→ Write into `cookies` / `localStorage`.

### A2. Interceptor gate — "What triggers a redirect to /login?"

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(未登录|请重新登录|登录已过期|unauthorized|登录失效|授权|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Determine: the **field name**, the **success value** (usually `0` or `200`), and the **failure value that triggers the redirect**. Verify with a junk session:

```bash
curl -sk -X POST -H 'Cookie: <fakekey>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

→ Write into `neutralize.fields` + `neutralize.success`.

### A3. Content gate — "Where do the menu/permissions come from?"

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

**Two layers of data** (common in enterprise back offices):

| API | Typical payload | Consumer |
|---|---|---|
| `.../role_permissions` | `{ permissions: string[], role_type }` | route guards, button-level ACL |
| `.../permissions/all` | `tree[{ code, position, children }]` | sidebar menu rendering |
| `userRouteAuth` in the bundle | `{ CODE: { url, name? } }` | code → frontend path |
| `routeMap` in the bundle | `{ KEY: { name, link } }` | alias resolution (webpack `o.DASHBOARD`) |

Read the consumer code to confirm: how `getResultTree(tree, permissions)` filters, and which field `v-if` / `hasAuth(code)` checks.

**Manual forge** (small sites): build a permissive payload → `stubs`.

**Full permission-tree reconstruction** (large sites, where the sidebar/submodules are still blank): see **section I**.

---

## B. config.json template

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

Field notes:
- `runtimeMode`: `depth` (Puppeteer), `coverage` (browser MCP), `both`
- `cookies[].value` prefix: `b64json:` → base64(JSON); `json:` → raw JSON; no prefix → literal
- `forward: true` forwards the real request and rewrites the code field; `false` is a fully offline stub
- `mockTier`: which tiers the coverage-mode preload enables, e.g. `L1+L2`, `L1+L2+L3`
- `routes` comes from `routes.txt`; after forging the menu, the harness auto-appends `<a href>`
- `captureResponses` / `recordWs` take effect in depth mode only
- `waitUntil`: use `domcontentloaded` for large SPAs to avoid `networkidle2` hanging
- `routeTimeout`: per-route `page.goto` timeout (milliseconds)
- `proxy`: Puppeteer `--proxy-server`; can also set `HTTP_PROXY` / `HTTPS_PROXY`

### B1. Dual-stub template (role_permissions + permissions/all)

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

The outer field names (`response_code` / `code` / `data`) must match the A2 interceptor gate; `permissions` must cover every leaf code in the tree.

---

## C. coverage mode: preload configuration

Edit the `CONFIG` object at the top of `scripts/preload.js`, or replace it before injecting via CDP:

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

Verify: `window.__API_RECON_PRELOAD__ === true` and the pathname is stable.

Export the recorded results:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

---

## D. preload / runtime hook capabilities

The built-in browser hook capabilities and coverage of preload (coverage) and runtime_harvest (depth):

| Hook capability | Value for API discovery | Coverage |
|---|---|---|
| Hook fetch / XHR.open | record request URL/method | ✅ `recordDetail` + `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | discover headers like Authorization | ✅ `observe.xhrHeaders` |
| Hook localStorage/cookie reads | confirm the session key name | ⚠️ optional `observe.storageReads/cookieReads` |
| Get Vue routes | complete frontendRoutes | ✅ `__API_RECON_ROUTES__` (already-loaded routes) |
| Neutralize Vue route guards / block login redirects | open up modules to trigger APIs | ✅ `neutralizeVueRouter` + native navigation neutralization |
| Get React routes | fill in routes | ⚠️ static + clicking; no dedicated hook |
| Block page navigation (login path) | stay on the page to analyze | ⚠️ only blocks the login path, to avoid blocking app navigation |
| Hook crypto libraries (CryptoJS/SM, etc.) | encrypted params → plaintext API body | ❌ must manually hook the crypto function's arguments; record the conclusion in config |
| Anti-debugging bypass | otherwise runtime records no APIs | ❌ must be handled manually; static still works |

---

## E. Endpoint extraction regex (when static results are too few)

Loosen `extract_endpoints` in `harvest_static.py`, or do it manually:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

---

## F. Troubleshooting

| Symptom | Cause → fix |
|---|---|
| very few static APIs | endpoint dialect doesn't match → loosen the regex (section D) |
| chunk count ≪ manifest | CSS-only or undeployed chunks; 404s already retried |
| runtime still shows the login page | wrong render gate → recheck A1: key name, container, encoding, domain |
| gets into the shell but modules are blank | content gate → forge the menu (A3); the `routes` path may be wrong |
| only bootstrap/locale per route | incomplete permission codes → section I permission-tree reconstruction; check the `role_permissions` + `permissions/all` dual stub |
| sidebar has items but subpages are blank | tree is missing intermediate nodes, or the code doesn't match `userRouteAuth` |
| every API redirects to login | interceptor gate → confirm `neutralize`; nested fields need an extended walk logic |
| 0 WS frames | subscription only happens after user interaction; increase `perRouteMs` |
| empty response body | real responses only exist with `forward: true` |
| Chromium missing | install chromium or set `config.chromium` / `CHROMIUM` |
| lots of mocks but still returns to login | hooked too late or missing the `location.href` setter → document-start + preload |
| lists all empty | an empty L3 array is normal; keep clicking Tabs/settings/details |
| mistaking a Redux action for a route | filter out internal paths containing get/set/change/clear/toggle/upload |
| Vue still redirects to login | preload isn't at document-start → change the injection timing; or clear guards manually when `neutralizeVueRouter: false` |
| URLs appear in a response but not in the log | enable `extractUrlsFromResponse`; or extract manually from `__API_RECON_DETAIL__` |
| don't know the Authorization header name | enable `observe.xhrHeaders`, or check the request headers in DevTools |
| runtime extremely slow / timing out | switch to `waitUntil: domcontentloaded`; lower `routeTimeout`; don't use `networkidle2` |
| proxy connection fails | check `proxy` / environment variables; Puppeteer and curl must use the same proxy port |

---

## G. Hardened targets

When the server validates the session step by step (unforgeable signed cookies, server-rendered menus that can't be stubbed), runtime gets stuck at the shell. Expected behavior:

- **Static is enough for endpoint enumeration** — module paths are in the code
- If authorized, run the same harness with a **real session**: `forward: true`, no neutralize needed, capturing real methods/params/responses

---

## H. Single-task checklist

1. Confirm the authorized scope
2. **Read** `scripts/harvest_static.py` → adjust for the target → run → review `api_static.txt`, `routes.txt`
3. **Phase 1b**: path anchor window expansion + binding layer → `param_candidates.json` (section J)
4. Reverse A1/A2/A3 → write a site-specific `config.json`
5. **Read and adjust** `runtime_harvest.js` / `preload.js` before running
6. `runtimeMode=depth`: `npm install` → run the adjusted harvest script
7. `runtimeMode=coverage/both`: inject the adjusted preload at document-start → browser MCP dynamic enumeration + **parameter trigger matrix**
8. modules don't render → **section I permission-tree reconstruction** → patch stubs → rerun
9. multi-sample parameter diff + error inference → `params_merged.json`
10. merge → `site_map.json` + `api_merged.txt`, honestly annotating coverage, gaps, and script changes

---

## I. Permission-tree reconstruction (Phase 4 deep dive)

Use this when forging a simple `menus: [{ path, show: true }]` doesn't work and submodules still won't mount.

### I1. Locate the auth module

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
```

Record: the **permission API path**, the **response field names**, and the **consumer chunk file name**.

### I2. Extract routeMap

```bash
python3 scripts/extract_route_map.py recon/js recon/
# produces recon/route_map.json
```

If `[!] no routeMap pattern found`: loosen the regex in `extract_route_map.py`, or grep manually:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

### I3. Build the permission tree + stub

```bash
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Script logic:
1. Parse `userRouteAuth={MONITOR:{url:...},...}` (including webpack aliases like `He=o.DASHBOARD`)
2. Use `route_map.json` to resolve alias → real path
3. Infer the parent from the code prefix (`MONITOR_ALERT` → `MONITOR`)
4. Output `permissions_tree.json`, `permissions_all_stub.json`, `role_permissions_stub.json`
5. With `--config`, automatically write `config.json`'s `stubs` and the extended `routes`

**Adjust for the target** (at the top of the script):
- `DEFAULT_ROOTS`: list of top-level module codes
- `DEFAULT_PREFIX_PARENT`: `PREFIX_` → parent mapping
- `DEFAULT_EXTRA_PARENT`: orphan nodes with non-prefix relationships

### I4. Verify stub consistency

```bash
# the permissions count should ≈ the number of userRouteAuth entries
wc -l recon/perm_codes_all.txt
# routes should cover every link in route_map
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
```

### I5. Rerun runtime and compare

```bash
node recon/runtime_harvest.js recon/config.json
# compare the runtime_api.json counts before/after forging; check whether module APIs like /attack, /asset appear
```

| Before forging | After forging (success) |
|---|---|
| the same 3–5 bootstrap calls per route | different routes trigger different module APIs |
| only `/api/locale/language` | `/api/web/...` module endpoints appear |
| single-digit routes in `routes.txt` | 80–110+ `routes` from route_map |

### I6. When it still fails

- **coverage mode**: click the sidebar + Tabs; permission gating may only make requests after interaction
- **stub fields**: compare the nesting of the real API (curl + a real session) against the stub
- **extra guards**: grep button-level checks like `hasPermission|checkRole|func.` and extend `role_permissions.permissions`
- **static fallback**: module API paths are still in `api_static.txt`, and runtime only fills in METHOD/body; keep parameters in `param_candidates.json` + the recorded samples

---

## J. Parameter reversing (Phase 1b / 5b / 5c)

**A methodology, not a general-purpose script.** Use regex to find paths; use anchor window expansion + the UI binding chain + multi-sample diff + error inference to find parameters.

### J1. Anchor window expansion — finding the payload-assembly object from a path

```bash
# anchor on a path already known from Phase 1
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head
```

### J2. Wrapper layer and transport shape

```bash
# axios / unified request
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head

# GraphQL
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head

# FormData / multipart
grep -rhoaE 'FormData|\.append\(' js | head

# path parameters
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"' js | head
grep -rhoaE 'useParams|route\.params|\$route\.params' js | head
```

### J3. Validation gate — required / format / enum

```bash
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'rules\s*:\s*\[|name:\s*["\'][a-zA-Z_]+["\']' js | head
grep -rhoaE 'label.*value|options\s*:\s*\[' js | head
```

### J4. Binding layer — form → API

```bash
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
```

Runtime complement: DevTools → Network → request → **Initiator** (call stack), tracing up from `fetch`/`send` to the payload-assembly function.

### J5. Encrypted parameters

```bash
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

**Don't guess fields on ciphertext** — hook the crypto function's **arguments** and record the plaintext payload before encryption; write the conclusion into `config.json` / `param_candidates.json`.

### J6. Parameter trigger matrix (required in Phase 3)

For each module, record once per operation and diff the request body/query:

| Operation | What to watch |
|---|---|
| list first screen | pagination defaults |
| search | keyword, filters |
| advanced filter | optional fields |
| create/edit | the complete entity |
| bulk/export | `ids[]`, `exportType` |
| sort/paginate | `sortField`, `order` |

Produces `param_samples.json`: `[{ "path", "method", "action": "search", "body", "query", "headers" }]`

### J7. Confidence rules

| Confidence | Condition |
|---|---|
| **high** | static callsite + ≥2 consistent runtime samples |
| **medium** | static only, or just 1 runtime sample |
| **low** | inferred from response/error, not re-verified |
| **pending trigger** | fields known statically, but the UI/permission path wasn't reached |

### J8. Scenario quick reference

| Scenario | Order |
|---|---|
| REST list page | J1 payload-assembly object → J6 four diffs → J3 rules |
| create/edit form | J3 Form name → J4 submit chain → runtime submit + intentionally leave blank to see 400 |
| GraphQL | J2 variables declaration → runtime records variables per operation |
| encrypted body | J5 hook the arguments → the pre-encryption fields are the real params |

### J9. Mapping to api-recon phases

| api-recon | parameter recon |
|---|---|
| Phase 1 static | J1 anchor window expansion |
| Phase 2 A2 interceptor | globally injected fields (tenantId, sign) |
| Phase 3 runtime | J6 trigger matrix + `param_samples.json` |
| Phase 4 permission tree | different modules have different forms → full fields only trigger with enough permissions |
| Phase 5 merge | `params_merged.json` + confidence; don't decide "required" from a single sample |

### J10. Troubleshooting

| Symptom | Fix |
|---|---|
| field name is in static but never appears at runtime | mark it "pending trigger"; reconstruct the permission tree / click advanced filters / cycle each linked select option |
| same path, different body shapes | normal — record separately by `action`, don't force-merge the schema |
| stub response is fake but you want the params | **look at the outbound request** body/headers, don't infer from the stub response |
| 400 reports a nested field | watch for outer wrappers `data`/`bizData`/`variables` |
| GraphQL only shows the operation name | expand the `variables` JSON; find `$var: Type` statically |

---
