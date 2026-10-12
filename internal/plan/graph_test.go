package plan

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func encode(t *testing.T, g any) []byte {
	t.Helper()
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeValidGraph(t *testing.T) {
	t.Parallel()

	g := newGraph().epic("billing", ApprovalApproved, "invoice", "tax").block("billing/invoice", "billing/tax").build()
	got, err := Decode(encode(t, g))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if len(got.Nodes) != 3 || len(got.Blocks) != 1 || got.SchemaVersion != 1 {
		t.Errorf("Decode() = %+v", got)
	}
}

func decodeErr(t *testing.T, raw []byte) string {
	t.Helper()
	_, err := Decode(raw)
	if _, ok := errors.AsType[*GraphError](err); !ok {
		t.Fatalf("Decode() error = %v, want *GraphError", err)
	}
	return err.Error()
}

func TestDecodeRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	g := newGraph().build()
	g.SchemaVersion = 2
	msg := decodeErr(t, encode(t, g))
	if !strings.Contains(msg, "schema_version 2") || !strings.Contains(msg, "version 1") {
		t.Errorf("error %q does not name both versions", msg)
	}
}

func TestDecodeRejectsMalformedDocuments(t *testing.T) {
	t.Parallel()

	valid := func() map[string]any {
		var m map[string]any
		_ = json.Unmarshal(encode(t, newGraph().epic("billing", ApprovalApproved, "invoice").build()), &m)
		return m
	}
	node := func(m map[string]any) map[string]any { return m["nodes"].([]any)[0].(map[string]any) }

	tests := []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"unknown field", func(m map[string]any) { m["extra"] = 1 }, "unknown field"},
		{"missing field", func(m map[string]any) { delete(m, "blocks") }, "missing field blocks"},
		{"bad kind", func(m map[string]any) { node(m)["kind"] = "task" }, `kind "task"`},
		{"missing node field", func(m map[string]any) { delete(node(m), "body") }, "missing field body"},
		{"bad digest", func(m map[string]any) { m["source_digest"] = "md5:x" }, "source_digest"},
		{"dangling block edge", func(m map[string]any) {
			m["blocks"] = []any{map[string]any{"blocker": "billing/nope", "blocked": "billing/invoice"}}
		}, `"billing/nope" is not a node`},
		{"dangling parent edge", func(m map[string]any) {
			m["parents"] = []any{map[string]any{"child": "billing/invoice", "parent": "ghost"}}
		}, `"ghost" is not a node`},
		{"not json", nil, "not a JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := []byte("[")
			if tt.mutate != nil {
				m := valid()
				tt.mutate(m)
				raw = encode(t, m)
			}
			if msg := decodeErr(t, raw); !strings.Contains(msg, tt.want) {
				t.Errorf("error %q does not contain %q", msg, tt.want)
			}
		})
	}
}
