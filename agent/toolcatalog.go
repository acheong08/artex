package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns built-in tools into an enumerable catalog that can be overridden
// in the database:
//   - BuiltinToolSeeds() expands the built-in tool sets for execution agents into
//     seed records (key, description, parameter schema, and default agent bindings),
//     which the server idempotently inserts into the tools table at startup.
//   - ToolResolve filters assembled tools by agent, overrides descriptions/schemas,
//     and injects parameter defaults based on database rows. Keys and handlers remain
//     code-owned; the database changes only model-facing prose and defaults.
// Call behavior always comes from code. The database can change only descriptions
// and default arguments visible to the model.

// ToolSeed is a seed snapshot of a built-in tool: key is CoreTool.Name() (tied to
// its handler and read-only in the UI), Desc/Schema come from the code definition,
// and Agents lists the agents to which code binds it by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(); immutable primary key.
	Desc   string         // Top-level description (overridable in the UI).
	Schema map[string]any // Parameter JSON Schema (structure is read-only; description/default are editable).
	Agents []string       // Agent keys bound by default (worker/planner/mainagent).
}

// builtinToolsByAgent builds each execution agent's domain tool set using a
// read-only ToolSet shell (nil stores). Constructors only put closures into Specs
// and do not dereference stores, so nil is safe. These tools are used only to read
// Name()/Description()/InputSchema(); they are never called here.
//
// Intentionally excludes SDK common tools actool.DefaultTools() (Read/Write/Edit/
// MultiEdit/LS/Glob/Grep/Bash): every agent always owns these, so there is no binding
// decision to make, and their descriptions mostly come from Prompt(). This catalog
// covers only Description(), so seeding them would misleadingly override only part
// of the prompt. Unseeded tools have no DB row and pass through ToolResolve unchanged,
// preserving prior behavior. Only artex domain tools are managed here.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals decomposer defaults to set_goals + set_constraints to store its goals
		// and extracted constraints. It shares these managed tools with mainagent;
		// descriptions/schemas can be edited and bindings selected per agent in the web UI.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto defaults to finding-reporting and asset-management tools; other domain
		// tools can be selected in the UI. New databases are seeded here; old ones are
		// migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (standalone penetration-testing agent) defaults to listing/inserting
		// assets, reporting/listing findings, and listing companies. New databases are
		// seeded here; old ones are migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound lists system tools that remain in the catalog (visible in the web
// UI and manually bindable per agent) but are bound to no agent by default.
// ToolResolve drops tools with empty bindings for every agent, requiring explicit
// opt-in. They remain in an agent's base tool set (e.g. goal_met in PlannerTools) so
// seeding can read their descriptions/schemas and a manually restored binding can
// be retained by ToolResolve at runtime.
//
// goal_met bypasses individual prove_goal calls and globally declares the entire
// task complete. It is consequential, risks false positives, and duplicates the
// automatic completion after the final prove_goal, so no agent gets it by default.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds merges the agents' built-in tool sets into a deduplicated seed
// list: same-name tools (e.g. list_assets) become one record with the union of agent
// bindings; tools in defaultUnbound are forced to have no bindings.
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
			agents = []string{} // Catalogued and manually bindable, but unbound by default ([] rather than null).
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
// Default arguments are injected. Tools with no matching DB row (MCP/skill/host tools like
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
// null. Structure (names/types/required) is untouched; only defaults are merged in.
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
