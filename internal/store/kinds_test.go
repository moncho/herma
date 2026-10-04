package store

import (
	"errors"
	"strings"
	"testing"
)

func TestBuiltinKindsMatchPreviousRules(t *testing.T) {
	want := map[string][]string{
		"knowledge": {"proposed", "accepted", "rejected", "superseded"},
		"principle": {"proposed", "accepted", "rejected", "superseded"},
		"project":   {"planned", "active", "paused", "completed"},
		"task":      {"open", "in_progress", "blocked", "done"},
		"note":      {"published"},
		"feedback":  {"open", "triaged", "resolved"},
		"kind":      {"proposed", "accepted", "retired"},
	}
	if len(builtinOrder) != len(want) {
		t.Fatalf("builtinOrder = %v", builtinOrder)
	}
	for name, statuses := range want {
		k, ok := builtinKinds[name]
		if !ok || !k.Builtin || k.Status != "accepted" || k.Name != name {
			t.Fatalf("%s: %+v", name, k)
		}
		if strings.Join(k.Statuses(), ",") != strings.Join(statuses, ",") || k.DefaultStatus() != statuses[0] {
			t.Errorf("%s statuses = %v", name, k.Statuses())
		}
		if k.review() != (name == "knowledge" || name == "principle") {
			t.Errorf("%s review = %v", name, k.review())
		}
	}
	if c := builtinKinds["note"].Definition.Policy.Context; c == nil || c.Order != "recent" {
		t.Errorf("note context = %+v", c)
	}
	if !builtinKinds["knowledge"].OptIn || !builtinKinds["knowledge"].ContextGlobal {
		t.Error("knowledge must be opt-in and include global records")
	}
}

func TestParseDefinitionAcceptsBookmark(t *testing.T) {
	d, err := parseDefinition("fields.definition", []byte(`{
		"schema": {
			"url": {"type": "url", "required": true, "unique": true},
			"platform": {"type": "enum", "values": ["web", "x"]},
			"rating": {"type": "integer", "min": 1, "max": 5},
			"authors": {"type": "string-list"}
		},
		"statuses": ["unread", "reading", "done"],
		"policy": {"recall": true, "context": {"statuses": ["unread"], "order": "recent", "max_records": 5}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Policy.Writers != RoleAgent {
		t.Errorf("writers defaults to agent, got %q", d.Policy.Writers)
	}
	k := Kind{Name: "bookmark", Definition: d}
	if k.DefaultStatus() != "unread" || !k.allows("done") || k.allows("accepted") {
		t.Errorf("statuses: %v", k.Statuses())
	}
}

func TestParseDefinitionRejectsInvalidDocuments(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown top key":      `{"statuses":["a"],"policy":{},"extra":1}`,
		"unknown field key":    `{"schema":{"a":{"type":"string","size":3}},"statuses":["a"],"policy":{}}`,
		"bad field name":       `{"schema":{"Bad":{"type":"string"}},"statuses":["a"],"policy":{}}`,
		"unknown type":         `{"schema":{"a":{"type":"blob"}},"statuses":["a"],"policy":{}}`,
		"min on string":        `{"schema":{"a":{"type":"string","min":1}},"statuses":["a"],"policy":{}}`,
		"fractional int min":   `{"schema":{"a":{"type":"integer","min":1.5}},"statuses":["a"],"policy":{}}`,
		"min above max":        `{"schema":{"a":{"type":"number","min":5,"max":1}},"statuses":["a"],"policy":{}}`,
		"enum without values":  `{"schema":{"a":{"type":"enum"}},"statuses":["a"],"policy":{}}`,
		"values on string":     `{"schema":{"a":{"type":"string","values":["x"]}},"statuses":["a"],"policy":{}}`,
		"repeated enum value":  `{"schema":{"a":{"type":"enum","values":["x","x"]}},"statuses":["a"],"policy":{}}`,
		"unique on text":       `{"schema":{"a":{"type":"text","unique":true}},"statuses":["a"],"policy":{}}`,
		"unique on list":       `{"schema":{"a":{"type":"string-list","unique":true}},"statuses":["a"],"policy":{}}`,
		"no statuses":          `{"policy":{}}`,
		"statuses with review": `{"statuses":["a"],"policy":{"review":true}}`,
		"bad writers":          `{"statuses":["a"],"policy":{"writers":"read-only"}}`,
		"context status":       `{"statuses":["a"],"policy":{"context":{"statuses":["b"],"order":"recent","max_records":1}}}`,
		"context order":        `{"statuses":["a"],"policy":{"context":{"statuses":["a"],"order":"newest","max_records":1}}}`,
		"context max":          `{"statuses":["a"],"policy":{"context":{"statuses":["a"],"order":"recent","max_records":21}}}`,
		"trailing data":        `{"statuses":["a"],"policy":{}} {}`,
		"trailing brace":       `{"statuses":["a"],"policy":{}} }`,
		"trailing bracket":     `{"statuses":["a"],"policy":{}} ]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseDefinition("fields.definition", []byte(doc))
			var validation *ValidationError
			if !errors.As(err, &validation) || !strings.HasPrefix(err.Error(), "fields.definition") {
				t.Fatalf("got %v", err)
			}
		})
	}
	big := `{"statuses":["a"],"policy":{},"schema":{` + strings.Repeat(" ", 16<<10) + `}}`
	if _, err := parseDefinition("fields.definition", []byte(big)); err == nil {
		t.Error("definitions above 16 KiB must be refused")
	}
	var many []string
	for i := range 31 {
		many = append(many, `"f`+strings.Repeat("x", i)+`":{"type":"string"}`)
	}
	if _, err := parseDefinition("fields.definition", []byte(`{"statuses":["a"],"policy":{},"schema":{`+strings.Join(many, ",")+`}}`)); err == nil {
		t.Error("more than 30 fields must be refused")
	}
}

func TestDefinitionValueRoundTrips(t *testing.T) {
	d, err := parseDefinition("fields.definition", []byte(`{"schema":{"n":{"type":"integer","min":0}},"statuses":["a"],"policy":{"recall":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	again, err := definitionFrom("fields.definition", definitionValue(d))
	if err != nil || again.Schema["n"].Min == nil || *again.Schema["n"].Min != 0 || !again.Policy.Recall {
		t.Fatalf("round trip: %+v %v", again, err)
	}
	if _, err := definitionFrom("fields.definition", "not an object"); err == nil {
		t.Error("a non-object definition must be refused")
	}
}
