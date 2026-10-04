package store

import (
	"context"
	"strings"
	"testing"
)

func queryKindDefinition() map[string]any {
	return map[string]any{
		"schema": map[string]any{
			"s":  map[string]any{"type": "string"},
			"t":  map[string]any{"type": "text"},
			"i":  map[string]any{"type": "integer"},
			"n":  map[string]any{"type": "number"},
			"b":  map[string]any{"type": "boolean"},
			"d":  map[string]any{"type": "date"},
			"dt": map[string]any{"type": "datetime"},
			"u":  map[string]any{"type": "url"},
			"e":  map[string]any{"type": "enum", "values": []any{"web", "x"}},
			"l":  map[string]any{"type": "string-list"},
		},
		"statuses": []any{"open", "done"},
		"policy":   map[string]any{},
	}
}

// queryFixture creates kind "item" with records A (priority 1), B (3) and C (2).
func queryFixture(t *testing.T) *Store {
	t.Helper()
	s := testStore(t)
	acceptKind(t, s, "item", queryKindDefinition(), "")
	createRecord(t, s, CreateInput{Kind: "item", Title: "A", Priority: 1, Fields: map[string]any{
		"s": "apple", "t": "alpha", "i": 1, "n": 1.5, "b": true, "d": "2026-01-01",
		"dt": "2026-10-03T10:00:00Z", "u": "https://a.example", "e": "web", "l": []any{"x", "y"},
	}})
	createRecord(t, s, CreateInput{Kind: "item", Title: "B", Priority: 3, Fields: map[string]any{
		"s": "banana", "t": "beta", "i": 5, "n": 2.5, "b": false, "d": "2026-02-01",
		"dt": "2026-10-03T10:00:00.5Z", "u": "https://b.example", "e": "x", "l": []any{"y"},
	}})
	createRecord(t, s, CreateInput{Kind: "item", Title: "C", Priority: 2, Fields: map[string]any{
		"s": "cherry", "i": 3, "u": "https://c.example", "e": "web",
	}})
	return s
}

func listTitles(t *testing.T, s *Store, o ListOptions) []string {
	t.Helper()
	result, err := s.List(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	titles := make([]string, len(result.Items))
	for i, r := range result.Items {
		titles[i] = r.Title
	}
	return titles
}

func TestWhereFiltersEveryType(t *testing.T) {
	s := queryFixture(t)
	// Default order is priority descending: B (3), C (2), A (1).
	for name, c := range map[string]struct {
		where []string
		want  string
	}{
		"integer >=":              {[]string{"i>=3"}, "B,C"},
		"integer =":               {[]string{"i=1"}, "A"},
		"integer !=":              {[]string{"i!=1"}, "B,C"},
		"number < skips missing":  {[]string{"n<2"}, "A"},
		"number != keeps missing": {[]string{"n!=1.5"}, "B,C"},
		"boolean true":            {[]string{"b=true"}, "A"},
		"boolean false":           {[]string{"b=false"}, "B"},
		"boolean !=":              {[]string{"b!=true"}, "B,C"},
		"date >=":                 {[]string{"d>=2026-01-15"}, "B"},
		"datetime by instant":     {[]string{"dt>2026-10-03T10:00:00Z"}, "B"},
		"datetime offsets":        {[]string{"dt>=2026-10-03T12:00:00+02:00"}, "B,A"},
		"dt = across offsets":     {[]string{"dt=2026-10-03T12:00:00+02:00"}, "A"},
		"string <":                {[]string{"s<banana"}, "A"},
		"string >=":               {[]string{"s>=banana"}, "B,C"},
		"string spaces":           {[]string{"s = apple"}, "A"},
		"text =":                  {[]string{"t=beta"}, "B"},
		"url =":                   {[]string{"u=https://c.example"}, "C"},
		"enum =":                  {[]string{"e=web"}, "C,A"},
		"enum !=":                 {[]string{"e!=web"}, "B"},
		"list has":                {[]string{"l has y"}, "B,A"},
		"list has one":            {[]string{"l has x"}, "A"},
		"exists":                  {[]string{"l exists"}, "B,A"},
		"missing":                 {[]string{"l missing"}, "C"},
		"combined":                {[]string{"e=web", "i>1"}, "C"},
	} {
		t.Run(name, func(t *testing.T) {
			got := strings.Join(listTitles(t, s, ListOptions{Kind: "item", Where: c.where}), ",")
			if got != c.want {
				t.Fatalf("where %v = %q, want %q", c.where, got, c.want)
			}
		})
	}
	result, err := s.List(context.Background(), ListOptions{Kind: "item", Where: []string{"i>=3"}})
	if err != nil || result.Total != 2 {
		t.Fatalf("total under filter: %+v %v", result, err)
	}
}

func TestWhereRejectsBadExpressions(t *testing.T) {
	s := queryFixture(t)
	for expr, want := range map[string]string{
		"i>=four":    `where "i>=four": fields.i: must be an integer`,
		"e<web":      `where "e<web": < is not allowed on enum fields`,
		"b>true":     `where "b>true": > is not allowed on boolean fields`,
		"t<z":        `where "t<z": < is not allowed on text fields`,
		"l=x":        `where "l=x": = is not allowed on string-list fields`,
		"s has a":    `where "s has a": has is only allowed on string-list fields`,
		"e=tv":       `where "e=tv": fields.e: must be one of web, x`,
		"colour=red": `where "colour=red": unknown field of kind item`,
		"i":          `where "i": expected <field><op><value>`,
		"i=":         `where "i=": expected <field><op><value>`,
		"i!3":        `where "i!3": expected <field><op><value>`,
	} {
		t.Run(expr, func(t *testing.T) {
			_, err := s.List(context.Background(), ListOptions{Kind: "item", Where: []string{expr}})
			assertValidation(t, err)
			if !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("got %q, want prefix %q", err, want)
			}
		})
	}
	if _, err := s.List(context.Background(), ListOptions{Where: []string{"i=1"}}); err == nil || err.Error() != "where requires a kind filter" {
		t.Fatalf("no kind: %v", err)
	}
	many := make([]string, 11)
	for i := range many {
		many[i] = "i>=0"
	}
	if _, err := s.List(context.Background(), ListOptions{Kind: "item", Where: many}); err == nil || err.Error() != "where supports at most 10 filters" {
		t.Fatalf("too many: %v", err)
	}
}

func TestSortByFieldsAndCoreColumns(t *testing.T) {
	s := queryFixture(t)
	for spec, want := range map[string]string{
		"-i":        "B,C,A",
		"i":         "A,C,B",
		"n":         "A,B,C",
		"-n":        "B,A,C",
		"dt":        "A,B,C",
		"-dt":       "B,A,C",
		"e,-i":      "C,A,B",
		"title":     "A,B,C",
		"-title":    "C,B,A",
		"-priority": "B,C,A",
		"priority":  "A,C,B",
	} {
		t.Run(spec, func(t *testing.T) {
			if got := strings.Join(listTitles(t, s, ListOptions{Kind: "item", Sort: spec}), ","); got != want {
				t.Fatalf("sort %q = %q, want %q", spec, got, want)
			}
		})
	}
	var pages []string
	for offset := range 3 {
		pages = append(pages, listTitles(t, s, ListOptions{Kind: "item", Sort: "-i", Limit: 1, Offset: offset})...)
	}
	if strings.Join(pages, ",") != "B,C,A" {
		t.Fatalf("paged sort = %v", pages)
	}
	if got := listTitles(t, s, ListOptions{Sort: "-priority", Kind: "item"}); len(got) != 3 {
		t.Fatalf("core sort: %v", got)
	}
	if _, err := s.List(context.Background(), ListOptions{Sort: "-priority"}); err != nil {
		t.Fatalf("core sort without kind: %v", err)
	}
}

func TestSortRejectsBadSpecs(t *testing.T) {
	s := queryFixture(t)
	for spec, want := range map[string]string{
		"a,b,c,d": "sort supports at most 3 keys",
		"colour":  `sort "colour": unknown field of kind item`,
		"l":       `sort "l": string-list fields cannot be sorted`,
		"i,-i":    `sort "-i": repeated sort key`,
		"i,":      `sort "": empty sort key`,
	} {
		t.Run(spec, func(t *testing.T) {
			_, err := s.List(context.Background(), ListOptions{Kind: "item", Sort: spec})
			assertValidation(t, err)
			if err.Error() != want {
				t.Fatalf("got %q, want %q", err, want)
			}
		})
	}
	if _, err := s.List(context.Background(), ListOptions{Sort: "i"}); err == nil || err.Error() != `sort "i": requires a kind filter` {
		t.Fatalf("field sort without kind: %v", err)
	}
}

func TestSortCoreKeyWinsOverFieldName(t *testing.T) {
	s := testStore(t)
	def := map[string]any{"schema": map[string]any{"title": map[string]any{"type": "string"}}, "statuses": []any{"a"}, "policy": map[string]any{}}
	acceptKind(t, s, "paper", def, "")
	createRecord(t, s, CreateInput{Kind: "paper", Title: "Zed", Fields: map[string]any{"title": "aaa"}})
	createRecord(t, s, CreateInput{Kind: "paper", Title: "Abe", Fields: map[string]any{"title": "zzz"}})
	if got := strings.Join(listTitles(t, s, ListOptions{Kind: "paper", Sort: "title"}), ","); got != "Abe,Zed" {
		t.Fatalf("sort title used the field, got %q", got)
	}
}

func TestWhereWithArchived(t *testing.T) {
	s := queryFixture(t)
	result, err := s.List(context.Background(), ListOptions{Kind: "item", Where: []string{"i=5"}})
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	b := result.Items[0]
	if _, _, err := s.Update(context.Background(), b.ID, reviewer("owner"), "", UpdateInput{Version: b.Version, Archived: pointer(true)}); err != nil {
		t.Fatal(err)
	}
	if got := listTitles(t, s, ListOptions{Kind: "item", Where: []string{"i=5"}}); len(got) != 0 {
		t.Fatalf("archived record listed: %v", got)
	}
	if got := strings.Join(listTitles(t, s, ListOptions{Kind: "item", Where: []string{"i=5"}, Archived: true}), ","); got != "B" {
		t.Fatalf("include archived: %q", got)
	}
}

func TestWhereOnRetiredKind(t *testing.T) {
	s := queryFixture(t)
	result, err := s.List(context.Background(), ListOptions{Kind: "kind"})
	if err != nil {
		t.Fatal(err)
	}
	k := result.Items[0]
	if _, _, err := s.Update(context.Background(), k.ID, reviewer("owner"), "", UpdateInput{Version: k.Version, Status: pointer("retired")}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(listTitles(t, s, ListOptions{Kind: "item", Where: []string{"e=web"}, Sort: "title"}), ","); got != "A,C" {
		t.Fatalf("retired kind: %q", got)
	}
}

func TestWhereValuesWithSpacesAndKeywords(t *testing.T) {
	s := testStore(t)
	acceptKind(t, s, "item", queryKindDefinition(), "")
	for _, v := range []string{"who has it", "x missing", "a<b", "a b", "k exists"} {
		createRecord(t, s, CreateInput{Kind: "item", Title: v, Fields: map[string]any{"s": v}})
	}
	for _, expr := range []string{"s=who has it", "s=x missing", "s=a<b", "s = a b", "s=a b", "s != a b", "s=k exists"} {
		got := listTitles(t, s, ListOptions{Kind: "item", Where: []string{expr}})
		if strings.HasPrefix(expr, "s !=") {
			if len(got) != 4 {
				t.Fatalf("%q = %v", expr, got)
			}
			continue
		}
		want := strings.TrimSpace(strings.SplitN(strings.SplitN(expr, "=", 2)[1], "\x00", 2)[0])
		if len(got) != 1 || got[0] != want {
			t.Fatalf("%q = %v, want [%s]", expr, got, want)
		}
	}
}
