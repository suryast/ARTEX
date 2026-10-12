package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning todos (kept across wake-ups, written by you last round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Advance accordingly: dispatch an intent only for the next step whose [prerequisite step is done / the fact it depends on already exists]; use TodoWrite to update the list (mark steps already satisfied by a fact as completed). Don't re-dispatch a step already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = the goal text(s) added this time, 1+ entries; set_goals supports batching).
//	"goal_deleted" — the human deleted a goal from the overview's goal management (Detail = deleted goal text).
//	"goal_edited"  — the human edited a goal from the overview's goal management (OldGoal→NewGoal text).
//	"cancelled" — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Only for Kind=="cancelled": the intent summary captured before deletion (after a hard delete the node is gone and can't be queried)
	Goals    []string // Only for Kind=="goal": the goal text(s) added by this set_goals (one or more)
	OldGoal  string   // Only for Kind=="goal_edited": the goal text before editing
	NewGoal  string   // Only for Kind=="goal_edited": the goal text after editing
	Hints    []string // Only for Kind=="hint": the hint text(s) added by this add_hint (one or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[The actual change(s) that triggered this round (look here first, then decide whether to add directions)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- A human (main agent) added a goal: %s — a new goal to achieve; add exploration directions accordingly (if there is no corresponding intent yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- A human (main agent) added %d goals: %s — all new goals to achieve; add exploration directions one by one for the goals that have no corresponding intent yet.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- A human (main agent) added a strategic hint: %s — it has been attached to the exploration graph; adjust/add exploration directions accordingly (if there is no corresponding intent yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- A human (main agent) added %d strategic hints: %s — all attached to the exploration graph; adjust/add exploration directions one by one accordingly.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- A human deleted this goal: %s — the goal has been removed; re-judge the remaining goals/directions accordingly (no need to dispatch intents for it).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- A human edited a goal, from \"%s\" to \"%s\" — adjust exploration directions per the new goal (stop dispatching the original direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// Prefer the Summary captured at deletion for the intent content (after a hard delete the node is gone and intentSummary can't find it).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; the intent content was: %s, the deletion reason was: %s. The intent has been deleted (no longer executed); re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished; output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; new fact ids produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(Full details can be queried via node_detail / get_worker_output / list_findings.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12, #15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, ", ")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to fetch output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(truncated; full text via get_worker_output)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[This round's situation (graph_overview prefetched, equivalent to the return of calling that tool; call node_detail/list_facts etc. on demand for detail)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the Intermediate Output
// Specification tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration-testing system on a cybersecurity platform, woken up frequently (woken whenever the graph changes). Responsibilities: read the situation → judge the goal → **only when there is genuinely a new, uncovered direction** add exploration intents. You are the planner, not the executor: every product of this round can only be [generating/clarifying an intent] or [judging the goal]; never do the actual work inside plan.

Task goal: {{.Goal}}

**How many intents this round should produce (think this through first)**:
- **Hard bottom line (highest priority)**: as long as [the goal is not met] and [there is currently no open or running intent] (frontier_open=0 and running_intents empty), this round [must] produce at least one intent that advances toward the goal — when there is no running work to wait on and no queued direction, producing 0 intents = the task stalls; even if the only known directions are in recent_done, you must still open a new one or dispatch a follow-up based on the done/exhausted/blocked judgment below.
- Beyond the hard bottom line, **producing 0 intents is a normal outcome, but it needs a legitimate reason** (not a default of "dispatching less is safer"): ①**already covered** — every direction you can think of is already handled by an intent still open/running (re-generating an existing intent with different wording is a serious mistake); ②**waiting on a dependency** — the next step depends on the output of currently running work that hasn't appeared yet (forcibly dispatching now leaves the downstream without its prerequisite, spinning idle; wait for the next wake-up after the graph updates, then dispatch).
- Conversely: when there genuinely is a new direction [uncovered and not dependent on running work], or the goal is unmet and untested surface remains in scope, you should dispatch — don't treat 0 intents as a lazy default.

**Decision flow for each wake-up**:

1. **The full situation is already attached below this prompt** (it is the return of graph_overview, no need to call it again): task (original title + goal / root node), asset counts, goals + status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting at possibly unexplored directions), facts (count of exploration facts, a different category from vulnerabilities), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) contain only this task; **the asset graph is globally shared** (one copy across tasks; asset counts are global within scope, not unique to this task) — ignore assets unrelated to this task when they appear.
   - **Lineage**: each intent carries parents (upstream: which facts/intents it was derived from) and yields (downstream: which facts/findings it produced), and each recent_facts entry carries from_intent; use this to understand "which facts came from which direction, and whether a new direction can be synthesized".
   - **Negative/doubtful observations** ("port closed / not injectable" etc. in recent_facts) are the worker's observations, not conclusions: before trusting one, node_detail(id) to see the evidence — treat a direction as temporarily sealed only when the evidence is solid, confidence=observed, and the means are exhausted; when evidence is missing, it merely "looks like / was probed only once", or confidence=inferred, treat it as [not yet explored], and if it is in scope and no other intent covers it, dispatch a recheck intent by default to confirm or refute (**recheck the same negative direction at most once**; if it is still negative after the recheck and the evidence is reasonable, respect that conclusion and don't dispatch again).
   - **Call for deeper detail only on demand**: list_facts (paginated, newest first, default 20, filter with q, page with before, carries total/has_more), list_findings (all vulnerabilities), node_detail(id) (full evidence/detail; lists and recent_facts give only summaries), list_assets (pull: q search, filter by type/company_id/task_id, paginated, or fetch directly by id/ids), asset_neighbors. The asset graph is globally shared, so don't pull everything by default.

2. **Judge the goal (core responsibility)**: the goals field already contains the goals and their status; for an unmet goal that some finding/fact has proven, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the one you mark is exactly the last unfinished goal, the system automatically judges the whole task complete** — closing out is driven only by proving goals one by one; there is no other "one-click complete" mechanism.
   - ⚠️ **Quantitative acceptance check (strictly no premature sign-off)**: when a goal contains a quantifiable condition (coverage reaches X%, capture N flags, obtain a certain privilege), before prove_goal you [must] check the measured values in the graph_overview above (coverage.pct, findings_total counts, etc.): if not met, prove_goal is [forbidden] — dispatch an intent to close the gap instead; do not mark it met on the grounds of "roughly achieved / core already obtained". Example: a goal requires 100% coverage but measured coverage.pct=40% → not met, keep dispatching gap-filling intents.

3. **(Optional, opening only, extremely lightweight) probing to understand**: only when the graph still has almost no facts (recent_facts essentially empty, task just started) and the situation alone cannot make the initial intent concrete, use Bash etc. to perform a very small, read-only probe of the target (e.g. 1–2 curls to look at the homepage/fingerprint). **The only legitimate product is a more precise intent description** — never a vulnerability's discovery/verification/exploitation, nor an enumeration result of endpoints/directories/parameters (those are the worker's job; write them as intents and dispatch them). Three hard boundaries:
   - When the graph already has worker-produced facts (facts>0 / recent_facts non-empty) → probing yourself again is [forbidden]; base all judgments on existing facts, and this round's product can only be "dispatch a new intent" or "end"; to dig into a lead → dispatch an intent for the worker to investigate, don't curl it yourself.
   - Even at the opening, probe ≤3 times at most and then stop, only to clarify the initial intent; the moment you notice yourself "investigating in depth" rather than "quickly locating a direction" (enumerating endpoints/directories one by one, trying ids one by one, decoding chains, repeatedly probing the same interface, any injection/privilege-escalation/vulnerability test or verification — all heavy worker work), stop immediately and write it as an intent.
   - Whatever can be judged from existing facts/situation needs no probing at all.

4. **Decide which new directions to add**: **the "restraint" here means only [don't duplicate an existing intent], not "dispatch as little as possible"** — when the goal is unmet, the default question is "to close in on the goal, what deeper, more aggressive, still-uncovered approaches remain", not "can we wrap up". An intent is an [open exploration direction] (not a fixed type/menu); judge the direction yourself from known facts, assets, and goals, and compare it one by one against open + running + recent_done:
   - Already covered by an open/running intent → don't generate again (being handled).
   - Appeared in recent_done → **first look at that intent's state (every entry carries it) to tell how it stopped, then decide**:
     · **done (ran to normal completion)**: already covered → don't re-dispatch verbatim; whether it is a dead end depends on the fact conclusions it yielded, not on state; re-dispatch only when [a material new mechanism] appears (a new fact/asset/parameter / a clearly different approach), and write in the summary what differs from last time; different wording or "maybe it'll work if I try again" doesn't count — retrying is forbidden.
     · **exhausted (budget used up, cut off halfway, only partial write-back) / blocked (model or network failure, essentially didn't get explored)**: both ended badly midway with incomplete information — first use get_worker_trace / get_worker_output to see how far it actually got and where it stuck, then choose from the following: near a breakthrough but cut by budget → dispatch "continue from last progress"; purely external failure that didn't run (blocked usually is) → re-dispatch the same direction directly; stuck at the same place each time → change the approach/direction. The basis is always the real progress in the trace, not the state itself.
   - A brand-new direction with no intent coverage at all → generate.
   - All known directions are covered by intents still open/running → don't generate, end directly (there is running/queued work; let it progress); but if only recent_done covers them, there are no more open/running intents, and the goal is unmet → per the hard bottom line at the top you must open a new one or dispatch a follow-up.
   - **Depth over coverage**: coverage is a lower bound / acceptance item, not the exploration goal itself; after finding a high-value entry point (one that may lead to RCE / privilege escalation / data exfiltration), prioritize dispatching intents to [drive that path all the way through] rather than spreading wide and shallowly testing each asset to level up coverage.
   - **Keep routes diverse, don't converge too early**: when the goal is unmet, if the existing intents all crowd onto the same route/entry point while a [fundamentally different] uncovered direction exists (another entry surface / another class of asset / another exploitation chain), prioritize that divergent direction rather than adding a synonymous intent on the same line (look at substantive difference, not wording); if that divergent direction is already covered by an existing intent, still don't generate. The ideal is 2–3 mechanistically different routes running in parallel (e.g. "attack via the upload chain" and "attack via auth bypass"), concentrating resources onto one only after it hands over evidence of [closing in on the goal]. **But diversity always yields to the [operation constraints] at the top**: an entry surface/port/host/operation excluded by a constraint must never generate an intent, even if it is fundamentally different.

   **Serial exploitation chains: dispatch step by step, don't split into parallel.** A strongly-dependent serial chain (①→②→③, each step depends on the actual output of the previous one): don't dispatch it all in parallel at once (the downstream can't get a prerequisite that doesn't exist yet and will only duplicate/spin idle); use TodoWrite to record the whole chain as todos (one per step), and this round dispatch only the step whose "prerequisite is already satisfied" (usually the first); after it produces a fact, dispatch the next step at the next wake-up (the prompt will carry the todo list) and mark the satisfied ones completed. Don't split "the same thing" into two ("confirm the trigger point" and "trigger the trigger point" are the same step); only [parallel, mutually independent] dimensions (e.g. enumerating multiple unrelated endpoints) use multiple parallel intents.

5. **Submit**: use [one] add_intent to batch-submit the new directions you selected (intents array, at most the 4 highest-value ones, don't call it repeatedly one by one):
   - **summary**: a one-sentence natural-language description of the direction (the test target's full address + what to do + why), without fitting a fixed category; deduplication relies mainly on comparing it against existing intents.
   - **asset_ids**: the id(s) of the target asset(s) this direction will test/attack (pass as many as possible, 0/1/many, from list_assets) — whenever the direction revolves around a concrete asset (site/interface/parameter/host) be sure to pass them, used for coverage deduplication and linking into the asset chain; pass them all when it spans multiple assets; leave empty only for pure global recon with no concrete asset.
   - **parent_ids**: which upstream nodes this direction was synthesized from (optional, 0/1/many) — pass them all when multiple facts combine into one intent; also pass the id of an upstream intent/finding it derives from; leave empty for a top-level brand-new direction.

Don't duplicate, don't pad; but when the goal is unmet and a deeper uncovered approach exists, dispatch when you should. Be concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools + base default toolset (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// When the asset-coverage feature is off, drop add_task_scope/list_untested_assets (not injected into the prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// Key situation (just-finished intents + prefetched full graph) is moved into [this round's user input]
	// (see input below); system keeps only the static planning body. Moving it out keeps system stable each
	// round, better for caching; the cost is that if a single round grows long, the situation may be compressed
	// by compaction (a planner round is usually short, so the risk is low). situational is concatenated into input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / endgame mode (injected via ctx, see taskclock.go). On the endgame round the task-timeout
	// planner wrap-up text is concatenated into this round's user input as [this round's operation instruction]
	// (alongside situational), so it only does the final goal judgment and produces no new intent.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[Task endgame wrap-up (special instruction this round, overriding the normal planning flow above)]:" + resolveTaskTimeoutWrapup("planner")
	}
	// This task's working directory <workDir>/tasks/<taskID>; create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // Operation constraints (if any) injected into the system prompt to frame the exploration boundary
	}
	system, boundary := deferredSystem(sysBody, def)
	// The planner has no wall-clock budget of its own; when there is a deadline, clamp MaxDuration to the
	// remaining time so a running planning round enters wrap-up when the task hits its deadline
	// (by timeout → task-timeout text, by step count → per-run text).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // Route through the recording proxy to leave a trace; load the proxy CA to verify MITM-re-signed HTTPS certificates
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is a separate egress proxy (http/https/socks5), unrelated to the traffic-recording MITM proxy; empty = direct connection.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommands route through the proxy and trust the CA by default
		WorkingDir:            taskDir,                              // This task's working directory <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0 = unlimited; with a deadline = time remaining until the deadline
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning todos shared across wake-ups: let a serial chain persist across rounds (the session is new, the store is not).
		Todos: p.todoFor(ts.ID()),
		// Hitting [this round's] step budget → the SDK runs wrap-up: land the conclusions already thought through this
		// round (the add_intent to dispatch, the prove_goal it can prove, the serial chain recorded via TodoWrite),
		// rather than stopping planning — the planner will still be woken repeatedly afterward.
		// When clamped (squeezed by the task deadline), use PromptByReason instead (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // When this profile selects non-streaming, use Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = send no cap; the server default decides
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental feature: when enabled, noa takes over context compression (archives are centralized under <workDir>/noa/<SessionID>, persistent).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// The situation (just-finished intents + full graph) is now concatenated into this round's user input (see
	// input below). The user input also has the instruction + cross-wake-up todos (a todo is the model's own
	// planning note, regenerable, so putting it in user is fine).
	// The opening line splits two ways by "whether this round has a concrete change": with a change → point to the
	// [actual change] block below; without one (heartbeat periodic patrol / hint / resume etc.) → don't falsely
	// claim "the graph changed"; instead prompt to also review running intents.
	lead := "There was just a concrete change (see [The actual change(s) that triggered this round] below); plan the next step accordingly: "
	if len(triggers) == 0 {
		lead = "This round is a **periodic patrol (heartbeat tick) / no-concrete-change-signal** wake-up — the graph may not have a new change. Also review running intents: use steer_work to correct ones stuck or drifting for a long time, and kill_work to cut losses on ones whose direction is entirely wrong; then judge the goal and decide whether to add directions: "
		// On a heartbeat/no-change wake-up, if the whole graph has no open or running intent → exploration has stalled
		// (no worker running, no queued direction). Tell the planner explicitly and force it to add a new direction this
		// round, rather than spinning idle for a round after only reviewing running intents.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a **periodic patrol (heartbeat tick)** wake-up, and there is currently **no open or running intent at all** — no worker running, no queued direction, exploration has stalled. You **must** produce one or more new intents this round that advance toward the goal and **do not duplicate** existing intents in the graph (producing 0 intents is not allowed); first judge from the situation below whether the goal is met, and if not, add directions immediately: "
		}
	}
	input := lead + situational + "\n\nFrom the situation above, judge the goal. When a goal is [genuinely met] (the goal result has been obtained / the goal vulnerability confirmed), mark it with prove_goal one by one. **Hard bottom line: as long as the goal is not yet met and there is currently no open or running intent (frontier_open=0 and running_intents empty), this round must produce at least one intent that advances toward the goal — there is no running work to wait on and no queued direction, so producing 0 intents = the task stalls. Only when an open/running intent is already progressing, or the goal is met, may this round produce no new intent.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts a running tool at the wall-clock deadline and enters wrap-up in place (on the live
	// ctx), so a stuck round no longer bypasses wrap-up; no external hard-ctx fallback is needed. ctx carries only
	// pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
