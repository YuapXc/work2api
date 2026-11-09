package toolschema

import (
	"encoding/json"
	"reflect"
	"strings"
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

func TestKimiLocalReferenceExpansionPreservesContracts(t *testing.T) {
	var source map[string]any
	json.Unmarshal([]byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$defs":{"a/b":{"type":"integer","minimum":1,"enum":[1,2]}},"type":"object","required":["x"],"additionalProperties":false,"properties":{"x":{"$ref":"#/$defs/a~1b","maximum":1,"allOf":[{"not":{"const":2}}]}},"const":{"$ref":"business data"},"description":"keep"}`), &source)
	before, _ := json.Marshal(source)
	out := inlineLocalSchema(source).(map[string]any)
	property := out["properties"].(map[string]any)["x"].(map[string]any)
	all := property["allOf"].([]any)
	if out["$defs"] != nil || property["$ref"] != nil || property["maximum"] != float64(1) || len(all) != 2 || !reflect.DeepEqual(all[0], source["$defs"].(map[string]any)["a/b"]) || !reflect.DeepEqual(all[1], source["properties"].(map[string]any)["x"].(map[string]any)["allOf"].([]any)[0]) || !reflect.DeepEqual(out["required"], source["required"]) || out["additionalProperties"] != false || !reflect.DeepEqual(out["const"], source["const"]) {
		t.Fatal("constraints weakened", out)
	}
	after, _ := json.Marshal(source)
	if string(before) != string(after) || !reflect.DeepEqual(out, inlineLocalSchema(out)) {
		t.Fatal("source changed or non-idempotent")
	}
	for _, raw := range []string{
		`{"$defs":{"x":{"$ref":"#/$defs/x"}},"$ref":"#/$defs/x"}`,
		`{"$defs":{"x":{"type":"string"}},"properties":{"a":{"$ref":"#/$defs/x"},"b":{"$ref":"#/$defs/missing"}}}`,
		`{"$defs":{"x":{"type":"string"}},"$ref":"https://example.test/schema"}`,
		`{"$defs":{"x":{"$id":"nested","type":"string"}},"$ref":"#/$defs/x"}`,
		`{"$schema":"http://json-schema.org/draft-07/schema#","definitions":{"x":{"type":"string"}},"$ref":"#/definitions/x"}`,
		`{"$schema":"https://example.test/custom-2020-12","$defs":{"x":{"type":"string"}},"$ref":"#/$defs/x"}`,
		`{"id":"legacy-resource","$defs":{"x":{"type":"string"}},"$ref":"#/$defs/x"}`,
		`{"$defs":{"x":{"type":"string"}},"$dynamicRef":"#x","$ref":"#/$defs/x"}`,
		`{"$defs":{"x":{"type":"string"}},"$ref":"#/$defs/x~2"}`,
		`{"$defs":{"x":{"type":"string"}},"$ref":"#/$defs/x","maxLength":2}`,
	} {
		var input map[string]any
		json.Unmarshal([]byte(raw), &input)
		if changedMap(input, inlineLocalSchema(input).(map[string]any)) {
			t.Fatal("uncertain schema partially expanded", raw)
		}
	}
	var booleans map[string]any
	json.Unmarshal([]byte(`{"definitions":{"no":false},"$ref":"#/definitions/no"}`), &booleans)
	if got := inlineLocalSchema(booleans).(map[string]any)["allOf"].([]any)[0]; got != false {
		t.Fatal("false schema loosened", got)
	}
	var tools any
	json.Unmarshal([]byte(`[{"type":"function","function":{"name":"lookup","parameters":{"$defs":{"x":{"type":"string"}},"type":"object","properties":{"x":{"$ref":"#/$defs/x"}}}}}]`), &tools)
	for _, model := range []string{"kimi-k3.1-pro", "KIMI-K2", "kimono", "glm-5.3-flash"} {
		out := WorkBuddyToolsForModel(tools, model).([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
		want := model == "kimi-k3.1-pro" || model == "KIMI-K2"
		if (out["$defs"] == nil) != want {
			t.Fatal("model scope changed", model, out)
		}
	}
	defs := map[string]any{"x": map[string]any{"type": "string"}}
	previous := "x"
	for i := 0; i < 14; i++ {
		name := previous + "x"
		defs[name] = map[string]any{"anyOf": []any{map[string]any{"$ref": "#/$defs/" + previous}, map[string]any{"$ref": "#/$defs/" + previous}}}
		previous = name
	}
	large := map[string]any{"$defs": defs, "$ref": "#/$defs/" + previous}
	if changedMap(large, inlineLocalSchema(large).(map[string]any)) {
		t.Fatal("over-budget expansion partially published")
	}
	wide := map[string]any{}
	for i := 0; i < 20; i++ {
		wide[strings.Repeat("x", i+1)] = map[string]any{"$ref": "#/$defs/large"}
	}
	amplified := map[string]any{"$defs": map[string]any{"large": map[string]any{"type": "string", "description": strings.Repeat("d", 50000)}}, "properties": wide}
	if changedMap(amplified, inlineLocalSchema(amplified).(map[string]any)) {
		t.Fatal("reference annotations amplified without a byte bound")
	}
}
func BenchmarkKimiReferencePreparation(b *testing.B) {
	for _, references := range []bool{false, true} {
		schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}
		name := "ordinary"
		if references {
			name = "local-ref"
			schema["$defs"] = map[string]any{"value": map[string]any{"type": "string", "minLength": 1}}
			schema["properties"] = map[string]any{"value": map[string]any{"$ref": "#/$defs/value"}}
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				inlineLocalSchema(schema)
			}
		})
	}
}
