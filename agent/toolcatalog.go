package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns the "built-in tools" from pure code into a catalog that can be
// enumerated and overridden by the DB:
//   - BuiltinToolSeeds(): expands the three executing agents' built-in toolsets into
//     seed records (key + description + parameter schema + the agents bound by default),
//     for the server to idempotently seed into the tools table on startup.
//   - the ToolResolve hook: at runtime, per the DB tools rows, it does "filter by agent +
//     override description/schema + inject parameter defaults" on the assembled tools.
//     key/handler stay in code; the DB only changes "the prose and defaults".
// The handler (Call behavior) always comes from code — the DB can't change it, only the
// description the model sees and the default inputs.

// ToolSeed is a seedable snapshot of one built-in tool: key is CoreTool.Name() (bound to
// the handler, read-only in the UI), Desc/Schema come from the tool definition in code,
// and Agents is which agents the code gives it to by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, immutable
	Desc   string         // top-level description (overridable in the UI)
	Schema map[string]any // parameter JSON-Schema (structure read-only; description/default editable in the UI)
	Agents []string       // agent keys bound by default (worker/planner/mainagent)
}

// builtinToolsByAgent constructs each executing agent's domain toolset using a "read-only
// shell" ToolSet (nil stores). Tool constructors only stuff a closure into the Spec and do
// not dereference the store at construction time, so nil is safe — these tools are used here
// only to read Name()/Description()/InputSchema(), never to Call.
//
// It deliberately [excludes] the SDK general tools actool.DefaultTools() (Read/Write/Edit/
// MultiEdit/LS/Glob/Grep/Bash): every agent always has them, there is no "bound to whom"
// choice, and their description is mostly in Prompt() (this table only overrides Description(),
// which would cause misleading half-coverage). Not seeded → no DB row → ToolResolve passes
// them through unchanged, behavior identical to before. Only artex's own domain tools enter the
// table to be managed.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (the goal decomposer) binds set_goals + set_constraints by default: it relies on them to write
		// the decomposed goals and extracted operation constraints into the DB. It shares the same managed tools
		// as mainagent; the web side can edit the description/schema and check per agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto binds the finding-reporting + asset-management tools by default; other domain tools can be checked
		// as needed in the UI. A new DB is seeded from here; an old DB is migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (the solo pentest agent) binds by default: list assets / insert assets / report findings /
		// list findings / list companies. A new DB is seeded from here; an old DB is migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools still enter the catalog as usual (visible on the web side,
// manually checkable per agent), but are [bound to no agent] by default — ToolResolve drops a
// tool with an empty binding for all agents, so it must be explicitly opted into. The reason one
// is still kept in some agent's base toolset (e.g. goal_met in PlannerTools) is twofold: first, so
// the seed can construct it to get its desc/schema; second, so that after the user manually binds
// it back, it is present in base at runtime for ToolResolve to keep.
//
// goal_met: bypasses proving goals one by one and directly declares [the whole task complete] from
// the global view — high-weight, with misjudgment risk, and redundant with "prove_goal marking the
// last goal → auto close-out", so it is given to no agent by default and bound manually when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds dedupes and merges each agent's built-in toolset into a seed list: a same-named
// tool (e.g. list_assets, which multiple agents have) becomes one entry with Agents as the union;
// tools in defaultUnbound have their binding forced to empty.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // enters the catalog, manually bindable, but given to no agent by default (store [] not null, consistent with other tools)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// the default inputs get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only the default values are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
