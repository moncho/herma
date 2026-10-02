package api

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/moncho/herma/internal/store"
)

// Context is a bounded preview, not the record/export representation. Creation
// metadata, tags, and archival flags are intentionally outside this projection.
// Every further omission or shortening is identified on the individual record.
type contextRecord struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Title           string     `json:"title"`
	Body            string     `json:"body"`
	Status          string     `json:"status"`
	Version         int64      `json:"version"`
	Priority        int        `json:"priority,omitempty"`
	ProjectID       string     `json:"project_id,omitempty"`
	Owner           string     `json:"owner,omitempty"`
	UpdatedBy       string     `json:"updated_by,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	Sources         []string   `json:"sources,omitempty"`
	Links           []string   `json:"links,omitempty"`
	ReviewedBy      string     `json:"reviewed_by,omitempty"`
	ReviewedAt      *time.Time `json:"reviewed_at,omitempty"`
	BodyTruncated   bool       `json:"body_truncated,omitempty"`
	TruncatedFields []string   `json:"truncated_fields,omitempty"`
}

type projectContext struct {
	Scope          string          `json:"scope"`
	Recall         string          `json:"recall"`
	Project        contextRecord   `json:"project"`
	Tasks          []contextRecord `json:"tasks"`
	Feedback       []contextRecord `json:"feedback"`
	Notes          []contextRecord `json:"notes"`
	Knowledge      []contextRecord `json:"knowledge"`
	Principles     []contextRecord `json:"principles"`
	GeneratedAt    time.Time       `json:"generated_at"`
	MaxBytes       int             `json:"max_bytes"`
	IncludeDurable bool            `json:"include_durable"`
	Truncated      bool            `json:"truncated"`
	Omitted        contextCounts   `json:"omitted"`
	// PrinciplesLabel is "changed" when principles replace the rules file a
	// Claude session already loaded, or "replace" when herma could not update that
	// file. Only the text format shows it.
	PrinciplesLabel string `json:"-"`
}

// contextFormat renders a packed context and measures one record preview, and
// the project preview, in the same representation, so every budget decision
// counts the bytes sent.
type contextFormat interface {
	encode(projectContext) ([]byte, error)
	recordSize(contextRecord) int
	projectSize(contextRecord) int
	contentType() string
}

type jsonFormat struct{}

func (jsonFormat) encode(c projectContext) ([]byte, error) { return encodeContext(c) }
func (jsonFormat) recordSize(r contextRecord) int          { return contextRecordSize(r) }
func (jsonFormat) projectSize(r contextRecord) int         { return contextRecordSize(r) }
func (jsonFormat) contentType() string                     { return "application/json; charset=utf-8" }

type contextOptions struct {
	budget          int
	includeDurable  bool
	format          contextFormat
	principlesLabel string
}

// packContext counts the actual wire representation, including JSON escaping,
// all metadata and the final newline. A single huge record cannot consume the
// packet: each preview may use at most a quarter of the budget or 2 KiB.
func packContext(snapshot contextSnapshot, opts contextOptions) ([]byte, error) {
	budget, includeDurable := opts.budget, opts.includeDurable
	if budget < minContextBytes || budget > maxContextBytes {
		return nil, errors.New("invalid context budget")
	}
	recordBudget := min(2048, budget/4)
	project, ok := fitContextRecord(snapshot.Project, recordBudget, opts.format.projectSize)
	if !ok {
		return nil, errors.New("project identity does not fit the context budget")
	}
	result := projectContext{
		Scope: contextScope, Recall: recallHint, Project: project, GeneratedAt: snapshot.GeneratedAt,
		Tasks: []contextRecord{}, Feedback: []contextRecord{}, Notes: []contextRecord{},
		Knowledge: []contextRecord{}, Principles: []contextRecord{},
		MaxBytes: budget, IncludeDurable: includeDurable, Omitted: snapshot.Totals,
		PrinciplesLabel: opts.principlesLabel,
	}
	if !includeDurable {
		result.Omitted.Knowledge = 0
	}
	clipped := recordClipped(project)
	result.Truncated = clipped || result.Omitted.any()
	data, err := opts.format.encode(result)
	if err != nil || len(data) > budget {
		return nil, errors.New("project context metadata does not fit the context budget")
	}
	type candidate struct {
		record  store.Record
		target  *[]contextRecord
		omitted *int
	}
	// add places one candidate if the packet stays within limit. It reports
	// false when no space is left at all, so callers can stop early.
	add := func(c candidate, limit int) (bool, error) {
		// Reserve one byte for a new array comma and one for true -> false if
		// this completes an otherwise untruncated packet. Omission counts only shrink.
		available := min(recordBudget, limit-len(data)-2)
		// The text format may also add a section heading and footer entries, so a
		// record clipped to available can still overflow. Retry with the overflow
		// subtracted rather than dropping a record that could be clipped to fit.
		for attempt := 0; attempt < 3; attempt++ {
			if available <= 0 {
				return attempt > 0, nil
			}
			record, fits := fitContextRecord(c.record, available, opts.format.recordSize)
			if !fits {
				return true, nil
			}
			*c.target = append(*c.target, record)
			*c.omitted--
			previousClipped := clipped
			clipped = clipped || recordClipped(record)
			result.Truncated = clipped || result.Omitted.any()
			next, err := opts.format.encode(result)
			if err != nil {
				return false, err
			}
			if len(next) <= limit {
				data = next
				return true, nil
			}
			// Keep the final check independent of size-estimation assumptions.
			*c.target = (*c.target)[:len(*c.target)-1]
			*c.omitted++
			clipped = previousClipped
			result.Truncated = clipped || result.Omitted.any()
			available -= len(next) - limit
		}
		return true, nil
	}
	// Principles are conventions every session follows, so they go first, but
	// they may grow the packet by at most half of the budget.
	// The clamp keeps the hard max_bytes guarantee independent of metadata size.
	principleLimit := min(budget, len(data)+budget/2)
	for _, record := range snapshot.Principles {
		if more, err := add(candidate{record, &result.Principles, &result.Omitted.Principles}, principleLimit); err != nil {
			return nil, err
		} else if !more {
			break
		}
	}
	coordination := make([]candidate, 0, len(snapshot.Tasks)+len(snapshot.Feedback))
	for _, record := range snapshot.Tasks {
		coordination = append(coordination, candidate{record, &result.Tasks, &result.Omitted.Tasks})
	}
	for _, record := range snapshot.Feedback {
		coordination = append(coordination, candidate{record, &result.Feedback, &result.Omitted.Feedback})
	}
	sort.Slice(coordination, func(i, j int) bool {
		return contextRecordLess(coordination[i].record, coordination[j].record, false)
	})
	candidates := coordination
	// Notes describe handoffs and recent session events, so recency outweighs
	// their priority. Knowledge is always considered last and opt-in.
	notes := append([]store.Record(nil), snapshot.Notes...)
	sort.Slice(notes, func(i, j int) bool { return contextRecordLess(notes[i], notes[j], true) })
	for _, record := range notes {
		candidates = append(candidates, candidate{record, &result.Notes, &result.Omitted.Notes})
	}
	if includeDurable {
		for _, record := range snapshot.Knowledge {
			candidates = append(candidates, candidate{record, &result.Knowledge, &result.Omitted.Knowledge})
		}
	}
	for _, c := range candidates {
		if more, err := add(c, budget); err != nil {
			return nil, err
		} else if !more {
			break
		}
	}
	return data, nil
}

func encodeContext(value projectContext) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func recordClipped(record contextRecord) bool {
	return record.BodyTruncated || len(record.TruncatedFields) > 0
}

func fitContextRecord(record store.Record, budget int, size func(contextRecord) int) (contextRecord, bool) {
	out := contextRecord{
		ID: record.ID, Kind: record.Kind, Title: record.Title, Body: record.Body,
		Status: record.Status, Version: record.Version, Priority: record.Priority,
		ProjectID: record.ProjectID, Owner: record.Owner, UpdatedBy: record.UpdatedBy,
		UpdatedAt: &record.UpdatedAt, Sources: record.Sources, Links: record.Links,
		ReviewedBy: record.ReviewedBy, ReviewedAt: record.ReviewedAt,
	}
	if size(out) <= budget {
		return out, true
	}
	// Reserve useful text space rather than letting large source lists or other
	// metadata crowd out the handoff itself. References are omitted whole: a URL
	// or record ID must never become a misleading shortened reference.
	out.Body = ""
	out.BodyTruncated = record.Body != ""
	metadataBudget := min(budget, max(384, budget/2))
	if size(out) > metadataBudget {
		if kept := fitReferences(out.Sources, min(256, metadataBudget/4)); len(kept) < len(out.Sources) {
			out.Sources = kept
			markContextField(&out, "sources")
		}
		if kept := fitReferences(out.Links, min(256, metadataBudget/4)); len(kept) < len(out.Links) {
			out.Links = kept
			markContextField(&out, "links")
		}
		if title := fitJSONString(out.Title, min(192, metadataBudget/3)); title != out.Title {
			out.Title = title
			markContextField(&out, "title")
		}
	}
	for _, field := range []string{"sources", "links", "owner", "updated_by", "project_id", "priority", "updated_at", "reviewed_by", "reviewed_at"} {
		if size(out) <= metadataBudget {
			break
		}
		switch field {
		case "sources":
			if len(out.Sources) == 0 {
				continue
			}
			out.Sources = nil
		case "links":
			if len(out.Links) == 0 {
				continue
			}
			out.Links = nil
		case "owner":
			if out.Owner == "" {
				continue
			}
			out.Owner = ""
		case "updated_by":
			if out.UpdatedBy == "" {
				continue
			}
			out.UpdatedBy = ""
		case "project_id":
			if out.ProjectID == "" {
				continue
			}
			out.ProjectID = ""
		case "priority":
			if out.Priority == 0 {
				continue
			}
			out.Priority = 0
		case "updated_at":
			out.UpdatedAt = nil
		case "reviewed_by":
			if out.ReviewedBy == "" {
				continue
			}
			out.ReviewedBy = ""
		case "reviewed_at":
			if out.ReviewedAt == nil {
				continue
			}
			out.ReviewedAt = nil
		}
		markContextField(&out, field)
	}
	if size(out) > metadataBudget {
		markContextField(&out, "title")
		title := out.Title
		out.Title = ""
		out.Title = fitText(title, func(prefix string) bool {
			out.Title = prefix
			return size(out) <= metadataBudget
		})
	}
	if size(out) > budget {
		return contextRecord{}, false
	}
	// Removing large metadata may allow the complete body to fit. Check that
	// before searching prefixes because clearing body_truncated saves bytes.
	out.Body, out.BodyTruncated = record.Body, false
	if size(out) <= budget {
		return out, true
	}
	out.Body, out.BodyTruncated = "", record.Body != ""
	out.Body = fitText(record.Body, func(prefix string) bool {
		out.Body = prefix
		return size(out) <= budget
	})
	return out, size(out) <= budget
}

func markContextField(record *contextRecord, field string) {
	for _, previous := range record.TruncatedFields {
		if field == previous {
			return
		}
	}
	record.TruncatedFields = append(record.TruncatedFields, field)
}

func contextRecordSize(record contextRecord) int {
	data, err := json.Marshal(record)
	if err != nil {
		return math.MaxInt
	}
	return len(data)
}

func fitReferences(values []string, budget int) []string {
	lo, hi := 0, len(values)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		data, _ := json.Marshal(values[:mid])
		if len(data) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return values[:lo]
}

func fitJSONString(value string, budget int) string {
	return fitText(value, func(prefix string) bool {
		data, _ := json.Marshal(prefix)
		return len(data) <= budget
	})
}

func fitText(value string, fits func(string) bool) string {
	// Rune boundaries preserve UTF-8 even when JSON escapes add bytes. The
	// original field is never changed in storage; callers expose clipping flags.
	runes := []rune(value)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if fits(string(runes[:mid])) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo])
}
