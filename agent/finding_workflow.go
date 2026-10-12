package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**Vulnerability-ID convention**: finding_id is the standalone vulnerability-record ID; finding_node_id is the exploration node ID. The id of list_findings / list_task_findings / node_detail / get_task_node_detail stays the exploration node ID, and the standalone number should be read from the finding_id in the same return. get_finding_traffic / bind_finding_traffic use the standalone finding_id. The legacy update_finding_report's finding_id parameter still takes finding_node_id. Don't use the number on report_finding's first line for the evidence tools, and don't guess other numbers after hitting an ID error."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\nBy default the report Agent verifies and binds the traffic before writing the report. The reporter keeps the verification command, key output, and the real traffic IDs already on hand plus their purpose in evidence, for the report Agent to verify against the execution records; no extra packet lookup is needed for binding. Explicit immediate binding is supported: traffic_refs or evidence_hint_id can submit verified references, the latter reading the structured references of a specified hint in this task; if either is invalid the whole report fails. TCP / no packets don't need these optional parameters. The returned finding_id and finding_node_id denote the standalone record and the exploration node respectively."
		case "add_hint", "add_task_hint":
			note = "\nWhen handing off a confirmed vulnerability, keep the verified traffic's ID, purpose, note, and order in the corresponding hint's traffic_refs (a single one at the top level, a batch in the corresponding hints elements), and state in text the specific vulnerability it proves. The caller must not hand off only text while discarding existing traffic references. Unverified candidates must not be passed on as evidence."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**Traffic-evidence handoff (optional)**: automatic binding is by default done by the report Agent after the vulnerability is stored and before the report is written. The reporter should keep the verification command, key output, and the real traffic IDs already on hand plus their purpose in evidence, carrying intent_id within a task so the report Agent can trace it; no extra packet lookup is needed for binding. When Auto / Planner report on someone's behalf, don't discard the executor's existing references. add_hint / add_task_hint can hand off via traffic_refs; explicit immediate binding is still compatible with report_finding's traffic_refs / evidence_hint_id. For TCP or no packets, register normally — don't guess IDs, and don't re-probe just to capture packets."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nWhen a platform conversation has no task context, don't call report_finding directly; hand off to an existing corresponding task via add_task_hint, let the task Agent register it, and verify the result with list_task_findings."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nBefore judging a goal complete, first finish reporting/handing off the evidence already on hand this time. Don't end the task or cancel the Worker merely because a textual vulnerability is registered while the evidence handoff is not yet done; no packets does not require waiting or forcing a capture."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**Auto-associate traffic before the report (enabled)**: you are responsible for verifying and binding the traffic for the vulnerability triggered this time, then writing the report. First obtain the explicit finding_id and finding_node_id from report_finding's returned JSON or from get_task_node_detail / list_task_findings. Read the vulnerability details, the execution records of the corresponding intent, and the existing evidence list, preferring the real IDs handed off by the reporter. If this verification is HTTP and the traffic tools are available, use traffic_search to filter candidates, then traffic_get to verify one by one that the request/response really supports the vulnerability; domain and time are used only for filtering and do not prove ownership. Associate the confirmed evidence in reproduction order via bind_finding_traffic(finding_id, traffic_refs), choosing baseline / proof / verification / supporting and stating its purpose. Operate only on this vulnerability; do not re-create vulnerabilities or re-probe the target. After binding succeeds, call get_finding_traffic again to get the latest version, read the needed body, then pass the version actually read as evidence_version to update_finding_report (whose finding_id parameter still uses finding_node_id). Existing bindings need not be re-appended. For TCP, nothing captured, a tool unavailable, or no exact match, skip auto-binding, write the report normally from textual/command evidence and state the reason — do not guess to fill out the traffic. On a binding failure don't claim success; keep the existing evidence and state in the report why nothing was bound."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Optional: verified traffic references corresponding to the specific vulnerability in this hint, preserving order; after handoff, report_finding can pass evidence_hint_id to carry these references.", "items": obj(map[string]any{"traffic_id": str("the real traffic ID"), "role": str("baseline / proof / verification / supporting"), "note": str("what conclusion this traffic supports")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d must be a hint node of this task (inherited hints cannot be used directly for binding)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
