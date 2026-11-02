package contentalias

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// t_7bd8c9b9 (2026-10-27 05:01/05:08 PT, claude-dpx-3): the model emitted a
// near-copy of a declared tool alias and DPX refused the response with
// unknown_tool, which failed the whole turn. The live slip shapes:
//   emitted dpx_v1_t_918e1f4ed50b193223191dce for ...223158dce (2 hex off)
//   emitted dpx_v1_t_54cc7511020bdcab648c7seed for ...648c7ce0 (word tail)
// A near-copy must leave DPX as an unknown-tool marker, never refuse the turn
// and never be repaired to a neighbour.

func nearCopySlips(alias string) map[string]string {
	n := len(alias)
	return map[string]string{
		"two_hex_slip": alias[:n-5] + flipHex(alias[n-5]) + flipHex(alias[n-4]) + alias[n-3:],
		"word_tail":    alias[:n-4] + "seed",
		"one_char":     alias[:n-1] + flipHex(alias[n-1]),
	}
}

func flipHex(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}

func preparedTool(t *testing.T) (*Session, *RequestMap, string, string) {
	t.Helper()
	s := testSession(t)
	wire, m, err := Prepare([]byte(requestFixture), s)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(wire)
	tool := n.get("tools").items[0]
	key := tool.get("input_schema").get("properties").fields[0].key.text
	return s, m, tool.get("name").str(), key
}

func streamAllSplits(t *testing.T, m *RequestMap, raw []byte) ([]byte, error) {
	t.Helper()
	var baseline []byte
	for split := 0; split <= len(raw); split++ {
		st := m.NewStream()
		a, err := st.Feed(raw[:split])
		if err != nil {
			return nil, err
		}
		b, err := st.Feed(raw[split:])
		if err != nil {
			return nil, err
		}
		c, err := st.Finish()
		if err != nil {
			return nil, err
		}
		got := bytes.Join([][]byte{a, b, c}, nil)
		if split == 0 {
			baseline = got
		} else if !bytes.Equal(baseline, got) {
			t.Fatalf("split %d: output differs from split 0", split)
		}
	}
	return baseline, nil
}

func jsonToolUse(name, key string) []byte {
	return []byte(`{"content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_9","name":"` + name + `","input":{"` + key + `":"Hermes Hermes"}}]}`)
}

func TestToolAliasNearCopyStreamBecomesMarker(t *testing.T) {
	_, m, alias, key := preparedTool(t)
	for shape, bad := range nearCopySlips(alias) {
		if bad == alias {
			t.Fatalf("%s: slip equals alias", shape)
		}
		out, err := streamAllSplits(t, m, toolEvents(bad, key))
		if err != nil {
			t.Fatalf("%s %s: turn refused: %v", shape, bad, err)
		}
		if !bytes.Contains(out, []byte(`"name":"unknown_tool_1"`)) {
			t.Fatalf("%s: marker missing: %s", shape, out)
		}
		if bytes.Contains(out, []byte("dpx_v1_")) {
			t.Fatalf("%s: alias token left DPX: %s", shape, out)
		}
		if bytes.Contains(out, []byte(`"name":"Bash"`)) || bytes.Contains(out, []byte(`"name":"Read"`)) {
			t.Fatalf("%s: near-copy repaired to a declared tool: %s", shape, out)
		}
		if !bytes.Contains(out, []byte(`"partial_json":"{}"`)) {
			t.Fatalf("%s: marker call input not emptied: %s", shape, out)
		}
	}
}

func TestToolAliasNearCopyJSONBecomesMarker(t *testing.T) {
	_, m, alias, key := preparedTool(t)
	for shape, bad := range nearCopySlips(alias) {
		out, err := m.RestoreJSON(jsonToolUse(bad, key))
		if err != nil {
			t.Fatalf("%s %s: turn refused: %v", shape, bad, err)
		}
		n, err := parse(out)
		if err != nil {
			t.Fatal(err)
		}
		block := n.get("content").items[1]
		if got := block.get("name").str(); got != "unknown_tool_1" {
			t.Fatalf("%s: name = %q, want unknown_tool_1", shape, got)
		}
		if in := block.get("input"); in == nil || in.kind != '{' || len(in.fields) != 0 {
			t.Fatalf("%s: marker call input not emptied: %s", shape, out)
		}
		if bytes.Contains(out, []byte("dpx_v1_")) {
			t.Fatalf("%s: alias token left DPX: %s", shape, out)
		}
	}
	// Two slips in one response get distinct markers.
	slips := nearCopySlips(alias)
	two := []byte(`{"content":[{"type":"tool_use","id":"a","name":"` + slips["one_char"] + `","input":{}},{"type":"tool_use","id":"b","name":"` + slips["word_tail"] + `","input":{}}]}`)
	out, err := m.RestoreJSON(two)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"unknown_tool_1"`)) || !bytes.Contains(out, []byte(`"unknown_tool_2"`)) {
		t.Fatalf("markers not numbered per response: %s", out)
	}
}

func TestToolAliasExactStillRestores(t *testing.T) {
	_, m, alias, key := preparedTool(t)
	out, err := streamAllSplits(t, m, toolEvents(alias, key))
	if err != nil {
		t.Fatalf("exact alias refused: %v", err)
	}
	if !bytes.Contains(out, []byte(`"name":"Bash"`)) || bytes.Contains(out, []byte("unknown_tool")) {
		t.Fatalf("exact alias not restored: %s", out)
	}
	jout, err := m.RestoreJSON(jsonToolUse(alias, key))
	if err != nil || !bytes.Contains(jout, []byte(`"name":"Bash"`)) || !bytes.Contains(jout, []byte(`"command"`)) {
		t.Fatalf("exact alias not restored (json): %v %s", err, jout)
	}
}

// A name that is not alias-shaped is not a copy slip (t_3f4b6bd1): the caller
// gets the model's real call, byte for byte, on both paths, and answers it
// with its own unknown-tool result.
func TestToolNameNotAliasShapedPassesThrough(t *testing.T) {
	_, m, _, key := preparedTool(t)
	for _, name := range []string{"Bash", "process_manage", "nonexistent", "xdpx_v1_t_abc"} {
		assertRawToolPassesThrough(t, m, name, key)
	}
}

// The next request carries the marker tool_use plus the caller's error result.
// It must Prepare cleanly (no history_tool), the marker must stay non-callable,
// and the declared tool set must not change.
func TestToolMarkerHistoryPreparesCleanly(t *testing.T) {
	s, m, alias, key := preparedTool(t)
	out, err := m.RestoreJSON(jsonToolUse(nearCopySlips(alias)["two_hex_slip"], key))
	if err != nil {
		t.Fatal(err)
	}
	n, _ := parse(out)
	marker := n.get("content").items[1].get("name").str()
	before, _ := s.load()
	beforeTools, _ := json.Marshal(before.Tools)

	history := strings.Replace(requestFixture, `"messages":[`, `"messages":[`+
		`{"role":"user","content":"run it"},`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_9","name":"`+marker+`","input":{}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","is_error":true,"content":"Tool '`+marker+`' does not exist. Available tools: Bash, Read"}]},`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_10","name":"Bash","input":{"command":"ls"}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_10","content":"ok"}]},`, 1)
	wire, m2, err := Prepare([]byte(history), s)
	if err != nil {
		t.Fatalf("history with marker refused: %v", err)
	}
	if bytes.Contains(wire, []byte(`"name":"`+marker+`"`)) {
		t.Fatal("marker tool name left DPX unaliased")
	}
	after, _ := s.load()
	afterTools, _ := json.Marshal(after.Tools)
	if !bytes.Equal(beforeTools, afterTools) {
		t.Fatalf("declared tool set changed: %s -> %s", beforeTools, afterTools)
	}
	if _, registered := after.Tools[marker]; registered {
		t.Fatal("marker registered as a declared tool")
	}
	// The marker's history alias is not callable: a response naming it is
	// itself a marker, never a restore to a real tool.
	markerAlias := storedAlias(t, after, "t", marker)
	if !bytes.Contains(wire, []byte(markerAlias)) {
		t.Fatalf("marker not aliased deterministically: %s", wire)
	}
	back, err := m2.RestoreJSON([]byte(`{"content":[{"type":"tool_use","id":"x","name":"` + markerAlias + `","input":{}}]}`))
	if err != nil || !bytes.Contains(back, []byte(`"name":"unknown_tool_1"`)) {
		t.Fatalf("history marker alias became callable: %v %s", err, back)
	}
	again, _, err := Prepare([]byte(history), s)
	if err != nil || !bytes.Equal(again, wire) {
		t.Fatalf("second Prepare not byte-identical: %v", err)
	}
}
