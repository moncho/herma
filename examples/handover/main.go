// This example uses two authenticated clients against a running herma service.
// Run it after adding session-a and session-b identities (herma identity add, agents by default), then starting herma serve.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/moncho/herma/internal/client"
	"github.com/moncho/herma/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	path := os.Getenv("HERMA_CREDENTIALS")
	if path == "" {
		path = ".herma/credentials.json"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read demo credentials: %w; first run herma init, then herma identity add session-a and herma identity add session-b (agents by default)", err)
	}
	var identities map[string]struct {
		Token string `json:"token"`
	}
	if err = json.Unmarshal(data, &identities); err != nil {
		return err
	}
	endpoint := os.Getenv("HERMA_URL")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8765"
	}
	a, err := client.New(endpoint, identities["session-a"].Token)
	if err != nil {
		return fmt.Errorf("session-a: %w", err)
	}
	b, err := client.New(endpoint, identities["session-b"].Token)
	if err != nil {
		return fmt.Errorf("session-b: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return err
	}
	runID := "handover-" + hex.EncodeToString(nonce[:])
	create := func(c *client.Client, key string, input store.CreateInput) (store.Record, error) {
		data, err := c.Do(ctx, http.MethodPost, "/v1/records", nil, input, runID+"-"+key)
		if err != nil {
			return store.Record{}, err
		}
		var record store.Record
		err = json.Unmarshal(data, &record)
		return record, err
	}
	project, err := create(a, "project", store.CreateInput{Kind: "project", Title: "Demo: shared session handover", Status: "active", Tags: []string{"demo", runID}})
	if err != nil {
		return err
	}
	// This is an illustrative reference only. The example makes no requests to
	// an issue tracker and leaves canonical issue status and durable knowledge
	// elsewhere.
	const issueReference = "https://issues.example/DEMO-1"
	initialNote, err := create(a, "initial-note", store.CreateInput{
		Kind: "note", Title: "Session A handoff", ProjectID: project.ID,
		Body: "Changed: Prepared this demo handoff.\nChecked: Session A can write coordination context.\nNext: Session B should retrieve the handoff and finish the coordination check.\nReferences: The issue URL is a placeholder; durable decisions belong in memory files or the wiki.",
		Tags: []string{"demo", "handoff"}, Sources: []string{issueReference},
	})
	if err != nil {
		return err
	}
	task, err := create(a, "task", store.CreateInput{
		Kind: "task", Title: "Session A is preparing the demo handoff", ProjectID: project.ID,
		Body:   "Working intent: preparing this project's session handoff. Session B should inspect the handoff, finish this coordination check, and leave an outcome note. The real issue and its status stay in the issue tracker.",
		Status: "in_progress", Owner: "session-a", Priority: 4,
		Links: []string{initialNote.ID}, Tags: []string{"demo"}, Sources: []string{issueReference},
	})
	if err != nil {
		return err
	}
	packet, err := b.Do(ctx, http.MethodGet, "/v1/context", url.Values{"project_id": {project.ID}}, nil, "")
	if err != nil {
		return err
	}
	var read struct {
		Sections []struct {
			Kind    string         `json:"kind"`
			Records []store.Record `json:"records"`
		} `json:"sections"`
	}
	if err = json.Unmarshal(packet, &read); err != nil {
		return err
	}
	sections := map[string][]store.Record{}
	for _, s := range read.Sections {
		sections[s.Kind] = s.Records
	}
	notes, tasks := sections["note"], sections["task"]
	if len(notes) != 1 || notes[0].ID != initialNote.ID || len(tasks) != 1 || tasks[0].ID != task.ID {
		return fmt.Errorf("fresh-session context did not contain the expected handoff and coordination record")
	}
	// Context is a bounded summary. Fetch the complete record and its current
	// version before editing, even when the summary already includes a version.
	fresh, err := b.Do(ctx, http.MethodGet, "/v1/records/"+task.ID, nil, nil, "")
	if err != nil {
		return err
	}
	var current store.Record
	if err = json.Unmarshal(fresh, &current); err != nil {
		return err
	}
	done, owner := "done", "session-b"
	updated, err := b.Do(ctx, http.MethodPatch, "/v1/records/"+task.ID, nil, store.UpdateInput{Version: current.Version, Status: &done, Owner: &owner}, runID+"-complete")
	if err != nil {
		return err
	}
	var complete store.Record
	if err = json.Unmarshal(updated, &complete); err != nil {
		return err
	}
	note, err := create(b, "final-note", store.CreateInput{
		Kind: "note", Title: "Session B handoff outcome", ProjectID: project.ID,
		Body:  "Changed: Session B retrieved session A's handoff and finished the coordination check.\nChecked: Read the complete coordination record before updating its version.\nNext: Use this example's pattern for session handoffs.\nReferences: No issue was updated; durable knowledge stays in memory files or the wiki.",
		Links: []string{initialNote.ID, task.ID}, Tags: []string{"demo", "handoff"}, Sources: []string{issueReference},
	})
	if err != nil {
		return err
	}
	history, err := b.Do(ctx, http.MethodGet, "/v1/records/"+task.ID+"/history", nil, nil, "")
	if err != nil {
		return err
	}
	var revisions struct {
		Items []store.Revision `json:"items"`
	}
	if err = json.Unmarshal(history, &revisions); err != nil {
		return err
	}
	if len(revisions.Items) != 2 || revisions.Items[0].Actor != "session-a" || revisions.Items[1].Actor != "session-b" {
		return fmt.Errorf("revision authors did not match the authenticated sessions")
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(map[string]any{
		"result": "handover verified", "project_id": project.ID, "initial_note_id": initialNote.ID, "coordination_id": task.ID, "final_note_id": note.ID,
		"coordination_status": complete.Status, "coordination_version": complete.Version, "created_by": complete.CreatedBy, "completed_by": complete.UpdatedBy,
		"next_command": "herma context --project " + project.ID,
	})
}
