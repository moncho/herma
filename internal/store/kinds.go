package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
)

// namePattern constrains kind names, field names, statuses and enum values.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

const (
	maxSchemaFields     = 30
	maxDefinitionBytes  = 16 << 10
	maxKindStatuses     = 20
	maxEnumValues       = 100
	maxContextRecords   = 20
	builtinContextLimit = 100
)

type FieldType string

const (
	FieldString     FieldType = "string"
	FieldText       FieldType = "text"
	FieldInteger    FieldType = "integer"
	FieldNumber     FieldType = "number"
	FieldBoolean    FieldType = "boolean"
	FieldDate       FieldType = "date"
	FieldDatetime   FieldType = "datetime"
	FieldURL        FieldType = "url"
	FieldEnum       FieldType = "enum"
	FieldStringList FieldType = "string-list"
)

// FieldDef declares one typed field of a kind.
type FieldDef struct {
	Type     FieldType `json:"type"`
	Required bool      `json:"required,omitempty"`
	Unique   bool      `json:"unique,omitempty"`
	Min      *float64  `json:"min,omitempty"`
	Max      *float64  `json:"max,omitempty"`
	Values   []string  `json:"values,omitempty"`
}

// ContextPolicy adds a kind's records to session context as one section.
type ContextPolicy struct {
	Statuses   []string `json:"statuses"`
	Order      string   `json:"order"`
	MaxRecords int      `json:"max_records"`
}

type Policy struct {
	Review  bool           `json:"review"`
	Writers Role           `json:"writers"`
	Recall  bool           `json:"recall"`
	Context *ContextPolicy `json:"context,omitempty"`
}

// Definition is the document a kind record holds in fields.definition.
type Definition struct {
	Schema   map[string]FieldDef `json:"schema,omitempty"`
	Statuses []string            `json:"statuses,omitempty"`
	Policy   Policy              `json:"policy"`
}

// Kind is a resolved record kind: a built-in compiled into herma or an
// unarchived record of kind "kind".
type Kind struct {
	Name       string
	Builtin    bool
	Status     string // proposed, accepted or retired; built-ins are accepted
	ProjectID  string // records of a project-scoped kind must belong to it
	RecordID   string
	Body       string
	Definition Definition
	Pending    bool
	// Built-in behaviour that a custom definition cannot set.
	Heading       string // context section heading
	ContextTier   int    // context sections in one tier compete by priority
	ContextGlobal bool   // context includes records without a project
	OptIn         bool   // the section loads only when durable content is requested
}

var reviewStatuses = []string{"proposed", "accepted", "rejected", "superseded"}

// Statuses lists the statuses records of the kind may have; the first is the default.
func (k Kind) Statuses() []string {
	if k.Definition.Policy.Review {
		return reviewStatuses
	}
	return k.Definition.Statuses
}

func (k Kind) DefaultStatus() string { return k.Statuses()[0] }

func (k Kind) allows(status string) bool { return slices.Contains(k.Statuses(), status) }

// review reports whether records of the kind wait for a reviewer's judgement.
func (k Kind) review() bool { return k.Definition.Policy.Review }

// builtinOrder fixes the order of built-in kinds in listings and context.
var builtinOrder = []string{"project", "task", "feedback", "note", "knowledge", "principle", "kind"}

var builtinKinds = map[string]Kind{
	"project": {Definition: Definition{Statuses: []string{"planned", "active", "paused", "completed"}}},
	"task": {Heading: "Tasks", ContextTier: 0, Definition: Definition{
		Statuses: []string{"open", "in_progress", "blocked", "done"},
		Policy:   Policy{Context: &ContextPolicy{Statuses: []string{"open", "in_progress", "blocked"}, Order: "priority", MaxRecords: builtinContextLimit}},
	}},
	"feedback": {Heading: "Feedback", ContextTier: 0, Definition: Definition{
		Statuses: []string{"open", "triaged", "resolved"},
		Policy:   Policy{Context: &ContextPolicy{Statuses: []string{"open", "triaged"}, Order: "priority", MaxRecords: builtinContextLimit}},
	}},
	"note": {Heading: "Notes", ContextTier: 1, Definition: Definition{
		Statuses: []string{"published"},
		Policy:   Policy{Context: &ContextPolicy{Statuses: []string{"published"}, Order: "recent", MaxRecords: builtinContextLimit}},
	}},
	"knowledge": {Heading: "Knowledge", ContextTier: 2, ContextGlobal: true, OptIn: true, Definition: Definition{
		Policy: Policy{Review: true, Recall: true, Context: &ContextPolicy{Statuses: []string{"accepted"}, Order: "priority", MaxRecords: builtinContextLimit}},
	}},
	// Principles load through their own context block and the rules file.
	"principle": {ContextGlobal: true, Definition: Definition{Policy: Policy{Review: true, Recall: true}}},
	// Kind records follow their own lifecycle; see kindrecords.go.
	"kind": {Definition: Definition{Statuses: []string{"proposed", "accepted", "retired"}}},
}

func init() {
	for name, k := range builtinKinds {
		k.Name, k.Builtin, k.Status = name, true, "accepted"
		k.Definition.Policy.Writers = RoleAgent
		builtinKinds[name] = k
	}
}

// parseDefinition decodes and validates a definition document. field names
// the document in errors, such as fields.definition or fields.pending.
func parseDefinition(field string, data []byte) (Definition, error) {
	if len(data) > maxDefinitionBytes {
		return Definition{}, invalid(field + ": must be at most 16 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var d Definition
	if err := decoder.Decode(&d); err != nil {
		return Definition{}, invalid(field + ": " + err.Error())
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Definition{}, invalid(field + ": must be a single JSON object")
	}
	if d.Policy.Writers == "" {
		d.Policy.Writers = RoleAgent
	}
	return d, d.validate(field)
}

// definitionFrom parses a definition held as a decoded JSON value.
func definitionFrom(field string, value any) (Definition, error) {
	if _, ok := value.(map[string]any); !ok {
		return Definition{}, invalid(field + ": must be a JSON object")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return Definition{}, invalid(field + ": must be a JSON object")
	}
	return parseDefinition(field, data)
}

// definitionValue is the canonical stored form of a definition.
func definitionValue(d Definition) any {
	data, err := json.Marshal(d)
	if err != nil {
		panic(err) // a Definition always encodes
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		panic(err)
	}
	return value
}

func (d Definition) validate(field string) error {
	bad := func(format string, args ...any) error { return invalid(field + ": " + fmt.Sprintf(format, args...)) }
	if len(d.Schema) > maxSchemaFields {
		return bad("schema supports at most %d fields", maxSchemaFields)
	}
	for _, name := range slices.Sorted(maps.Keys(d.Schema)) {
		f := d.Schema[name]
		if !namePattern.MatchString(name) {
			return bad("field name %q must match %s", name, namePattern)
		}
		switch f.Type {
		case FieldString, FieldText, FieldInteger, FieldNumber, FieldBoolean, FieldDate, FieldDatetime, FieldURL, FieldEnum, FieldStringList:
		default:
			return bad("field %s: unknown type %q", name, f.Type)
		}
		if (f.Min != nil || f.Max != nil) && f.Type != FieldInteger && f.Type != FieldNumber {
			return bad("field %s: min and max apply only to integer and number", name)
		}
		if f.Type == FieldInteger && (!integral(f.Min) || !integral(f.Max)) {
			return bad("field %s: integer bounds must be whole numbers", name)
		}
		if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
			return bad("field %s: min exceeds max", name)
		}
		if f.Type == FieldEnum {
			if len(f.Values) == 0 || len(f.Values) > maxEnumValues {
				return bad("field %s: enum needs 1 to %d values", name, maxEnumValues)
			}
			if err := validNames(f.Values); err != nil {
				return bad("field %s: values: %v", name, err)
			}
		} else if len(f.Values) > 0 {
			return bad("field %s: values apply only to enum", name)
		}
		if f.Unique {
			switch f.Type {
			case FieldString, FieldURL, FieldInteger, FieldDate, FieldDatetime:
			default:
				return bad("field %s: unique applies only to string, url, integer, date and datetime", name)
			}
		}
	}
	p := d.Policy
	if p.Writers != RoleAgent && p.Writers != RoleReviewer {
		return bad("policy.writers must be agent or reviewer")
	}
	statuses := d.Statuses
	if p.Review {
		if len(d.Statuses) > 0 {
			return bad("statuses must be omitted when policy.review is true")
		}
		statuses = reviewStatuses
	} else {
		if len(d.Statuses) == 0 || len(d.Statuses) > maxKindStatuses {
			return bad("statuses needs 1 to %d names", maxKindStatuses)
		}
		if err := validNames(d.Statuses); err != nil {
			return bad("statuses: %v", err)
		}
	}
	if c := p.Context; c != nil {
		if len(c.Statuses) == 0 {
			return bad("policy.context.statuses needs at least one status")
		}
		for _, status := range c.Statuses {
			if !slices.Contains(statuses, status) {
				return bad("policy.context.statuses: %q is not a status of this kind", status)
			}
		}
		if c.Order != "priority" && c.Order != "recent" {
			return bad("policy.context.order must be priority or recent")
		}
		if c.MaxRecords < 1 || c.MaxRecords > maxContextRecords {
			return bad("policy.context.max_records must be 1 to %d", maxContextRecords)
		}
	}
	return nil
}

func validNames(values []string) error {
	seen := map[string]bool{}
	for _, value := range values {
		if !namePattern.MatchString(value) {
			return fmt.Errorf("%q must match %s", value, namePattern)
		}
		if seen[value] {
			return fmt.Errorf("%q is repeated", value)
		}
		seen[value] = true
	}
	return nil
}

func integral(v *float64) bool { return v == nil || *v == math.Trunc(*v) }
