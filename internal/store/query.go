package store

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	maxWhereFilters = 10
	maxSortKeys     = 3
)

// condition is one SQL predicate over records r with its bound arguments.
type condition struct {
	sql  string
	args []any
}

// comparisonOps lists two-character operators before their prefixes.
var comparisonOps = []string{"<=", ">=", "!=", "=", "<", ">"}

// parseWhere turns one filter expression into a condition on a typed field
// of k. Values are parsed with the field's write rules.
func parseWhere(k Kind, expr string) (condition, error) {
	bad := func(format string, args ...any) error {
		return invalid(fmt.Sprintf("where %q: ", expr) + fmt.Sprintf(format, args...))
	}
	name, op, value, ok := splitWhere(expr)
	if !ok {
		return condition{}, bad(`expected <field><op><value>, "<field> has <value>", "<field> exists" or "<field> missing"`)
	}
	f, found := k.Definition.Schema[name]
	if !found {
		return condition{}, bad("unknown field of kind %s", k.Name)
	}
	path := "$.fields." + name
	switch op {
	case "exists":
		return condition{"json_type(r.data, ?) IS NOT NULL", []any{path}}, nil
	case "missing":
		return condition{"json_type(r.data, ?) IS NULL", []any{path}}, nil
	case "has":
		if f.Type != FieldStringList {
			return condition{}, bad("has is only allowed on string-list fields")
		}
		item, err := normalizeValue(FieldDef{Type: FieldString}, value)
		if err != nil {
			return condition{}, bad("fields.%s: %v", name, err)
		}
		return condition{"EXISTS (SELECT 1 FROM json_each(r.data, ?) WHERE value = ?)", []any{path, item}}, nil
	}
	if !operatorAllowed(f.Type, op) {
		return condition{}, bad("%s is not allowed on %s fields", op, f.Type)
	}
	normalized, err := normalizeValue(f, value)
	if err != nil {
		return condition{}, bad("fields.%s: %v", name, err)
	}
	field, placeholder := "json_extract(r.data, ?)", "?"
	if f.Type == FieldDatetime {
		field, placeholder = "unixepoch(json_extract(r.data, ?), 'subsec')", "unixepoch(?, 'subsec')"
	}
	arg := sqlArg(f.Type, normalized)
	if op == "!=" {
		return condition{"(json_extract(r.data, ?) IS NULL OR " + field + " <> " + placeholder + ")", []any{path, path, arg}}, nil
	}
	return condition{field + " " + op + " " + placeholder, []any{path, arg}}, nil
}

// splitWhere separates a filter into field name, operator and value. The name
// ends at the first operator; everything after it, trimmed, is the value.
func splitWhere(expr string) (name, op, value string, ok bool) {
	expr = strings.TrimSpace(expr)
	if i := strings.IndexFunc(expr, unicode.IsSpace); i > 0 && !strings.ContainsAny(expr[:i], "=!<>") {
		head, rest := expr[:i], strings.TrimSpace(expr[i:])
		switch {
		case rest == "exists" || rest == "missing":
			return head, rest, "", true
		case len(rest) > 3 && strings.HasPrefix(rest, "has") && unicode.IsSpace(rune(rest[3])):
			return head, "has", strings.TrimSpace(rest[3:]), true
		}
	}
	i := strings.IndexAny(expr, "=!<>")
	if i <= 0 {
		return "", "", "", false
	}
	for _, candidate := range comparisonOps {
		if strings.HasPrefix(expr[i:], candidate) {
			op = candidate
			break
		}
	}
	if op == "" {
		return "", "", "", false
	}
	name = strings.TrimSpace(expr[:i])
	value = strings.TrimSpace(expr[i+len(op):])
	return name, op, value, name != "" && value != ""
}

func operatorAllowed(t FieldType, op string) bool {
	switch t {
	case FieldStringList:
		return false
	case FieldBoolean, FieldEnum, FieldText:
		return op == "=" || op == "!="
	}
	return true
}

// sqlArg binds a normalized value the way json_extract returns it: integers
// and booleans as integers, everything else as stored.
func sqlArg(t FieldType, value any) any {
	switch t {
	case FieldInteger:
		return int64(value.(float64))
	case FieldBoolean:
		if value.(bool) {
			return int64(1)
		}
		return int64(0)
	}
	return value
}

// coreSortKeys maps sortable record columns to SQL. They win over a typed
// field with the same name.
var coreSortKeys = map[string]string{
	"priority": "r.priority",
	"updated":  "r.updated_ns",
	"created":  "unixepoch(json_extract(r.data, '$.created_at'), 'subsec')",
	"title":    "json_extract(r.data, '$.title')",
	"status":   "r.status",
}

// parseSort turns a sort spec into an ORDER BY list that always ends with the
// record ID. k is nil when the list has no kind filter.
func parseSort(k *Kind, spec string) (string, error) {
	keys := strings.Split(spec, ",")
	if len(keys) > maxSortKeys {
		return "", invalid(fmt.Sprintf("sort supports at most %d keys", maxSortKeys))
	}
	var terms []string
	seen := map[string]bool{}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		bad := func(message string) error { return invalid(fmt.Sprintf("sort %q: %s", key, message)) }
		name := strings.TrimPrefix(key, "-")
		direction := ""
		if name != key {
			direction = " DESC"
		}
		if name == "" {
			return "", bad("empty sort key")
		}
		if seen[name] {
			return "", bad("repeated sort key")
		}
		seen[name] = true
		if column, ok := coreSortKeys[name]; ok {
			terms = append(terms, column+direction)
			continue
		}
		if k == nil {
			return "", bad("requires a kind filter")
		}
		f, ok := k.Definition.Schema[name]
		if !ok {
			return "", bad("unknown field of kind " + k.Name)
		}
		if f.Type == FieldStringList {
			return "", bad("string-list fields cannot be sorted")
		}
		// Schema field names match namePattern, so the path is safe to inline.
		value := "json_extract(r.data, '$.fields." + name + "')"
		sortValue := value
		if f.Type == FieldDatetime {
			sortValue = "unixepoch(" + value + ", 'subsec')"
		}
		terms = append(terms, "("+value+" IS NULL)", sortValue+direction)
	}
	return strings.Join(append(terms, "r.id ASC"), ", "), nil
}
