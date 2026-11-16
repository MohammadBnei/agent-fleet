package definitions

import (
	"strings"
	"testing"
)

// docs/adr/0070 §3: output.schema compiles, has `type: object` at the root,
// and is at most 64 KiB.
func TestLoad_OutputSchema(t *testing.T) {
	big := `{ type: object, description: "` + strings.Repeat("x", maxOutputSchema) + `" }`
	cases := []struct {
		name, output, want string
	}{
		{"absent", "", ""},
		{"object with properties", `{ schema: { type: object, required: [n], properties: { n: { type: integer } } } }`, ""},
		{"bare object", `{ schema: { type: object } }`, ""},
		{"empty schema", `{ schema: {} }`, "output.schema: is required"},
		{"no schema key", `{}`, "output.schema: is required"},
		{"array root", `{ schema: { type: array, items: { type: string } } }`, `root must be "type": "object"`},
		{"no root type", `{ schema: { properties: { n: { type: integer } } } }`, `root must be "type": "object"`},
		{"list of types", `{ schema: { type: [object, "null"] } }`, `root must be "type": "object"`},
		{"does not compile", `{ schema: { type: object, properties: 3 } }`, "does not compile"},
		{"unknown type name", `{ schema: { type: object, properties: { n: { type: nope } } } }`, "does not compile"},
		{"over 64 KiB", `{ schema: ` + big + ` }`, "over 65536"},
		{"file ref", `{ schema: { type: object, properties: { a: { $ref: "file:///etc/passwd" } } } }`, "external references are refused"},
		{"http ref", `{ schema: { type: object, properties: { a: { $ref: "http://127.0.0.1:1/x.json" } } } }`, "external references are refused"},
		{"relative ref", `{ schema: { type: object, properties: { a: { $ref: "other.json" } } } }`, "external references are refused"},
		{"local ref ok", `{ schema: { type: object, properties: { a: { $ref: "#/$defs/n" } }, $defs: { n: { type: integer } } } }`, ""},
		{"draft-07 dodge", `{ schema: { $schema: "http://json-schema.org/draft-07/schema#", type: object, $ref: "#/definitions/x", definitions: { x: { type: array } } } }`, "$schema, when set"},
		{"2020-12 explicit ok", `{ schema: { $schema: "https://json-schema.org/draft/2020-12/schema", type: object } }`, ""},
		{"unknown output key", `{ schema: { type: object }, format: json }`, "field format not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fileErrs := load(t, map[string]string{
				"definitions/base.yaml": baseYAML,
				"definitions/cee.yaml":  childYAML("cee", map[string]string{"output": tc.output}),
			})
			var msgs []string
			for _, r := range got {
				msgs = append(msgs, errText(r.Err))
			}
			for _, e := range fileErrs {
				msgs = append(msgs, e.Error())
			}
			all := strings.TrimSpace(strings.Join(msgs, "\n"))
			if tc.want == "" {
				if all != "" {
					t.Fatalf("want valid, got %q", all)
				}
				return
			}
			if !strings.Contains(all, tc.want) {
				t.Fatalf("want %q in %q", tc.want, all)
			}
		})
	}
}

func TestOutputSchemaJSON(t *testing.T) {
	got, _ := load(t, map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/cee.yaml":  childYAML("cee", map[string]string{"output": `{ schema: { type: object, properties: { b: { type: integer }, a: { type: string } } } }`}),
	})
	s, err := OutputSchemaJSON(got["cee"].Effective)
	if err != nil || s != `{"properties":{"a":{"type":"string"},"b":{"type":"integer"}},"type":"object"}` {
		t.Errorf("got %q, %v", s, err)
	}
	if s, err := OutputSchemaJSON(&Spec{}); s != "" || err != nil {
		t.Errorf("no output: %q, %v", s, err)
	}
}

// The refusal never carries the target's content.
func TestLoad_OutputSchemaRefusalDoesNotLeak(t *testing.T) {
	got, fileErrs := load(t, map[string]string{
		"definitions/base.yaml": baseYAML,
		"definitions/cee.yaml":  childYAML("cee", map[string]string{"output": `{ schema: { type: object, properties: { a: { $ref: "file:///etc/passwd" } } } }`}),
	})
	all := errText(got["cee"].Err)
	for _, e := range fileErrs {
		all += e.Error()
	}
	if all == "" || strings.Contains(all, "root:") || strings.Contains(all, "/etc/passwd") {
		t.Fatalf("refusal %q", all)
	}
}
