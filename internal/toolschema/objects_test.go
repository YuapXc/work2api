package toolschema

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestObjectUnionContract(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		change bool
	}{
		{`{"oneOf":[{"type":"object","required":["a"]},{"type":"object","required":["b"]}]}`, true},
		{`{"$defs":{"a":{"type":"object"}},"oneOf":[{"$ref":"#/$defs/a"}]}`, true},
		{`{"$defs":{"a/b":{"type":"object"}},"oneOf":[{"$ref":"#/$defs/a~1b"}]}`, true},
		{`{"oneOf":[{"type":"string"},{"type":"object"}]}`, false},
		{`{"anyOf":[{"type":"object"}]}`, false},
		{`{"oneOf":[]}`, false},
		{`{"oneOf":[{"$ref":"https://example.com/schema"}]}`, false},
		{`{"$defs":{"a":{"$ref":"#/$defs/a"}},"oneOf":[{"$ref":"#/$defs/a"}]}`, false},
		{`{"examples":[{"oneOf":[{"type":"object"}]}],"default":{"oneOf":[{"type":"object"}]}}`, false},
		{`{"$defs":{"a":{"$id":"other","type":"object"}},"oneOf":[{"$ref":"#/$defs/a"}]}`, false},
	} {
		var schema map[string]any
		json.Unmarshal([]byte(tc.raw), &schema)
		before, _ := json.Marshal(schema)
		out := ObjectUnions(schema).(map[string]any)
		after, _ := json.Marshal(schema)
		if string(before) != string(after) {
			t.Fatal("mutated input")
		}
		if (out["type"] == "object") != tc.change {
			t.Fatal(tc.raw, out)
		}
		if !tc.change && !reflect.DeepEqual(schema, out) {
			t.Fatal("changed non-object union")
		}
	}
}
func TestToolSchemaShapesPreserveContracts(t *testing.T) {
	var body any
	json.Unmarshal([]byte(`[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"read","description":"Must preserve","parameters":{"oneOf":[{"type":"object","properties":{"x":{"type":"string"}}}]}}]}]`), &body)
	before, _ := json.Marshal(body)
	out := Tools(body).([]any)
	tool := out[0].(map[string]any)["tools"].([]any)[0].(map[string]any)
	if tool["parameters"].(map[string]any)["type"] != "object" || tool["description"] != "Must preserve" {
		t.Fatal(tool)
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("source mutated")
	}
}

func TestWorkBuddySchemaPreservesConstraints(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","required":["x"],"properties":{"x":{"enum":[1,"one",null]},"y":false},"additionalProperties":false}`,
		`{"type":["integer","string"],"enum":[1,1.5,"x"],"anyOf":[{"const":1}],"allOf":[{"description":"keep"}]}`,
		`{"enum":[],"description":"unchanged"}`,
		`{"properties":{"x":{"pattern":"\\0"},"y":{"pattern":"\\\\0"},"z":{"pattern":"\\01"}},"const":{"pattern":"\\0"},"description":"\\0"}`,
		`{"oneOf":[{"type":"object"},{"type":"object"}]}`,
	} {
		var source map[string]any
		if err := json.Unmarshal([]byte(raw), &source); err != nil {
			t.Fatal(err)
		}
		before, _ := json.Marshal(source)
		out := workBuddySchema(source)
		after, _ := json.Marshal(source)
		if string(before) != string(after) {
			t.Fatal("mutated source")
		}
		if !reflect.DeepEqual(out, workBuddySchema(out)) {
			t.Fatalf("not idempotent: %s", raw)
		}
		m := out.(map[string]any)
		for _, key := range []string{"required", "additionalProperties", "description", "const", "anyOf"} {
			if !reflect.DeepEqual(source[key], m[key]) {
				t.Fatalf("changed %s", key)
			}
		}
		if values, ok := source["enum"].([]any); ok && len(values) == 0 {
			if !reflect.DeepEqual(m["not"], map[string]any{}) {
				t.Fatal("empty enum widened")
			}
		}
	}
	var source map[string]any
	json.Unmarshal([]byte(`{"properties":{"x":{"pattern":"\\0"},"y":{"pattern":"\\\\0"},"z":{"pattern":"\\01"}}}`), &source)
	fields := workBuddySchema(source).(map[string]any)["properties"].(map[string]any)
	for key, want := range map[string]string{"x": `\u0000`, "y": `\\0`, "z": `\01`} {
		if fields[key].(map[string]any)["pattern"] != want {
			t.Fatal(key, fields[key])
		}
	}
}
