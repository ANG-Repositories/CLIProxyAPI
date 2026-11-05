package contentalias

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// t_3f4b6bd1 (2026-11-01 06:03 PT, claude-dpx-24, wire 89655-1147, bridge
// 7180588a, cpa-6ee87a7c): the model called process_manage, a deferred tool
// that Hermes's tool catalog names in system-prompt prose but does not
// declare. contentalias refused it with unknown_tool, which failed the turn and
// reaped the TUI session. A raw (non-dpx_v1_) name must reach the caller as the
// model emitted it, so the caller answers "no such tool" and the turn goes on.

func assertRawToolPassesThrough(t *testing.T, m *RequestMap, name, key string) {
	t.Helper()
	events := toolEvents(name, key)
	out, err := streamAllSplits(t, m, events)
	if err != nil {
		t.Fatalf("stream %q: turn refused: %v", name, err)
	}
	if !bytes.Equal(out, events) {
		t.Fatalf("stream %q: not passed through verbatim:\n got %s\nwant %s", name, out, events)
	}
	raw := jsonToolUse(name, key)
	jout, err := m.RestoreJSON(raw)
	if err != nil {
		t.Fatalf("json %q: turn refused: %v", name, err)
	}
	if !bytes.Equal(jout, raw) {
		t.Fatalf("json %q: not passed through verbatim:\n got %s\nwant %s", name, jout, raw)
	}
}

type rawNameRow struct {
	Unit    string `json:"unit"`
	TS      string `json:"ts"`
	Wire    string `json:"wire"`
	Emitted string `json:"emitted"`
}

func TestLiveRawNameCorpusPassesThrough(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "tool_alias_near_copy_corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		RawNames []rawNameRow `json:"raw_names"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.RawNames) == 0 {
		t.Fatal("corpus has no raw_names rows")
	}
	_, m, _, key := preparedTool(t)
	for _, row := range doc.RawNames {
		if aliasShapedToolName(row.Emitted) {
			t.Fatalf("raw_names row %s is alias-shaped: %q", row.Wire, row.Emitted)
		}
		assertRawToolPassesThrough(t, m, row.Emitted, key)
	}
}

// A raw name and an alias slip in one response: the raw call passes through,
// the slip still becomes the marker, the declared call still restores.
func TestRawNameBesideSlipAndDeclared(t *testing.T) {
	_, m, alias, key := preparedTool(t)
	body := []byte(`{"content":[` +
		`{"type":"tool_use","id":"a","name":"process_manage","input":{"action":"poll"}},` +
		`{"type":"tool_use","id":"b","name":"dpx_v1_` + alias + `","input":{"x":1}},` +
		`{"type":"tool_use","id":"c","name":"` + alias + `","input":{"` + key + `":"ls"}}]}`)
	out, err := m.RestoreJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"type":"tool_use","id":"a","name":"process_manage","input":{"action":"poll"}}`,
		`"id":"b","name":"unknown_tool_1","input":{}`,
		`"id":"c","name":"Bash","input":{"command":"ls"}`,
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
	if bytes.Contains(out, []byte("dpx_v1_")) {
		t.Fatalf("alias token left DPX: %s", out)
	}
}
