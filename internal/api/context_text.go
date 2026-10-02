package api

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/moncho/herma/internal/rules"
)

const (
	principlesChangedNote = "herma principles changed after this session loaded them; this version replaces " + rules.Path + "."
	principlesNoneNote    = "No herma principles apply now; disregard the herma principles loaded earlier from " + rules.Path + "."
	principlesPartialNote = "herma principles changed after this session loaded them; only some fit here. The full updated set is in " + rules.Path + "; read it before relying on principles not listed here."

	principlesReplaceNote        = "herma could not update " + rules.Path + "; these principles replace it, and any herma principle that appears only there no longer applies."
	principlesReplacePartialNote = "herma could not update " + rules.Path + " and only some current principles fit here; they replace that file. Run herma recall before relying on a herma principle not listed here."
	principlesReplaceNoneNote    = "herma could not update " + rules.Path + "; no herma principles apply now, so disregard the ones loaded from it."
)

// principlesLabels holds the heading and notes for principles that stand in
// for the rules file, keyed by contextOptions.principlesLabel.
var principlesLabels = map[string]struct{ heading, all, partial, none string }{
	"changed": {"Principles changed", principlesChangedNote, principlesPartialNote, principlesNoneNote},
	"replace": {"Principles replacing the rules file", principlesReplaceNote, principlesReplacePartialNote, principlesReplaceNoneNote},
}

// textFormat is the compact rendering sessions read. Record text is indented
// under its header line, so no stored title or body can start a section.
type textFormat struct{}

func (textFormat) contentType() string { return "text/plain; charset=utf-8" }

func (textFormat) recordSize(r contextRecord) int {
	var b strings.Builder
	writeTextRecord(&b, r)
	return b.Len()
}

// generatedAtLayout always formats to 17 bytes, so projectSize can measure the
// header with a placeholder of the same width.
const generatedAtLayout = "2006-01-02T15:04Z"

func (textFormat) projectSize(r contextRecord) int {
	var b strings.Builder
	writeTextProject(&b, r, generatedAtLayout)
	return b.Len()
}

func (textFormat) encode(c projectContext) ([]byte, error) {
	var b strings.Builder
	writeTextProject(&b, c.Project, c.GeneratedAt.UTC().Format(generatedAtLayout))
	b.WriteString(contextScope + "\n" + recallHint + "\n")
	if notes, ok := principlesLabels[c.PrinciplesLabel]; ok {
		b.WriteString("\n## " + notes.heading + "\n")
		// A clipped principle is not the complete set either.
		partial := c.Omitted.Principles > 0
		for _, r := range c.Principles {
			partial = partial || recordClipped(r)
		}
		switch {
		case len(c.Principles) == 0 && c.Omitted.Principles == 0:
			b.WriteString(notes.none + "\n")
		case partial:
			b.WriteString(notes.partial + "\n")
		default:
			b.WriteString(notes.all + "\n")
		}
		for _, r := range c.Principles {
			writeTextRecord(&b, r)
		}
	} else {
		writeTextSection(&b, "Principles", c.Principles)
	}
	writeTextSection(&b, "Tasks", c.Tasks)
	writeTextSection(&b, "Feedback", c.Feedback)
	writeTextSection(&b, "Notes", c.Notes)
	writeTextSection(&b, "Knowledge", c.Knowledge)
	writeTextFooter(&b, c)
	return []byte(b.String()), nil
}

// writeTextProject prints the packet header line followed by the project's
// body, sources and links, indented like any record text.
func writeTextProject(b *strings.Builder, project contextRecord, generatedAt string) {
	fmt.Fprintf(b, "herma context · project %s (%s) · %s\n", oneLine(project.Title), project.ID, generatedAt)
	writeIndented(b, project.Body)
	writeTextReferences(b, project)
}

func writeTextSection(b *strings.Builder, heading string, records []contextRecord) {
	if len(records) == 0 {
		return
	}
	b.WriteString("\n## " + heading + "\n")
	for _, r := range records {
		writeTextRecord(b, r)
	}
}

func writeTextRecord(b *strings.Builder, r contextRecord) {
	parts := []string{r.ID, r.Status}
	if r.Priority != 0 {
		parts = append(parts, "p"+strconv.Itoa(r.Priority))
	}
	if r.Owner != "" {
		parts = append(parts, "owner "+oneLine(r.Owner))
	}
	if r.Kind == "note" && r.UpdatedBy != "" {
		parts = append(parts, "by "+oneLine(r.UpdatedBy))
	}
	if r.UpdatedAt != nil {
		parts = append(parts, r.UpdatedAt.UTC().Format("2006-01-02"))
	}
	// A clipped record must not read as complete where it appears; the footer
	// alone is too far away.
	if recordClipped(r) {
		parts = append(parts, "clipped")
	}
	b.WriteString("- " + strings.Join(parts, " · ") + "\n")
	writeIndented(b, r.Title)
	writeIndented(b, r.Body)
	writeTextReferences(b, r)
}

func writeTextReferences(b *strings.Builder, r contextRecord) {
	if len(r.Sources) > 0 {
		writeIndented(b, "sources: "+strings.Join(r.Sources, ", "))
	}
	if len(r.Links) > 0 {
		writeIndented(b, "links: "+strings.Join(r.Links, ", "))
	}
}

// writeIndented keeps every nonblank line four spaces in, so Markdown renders
// stored text as code and headings stay reserved for the packet's own structure.
// lineBreaks maps every break a renderer might honor to "\n" so stored text
// cannot start a line, and so a section, without being indented.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u0085", "\n", "\u2029", "\n", "\v", "\n", "\f", "\n")

func writeIndented(b *strings.Builder, text string) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return
	}
	text = lineBreaks.Replace(text)
	for _, line := range strings.Split(text, "\n") {
		if line != "" {
			b.WriteString("    " + line)
		}
		b.WriteByte('\n')
	}
}

func writeTextFooter(b *strings.Builder, c projectContext) {
	var omitted []string
	for _, item := range []struct {
		name  string
		count int
	}{
		{"principles", c.Omitted.Principles}, {"tasks", c.Omitted.Tasks}, {"feedback", c.Omitted.Feedback},
		{"notes", c.Omitted.Notes}, {"knowledge", c.Omitted.Knowledge},
	} {
		if item.count > 0 {
			omitted = append(omitted, fmt.Sprintf("%s %d", item.name, item.count))
		}
	}
	var clipped []string
	records := append([]contextRecord{c.Project}, c.Principles...)
	for _, group := range [][]contextRecord{c.Tasks, c.Feedback, c.Notes, c.Knowledge} {
		records = append(records, group...)
	}
	for _, r := range records {
		if r.BodyTruncated {
			clipped = append(clipped, r.ID+" body")
		}
		for _, field := range r.TruncatedFields {
			clipped = append(clipped, r.ID+" "+field)
		}
	}
	if len(omitted) == 0 && len(clipped) == 0 {
		return
	}
	b.WriteByte('\n')
	if len(omitted) > 0 {
		b.WriteString("Omitted: " + strings.Join(omitted, ", ") + ". ")
	}
	if len(clipped) > 0 {
		b.WriteString("Clipped: " + strings.Join(clipped, ", ") + ". ")
	}
	b.WriteString("Get the full record with: herma get <id>\n")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
