package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func testDefinition(t *testing.T, doc string) Definition {
	t.Helper()
	d, err := parseDefinition("fields.definition", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

const everyTypeDefinition = `{"schema":{
	"s":{"type":"string"},"t":{"type":"text"},"i":{"type":"integer","min":1,"max":5},
	"n":{"type":"number"},"b":{"type":"boolean"},"d":{"type":"date"},"dt":{"type":"datetime"},
	"u":{"type":"url","required":true},"e":{"type":"enum","values":["web","x"]},"l":{"type":"string-list"}
},"statuses":["a"],"policy":{}}`

func TestNormalizeFieldsAcceptsEveryType(t *testing.T) {
	d := testDefinition(t, everyTypeDefinition)
	got, err := normalizeFields(d, map[string]any{
		"s": " One line ", "t": "line\nline", "i": 3, "n": "2.5", "b": "true", "d": "2026-10-03",
		"dt": "2026-10-03T12:00:00+02:00", "u": "https://example.com/a", "e": "web", "l": "a, b",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"s": "One line", "t": "line\nline", "i": float64(3), "n": 2.5, "b": true, "d": "2026-10-03",
		"dt": "2026-10-03T10:00:00Z", "u": "https://example.com/a", "e": "web", "l": []any{"a", "b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestNormalizeFieldsRejectsBadValues(t *testing.T) {
	d := testDefinition(t, everyTypeDefinition)
	ok := map[string]any{"u": "https://example.com"}
	for name, c := range map[string]struct {
		field string
		value any
		want  string
	}{
		"unknown":        {"zzz", "x", "fields.zzz: unknown field"},
		"null":           {"s", nil, "fields.s: must not be null"},
		"string newline": {"s", "a\nb", "fields.s:"},
		"string long":    {"s", strings.Repeat("a", 301), "fields.s:"},
		"string blank":   {"s", "  ", "fields.s:"},
		"text long":      {"t", strings.Repeat("a", 16<<10+1), "fields.t:"},
		"int range":      {"i", 9, "fields.i: must be an integer from 1 to 5"},
		"int fraction":   {"i", 2.5, "fields.i: must be an integer from 1 to 5"},
		"int string":     {"i", "two", "fields.i: must be an integer from 1 to 5"},
		"number":         {"n", "NaN", "fields.n: must be a number"},
		"boolean":        {"b", "yes", "fields.b: must be true or false"},
		"date":           {"d", "2026-02-30", "fields.d: must be a date (YYYY-MM-DD)"},
		"datetime":       {"dt", "yesterday", "fields.dt: must be an RFC 3339 datetime"},
		"url scheme":     {"u", "ftp://example.com", "fields.u: must be an absolute http or https URL"},
		"url relative":   {"u", "/a", "fields.u: must be an absolute http or https URL"},
		"enum":           {"e", "tv", "fields.e: must be one of web, x"},
		"list dup":       {"l", []any{"a", "a"}, "fields.l: \"a\" is repeated"},
		"list type":      {"l", []any{"a", 1}, "fields.l: must be a list of strings"},
	} {
		t.Run(name, func(t *testing.T) {
			fields := map[string]any{"u": ok["u"], c.field: c.value}
			_, err := normalizeFields(d, fields)
			if err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Fatalf("got %v, want prefix %q", err, c.want)
			}
		})
	}
	if _, err := normalizeFields(d, map[string]any{}); err == nil || err.Error() != "fields.u: required" {
		t.Fatalf("missing required: %v", err)
	}
	empty, err := normalizeFields(Definition{Statuses: []string{"a"}}, nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("no fields: %#v %v", empty, err)
	}
}

func TestIntegerCoercion(t *testing.T) {
	d := testDefinition(t, `{"schema":{"i":{"type":"integer"}},"statuses":["a"],"policy":{}}`)
	for _, value := range []any{5, int64(5), 5.0, "5", json.Number("5")} {
		got, err := normalizeFields(d, map[string]any{"i": value})
		if err != nil || got["i"] != float64(5) {
			t.Errorf("%#v: %#v %v", value, got, err)
		}
	}
	for _, value := range []any{"5.5", 1e300, "1e3", true} {
		if _, err := normalizeFields(d, map[string]any{"i": value}); err == nil {
			t.Errorf("%#v accepted", value)
		}
	}
}

func TestStringFieldKeepsCommas(t *testing.T) {
	d := testDefinition(t, `{"schema":{"note":{"type":"string"}},"statuses":["a"],"policy":{}}`)
	got, err := normalizeFields(d, map[string]any{"note": "a, b"})
	if err != nil || got["note"] != "a, b" {
		t.Fatalf("%#v %v", got, err)
	}
}

func TestPatchFieldsSetsAndRemoves(t *testing.T) {
	current := map[string]any{"a": "1", "b": "2"}
	got := patchFields(current, map[string]any{"a": nil, "c": "3"})
	if !reflect.DeepEqual(got, map[string]any{"b": "2", "c": "3"}) || len(current) != 2 {
		t.Fatalf("got %#v; current changed to %#v", got, current)
	}
}

func TestSearchTextIncludesTextualFieldsOnly(t *testing.T) {
	d := testDefinition(t, everyTypeDefinition)
	k := Kind{Name: "thing", Definition: d}
	text := searchText(k, map[string]any{"s": "alpha", "t": "beta", "u": "https://gamma.example", "l": []any{"delta", "eps"}, "i": float64(3), "e": "web", "d": "2026-10-03"})
	for _, want := range []string{"alpha", "beta", "gamma.example", "delta", "eps"} {
		if !strings.Contains(text, want) {
			t.Errorf("search text %q lacks %q", text, want)
		}
	}
	if strings.Contains(text, "web") || strings.Contains(text, "2026") {
		t.Errorf("search text %q includes non-text fields", text)
	}
	if searchText(builtinKinds["kind"], map[string]any{"definition": map[string]any{}}) != "" {
		t.Error("kind records have no field search text")
	}
}
