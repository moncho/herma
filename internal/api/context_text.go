package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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
	if line := kindsLine(c.Kinds, c.KindsMore); line != "" {
		b.WriteString(line + "\n")
	}
	if notes, ok := principlesLabels[c.PrinciplesLabel]; ok {
		b.WriteString("\n## " + notes.heading + "\n")
		// A clipped principle is not the complete set either.
		partial := c.PrinciplesOmitted > 0
		for _, r := range c.Principles {
			partial = partial || recordClipped(r)
		}
		switch {
		case len(c.Principles) == 0 && c.PrinciplesOmitted == 0:
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
	for _, s := range c.Sections {
		writeTextSection(&b, s.Heading, s.Records)
	}
	writeTextFooter(&b, c)
	return []byte(b.String()), nil
}

const maxKindsInLine = 20

// kindsLine names the custom kinds an agent can write in this project, so it
// finds an existing kind instead of proposing a duplicate.
// The packet already holds at most maxKindsInLine names; more counts the rest.
func kindsLine(kinds []string, more int) string {
	if len(kinds) == 0 && more == 0 {
		return ""
	}
	line := "Custom kinds:"
	if len(kinds) > 0 {
		line += " " + strings.Join(kinds, ", ")
	}
	if more > 0 {
		line += fmt.Sprintf(" +%d more", more)
	}
	return line + " (herma schema for fields)"
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
	writeTextFields(b, r.Fields)
	writeTextReferences(b, r)
}

// writeTextFields prints one indented line per typed field, by name. A text
// field may span lines; writeIndented keeps each of them indented.
func writeTextFields(b *strings.Builder, fields map[string]any) {
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		writeIndented(b, name+": "+fieldText(fields[name]))
	}
}

func fieldText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case []any:
		items := make([]string, len(v))
		for i, item := range v {
			items[i] = fieldText(item)
		}
		return strings.Join(items, ", ")
	}
	data, _ := json.Marshal(value)
	return string(data)
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
	if c.PrinciplesOmitted > 0 {
		omitted = append(omitted, fmt.Sprintf("principles %d", c.PrinciplesOmitted))
	}
	for _, s := range c.Sections {
		if s.Omitted > 0 {
			omitted = append(omitted, fmt.Sprintf("%s %d", strings.ToLower(s.Heading), s.Omitted))
		}
	}
	if c.CustomOmitted > 0 {
		omitted = append(omitted, fmt.Sprintf("custom %d", c.CustomOmitted))
	}
	var clipped []string
	records := append([]contextRecord{c.Project}, c.Principles...)
	for _, s := range c.Sections {
		records = append(records, s.Records...)
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
