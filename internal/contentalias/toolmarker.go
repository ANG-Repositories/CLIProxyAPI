package contentalias

import (
	"strconv"
	"strings"
)

// Near-copied tool aliases (t_7bd8c9b9). Tool aliases are dpx_v1_t_<24 hex>.
// The model mis-copies them at ~0.16% of tool calls (2026-10-25..27: 10 of
// 6,347): a 2-hex slip (...223191dce for ...223158dce) or a tail rewritten as a
// word (...648c7seed for ...648c7ce0). Refusing the response with unknown_tool
// failed the whole turn and reaped the caller's session.
//
// A response tool_use whose name is alias-SHAPED (strayClassPattern, the same
// net the property marker uses) but not declared by this request leaves DPX as
// unknownToolMarker with input {}. The caller has no tool of that name and
// answers with its own unknown-tool error, which names its tools (Hermes:
// "Tool 'unknown_tool_1' does not exist. Available tools: ..."). The model
// re-calls on the next step. There is no nearest-neighbour repair: two declared
// aliases can sit a few characters apart, and a guess could run the wrong tool.
//
// The marker is a plain tool-name token (^[a-zA-Z0-9_-]{1,64}$), unlike the
// property marker unknown_key_N (...): the caller stores it as the tool_use
// name in history, and a later hop that does not run through DPX (fallback to
// another lane) sends that history to Anthropic raw, which validates names.
//
// A name that is not alias-shaped (for example a raw "exec" or
// "process_manage") passes through to the caller verbatim, name and input as
// emitted (t_3f4b6bd1). It is not a copy slip: the model saw that raw name in
// prompt prose (2026-11-01 06:03, claude-dpx-24, wire 89655-1147: Hermes's
// deferred-tool catalog lists process_manage, which is not a declared tool).
// The caller answers it the way it answers any model call to a tool it lacks
// (Claude Code: "No such tool available"), and the turn continues. Refusing it
// with unknown_tool failed the turn and reaped the caller's session.
//
// The marker contains no dpx_v1_ token, so the next request's history maps it
// with historyToolAlias like any other history-only name: a deterministic,
// non-callable alias, no history_tool refusal.
func unknownToolMarker(i int) string {
	return "unknown_tool_" + strconv.Itoa(i)
}

// aliasShapedToolName reports whether a response tool_use name is a copy slip
// of an alias: any name in the dpx_v1_ codec namespace. No declared tool can
// carry that prefix (Prepare refuses it as reserved_tool), and the model only
// sees aliases, so an undeclared dpx_v1_ name is always a mangled alias.
// Narrower shapes kept missing live slips and killing turns:
//   - t_8621d5da (10-29, claude-dpx-24): dpx_v1_dpx_v1_t_acorn_brick, prefix doubled.
//   - t_388a6308 (10-30, claude-dpx-11): dpx_v1_stork_couch_placeholder for
//     dpx_v1_t_stork_couch, class letter dropped and a word tail added.
func aliasShapedToolName(name string) bool {
	return strings.HasPrefix(name, "dpx_v1_")
}
