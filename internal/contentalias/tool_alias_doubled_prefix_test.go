package contentalias

import (
	"bytes"
	"testing"
)

// t_8621d5da (2026-10-29 09:09 PT, claude-dpx-24, bridge id 8c04806b): the
// model emitted dpx_v1_dpx_v1_t_acorn_brick, a declared v3 alias with the
// dpx_v1_ prefix written twice. strayClassPattern reads that as class 'd'
// followed by 'p', so it was not alias-shaped and the turn was refused with
// unknown_tool, after the #42 marker net had shipped. A repeated prefix is a
// copy slip like any other and must leave DPX as the marker.
func TestToolAliasDoubledPrefixBecomesMarker(t *testing.T) {
	_, m, alias, key := preparedTool(t)
	for _, bad := range []string{
		"dpx_v1_" + alias,
		"dpx_v1_dpx_v1_" + alias,
		"dpx_v1_dpx_v1_t_acorn_brick",
	} {
		out, err := streamAllSplits(t, m, toolEvents(bad, key))
		if err != nil {
			t.Fatalf("stream %s: turn refused: %v", bad, err)
		}
		if !bytes.Contains(out, []byte(`"name":"unknown_tool_1"`)) || bytes.Contains(out, []byte("dpx_v1_")) {
			t.Fatalf("stream %s: want marker, no alias token: %s", bad, out)
		}
		if bytes.Contains(out, []byte(`"name":"Bash"`)) {
			t.Fatalf("stream %s: repaired to a declared tool: %s", bad, out)
		}
		jout, err := m.RestoreJSON(jsonToolUse(bad, key))
		if err != nil {
			t.Fatalf("json %s: turn refused: %v", bad, err)
		}
		if !bytes.Contains(jout, []byte(`"name":"unknown_tool_1"`)) || bytes.Contains(jout, []byte("dpx_v1_")) {
			t.Fatalf("json %s: want marker, no alias token: %s", bad, jout)
		}
	}
}

// A doubled prefix around a name that is not in the codec namespace at all is
// still not a copy slip: raw names pass through verbatim (t_3f4b6bd1).
func TestToolDoubledPrefixNonAliasPassesThrough(t *testing.T) {
	_, m, _, key := preparedTool(t)
	for _, name := range []string{"xdpx_v1_dpx_v1_t_acorn_brick", "DPX_V1_DPX_V1_t_acorn_brick", "exec"} {
		assertRawToolPassesThrough(t, m, name, key)
	}
}
