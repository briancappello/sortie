package plan

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// testdata/plan-graph.schema.json is the published plan graph schema. The
// plan workbench ships the same file and checks it against this copy.
// This test keeps the copy in step with the decoder.
func TestPublishedSchemaMatchesTheDecoder(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("testdata/plan-graph.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string `json:"required"`
		Properties struct {
			SchemaVersion struct {
				Enum []int `json:"enum"`
			} `json:"schema_version"`
		} `json:"properties"`
		Defs struct {
			Node struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"node"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sortedCopy(schema.Required), sortedCopy(requiredTop)) {
		t.Errorf("top-level required = %v, decoder requires %v", schema.Required, requiredTop)
	}
	if !slices.Equal(sortedCopy(schema.Defs.Node.Required), sortedCopy(requiredNode)) {
		t.Errorf("node required = %v, decoder requires %v", schema.Defs.Node.Required, requiredNode)
	}
	var versions []int
	for v := 1; v <= SupportedVersion; v++ {
		versions = append(versions, v)
	}
	if !slices.Equal(schema.Properties.SchemaVersion.Enum, versions) {
		t.Errorf("schema_version enum = %v, decoder reads %v", schema.Properties.SchemaVersion.Enum, versions)
	}
	var fields []string
	for f := range reflect.TypeFor[Node]().Fields() {
		fields = append(fields, strings.Split(f.Tag.Get("json"), ",")[0])
	}
	var props []string
	for k := range schema.Defs.Node.Properties {
		props = append(props, k)
	}
	if !slices.Equal(sortedCopy(props), sortedCopy(fields)) {
		t.Errorf("node properties = %v, decoder fields = %v", sortedCopy(props), sortedCopy(fields))
	}
}

func sortedCopy(s []string) []string {
	c := slices.Clone(s)
	slices.Sort(c)
	return c
}
