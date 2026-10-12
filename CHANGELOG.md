# Changelog

Notable changes to this project are recorded in this file, following the format of [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/).

## [Unreleased]

### Interception

#### Added

- **Added a built-in interception rule for "delete-type endpoint paths"**: the previous built-in destructive HTTP rules only recognized the DELETE **method** (`curl -X DELETE`, `requests.delete(`, `method:'DELETE'`), and the path-based rule's word list contained only `/clear /wipe /flush /purge /truncate /drop /destroy /factory-reset /reset-all` — yet most applications' delete endpoints can be triggered with GET/POST, so calls like `curl 'http://t/api/user/delete?id=1'` matched no built-in rule and really deleted the target data. A `deny` rule has now been added covering `/delete /del /remove /unlink /erase /destroy` (allowing suffix forms like `/deleteAll`, `/delete_user`, `/delete-user`); a separator must follow the verb, so `/delivery`, `/details`, `/delta`, `/delegate` are not falsely blocked. The rule uses an independent seed flag, so **existing instances also receive it after upgrading**; like the other built-in rules, it can be disabled or deleted under "System → Command Interception".
### Vulnerability push

#### Added

- **Vulnerability findings can be pushed to IM**: added a "System → Notification Push" page that can push scan findings to six channel types — **DingTalk, Feishu, WeCom, generic Webhook, Telegram, email**. Any number of bot instances can be configured per type (e.g. one DingTalk bot each for an "incident response group" and a "daily broadcast group"), and each instance has independent enable/disable, rate-limit, and filter rules.
- **Push timing has two modes, real-time and digest**: in real-time mode, matches are sent one by one as they occur; in digest mode, a batch of vulnerabilities is merged into one message on a global cycle (default 30 minutes), leading with "M new vulnerabilities in the last N minutes" and a severity breakdown, so you can tell at a glance whether immediate action is needed. To do "high-severity real-time, the rest digested", create two channels configured separately — the policy isn't hardcoded.
- **Filter rules support four dimensions**: minimum severity, limiting to a task/asset scope, vulnerability-type keyword inclusion and exclusion (exclusion takes priority), and whether to receive disposition status changes (off by default, since what most people mean by "push" is finding new vulnerabilities, not a status change log). Each message carries a "View details" button linking to the vulnerability detail; the target address is set by the global "callback link" configuration, and leaving it empty omits the button.
- **Delivery history and manual resend**: the notification page lists each delivery's status, attempt count, failure reason, and owning channel, filterable by channel and status; failed items can be resent with one click (resending resets the retry count to zero, since a manual resend means the failure cause has been dealt with).
- **Channel credential masked display**: the Webhook address, signing secret, Bot Token, SMTP password, etc. are declared by each channel itself (`SecretKeys()`), and the API only returns masked values with a trailing-digit hint; submitting them back unchanged means "do not modify", while clearing the input field deletes that field.

#### Fixed

> This section records the conclusions of a security audit conducted after the first version of the feature was completed. Each item was first reproduced, then fixed, then backed by a regression test.

- **Fixed a mask bypass via "change the target address, keep the credentials" (critical)**: the mask mechanism's purpose is "credentials are not echoed back to the browser", but the target address and the credentials are two separate sets of fields, and config merging retains the stored value for any "unmentioned key". So **changing only the address and saying nothing about the credentials** would make the server send the real stored credentials to an arbitrary address — the generic Webhook's `Authorization` header, Telegram's Bot Token (which goes into the request path), and the email channel's SMTP password (handed to the peer after STARTTLS) would all leak, completely silently and without relying on a redirect. This path was verified to work and reproduced on all four channels one by one. The rule now is: **whenever the target address changes, the caller must explicitly state every credential field** (give a new value, or explicitly leave it empty to mean "no longer needed") — echoing the masked value back unchanged means "keep the old credential", which is exactly the attack shape and is likewise rejected. Deliberately not doing "automatically drop the credentials", because for optional credential fields (like `headers`) that would become "auth silently disappears but the API returns success", which is harder to diagnose than an error.
- **Fixed zero escaping of untrusted content in the markdown-based channels**: the three channels DingTalk, WeCom, and Feishu previously did no escaping at all (Telegram and email both did). Vulnerability titles and summaries come from model output (the model reads the responses of the target under test), and an asset's `url` is a full URL harvested by scanning that contains a target-controlled query string. A vulnerability titled `Login SQL injection\n[Urgent: click here to verify your account](http://attacker.tld)` would render as a **clickable external link** in a security engineer's DingTalk/Feishu; `![](http://attacker.tld/beacon)` would be fetched by the client at render time — equivalent to reporting "this vulnerability has been viewed" and leaking the reader's IP. Now a single-lining + markdown metacharacter escaping is done uniformly. A related design error was also fixed: escaping must happen at each channel's own render output, not in the title function shared across the four contexts (markdown escaping leaking into Telegram's HTML would leave visible backslashes).
- **Fixed the SSRF surface of the delivery address**: previously only the scheme and host were validated, so `169.254.169.254` (cloud metadata, from which instance credentials can be read), loopback addresses, and intranet addresses were all deliverable; and on delivery failure the first 200 bytes of the response body went into `last_error` and were echoed by the delivery-history API, forming a semi-blind read primitive (able to read the first 200 bytes of any intranet HTTP endpoint's response). Protection is now applied at the **dial stage** (not just validation when saving the config) — that is the actual point of effect, and it also covers DNS rebinding and same-host redirects, and rejects cross-host redirects (these providers' credentials are right in the URL, so following a redirect means handing them to the redirect target). Loopback and link-local addresses require explicitly setting `ARTEX_NOTIFY_ALLOW_LOCAL=1` to be allowed (a local SMTP relay is a legitimate configuration and can't be blanket-blocked); **RFC1918 private networks are deliberately allowed**, since self-hosted intranet Mattermost / SMTP relays are very common, and hardening to that level would break legitimate deployments too.
- **Fixed push credentials leaking via error messages**: on delivery failure, the `*url.Error` returned by `http.Client.Do` puts the **full URL** into the error text, and DingTalk's `access_token`, WeCom's `key`, Feishu's hook id, and Telegram's `/bot<token>/` are all **right in the URL**. That string therefore flowed into four outlets: plaintext storage in `notification_deliveries.last_error`, the delivery-history API's verbatim echo (bypassing the channel config's masking), the server log, and the error message the test-send API returns to the frontend. It is now redacted uniformly — the error message keeps only `scheme://host` and the underlying cause (enough to pin down DNS / connectivity / certificate issues), discarding the path and query. The error text of address validation itself (`url.Parse` failure) also carries the full address and gets the same treatment; **the previous round only fixed the former and missed the latter, and the regression cases back then all went through the scheme branch and never covered the parse-failure path, so it was a false guarantee** — a case that genuinely covers that branch has now been added.
- **Fixed silent loss caused by marking a whole batch delivered after a digest message is truncated**: channels all have length limits (WeCom's 4096 bytes is the tightest), and when a batch doesn't fit the message is truncated, yet on send success the whole batch of deliveries was marked delivered — the truncated ones were **neither in the message nor in the failure list**, the delivery history still showed success, and the vulnerabilities simply vanished. It now packs **by whole item**: only those that fit into this message are marked delivered, and the rest go back to the queue to continue in the next message, with the message header honestly stating "this message shows the first N items; the remaining M will continue in the next message". Deferred items **do not consume a retry attempt** (the optimistic +1 at claim time is subtracted back), otherwise a backlog of 500 would judge the tail items as failed by the third segment — even though they never errored at all.
- **Fixed a typo in the minimum-severity threshold silently disabling the filter**: when `min_severity` is a typo like `hgih`, the unknown level is 0 in the ordinal table, and the check degenerates into the always-true `rank >= 0` — the user thinks they limited it to "high-severity only" but actually floods the group with every vulnerability, and it's completely indistinguishable from a correct config in the UI. The value is now validated on the write path, with the error message listing the allowed values (the read path stays lenient: a bad value already in the database won't make the whole channel unreadable).
- **Fixed `rate_per_min = 0` (no rate limit) being unreachable**: the docs, the UI hint, and the token bucket all interpret 0 as "no rate limit", but the database-write layer alone had `if RatePerMin <= 0 { use default }`, silently turning an explicit 0 into 20 (DingTalk/WeCom/Telegram) or 100 (Feishu) — the operator thinks they removed the rate limit but is actually throttled with no indication. The distinction between "unspecified" and "explicit 0" can only be expressed by the request body, so the default is now filled at the API layer when the field is absent.
- **Fixed digest channels completely bypassing the token bucket**: `allow` was decremented by `takeTokens` but never used, and `rate_per_min` had no effect on digest mode. The claimed count is now constrained by both "this cycle's quota" and the in-memory upper bound.
- **Fixed failure handling judging by the batch's maximum attempt count, letting old deliveries drag down new ones**: the attempt counts within a batch differ, and one old delivery that has already retried twice would drag brand-new deliveries in the same batch into failed — a new vulnerability is lost permanently without using a single retry, the exact opposite of the intent to "not let old rows drag new rows under". It now decides per item: permanent failures are judged dead immediately, those that have exhausted their own retries are judged dead, and the rest are rescheduled by their own backoff tier.
- **Fixed deliveries with an unparseable snapshot in a digest batch being silently marked successful**: such items are skipped at the render stage (so they don't take down the whole batch), but then the whole-batch "mark delivered" counted them as successful too. They are now explicitly judged failed with a reason that can be found in the delivery history.
- **Fixed a single channel's per-cycle delivery count potentially exceeding the lease duration**: the lease is 3 minutes, and if a cycle's serially deliverable count is large enough that the worst-case time exceeds the lease, then in a multi-instance deployment the peer re-claims the lease-expired rows, sends them again, and double-increments the attempt count. The per-cycle cap is now derived backward from "lease / per-call timeout", with an assertion that pins down the relationship among these three constants (writing that assertion immediately revealed the original value of 6 used up the lease exactly with zero margin, now adjusted to 5).
- **Fixed Telegram truncation potentially cutting an HTML entity in half**: truncation only avoided a half-cut tag, not a cut-off entity fragment like `&amp`, and the parser might therefore reject the **entire** message — overly long digest messages are common to begin with, so the cost is too high. It now avoids both unclosed tags and entity fragments.
- **Fixed the email channel judging a transient SMTP failure as permanent**: an SMTP 4xx (like a greylisting `450`) is a temporary rejection, and the correct practice is to retry later, but previously it was always judged a permanent failure — a mail server with greylisting enabled would make **every** push fall into failure after the first attempt, which is exactly the scenario where automatic retry should help most. It now distinguishes by the first digit of the reply code: 4xx is retryable, 5xx is permanent failure, and when no code can be obtained it's treated as retryable.
- **Fixed a nested structure submission actually writing the mask literal into the database**: an object field like `webhook.headers` can only be masked or submitted as a whole; stuffing the mask sentinel inside the object can neither express "keep unchanged" nor be stored as a real value, causing subsequent auth to silently fail with no error at all. Such a submission is now explicitly rejected.

#### Design notes

- **The vulnerability-write transaction does just one blind INSERT**: `notification_events` is written by `RecordFindingTx` within the **same transaction**, so committing guarantees atomic consistency between "vulnerability stored" and "push task exists". This INSERT deliberately does not read the channel table or run the user's filter rules — otherwise one misconfigured filter condition could pollute or even abort the transaction and prevent a high-severity vulnerability from being stored. To isolate failure to this one statement (in PostgreSQL, an error in any statement within a transaction voids the whole transaction, and even `COMMIT` fails), it is wrapped in a `SAVEPOINT`, and on failure it only logs and does not affect the vulnerability write.
- **Delivery uses lease-based claiming rather than a long transaction**: after claiming with `FOR UPDATE SKIP LOCKED`, the row is set to `sending` and `next_attempt_at` is pushed into the future as a lease; the transaction is committed before the network delivery, so no database lock is held during delivery; a `sending` row left by a process crash is re-claimed by the next cycle after the lease expires, which self-heals and does not cause infinite retries.
- **Rate limiting does not consume the retry budget**: the engine first computes how many it can still send this cycle by the channel's token bucket, then claims that many. Reversing the order (claim first, discard later) would make a rate-limited delivery waste one attempt, and after the three-attempt budget is burned on pure waiting it falls into failure. Exceeding the limit doesn't lose messages, it only defers delivery to the next tick.

### Test infrastructure

#### Fixed

- **Fixed permanent asset residue caused by the cleanup order in the company ICP attribution test case**: that case registered its cleanup in `t.Cleanup`, but closed the connection with `defer d.Close()` — `defer` runs first when the function returns and `t.Cleanup` runs after, so all cleanup statements ran against an **already-closed connection**, the error was discarded by `_, _ =`, and the test assets and company stayed in the database permanently. It used `MAX(companies.id)+1` as a fake TaskID to tag assets, and once that number collided with another case's task id, the case that asserts on "exactly N assets" would fail inexplicably and be extremely hard to locate. Closing the connection now also goes through `t.Cleanup` and is registered first (LIFO guarantees cleanup runs first), and cleanup failures are now surfaced rather than swallowed. The same pattern (`defer d.Close()` + writing to the database in `t.Cleanup`) appears in a dozen-odd other places in the `db` package; only the one empirically triggered was fixed this time.

### Traffic

#### Added

- **Added "Clear all" to the traffic list**: deletes all traffic records at once, ignoring the current filter, and additionally cleans up historical host directories the index no longer records, leaving no residue. After clearing, it does a full compaction (`optimize` + `VACUUM` + `wal_checkpoint(TRUNCATE)`), returning the disk space the index occupied to the system and reporting how much was actually freed afterward. Traffic evidence already bound to vulnerabilities is kept in a separate evidence database and is unaffected. `VACUUM` on an empty database costs almost nothing, so this is also **a way for an existing instance to convert its index database to incremental reclaim mode** — after clearing once, routine per-host deletion will reclaim space on its own.

#### Fixed

- **Fixed disk space not being released after deleting traffic**: deletion only made the data invisible, and the space stayed in the index file. SQLite row deletion only puts pages on the freelist, and the index database didn't enable `auto_vacuum` at creation, so the file never shrinks; meanwhile `ex_fts` is a `contentless_delete` full-text index where `DELETE` only writes a tombstone without reclaiming the original postings, accumulating permanently unless merged — so deleting traffic actually grew the index. Since bodies under 256KB are all inlined in this database, and the trigram index is about 2x the body size, an instance with a large capture volume would occupy several times the disk for long-deleted traffic (measured: 6MB of captured body → 16MB index, still 16MB after deleting everything). Now a newly created index database enables `auto_vacuum=incremental` directly, and after each delete commits, it runs "full-text index incremental merge + `incremental_vacuum` + `wal_checkpoint(TRUNCATE)`" in the background in chunks, gradually returning space to the OS (the same scenario drops to 104KB after deletion). Reclamation proceeds in chunks and releases the write lock between chunks, not blocking traffic recording; it yields immediately on process exit and continues the remaining work on the next delete.

> Upgrade note: `auto_vacuum` can only be set at database creation, so **an existing instance's index database is still in the old mode**, where `incremental_vacuum` is a no-op — a line is printed at startup to note this. For such databases, after upgrading the continuous growth of tombstones has already stopped (full-text index merging runs as usual), and the already-occupied volume can be reclaimed once with the traffic list's "Clear all" — that step converts the database to incremental reclaim mode, after which routine deletion takes effect on its own.

### LLM

#### Fixed

- **Fixed the custom session header not taking effect on the "one-off LLM call" path**: the session header's value is taken from the session id on the request context, which agentcore writes to the context only when a transcript store is mounted. Goal decomposition (round 0) and cold-node compaction (the §4 body call) are both one-off calls without a store mounted, so there is no session id on the context and the gateway side can't read the header — for an endpoint like opencode zen that "returns 400 MissingSessionID when `x-opencode-session` is absent", this manifested as "round-0 goal decomposition fails with 400 while subsequent planner rounds work perfectly", and the compaction failure left only a one-line log that was extremely hard to correlate. These two paths now explicitly attach an exploration-stable session id (`exp<N>-goals` / `exp<N>-compactor`): the header is carried normally, and llmrec can correctly attribute these two paths' token usage to the corresponding exploration (previously it wasn't recorded at all).

### Discovery

#### Fixed

- **Fixed the "By asset" view's asset list having no scrollbar when it overflows**: the left-hand asset tree was already wrapped in a scroll container, but the outer card only set `max-height` without a definite height, and the scroll viewport couldn't resolve a height from `height:100%` (in CSS, when only `max-height` is set and `height` is still `auto`, a percentage height doesn't take effect), so with many assets the list either burst the card or was cut off and couldn't scroll. The height cap is now applied directly to the asset tree's native scroll container (`overflow-y-auto` + `max-h`, adapting to the window height), so with few assets the card shrinks to its content and with many assets it caps and shows a scrollbar.

## [0.3.14] - 2026-09-24

### Task list

#### Added

- **Added a "Vulnerabilities" column to the task list**: shows the counts of Critical / High / Medium / Low by severity tier, with non-zero tiers colored by severity, so you can see each task's vulnerability volume and distribution at a glance.

#### Changed

- **Narrowed the width of the "Description / Goal" columns**: overly long content is ellipsized and the full text can be viewed on hover, reducing the horizontal space long text takes up in the list.

### Exploration graph and planning situation overview

#### Changed

- **Streamlined the planning situation overview (graph_overview) and set count caps on each list**: recent facts, ended intents, pending intents, cold-region digests, and confirmed-vulnerability details were all changed to "latest window + count fallback", with the unshown items queryable on demand; this significantly reduces the per-round LLM context size and avoids context bloat in long tasks (related-task overview cold regions are rate-limited too).

#### Fixed

- **Eliminated the reverse duplicate edge between digest and finding in the exploration graph after collapsing cold nodes**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.13] - 2026-09-19

### Asset interception

#### Added

- **Added global asset interception rules (blacklist)**: supports entering exact-match and fuzzy-match domains / IPs / URLs and CIDR network segments, with create/read/update/delete and enable/disable for rules; the management page is under "System → Asset Interception"; by default it fuzzily intercepts government (`.gov` / `.gov.cn`) and education (`.edu` / `.edu.cn`) sites.
- **Asset interception wired into the execution chain**: before the Agent dispatches an intent (`add_intent`) or inserts assets (`insert_assets`), it first runs an interception check on the target assets — an intercepted intent is not dispatched, an intercepted asset is not inserted, and the asset info and interception reason are returned to the Agent.
- **Added task-level interception / allow (whitelist) rules**: independent of the global rules and effective only for the current task, with the evaluation order "intercept first, then allow" — a match on interception forbids it; if interception doesn't match but the task has allow rules configured and none of them match, then it's "not allowed to test"; when no allow rules are configured, the whitelist is not enabled. These can be entered when creating a task, or created/edited/deleted and enabled/disabled in the task detail "Overview".
- **Task templates support preset categories and task-level interception/allow rules**: a template can save the task category and a set of task-level rules, which are carried into the new-task form when the template is applied.

### Operation review

#### Fixed

- **Tightened the model-judge output protocol to reduce false allows caused by truncation**: lowered the verdict comment cap from 500 Chinese characters to 120 and enforced "output JSON only, no preamble or code block", to avoid a verdict being unparseable after `MaxTokens` truncation and then allowed under the model-failure policy (fail-open).

### Task archiving

#### Fixed

- **Archiving skips symlinks rather than failing the whole package**: the archive format end to end only supports regular files and directories, and previously any symlink in the working directory would fail the entire task archive; it now skips the symlink and logs it while archiving the rest normally (not following the link, not escaping the directory tree).

### Account and compliance

#### Added

- **Added a "Terms of Use and Disclaimer" dialog before login**: agreement must be checked before logging in.

### License and dependencies

#### Changed

- **The project adopts the AGPL-3.0 open-source license**, and the README's license and disclaimer notes were improved.
- **Upgraded norma to v0.4.1**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.12] - 2026-09-17

### Exploration-chain broadcast board

#### Added

- **Added an "Exploration-chain broadcast board" to the task detail page** (#144): presents the task's exploration nodes (start/goal/intent/fact/vulnerability/hint/compaction) in a timeline-feed view, with filtering by type, keyword search, ascending/descending order, pagination, and auto-refresh; while on the "newest-first first page" it is a live slot and refreshes every round, and when you leave that slot it only accumulates an unread count without disturbing your current reading, with "N new broadcasts · back to latest" for one-click return. Grouped by day, so even when paging through a long task you can recognize "which day this happened".
- **The broadcast board shows a node id on each row** (#145), making it easy to cross-reference the exploration-chain graph and locate a specific node.
- **Broadcast-board node detail expansion shows upstream/downstream and anchored assets** (#147): expanding a broadcast shows that node's upstream (came from) / downstream (produced from) relationships, and hovering over a related entry pops up that node's card (type/status/source/time/summary/payload fragment); it also lists the assets anchored to that node (type label + recognizable text). The relevant data is sent along with the broadcast page, so expanding makes no extra request.
- **Broadcast-board search supports filtering by node id** (#150): in addition to "content / source", the search box now matches node id, so entering a plain number or the "#41" form shown in the UI precisely locates the corresponding node.

### Intent management

#### Added

- **Intent deletion supports two modes, "soft delete / hard delete"** (#149): pending, running, and paused intents can all be deleted, and deletion requires a reason; the deletion mode is chosen in the confirmation dialog.
  - **Soft delete (default)**: the intent is set to "deleted" and the reason recorded in a separate field, keeping the intent node and all its outputs and lineage.
  - **Hard delete**: physically removes the intent and the exclusive descendant nodes "supported only by it" (cascading along the output / intent chain to the leaves), avoiding orphan data; the deleted intent's token accounting is archived and kept by its original date; shared nodes (still referenced by other intents), goals, and the task root fact are all kept, and the hard-delete dialog shows the estimated number of cascade-affected nodes.

  Both modes notify the planner "this intent was deleted by the user + reason" and re-plan accordingly.

### Operation review

#### Added

- **Approval records support filtering by status and verdict source** (#139): approval records can be filtered by approval status and verdict source to quickly locate the target record.

#### Fixed

- **Pending approval requests load independently of history pagination** (#133): pending requests are no longer affected by history-list pagination and remain fully visible while browsing history.

### Agent

#### Changed

- **The Worker role description was changed to generic cybersecurity-platform wording** (#138).

#### Fixed

- **Fixed cold-node compaction never triggering**: the Compactor was wired into the per-task planner; cold-node compaction (cold-digest) previously never ran because it wasn't wired in, and is now restored.

### Chat

#### Added

- **Chat supports @-referencing multiple record types with scroll pagination** (#135): multiple types of records can be @-referenced in chat, and reference candidates support scroll-paginated loading.

#### Fixed

- **Long message bubbles are constrained within the session panel** (#137): overly long message bubbles no longer overflow the session area.
- **Fixed "missing uploaded file" when uploading a file before a new conversation is created**: the Composer's file selection now snapshots the `FileList` to an array before clearing the input, then invokes the callback; previously the draft state had to asynchronously create the session first, and by the time execution resumed the `FileList` live-bound to the input had been cleared, so the upload was missing the file field and the backend returned 400.

### Traffic

#### Fixed

- **The traffic-recording proxy listens on 127.0.0.1 by default** (#129, #130): to avoid the open proxy being exposed on other network interfaces under the default configuration.

### Web

#### Fixed

- **Fixed the static-export task list page losing the global header**.
- **Added the demo vulnerability detail page's associated-traffic mock, fixing a white screen**.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)
- [@dingpotian](https://github.com/dingpotian)

## [0.3.11] - 2026-09-15

### Operation review

#### Added

- **Approval records support source navigation** (#125): clicking an approval record's "source" jumps back to the exact tool execution that triggered that approval — opening the original full session, auto-paginating to load to the target position, expanding the command and result in place and center-highlighting them in the scroll area, keeping the surrounding messages readable; auto-correction stops when the user scrolls manually. Across ordinary conversations, task Workers, the planner, and main-Agent segmented sessions, it locates precisely via the persisted `tool_use_id` and task mapping (a new in-scope call-ID index), and when the record is missing, duplicated, or ambiguously associated it shows an explicit hint rather than jumping to a different execution. A clear hint is given when the session has been deleted or the task is archived / the record is missing.
- **Approval records support pagination** (#115).

#### Changed

- **Streamlined the model-review input** (#124, #125): the model-review (the LLM judge after rules don't match) input is reduced to versioned JSON of "this one complete tool call + explicitly selected brief context + the local working directory". The context takes only real user messages (the current user message of the chat / task main Agent), the Worker no longer attaches an intent summary and clears context inherited from a parent Agent; the planner and auto-triggered sessions don't fabricate a user message. It no longer attaches the task description, goals, operation constraints, global exploration situation, call history, or the Worker's full intent — these are still used for Agent execution and independent session auditing per the original logic, just not fed into operation review. All verdicts are required to output JSON `decision`/`comment`, and the explanation must include "the actual operation, the consequences after success, the matched rule"; instructions in parameters or context cannot change the review policy. A snapshot and fingerprint of the input actually sent to the model is saved, and old snapshots are kept and version-tagged rather than fabricating old input from current data.

#### Fixed

- **A model verdict wrapped in a code block is no longer silently allowed** (#126): if the model-returned verdict JSON was wrapped in a ``` code block, parsing previously failed and it was silently allowed under the model-failure policy; now the code-block fences are stripped before parsing, and only if parsing still fails is the configured failure policy applied.

### Agent

#### Added

- **Added experimental noa context compaction** (norma upgraded to v0.4.0): an experimental feature that can be enabled in System Settings, off by default. When enabled, noa (model-driven context compaction) takes over context compaction for the four Agent types — main Agent, planner, Worker, chat — replacing the built-in compaction; the compaction source text is persistently archived, concentrated under `<workDir>/noa/<sessionID>/` (not scattered across task directories), with a globally unique directory per session ID. On integration failure it automatically falls back to the built-in compaction without interrupting a real task; the switch is read once per run, and toggling it only affects runs started afterward.

#### Fixed

- **Exploration-graph tools reject rather than crash with nil when there is no task context**: calling exploration-graph-related tools in a non-task context now returns a clear error instead of crashing on a nil-store dereference.

### MCP

#### Added

- **Support for legacy SSE MCP services** (#117): compatible with MCP servers that only provide the old-style SSE transport.

### Traffic

#### Fixed

- **Traffic retrieval matches a record's host in a port-aware way** (#114): `traffic_search`'s host matching now includes the port, avoiding cross-contamination between same-host records on different ports.
- **A `traffic_search` description migration error no longer interrupts the subsequent reporter migration**: a single migration failure is isolated and does not affect subsequent migrations.

### Web

#### Changed

- **Added a divider to the task-retest vulnerability options** (#122): a separator was added between retest vulnerability choices for better visual clarity.

#### Fixed

- **Fixed a full-page crash on the demo task detail page**: in mock mode the task detail "Session" tab fetches `GET /api/tasks/<id>/side-questions` (side-question history), but the mock handler had no such route, and the read fallback returned `[]` under "a path ending in s is a collection", so `data.items` was `undefined` and the side-question hook's `merge()` threw `TypeError: t is not iterable` when iterating it; that exception occurred inside the `setItems` updater, was deferred by React to re-throw at the render stage, the caller's `catch` couldn't catch it, and the whole page was taken over by the error boundary showing "This page couldn't load". Now the mock handler explicitly returns an empty side-question history, and `sideAPI.history` also normalizes the return value (a non-array `items` is always coerced to `[]`) as defense in depth.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.10] - 2026-09-13

### Network

#### Added

- **Added a DeepSeek official source to web search**: it directly reuses the currently active LLM configuration. It differs in nature from the other three sources — DeepSeek has no directly callable search API, and search exists only inside its Anthropic-compatible interface (the `web_search_20250305` server-side tool), executed by the DeepSeek server. Therefore it **only supports the DeepSeek official endpoint + the anthropic protocol** (OpenAI-protocol endpoints reject server-side tools outright), and each search **consumes an extra model call**, the request **does not go through the search egress proxy**, and it is **not recorded in the traffic trace**, and the returned results have **only titles and links** (no summaries; when the body is needed, WebFetch fetches it). The settings page explains these limits but **does not validate or block**, leaving it to the user to confirm whether they're met, and the "Test search" button can run one real search to verify.

### Agent

#### Added

- **The worker gained cross-work look-back capability**: added `search_all_worker_traces` (globally searches for matching steps across the execution of all work in this task by keyword, without needing to know the intent_id first) and `get_worker_trace` (after locking onto a specific work, lists its step flow, searches it in place by keyword, and fetches full content by step_id), used to reuse observations another work saw but didn't write into a fact, avoiding duplicated effort.
- **`node_detail` delegated to the worker**: paired with the look-back tools above, once the worker has an intent_id / node id it can directly query that node's full detail.
- **The planning round triggered by `add_hint` is now explicitly broadcast**: previously adding a hint only folded the hint into the situation overview and left the planner to discover it; now one `add_hint` records a trigger for the planner saying "the user added N strategic hints: …" (a batch counts as one, not flooding one by one), and the planner is explicitly told "this round was triggered by a new hint" and sees the hint content directly.

#### Changed

- **The global-situation prompt was changed to a "loosening" tone**: encouraging divergent exploration and timely reporting of cross-intent leads, rather than converging too early.
- **Streamlined the worker-boundary wording**: being blocked at first doesn't mean fully explored; focus on "walking through all the bypass techniques within this intent before drawing a conclusion".
- `insert_assets` removed the per-asset `related` input: its only effect was to decide whether to add this asset into the task's scope, and the value wasn't stored (re-registering the same asset voided it, and the UI didn't show who was judged unrelated), so in practice it just made the model do one extra judgment that wasn't retained.
- **The test scope (`task_scope`) is no longer affected by the asset-coverage switch**: `insert_assets`'s auto-add-to-scope (`source='auto'`) runs whether or not coverage is enabled, and `add_task_scope` is always provided to the planner / in-task main Agent / goal-decomposition Agent. The scope is the task's authorization boundary and the filter basis for asset queries; the coverage switch only decides whether to use it as the denominator for metrics, and should not decide whether to accumulate the scope itself. Previously turning off coverage disabled both the `auto`/`agent` write paths, leaving `task_scope` with only rows manually added in the UI. `list_untested_assets` is still hidden when coverage is off (it is a purely coverage-oriented view by nature).

#### Fixed

- **Fixed cross-task asset leakage** (#59): `list_assets` previously hardcoded the task id to 0 and bare-queried the entire shared asset library, and the model had no way to write a scope filter condition, so it treated other tasks' assets (especially IPs) as this task's targets and steered the testing direction off course. It now returns only assets within the test scope (`task_scope`) of [this task and directly related tasks], matching by **attribution** rather than literal value: if the scope contains a root domain, all its subdomains/services/endpoints are queryable; if it contains a network segment, hosts and services within the segment are queryable; directly fetching an out-of-scope asset by id likewise returns nothing. A non-task context (Auto/pentest) has no scope to rely on and still falls back to the whole library. This also fixes the attribution blind spot where an IP directly-connected host (like `http://1.2.3.4/api`) couldn't be matched by a network-segment scope. The UI's "Test assets" view (filtered by task producer) is unaffected.
- **Fixed the worker's cross-work look-back tools being accidentally removed**: after `search_all_worker_traces` / `get_worker_trace` (and `node_detail`) were added to the worker's default tool set, an old "converge the worker tool surface" migration unbound them again at startup, so the worker couldn't actually get these tools. They have been moved out of that unbind list, and databases that already ran the old migration have been re-bound to the worker once.
- **Fixed occasional database-insert failure of `/btw` side questions**: the side_question checkpoint strips the JSONB-unsupported NUL (`\u0000`) escape before writing, avoiding a write error for content containing that character.

### Triggers

#### Fixed

- **Merged trigger sessions deduplicate task descriptions/goals by task**: when triggering a merge of multiple tasks, the same task's description/goal is no longer concatenated into the message repeatedly, avoiding long goals stacking up and blowing out the context.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.9] - 2026-09-11

### Agent

#### Added

- The in-task main Agent supports **multiple sessions**: you can create, switch, and independently reset context, and interact in each session.
- Support for persistent `/btw` **side questions**: ask without interrupting the main line, the Q&A is stored, and it survives restarts.
- `spawn_task` added `source_task_ids`, so a new task can **read-only inherit** the source task's assets and findings.

#### Changed

- Disabled the worker's cross-engagement memory and converged its default tool surface: reading context and cross-work review are planning responsibilities, and the worker only handles the execution and write-back of a single intent.
- Streamlined the worker's default prompt; `get_worker_output` removed the `terminated` and `worker_name` fields; streamlined the descriptions of `insert_assets` / `list_assets`.

#### Fixed

- Fixed work being interrupted early due to an **idle round**: the model sometimes spends a whole round outputting only thinking, giving neither body nor a tool call, and the harness sees this as a natural end (`end_turn` with no `tool_use`) and ends with `completed` + an empty summary — an unfinished intent is cut off halfway, manifesting as "the task ends normally but with no text summary at all". None of the five LLM retry layers can reach it: it is not an error, the same-provider safe-window retry's entry point is "stream failure", the circuit breaker judges success directly when `err == nil`, and intent rerun only recognizes `model_error`; the SDK's empty-response retry can't reach it either, because that layer judges emptiness by "whether any event was yielded", and the thinking delta is itself an event. Now the work's Stop hook recognizes this kind of round and injects a continuation instruction, letting the model carry the thinking it already produced and continue with the next step — deliberately not choosing "resend as-is", because this kind of idling is usually determined by the prompt and context shape, is stable behavior rather than random glitching, and resending would just make the model think it over again.
- The count reuses the **empty-response retry count** from the LLM page's "Retry and backoff" (both answer "the model finished but produced no substantive content", just by a different criterion and means), defaulting to 2, and setting `-1` turns it off and reverts to the old "idle means end" behavior. It is the **total** cap for an intent, not a consecutive count: the harness itself hard-limits consecutive idling to a single nudge, and the quota only refreshes when a real tool round actually occurred, so this number guards against a loop like "tool → idle → nudge → tool → idle" exhausting the intent budget. On trigger, the log records `[work <worker> · #<intent>] idle round… injecting continuation instruction (n/N)`. See `docs/LLM重试设计.md` §1.1 for the design.
- Fixed `/btw`'s context budget and input layout in long conversations; a non-secure context (non-HTTPS access) degrades to generate the side-question request ID.

### Traffic

#### Added

- A vulnerability can **associate multiple traffic evidence items**, which can be ordered, annotated, and distinguished by role (request/response/supporting).
- The report Agent **automatically associates** relevant traffic evidence before writing the report.
- Added an Agent **traffic-binding switch** and completed the hand-off of evidence between Agents.

### Task

#### Added

- Support for **session-level vulnerability retest**: the retest runs in an independent Agent session, and the list and detail show the run status.

### Interception

#### Added

- Approval records gained **detail and execution auditing**: you can view the tool request context, the model/rule initial verdict, the execution output, and the parameter fingerprint.

### Assets

#### Added

- Task test assets support **DSL search**.

### LLM

#### Fixed

- Fixed the issue where the test connection didn't send the custom session header, causing opencode zen to return 400.

### Network

#### Added

- MCP's HTTP transport supports **skipping TLS certificate verification**, making it easier to integrate services with self-signed certificates.

### UI

#### Added

- The session list is grouped by Agent, supporting expand/collapse and independent pinning; conversations can be filtered by Agent.
- The attack-chain graph renders compacted (digest) nodes and collapses their members.

#### Fixed

- Fixed a white screen caused by login credentials being out of sync.
- The interception prompt text was changed to clearly indicate "platform control", to avoid it being misread as a defense on the target side.

### Deployment and updates

#### Added

- Support for **one-click update in the UI**: the system config page gained a "Version and Updates" card, and the top bar shows a hint when a new version is available. The flow is: download the release package → verify `SHA256SUMS` → smoke test → stage → exit, and the supervisor script relaunches to complete the binary swap, then the page auto-refreshes.
- Added supervisor startup scripts `start.sh` / `start.bat` as the official startup entry point (included in both the release package and the Docker image; `install.sh` is unchanged), which decide whether to relaunch based on the exit code and forward SIGTERM to artex. The verification and swap logic is all in Go, keeping the script dead-simple.
- Automatic failure fallback: if verification or the smoke test fails, it's discarded and the current version keeps running; if a new version fails to start 3 times in a row, it rolls back to the previous version. The settings page also has a manual rollback (note that the database schema does not roll back).
- Updates only recognize GitHub domains and force HTTPS, and the release source is not configurable; development builds disable one-click update. GitHub query results are cached for 30 minutes to avoid the top-bar hint exhausting the API quota.

#### Known limitations

- Under Docker only the program is swapped, not the image: the toolchain is not upgraded along with it, and rebuilding the container reverts to the image's bundled version, so use `docker compose pull artex` when needed.
- The `skills/` in the release package is not synced, so a new version's newly added built-in skills do not take effect automatically.
- An update means a restart, which interrupts running tasks.

### Dependencies

#### Changed

- Upgraded norma to v0.3.7.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@RuoJi6](https://github.com/RuoJi6)

## [0.3.8] - 2026-09-09

### LLM

#### Added

- The LLM page gained a "Retry and backoff" tab: both the **count** and **interval** of the five retry layers are configurable. A single model call's failure passes, from inner to outer, through connection retry (SDK, connection reset / timeout / 429 / 5xx before the stream starts), empty-response retry (SDK, a normal end with no content at all, openai format only), same-provider safe-window retry (stream-break replay before any output has been delivered to the caller), failover circuit-break (cool down and skip after consecutive failures reach the threshold), and intent rerun (the whole intent reruns after a worker ends with model_error) — only when an inner layer is exhausted does the next outer one take over. Each layer has two knobs with unified semantics: leave empty = use the original default count and exponential backoff; enter a count to use that count; enter an interval to replace exponential backoff with a fixed interval; enter -1 = turn off that retry layer. The first three layers follow the endpoint and can override the global default field by field in each model configuration (one that pins only the interval still inherits the global count); the circuit breaker and intent rerun are process-level semantics with only one global copy. Changes take effect hot, without a restart. Leaving everything empty is the current behavior, and an upgraded old database is byte-for-byte unchanged (new columns default to 0, and an absent settings key means all defaults). The connection test deliberately doesn't carry these parameters — it has a 30s hard timeout, and stacking user-configured retries on top would just test a working endpoint into a timeout failure. See `docs/LLM重试设计.md` for the design.
- Each LLM configuration supports a custom **session header** (`session_header_key`): when non-empty, every LLM request carries this HTTP header whose value is the current run's session id (a chat session is `conv-<id>`, a worker is `exp<x>-worker-i<intent>`, etc.), for gateways that do prompt caching / sticky routing by the session-id header. It is implemented by reading the session id from the request context and injecting it, with no need to modify norma, and the same shared provider can emit different header values per session. An old database gets a migration, and the frontend LLM config dialog gains an input field.

#### Fixed

- Fixed the issue where saving an LLM configuration reported `23502` directly when the session-header field was empty: that column is `NOT NULL DEFAULT ''`, and previously `NULLIF($n,'')` was mistakenly applied, writing an unfilled session header as NULL and triggering the not-null constraint; it now passes the parameter directly with empty-string semantics.

### Agent

#### Added

- The wall-clock timeout was changed to **wrap up in place**: relying on norma v0.3.6, when `MaxDuration` elapses it interrupts the running tool and continues on the live ctx for the wrap-up rounds (writing back identified content + a summary, with a final state of timeout), no longer relying on the worker/planner's own `maxDur+90s` external hard ctx to kill a stuck run outright into `aborted_tools`. Chat uses the same harness and automatically gets the same behavior; the mainagent has no `MaxDuration` and is unaffected.
- Stall fallback: when the planner wakes on a heartbeat / no-change and the whole graph has no open or running intents, the opening message is changed to a stall warning, clearly stating that no worker is running and there is no queued direction, and that this round must produce one or more mutually distinct new intents (producing 0 intents is not allowed).

#### Changed

- Worker prompt refactor: the intent / startup instruction / anchored-asset raw JSON are moved into the system prompt, re-assembled every round, never compacted away, and continuation no longer depends on the transcript's first message being retained; the startup user message is slimmed down to only the global-situation overview (degradable, tolerant of staleness). The cost is that the system prompt mixes in per-intent data and loses cross-intent cache reuse, a deliberate trade-off for "don't lose the intent".
- Streamlined the planner / worker default body and fixed several real-world issues: the planner distinguishes `recent_done` by status (for blocked/exhausted it checks the trace before deciding, treating it as neither a dead end nor a blind rerun), negative conclusions are changed to "observation / in doubt, not final" and evidence is checked before being believed, it must produce output when the goal isn't met and there are no open/running intents (a hard floor), and depth takes priority over coverage; the worker writes negative conclusions only as "observation + tentative reading", the verdict belongs to the planner, and cross-intent leads are written into a fact's summary for the planner rather than chased by the worker itself. Reseed appends the new defaults as a new version and switches to it, while user customizations / old versions are kept in history and can be rolled back.
- Converged the work agent's default tool set and asset write-back prompt: the worker only handles the execution and write-back of a single intent, and reading context / cross-work review are planning responsibilities, so `list_facts` / `node_detail` / `list_companies` and cross-work search (`search_all_worker_traces` / `list_worker_traces` / `get_worker_trace`) were removed from its default tools, leaving only `list_findings` (dedup check before reporting a vulnerability) + `add_finding` / `record_fact` + `insert_assets` / `list_assets`; the prompt dropped stale field descriptions that conflicted with the `insert_assets` schema (`type=tech`/`on_url`/`props`). An old database gets a one-time migration stripping the corresponding bindings, while the planner/main bindings of the same names are untouched.

### Exploration graph

#### Added

- **Exploration-graph cold-node compaction (cold-digest)**: folds old, long-inactive intents / facts into digest nodes shown in `graph_overview`, while the original nodes are kept permanently and can be fully restored by id (lossless storage, presentation-only compaction, reversible folding). Cold/hot is judged by reverse reachability + "hot if any live branch", with R=6-round debouncing and connected-component grouping; background compaction (minor folds uncovered cold blocks, major re-fetches and re-compacts to merge fragments) has activity re-checks and cool-down mutual exclusion, never runs on the hot path, and never overwrites a revived node. The overview side provides `cold_digests` and the asset-indexed `cold_index`, with `expand_digest` / `expand_index` handling restoration. A related / inherited task's overview likewise reuses its own folded view, and `expand_digest` supports cross-task read-only restoration. `expand_digest` / `expand_index` are given only to the planner and main agent, not the worker. All come with old-database migrations.
- `graph_overview` outputs the full `finding_list`: findings are the task's highest-value product and there usually aren't many per task, so they are now carried in full in the overview (unlike facts, which only give a recent window), letting the planner/worker see all confirmed vulnerabilities at a glance each round without calling `list_findings` again. Each is reduced to `{id, summary, evidence?, from_intent?, assets?}`, where affected assets are given directly as readable content (url / domain / ip:port) rather than a bare id.

#### Changed

- `graph_overview` no longer flattens `hosts`: the coverage block removed the host list (a large-scope task carries up to 500 host strings per round, of limited value for planning decisions), keeping only `host_count`, with specific hosts queried on demand via `list_assets`. A top-level `done_intents_total` (total number of ended intents) was added, parallel to `recent_done_intents` which is truncated to ≤15, so the planner knows whether it's been truncated when deduplicating.

### Tools

#### Changed

- `add_company_scope`'s default binding was changed from worker to planner: defining a company's asset scope is a planning / main-control / Auto responsibility, and the worker only performs exploration. A new database's seed default binding is mainagent/planner/auto, and an old database gets a one-time migration.
- The planner is bound to `list_assets` by default: it now has both `list_assets` (DSL whole-library search) and `list_untested_assets` (untested within scope), and an old database is backfilled once without overriding a user's unbinding.

### Task

#### Added

- The task list gained a "Running Workers" column: it counts the task's intent nodes with `state='running'`, consistent with the "Running Workers" in the task detail page overview, and shows 0 when there are no running workers.

### Network

#### Added

- Added a **global egress proxy** configuration: all target traffic can go out through a unified global proxy (http/https/socks5, with `user:pass` support). When traffic capture is enabled, it serves as the upstream of the MITM recording proxy (traffic is recorded as usual and then goes out through the proxy, with both the intercept and pass-through paths going upstream, not leaking the source IP); when capture is disabled, it is injected directly into the agent's bash environment and WebFetch (`proxyEnv` adds `ALL_PROXY` to support socks5). The configuration is stored in the settings KV table (no migration needed), and the frontend system config page gains a "Global proxy" card, independent of the web-search proxy and the LLM proxy.

### UI

#### Fixed

- Corrected the semantic errors of the intent status labels: `exhausted` "fully explored" → "budget exhausted" (it actually means the step / time budget was cut off midway and only partial results were written back, not that the direction is fully explored), `blocked` "intercepted" → "execution error" (it actually means the model / API / network failure retries were exhausted and the intent basically wasn't truly explored, not that the target / WAF intercepted it), and added the previously missing `stopped` "stopped" (work the user manually stopped).

### Dependencies

#### Changed

- Upgraded norma to v0.3.4: MCP tool output gained truncation and disk persistence (see `651b961`), and the later v0.3.6 supports wall-clock wrap-up in place.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.7] - 2026-08-31

### LLM

#### Added

- Model configuration gained an "Output limit", with a choice of which request field name the limit uses: the limit caps how many tokens a single reply can generate at most, sent with every request, where 0 (default) = don't send the field and let the server default decide; it is distinct from "context window" — the latter is the model's total capacity, used only locally to compute the compaction threshold and not included in the request. The field-name switch is only meaningful for the `openai` (Chat Completions) format: empty (default) sends `max_tokens`, which the vast majority of compatible gateways recognize; OpenAI's own reasoning models (the o-series / GPT-5) conversely only recognize `max_completion_tokens` and report `unsupported_parameter` on receiving `max_tokens`, so such endpoints need to be manually switched to the new field. The two other formats hardcode their field names (Anthropic uses `max_tokens`, the Responses API uses `max_output_tokens`), so for non-openai formats this switch is grayed out and cleared on save. This also connects a path that was never wired up before: the planner / worker / chat / main Agent / goal-decomposition never set an output limit, the openai family simply didn't send it, and Anthropic used the SDK's 8192 fallback, so just adding the config option without connecting this line meant a number you entered wouldn't take effect; now the value is resolved per round from the configuration, just like "streaming output", taking effect the next round after a failover switch. An upgraded old database behaves exactly as before (new columns default to 0 and an empty field name).
- When clicking "Test" in the backend, each HTTP attempt's status code and the gateway's raw response body are written to the server log (the body is trimmed to 4K), making it easier to diagnose 401s, quota messages, empty frames, returned HTML pages, etc., instead of only seeing the UI's collapsed ok / err. This log does not depend on the "LLM recording" switch.

#### Changed

- The task LLM config chain can now be modified in any state, no longer restricted to "running / paused / config chain exhausted": after a task ends (done / failed / timeout) the main Agent conversation still uses this chain, and previously when a model on the chain had a problem you could neither edit it nor continue interacting. The final-state interception in both the backend HTTP and the DB transaction was removed, and the frontend dialog also opens editing and saving for final-state tasks. Saving a final-state task no longer re-opens quota-blocked intents (those intents would be moved to open, where there's no worker to execute them and they no longer meet the "rerun intent" condition); to keep running, still use rerun intent / add goal, which pull the task back into a running state.

### Agent

#### Changed

- "Send a message to a running Worker to adjust direction" was changed to reuse the existing pause / resume + transcript resume mechanism, behaving consistently with the main Agent conversation: previously it used a self-built intervention-persistence protocol that touched the scheduling barrier, the resume flow, and a dozen-odd activity-query filters. Now the message is injected via the next round's input, and the intent runs directly in a dedicated goroutine, not limited by the 3-slot worker pool and running as soon as it's sent; the frontend keeps the message box and restores the "continue directly" button, with sending now carried over SSE. No DB schema changes. The trade-off: the message is stored in memory only with no crash recovery, and when sending a message the task may momentarily exceed concurrency by one worker (a low-frequency scenario, acceptable).

### Task

#### Fixed

- Fixed an out-of-memory (OOM) crash when archiving a large task: archiving was changed to stream the snapshot to disk instead of reading the whole task into memory at once; it also filled three recovery gaps on the cold-archive path — a modern installation's traffic lives only in SQLite, and a crash between the PostgreSQL commit and the SQLite commit previously left an archived task's traffic stranded in hot storage with no way to recover, and now a staging log is written whether or not there is a history directory, and a missing `journal.json` is treated as discardable.
- Fixed system lag when there are many tasks: loading conversations and loading the UI had noticeable delays. The queries for the task list, task context, and exploration records were all optimized, and the frontend's dashboard, conversation page, and task detail page accordingly reduced duplicate requests.

### Assets

#### Changed

- Removed the "at most 256 rules" limit on company asset scope: a company that enters its scope IP by IP / domain by domain easily hits this cap, after which it can only be split into multiple companies, yet the scope itself isn't slower just because it has more entries. The cap was removed in all three places — backend, frontend, and demo mock — a single rule is still limited to 1024 characters, and the 2 MiB request-body cap is kept as a fallback (roughly forty to fifty thousand rules).

### Skills

#### Fixed

- Fixed the issue where uploading a skill archive reported "upload failed: zip: unsupported compression": the Go standard library only builds in the Store / Deflate decompressors, and bzip2 and Zstandard archives written by compression software at non-default levels could not be decompressed at all. These two decompressors are now added (pure Go, introducing no new external dependency); for methods that genuinely can't be decompressed, such as Deflate64 / LZMA / XZ / PPMd, and for encrypted archives, a hint is now reported before decompression, directly naming which file used which compression method and how to repackage it, instead of dumping the underlying English error on the user.
- Fixed the issue where in-skill filename validation judged Chinese filenames as illegal: path validation was previously an ASCII whitelist of `[A-Za-z0-9-_./]`, and if the archive contained even one Chinese-named file (something like `参考/说明.md`), the whole upload would fail as "archive contains an illegal path". It was changed to a Unicode blacklist: filenames and spaces in all languages are allowed, while control characters, invalid UTF-8, zero-width and bidirectional control characters (RLO filename spoofing), `\ % # ? * : " < > |`, and `..` / absolute paths / empty path segments are still rejected, with the directory-traversal protection unchanged. Skill names were likewise loosened — ASCII is still limited to lowercase letters, digits, and hyphens (the agentskills.io spec), and non-ASCII letters such as Chinese can be used directly as a skill name, but spaces, dots, and path separators are not accepted.
- Fixed the issue where a Chinese archive produced by Windows compression software had garbled filenames or was wholly rejected after decompression: such zips don't set the UTF-8 flag and write filenames in GBK, so filenames are now decoded with a GBK fallback before path validation. A quoted frontmatter like `name: "中文技能"` also now correctly yields the skill name.

### UI

#### Added

- The findings page gained an "All findings" flat view, set as the default, switchable with the original "Grouped by task" view via a page-header tab: the grouped view requires expanding task by task to see vulnerabilities, which is a detour when you just want to scan the list across tasks. The flat view is one big cross-task table (10/20/50/100 per page), behaving exactly like the grouped view — select for export, expand inline to see evidence and the detailed report, inline edit of name/category/severity, change disposition status, deep-dive, and delete. The two views share the stat cards and the filter bar, switching doesn't lose the filter, and the current view and filter are recorded locally together; polling only hits the current view, inline changes are written to both caches in sync, and switching over won't show stale data.
- The findings page gained a "By asset" view: a left-hand asset tree (company → root domain / IP → subdomain → service → endpoint, with the company level appearing only when the asset truly has an attribution), and on the right the findings under the entire subtree of the selected node, sharing the same table and the same set of filters as the other two views. The tree only collects assets that have findings, with the ancestor chain filled in on demand (when a finding is attached only to the deepest endpoint, the levels above can still be assembled), and a node's count is a subtree aggregate deduplicated by finding; findings whose asset has been deleted or that never had an associated asset go into an "Unassociated assets" bucket. Unlike the other two views, the asset view does not poll — it queries only on entering the view, changing a filter, adding/editing/deleting a finding on the page, or clicking the refresh button on the tree, since the left tree is a navigation structure and there's no need to recompute every 5 seconds. Each level of the tree shows only the increment relative to the level above (a subdomain drops the root-domain suffix, a service shows `https :443`, an endpoint shows only the path), with the full value in the hover tooltip and breadcrumb; when there are too many nodes, the whole endpoint/service level is dropped with a hint, and the counts still roll up to the level above.

#### Fixed

- Fixed the issue where, in the mobile session detail, the session records were squeezed and not fully visible: on a narrow screen the session list is collapsed to give the width to the records themselves.

### Dependencies

#### Changed

- Upgraded norma v0.3.2 → v0.3.3: added `Config.MaxTokensField`, so the OpenAI Chat Completions output limit can instead be sent as `max_completion_tokens` (reasoning models only recognize this key). The two keys are mutually exclusive and only one is sent, with the default still `max_tokens`.
- Upgraded `golang.org/x/mod` v0.37.0 → v0.40.0, fixing two dependency security warnings.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)
- [@begininvoke](https://github.com/begininvoke)

## [0.3.6] - 2026-08-27

### LLM

#### Fixed

- Fixed the issue where the "non-streaming" switch reverted to "streaming" after being saved and reopened (#69): the config-list API's DTO omitted the `streaming` field and never returned it in the response, so the frontend read `undefined` and always fell back to the default streaming; the actual write and DB storage were fine, it just couldn't be read back. The field was added back to the DTO (without `omitempty`, so `false` must also appear in the response).
- The test connection was changed to verify the model actually replies and to test using the config's real send/receive mode (#65): previously it only judged whether the HTTP succeeded, so a request that went through but got a zero reply from the model (thinking burning through the budget / the body swallowed by a safety policy / the compatibility layer dropping `content`) also reported "connection successful", disconnected from the "no reply" behavior in a session; and the test always used streaming, so a non-streaming config was actually testing a different channel. Now an empty reply is judged a failure directly, on success the model's reply is shown in the hint, and the current streaming switch is carried into the test, keeping "test passes" consistent with "a session works".

### Agent

#### Changed

- `list_facts` was changed to pagination + keyword filtering, to avoid a single call blowing out the context when there are many facts (#74): it returns the latest 20 by default, supporting `limit` (max 100) / a `before` cursor / a `q` summary keyword, returning `{facts, total, has_more, next_before}`; an overly long single summary is truncated by character count (the full text still goes through `node_detail`). The worker / planner prompts were updated to the pagination semantics. An old database flushes the new parameter schema into the tool-catalog table via a one-time migration (`SeedTool` on first insert only, otherwise the tool management page shows "no parameters").

### Tools

#### Added

- The "Tool Execution" page gained a tool-call-count statistic (#72): the toolbar's "Statistics" button opens a dialog showing each tool's call count, proportion, and failure count (sorted by count descending); it reuses the list's task / keyword filter, counts the entire result set rather than the current page, and fetches only when the dialog opens.

### Dependencies

#### Changed

- Upgraded norma v0.3.1 → v0.3.2: fixed reasoning/refusal being silently dropped in OpenAI-family responses (Responses adds `reasoning_text`), three-name deduplication of the reasoning field, bounded retry on an empty response (zero content blocks), and a gateway 400 caused by a compaction boundary cutting a tool pairing (also fixing existing orphans already baked into the transcript).

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)

## [0.3.5] - 2026-08-25

### LLM

#### Added

- Each LLM configuration supports a streaming/non-streaming toggle (streaming by default): on uses streaming SSE; off uses true non-streaming (`stream:false`, returning the complete JSON at once), which can work around some gateways' poor SSE implementations (empty frames, dropped thinking-field frames), at the cost of losing in-run real-time progress and real-time token counting. The worker / planner / mainagent / chat / goals all take the value dynamically from the currently active configuration; all three Provider-wrapping layers — `llmpool` / `llmrec` / the task runtime — are compatible with non-streaming; `llm_profiles` added a `streaming` column and `ALTER`-patches old databases (default `true`, transparent to old configs).
- Support for OpenAI Responses API format LLM configurations: each config gained a third format `openai-responses` (hitting `POST /v1/responses`), alongside Chat Completions / Anthropic; `BaseURL` is normalized with a default model of `gpt-5`; the `format` constraint of `llm_profiles` added `openai-responses` and idempotently migrates old databases; the frontend format dropdown gained "OpenAI (Responses API)". The dependency was upgraded to norma v0.3.1 (including the `reasoning_content` return fix).
- Recording the raw HTTP of LLM requests/responses: the real wire body is captured at the HTTP transport layer, preserving the tool schema, `tool_use` blocks, and raw SSE frames that the normalized view can't see; norma's internal retry attempts are preserved one by one; `llm_records` added `raw_request` / `raw_response` columns and `ALTER`-patches old databases; the recording page's detail panel gained a "raw" view toggle and request/response copy buttons (with an `execCommand` fallback compatible with non-secure contexts).

### Conversations

#### Changed

- The session-list multi-select was changed to a "multi-select" mode toggle: by default the list no longer permanently shows a per-row checkbox (cleaner look), and the header changed to a "N total + Multi-select" button; clicking "Multi-select" enters selection mode (checkboxes, select all, bulk delete), "Done" exits and clears the selection, and a bulk delete that fully succeeds exits automatically (if any fail, it stays in selection mode for easy retry); single-item rename / pin / delete still go through the per-row ⋯ menu.

### UI

#### Changed

- Enhanced the experience of task management, session operations, and traffic viewing (#57): the task list's column-sort preference can be persistently remembered, inline rename is now triggered directly by an icon (no longer via a menu), and the sheet interaction and traffic-viewing details were refined.

#### Fixed

- Worker asset labels show only domains and IPs, and correctly handle the empty-label case.

### Agent

#### Added

- The planner gained a "quantitative acceptance check" when judging goals: when a goal has a quantifiable acceptance condition (asset test coverage reaching X%, capturing N flags, obtaining some privilege), it must check the measured value in `graph_overview` (`coverage.pct`, finding counts, etc.) before `prove_goal`, and if the measured value falls short, `prove_goal` is forbidden and it instead dispatches intents to close the gap, and may not mark it met early on the grounds of "the main part is done / basically achieved". This fixes the issue where a goal requiring 100% coverage was judged complete at a measured 40%.

#### Changed

- Rewrote the basis for "0 intents this round" in the planner prompt: the original described 0 intents as "the most common, most important principle", which made the planner stop too early when the goal wasn't met and there was still untested surface within scope. It now says 0 intents should be produced only in two specific cases — ① every direction thought of is already covered by open/running/recent_done intents; ② the next step depends on the output of a currently running work and that output hasn't appeared yet (it should wait for that to finish and plan on the next wake after the graph updates). And it adds a reverse constraint: when there genuinely is a new direction that isn't covered and doesn't depend on a running work, or the goal isn't met and there's still untested surface, don't stop just because "0 intents is common".
- Strengthened the "evidence bar for negative conclusions" in the worker prompt: for negative conclusions that might make the planner abandon an entire direction (like "not injectable / port closed / no login entry"), it requires exhausting the reasonable techniques within the intent (changing encoding/parameters/path/method) before drawing a conclusion; if the techniques aren't fully walked through or the evidence is weak, it must be marked `confidence=inferred`, to avoid a careless `observed` negative welding an entire route shut (an incorrect negative early in a task especially steers the direction off course and is hard to self-heal from).
- The above planner / worker default-prompt changes are appended as a new version and switched to current via a one-time migration (`reseedPlannerPrompt` / `reseedWorkerPrompt`, each guarded by a settings flag); old versions are kept in the version history, and users who customized the prompt can restore from the version record.

### Operations

#### Added

- Added `reset-password.sh` to reset the admin (username fixed to `ARTEX`) password: it supports both local / docker deployments, and the connection info can be specified explicitly or read automatically from `--dsn`/`$ARTEX_PG_DSN`/`config.json`; it uses `pgcrypto` inside the database to generate a bcrypt hash compatible with the backend login and writes it back to `settings.auth.password_hash`, with no service restart needed after reset. The password is passed via an environment variable, never enters the process argv, and is escaped against injection.

### Contributors

- [@Autumn-27](https://github.com/Autumn-27)
- [@neouks](https://github.com/neouks)

## [0.3.4] - 2026-08-24

### UI

#### Added

- After selecting multiple tasks in the task list, their category can be bulk-changed, with the target category supporting "Uncategorized" to move tasks out of their current category; the whole batch write completes in one transaction, and the only failed items would be tasks already deleted after selection.

#### Changed

- The "Task category" of a new task was changed from a dropdown to a searchable input: typing filters existing categories, and a name not in the database is instantly created and selected on Enter (or by clicking "Create" in the dropdown), with the selected item shown as a removable tag; still limited to a single category.

### Conversations

#### Fixed

- Fixed the issue where, when a session had a selected LLM configuration but there was no globally active configuration, sending a message was blocked by "LLM not configured, cannot converse". The pre-send check originally looked only at the globally active configuration, while actual execution prefers the session's selected configuration, so the two resolution logics were inconsistent; they are now unified into one (session/Agent binding takes priority, global as fallback).
- The "cannot converse" hint is now broken down by state: with no configuration at all it suggests "Add a configuration", and with a configuration present but inactive it suggests "Activate one or specify one for this session", instead of always showing "not configured", making it easier to pinpoint.

### Agent

#### Added

- For a task whose goals are all achieved, an intent dispatched by the main Agent via `add_intent` can be claimed and executed directly by a Worker and stops when done: the Planner no longer runs at this point (a task with no open goal doesn't enter planning), avoiding it re-judging the goals as achieved and canceling the just-dispatched intent; after the frontier is drained the task returns to completed. Before dispatching an intent, the main Agent asks the user, based on the intent content, whether to register it as a formal goal, and if registered the task resumes normal autonomous planning.

#### Changed

- When `get_worker_trace` / `get_task_worker_trace`'s `step_ids` exceeds the per-call cap (5), it no longer errors out directly, but returns the full content of the first 5 steps and uses `returned_step_ids` / `omitted_step_ids` / `notice` in the result to say which were taken this time and which weren't, with a hint "no need to fetch more if this is enough". Duplicate and illegal ids passed in are deduplicated and removed before counting.

#### Fixed

- The task list was changed to sort by task id descending (newest created first), replacing the previous sort by creation time. Multiple tasks created at the same moment have identical timestamps and sort unstably, which, combined with the 10-second polling and the random iteration of an in-memory map, caused them to frequently swap positions in the list.

### Assets

#### Fixed

- Fixed company-attribution recomputation being broken by a single dirty row: when `assets.ip` stored a hostname (written by the Agent or the asset API), `a.ip::inet` would throw `22P02`, causing all of add/modify company asset scope and delete company to fail and roll back. It was changed to the safe conversion `try_inet()`, which skips an illegal value rather than aborting the whole statement.
- An unparseable `ip` is no longer silently skipped: after saving a company scope, it now clearly reports how many assets have an `ip` that isn't a valid IP (with the specific id and value), warning that these assets won't be attributed by IP/CIDR rules; the same information is also written to the server log, covering the paths with no frontend response (delete company, scopesentry sync, Agent write).
- The task test scope's IP/CIDR matching was likewise changed to `try_inet()`, replacing the previous regex guard that only checked the character set (a hostname of all hex letters like `abc.def` previously slipped through and triggered the same error).

#### Changed

- The `ip` field of `ip`, `service`, and `endpoint` assets no longer accepts a hostname: `insert_assets` and the asset API return a per-item error with an `index` and directly give the correction method (use `type=subdomain` with `domain`, or resolve the A/AAAA record first), so the Agent can fix it itself and re-insert. Other assets in the same batch are unaffected and stored as usual.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.3] - 2026-08-23

### Worker

#### Added

- Running Workers support per-item pause, resume, and cancel; pausing keeps the intent, facts, and vulnerabilities, while canceling transactionally cleans up the current intent and its direct products after execution exits.
- Added a `paused` intent state, an execution barrier, and named termination reasons, to prevent late blackboard writes after pause, cancel, or task deletion.
- Added an optional task concurrency cap; creation, resumption, vulnerability deep-dive, and queue backfill all go through a persistent FIFO admission path.

#### Changed

- The wall-clock default duration of a single Worker run was changed from 600 seconds to 1200 seconds; a timed-out task re-times from scratch on its next real execution after being revived.
- The real execution state of the Worker, Planner, and main Agent uniformly drives the task status badge, fixing the issue where it still showed "idle" while the Worker was running.
- Worker sessions keep per-item controls and a current-session token total, and the model name was moved to the right of the current session title.

#### Removed

- Removed the Worker multi-select, select-all, and bulk pause/resume UI, and removed the Worker bulk-control API and Mock contract.
- Removed the token badge and tooltip in the Worker list rows; the underlying full token ledger and task-aggregation API are kept.

### LLM

#### Added

- Tasks support an ordered LLM configuration chain; when a provider's quota shortage is clearly identified, it automatically switches to the next configuration, persisting the current configuration, exhaustion state, and error summary.
- Running and paused tasks support editing the full configuration chain, reordering, and manually switching the current configuration, taking effect from the next LLM call.
- Automatic switches, manual switches, and full-chain exhaustion are written as structured system activity, with a deduplicatable reminder popped up through the task activity stream.
- Added a task-role model resolution API that uniformly resolves the configuration and model that the Main Agent, Planner, and Worker will use on their next call.
- Added an optional global LLM Pool, supporting configurable call order, designated per-model failure fallback, health status, cooldown recovery, and manual reset.
- Added an always-on LLM usage ledger that aggregates input, output, and cached tokens by task, session, model, and configuration.

#### Changed

- Goal decomposition, the Planner, the Worker, and the main Agent share the task-level LLM runtime (resolution priority is below).
- Failover responds only to clear quota, balance, or billing errors; ordinary rate-limit, auth, network, server, and context errors don't cause an incorrect switch.
- The LLM config page was changed to a list of configuration cards with right-side drawer editing, and provides the Pool's round-robin order, priority, exclusions, health status, and recovery actions.
- Config-chain exhaustion messages support wrapping on a narrow screen; the current model was changed from a session-list icon to a text label beside the current session title with a full tooltip.
- The order in which each in-task role resolves its LLM was changed to "Agent binding → task configuration chain → global": a role with an explicitly bound model always runs on that model, an unbound role falls to the task chain, and when the task chain is empty it falls to global.
- An empty outbound proxy means a direct connection, no longer falling back to the `HTTP_PROXY`/`HTTPS_PROXY` environment variables (an explicit `ARTEX_LLM_PROXY` is unaffected); the proxy input supports `socks5://user:pass@host:port` with credentials.

#### Fixed

- A transient streaming failure before submission (before any output has been produced) is safely backoff-retried on the same provider, significantly reducing cases of "interrupted after running just one or two tools, no summary, final state `model_error`"; quota exhaustion, overlong context, and deterministic 4xx errors are not retried and are still handed to failover and compaction recovery respectively.

#### Removed

- Removed the LLM icon-style indicator in the session list, to avoid confusion with the Worker status icon.

### UI

#### Added

- Tasks support creating, renaming, deleting, and filtering global categories; a new task can select a category directly.
- Added global task template CRUD and a right-side management drawer; a new task can load a preset description and goals, or save the current content as a template.
- A new task supports associating multiple source tasks and multiple company asset scopes; the company scope automatically recognizes domains, URLs, IPs, CIDRs, ICPs, and company keywords through a single multi-line text box.
- Task test assets support real-time add and delete, recording the source as manual, company, inherited, or Agent-discovered; the Worker session title shows a summary of the current test assets and their sources.
- The findings page groups by task with independent in-group pagination, and supports filling in a vulnerability description and creating a high-priority deep-dive intent.
- Conversations support rename, pin, unpin, and delete; the task list supports bulk pause and resume on the current page.
- Traffic detail and tool-execution detail were changed to right-side drawers; the HTTP message fills in `Host` and highlights the request line, status code, headers, JSON, and markup-language body.
- The task list's actions column gained "Detail" and "Pause/Resume" buttons; the session send key can be configured in system settings (`Enter` / `Cmd+Enter`, etc.), and the web-search area gained a proxy input box.
- The badge to the right of the session title was changed to show the LLM configuration name, with the model ID moved to the hover tooltip; a task can be given an optional name, and the task list gained a name column and search (falling back to the description when empty).
- The skills page gained usage count, last-used time, and missing-dependency statistics, to help locate skills that aren't taking effect or aren't installed.

#### Changed

- Task titles were changed to focusable detail links; task assets, company assets, finding task groups, and in-group vulnerabilities use real server-side pagination, stable sorting, and exact totals.
- Adding a company was changed to a right-side drawer, with scope entry unified as multi-line text and providing real-time recognition, validation, and a type preview.
- The main Agent input box supports auto-grow, `Enter` to send, `Shift+Enter` for a newline, and avoids accidental sends while the Chinese IME is composing.
- Agent preview, task reports, and related details uniformly use shared Markdown rendering; delete confirmation, long errors, and mobile drawer width were fixed uniformly.
- The task template selector displays and searches by template name and submits by template ID; the system's original font size was restored, and the app version shows uniformly as `0.3.3`.
- The task total and current-session tokens highlight only the input, cache-read, and output values; the send button uses a cleaner up-arrow icon.
- The task overview's test scope shows the company name rather than the ID.

#### Fixed

- Plain text containing a dot is no longer mistaken for an ICP filing number.
- Fixed the issue where a name in the variable catalog collided with a global runtime variable (like `{{.Now}}`), causing the Agent editor's variable list to render a duplicate key.

#### Removed

- Removed the task card's separate "Enter" button, unifying entry to the detail by clicking the task title.
- Removed the dashboard's "New task" button, the Logo URL input when adding a company, and the vulnerability-count badges at the top of the findings page and in the task groups.
- Reverted the global 10% font-size increase, and removed the Worker selection box, model icon, and row-level token statistics in the session list.

### Agent

#### Added

- A new task can directly associate multiple existing tasks, read-only inheriting in real time the direct source tasks' goals, facts, vulnerabilities, completed intents, asset scope, and blackboard context.
- The blackboard read tools support querying source-task nodes, facts, vulnerabilities, and execution traces on demand; inherited nodes carry a source mark and all write tools refuse to modify them.
- The company-scope tools support domains, URLs, IPs, CIDRs, ICPs, and company keywords; a keyword serves only as an Agent scope hint and does not participate in automatic asset attribution.
- A vulnerability deep-dive operation creates a high-priority manual Worker intent in the original task, with an asset anchor and a `derived_from` edge.
- The overview gained a "Goal management" card, supporting manually viewing, adding, modifying, and deleting goals; adding or modifying notifies the Planner and revives the task, while deleting hard-deletes the goal node (cascading edge and anchor deletion) but does not revive.
- The main Agent gains the `steer_work` tool by default, which can inject correction instructions into a running Worker in real time without interrupting it or losing progress (still validating that the intent belongs to the current task).

#### Changed

- Source-task intents do not enter the new task's frontier, and the new task continues to use an independent exploration, execution queue, working directory, and conversation history.
- Deleting a running intent no longer destroys data: it is changed to stop at `stopped`, requires a deletion reason, attaches the reason as a fact to that intent and writes it into the payload, and the planner perceives it via a `cancelled` trigger (keeping the intent content and reason).
- The main Agent conversation was changed to be driven by the server activity stream, with recoverable input on a send failure; opening a session auto-sticks to the bottom and keeps the last reply visible after the detail lazy-loads.
- Pausing a task terminates the current Main Agent, Planner, and Worker calls, but does not prevent the user from proactively starting a new main-Agent orchestration session during the pause.
- Cancel, close, and stream-interruption uniformly preserve the real termination reason, generated content, number of run rounds, elapsed time, tokens, and unreturned tool calls.

#### Removed

- Removed the main Agent client's optimistic message echo, to avoid duplicate messages and cross-session stream mixing under pause, failure, or concurrent activity.

### Constraints

#### Added

- The goal-decomposition stage first extracts operation constraints (allow/deny) before decomposing goals, and the main Agent can supplement them at runtime; constraints are injected into the Planner and Worker system prompts at the highest priority, and the de-biased diversity and surface-expanding exploration explicitly obey the constraints.
- The overview gained a "Constraint management" card and create/update/delete APIs; the constraint injection scope can be toggled separately for the Planner / Worker (both on by default, read every round, taking effect immediately). The new table `task_constraints` uses `CREATE TABLE IF NOT EXISTS`, so an old database creates it automatically.

### Interception

#### Added

- Command interception gained a model fallback approval beyond the regex/string rules: when no rule matches, the model makes an `ALLOW`/`ASK`/`DENY` semantic judgment, with configurable fallback actions on failure and on approval timeout; the interception page is split into two tabs, "Interception rules / Model configuration", and the model's verdict is tagged with a `[model]` prefix and a reason.

#### Fixed

- Hardened the judge-output parsing, to avoid a correct `DENY` verdict being mistaken for an allow.

### Traffic

#### Changed

- Traffic recording was changed to store the whole exchange in SQLite (a large body overflows into a hash-deduplicated blob bucket), with a trigram full-text index built on the text body, supporting arbitrary-substring and Chinese search; deletion was reduced to a single SQL transaction, going from hours down to the millisecond level and no longer stalling recording during it. Added streaming download of a large body and a `traffic_search` full-text parameter; old file-tree data needs no migration and is still readable, searchable, and deletable.

### Tasks and assets

#### Added

- Task creation gained an "asset coverage feature" switch (on by default): when off, coverage is not computed or displayed, the situation graph shows only assets, `task_scope` is not accumulated automatically, and the related tools are removed from the Planner / main Agent; company association is unaffected by this switch.
- `insert_assets` gained a `related` mark per asset (default `true`): it takes effect only when coverage is on, and `false` only enters the shared asset library without counting toward this task's coverage (e.g. an incidentally discovered sibling site or unrelated asset).
- Company scope and task test assets uniformly support text recognition of domains, URLs, IPs, CIDRs, ICPs, and keywords; a domain/IP can create or reuse a global asset, while a CIDR/ICP/keyword is kept as task-scope context.

### Build

- `build.sh --release` supports building Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 in one go, and generates zip release packages containing `skills/`, a config example, and the README.
- Uses the Go linker to strip debug info and generates zip release packages; UPX was changed to explicitly optional, to avoid its self-extracting ELF causing a segfault at startup in some Linux environments.
- The Release Workflow runs a startup smoke test on the Linux amd64 binary and provides `SHA256SUMS` alongside the release package.

### Contributors

- [@neouks](https://github.com/neouks)

## [0.3.2] - 2026-08-20

### Added

- A new task supports associating multiple existing tasks, read-only inheriting in real time the direct source tasks' facts, vulnerabilities, completed intents, asset scope, and blackboard context; the new task still uses an independent execution queue, working directory, and session history.
- Tasks support an ordered LLM configuration chain. When a provider's quota shortage is clearly identified, it automatically switches to the next configuration, persisting the current configuration, exhaustion state, and structured audit activity.
- Running and paused tasks support editing the LLM configuration chain, reordering, and manually switching the current configuration; automatic switches, manual switches, and full-chain exhaustion are shown on the task detail page.
- Running Workers support pause, resume, and cancel. Pausing keeps the blackboard data, while canceling transactionally cleans up the intent and its directly produced facts, vulnerabilities, and execution records after the Worker stops writing.
- Deleting a task can optionally also clean up the associated assets, traffic, vulnerabilities, and task-landed files, and adds a deletion barrier, concurrency protection, and auditable deletion statistics.
- Added an optional task concurrency cap; a new task that reaches the cap queues FIFO and starts automatically once a run slot is freed.
- Added task-asset pagination, company-asset pagination, task-category statistics, and a paginated-intent Mock contract.
- Added the `build.sh` single-binary build script, supporting static frontend export, resource embedding, cross-platform targets, and build-version injection.

### Changed

- The task-level explicit LLM configuration chain coexists with the existing global LLM Pool; the explicit chain continues to use strict quota-failover semantics, and without an explicit chain it follows the Agent-binding and global-configuration rules.
- The main Agent input box supports auto-growing multi-line input, `Enter` to send, `Shift+Enter` for a newline, and avoids accidental sends while the Chinese IME is composing.
- The Planner, Worker, and main Agent share the task-level LLM runtime; the task chain uses the smallest context window among the candidate models as the safe compaction threshold.
- The task detail page shows running, idle, and paused states based on the real LLM-call state; a source task's facts, vulnerabilities, intents, asset references, and graph nodes are uniformly marked with their source and kept read-only.
- Agent preview, task reports, and related detail views uniformly use a shared Markdown rendering component.
- LLM model configuration uses a card and drawer interaction, with the model list supporting independent scrolling within the drawer.
- Pausing a task no longer blocks the main Agent conversation: the main-Agent orchestration session is independent of the task pause, and new messages can still be sent during the pause (the pause only terminates the round currently in progress).
- The wall-clock default duration of a single Worker run was changed from 600 seconds to 1200 seconds.

### Fixed

- Removed the main Agent console's optimistic echo in favor of pure server-side data rendering, fixing message disorder and content bleeding into other sessions under pause or send-failure scenarios.
- Opening a main Agent session scrolls to the bottom by default and fully shows the last reply: the full content of the last reply auto-sticks to the bottom after lazy-loading and is no longer pushed off screen.
- Fixed the issue where the main Agent session kept running after a task was paused, and where the orchestration Agent didn't stop the main Agent in sync when pausing a task.
- Fixed the issue where the task status badge and the action-button state were out of sync while the task completed or the Planner, Worker, or main Agent was actually running.
- Fixed the issue where late blackboard writes or residual files could occur between Worker cancellation, task deletion, and concurrent writes.
- Fixed issues with Markdown preview not working, delete-confirmation long text overflowing, mobile width, and some task-detail buttons being misaligned.
- Added named termination reasons to all Agent cancellation paths, so activity detail can show the canceller, final state, number of run rounds, elapsed time, token usage, and unreturned tool calls.
- Fixed a race where, on backend shutdown, the parent context could preemptively overwrite the named `shutdown` reason, and the issue where a Chinese activity summary was garbled by byte-based truncation.
- Fixed the issue where a cancellation event with partial streaming output lost the real termination reason, while preserving the content generated before cancellation.

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
