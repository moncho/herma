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
	BodyTruncated   bool       `json:"body_truncated,omitempty"`
	TruncatedFields []string   `json:"truncated_fields,omitempty"`
}

type projectContext struct {
	Scope          string          `json:"scope"`
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
}

// packContext counts the actual wire representation, including JSON escaping,
// all metadata and the final newline. A single huge record cannot consume the
// packet: each preview may use at most a quarter of the budget or 2 KiB.
func packContext(snapshot contextSnapshot, budget int, includeDurable bool) ([]byte, error) {
	if budget < minContextBytes || budget > maxContextBytes {
		return nil, errors.New("invalid context budget")
	}
	recordBudget := min(2048, budget/4)
	project, ok := fitContextRecord(snapshot.Project, recordBudget)
	if !ok {
		return nil, errors.New("project identity does not fit the context budget")
	}
	result := projectContext{
		Scope: contextScope, Project: project, GeneratedAt: snapshot.GeneratedAt,
		Tasks: []contextRecord{}, Feedback: []contextRecord{}, Notes: []contextRecord{},
		Knowledge: []contextRecord{}, Principles: []contextRecord{},
		MaxBytes: budget, IncludeDurable: includeDurable, Omitted: snapshot.Totals,
	}
	if !includeDurable {
		result.Omitted.Knowledge, result.Omitted.Principles = 0, 0
	}
	clipped := recordClipped(project)
	result.Truncated = clipped || result.Omitted.any()
	data, err := encodeContext(result)
	if err != nil || len(data) > budget {
		return nil, errors.New("project context metadata does not fit the context budget")
	}
	type candidate struct {
		record  store.Record
		target  *[]contextRecord
		omitted *int
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
	// their priority. Durable information is always considered last and opt-in.
	notes := append([]store.Record(nil), snapshot.Notes...)
	sort.Slice(notes, func(i, j int) bool { return contextRecordLess(notes[i], notes[j], true) })
	for _, record := range notes {
		candidates = append(candidates, candidate{record, &result.Notes, &result.Omitted.Notes})
	}
	if includeDurable {
		for _, record := range snapshot.Knowledge {
			candidates = append(candidates, candidate{record, &result.Knowledge, &result.Omitted.Knowledge})
		}
		for _, record := range snapshot.Principles {
			candidates = append(candidates, candidate{record, &result.Principles, &result.Omitted.Principles})
		}
	}
	for _, candidate := range candidates {
		// Reserve one byte for a new array comma and one for true -> false if
		// this completes an otherwise untruncated packet. Omission counts only shrink.
		available := min(recordBudget, budget-len(data)-2)
		if available <= 0 {
			break
		}
		record, fits := fitContextRecord(candidate.record, available)
		if !fits {
			continue
		}
		*candidate.target = append(*candidate.target, record)
		*candidate.omitted--
		previousClipped := clipped
		clipped = clipped || recordClipped(record)
		result.Truncated = clipped || result.Omitted.any()
		next, err := encodeContext(result)
		if err != nil {
			return nil, err
		}
		if len(next) > budget {
			// Keep the final check independent of size-estimation assumptions.
			*candidate.target = (*candidate.target)[:len(*candidate.target)-1]
			*candidate.omitted++
			clipped = previousClipped
			result.Truncated = clipped || result.Omitted.any()
			continue
		}
		data = next
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

func fitContextRecord(record store.Record, budget int) (contextRecord, bool) {
	out := contextRecord{
		ID: record.ID, Kind: record.Kind, Title: record.Title, Body: record.Body,
		Status: record.Status, Version: record.Version, Priority: record.Priority,
		ProjectID: record.ProjectID, Owner: record.Owner, UpdatedBy: record.UpdatedBy,
		UpdatedAt: &record.UpdatedAt, Sources: record.Sources, Links: record.Links,
	}
	if contextRecordSize(out) <= budget {
		return out, true
	}
	// Reserve useful text space rather than letting large source lists or other
	// metadata crowd out the handoff itself. References are omitted whole: a URL
	// or record ID must never become a misleading shortened reference.
	out.Body = ""
	out.BodyTruncated = record.Body != ""
	metadataBudget := min(budget, max(384, budget/2))
	if contextRecordSize(out) > metadataBudget {
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
	for _, field := range []string{"sources", "links", "owner", "updated_by", "project_id", "priority", "updated_at"} {
		if contextRecordSize(out) <= metadataBudget {
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
		}
		markContextField(&out, field)
	}
	if contextRecordSize(out) > metadataBudget {
		markContextField(&out, "title")
		title := out.Title
		out.Title = ""
		out.Title = fitText(title, func(prefix string) bool {
			out.Title = prefix
			return contextRecordSize(out) <= metadataBudget
		})
	}
	if contextRecordSize(out) > budget {
		return contextRecord{}, false
	}
	// Removing large metadata may allow the complete body to fit. Check that
	// before searching prefixes because clearing body_truncated saves bytes.
	out.Body, out.BodyTruncated = record.Body, false
	if contextRecordSize(out) <= budget {
		return out, true
	}
	out.Body, out.BodyTruncated = "", record.Body != ""
	out.Body = fitText(record.Body, func(prefix string) bool {
		out.Body = prefix
		return contextRecordSize(out) <= budget
	})
	return out, contextRecordSize(out) <= budget
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
