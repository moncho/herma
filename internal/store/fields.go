package store

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxStringRunes = 300
	maxTextBytes   = 16 << 10
	maxURLBytes    = 2048
	maxListItems   = 50
	maxSafeInteger = 1 << 53
)

// normalizeFields validates fields against a definition and returns their
// canonical values: string, float64, bool or []any of strings.
func normalizeFields(d Definition, fields map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(fields))
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		f, ok := d.Schema[name]
		if !ok {
			return nil, invalid("fields." + name + ": unknown field")
		}
		if fields[name] == nil {
			return nil, invalid("fields." + name + ": must not be null")
		}
		value, err := normalizeValue(f, fields[name])
		if err != nil {
			return nil, invalid("fields." + name + ": " + err.Error())
		}
		out[name] = value
	}
	for _, name := range slices.Sorted(maps.Keys(d.Schema)) {
		if _, ok := out[name]; d.Schema[name].Required && !ok {
			return nil, invalid("fields." + name + ": required")
		}
	}
	return out, nil
}

func normalizeValue(f FieldDef, value any) (any, error) {
	switch f.Type {
	case FieldString:
		s, ok := value.(string)
		s = strings.TrimSpace(s)
		if !ok || s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxStringRunes || strings.ContainsAny(s, "\r\n") {
			return nil, fmt.Errorf("must be one line of 1 to %d characters", maxStringRunes)
		}
		return s, nil
	case FieldText:
		s, ok := value.(string)
		if !ok || len(s) > maxTextBytes || !utf8.ValidString(s) {
			return nil, fmt.Errorf("must be text of at most 16 KiB")
		}
		return s, nil
	case FieldInteger:
		n, ok := number(value)
		if !ok || n != math.Trunc(n) || math.Abs(n) > maxSafeInteger || !inBounds(f, n) {
			return nil, fmt.Errorf("must be an integer%s", boundsText(f))
		}
		return n, nil
	case FieldNumber:
		n, ok := number(value)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) || !inBounds(f, n) {
			return nil, fmt.Errorf("must be a number%s", boundsText(f))
		}
		return n, nil
	case FieldBoolean:
		switch v := value.(type) {
		case bool:
			return v, nil
		case string:
			if v == "true" || v == "false" {
				return v == "true", nil
			}
		}
		return nil, fmt.Errorf("must be true or false")
	case FieldDate:
		s, ok := value.(string)
		if _, err := time.Parse(time.DateOnly, s); !ok || err != nil {
			return nil, fmt.Errorf("must be a date (YYYY-MM-DD)")
		}
		return s, nil
	case FieldDatetime:
		s, ok := value.(string)
		t, err := time.Parse(time.RFC3339Nano, s)
		if !ok || err != nil {
			return nil, fmt.Errorf("must be an RFC 3339 datetime")
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	case FieldURL:
		s, ok := value.(string)
		u, err := url.Parse(s)
		if !ok || err != nil || len(s) > maxURLBytes || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("must be an absolute http or https URL of at most %d bytes", maxURLBytes)
		}
		return s, nil
	case FieldEnum:
		s, ok := value.(string)
		if !ok || !slices.Contains(f.Values, s) {
			return nil, fmt.Errorf("must be one of %s", strings.Join(f.Values, ", "))
		}
		return s, nil
	case FieldStringList:
		return normalizeList(value)
	}
	return nil, fmt.Errorf("has unknown type %q", f.Type)
}

func normalizeList(value any) (any, error) {
	var items []string
	switch v := value.(type) {
	case string:
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				items = append(items, item)
			}
		}
	case []string:
		items = v
	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("must be a list of strings")
			}
			items = append(items, s)
		}
	default:
		return nil, fmt.Errorf("must be a list of strings")
	}
	if len(items) > maxListItems {
		return nil, fmt.Errorf("supports at most %d items", maxListItems)
	}
	out := make([]any, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || !utf8.ValidString(item) || utf8.RuneCountInString(item) > maxStringRunes || strings.ContainsAny(item, "\r\n") {
			return nil, fmt.Errorf("items must be one line of 1 to %d characters", maxStringRunes)
		}
		if seen[item] {
			return nil, fmt.Errorf("%q is repeated", item)
		}
		seen[item] = true
		out = append(out, item)
	}
	return out, nil
}

// number accepts JSON and Go numbers, and decimal strings from the CLI.
func number(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	case string:
		if strings.ContainsAny(v, "eE") {
			return 0, false
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n, err == nil
	}
	return 0, false
}

func inBounds(f FieldDef, n float64) bool {
	return (f.Min == nil || n >= *f.Min) && (f.Max == nil || n <= *f.Max)
}

func boundsText(f FieldDef) string {
	format := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	switch {
	case f.Min != nil && f.Max != nil:
		return " from " + format(*f.Min) + " to " + format(*f.Max)
	case f.Min != nil:
		return " of at least " + format(*f.Min)
	case f.Max != nil:
		return " of at most " + format(*f.Max)
	}
	return ""
}

// patchFields applies an update patch: a nil value removes the field.
func patchFields(current, patch map[string]any) map[string]any {
	next := maps.Clone(current)
	if next == nil {
		next = map[string]any{}
	}
	for name, value := range patch {
		if value == nil {
			delete(next, name)
		} else {
			next[name] = value
		}
	}
	return next
}

// searchText joins the values of a record's textual fields for full-text search.
func searchText(k Kind, fields map[string]any) string {
	var parts []string
	for _, name := range slices.Sorted(maps.Keys(k.Definition.Schema)) {
		switch k.Definition.Schema[name].Type {
		case FieldString, FieldText, FieldURL:
			if s, ok := fields[name].(string); ok {
				parts = append(parts, s)
			}
		case FieldStringList:
			if items, ok := fields[name].([]any); ok {
				for _, item := range items {
					parts = append(parts, item.(string))
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}
