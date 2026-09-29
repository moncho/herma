// This example uses two authenticated clients against a running herma service.
// Run it after creating session-a and session-b identities, then starting herma serve.
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
		return fmt.Errorf("read demo credentials: %w; first run herma init and herma identity add session-a/session-b", err)
	}
	var identities map[string]string
	if err = json.Unmarshal(data, &identities); err != nil {
		return err
	}
	endpoint := os.Getenv("HERMA_URL")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8765"
	}
	a, err := client.New(endpoint, identities["session-a"])
	if err != nil {
		return fmt.Errorf("session-a: %w", err)
	}
	b, err := client.New(endpoint, identities["session-b"])
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
	decision, err := create(a, "decision", store.CreateInput{Kind: "knowledge", Title: "Keep decisions traceable", Body: "Record the evidence behind a decision so the next session can verify it.", Status: "accepted", ProjectID: project.ID, Tags: []string{"demo"}, Sources: []string{"example:handover"}})
	if err != nil {
		return err
	}
	task, err := create(a, "task", store.CreateInput{Kind: "task", Title: "Record a successful session handover", Body: "A fresh session should retrieve the decision and this task, then leave a note.", ProjectID: project.ID, Priority: 4, Links: []string{decision.ID}, Tags: []string{"demo"}})
	if err != nil {
		return err
	}
	packet, err := b.Do(ctx, http.MethodGet, "/v1/context", url.Values{"project_id": {project.ID}}, nil, "")
	if err != nil {
		return err
	}
	var read struct {
		Knowledge []store.Record `json:"knowledge"`
		Tasks     []store.Record `json:"tasks"`
	}
	if err = json.Unmarshal(packet, &read); err != nil {
		return err
	}
	if len(read.Knowledge) != 1 || len(read.Tasks) != 1 || read.Tasks[0].ID != task.ID {
		return fmt.Errorf("fresh-session context did not contain the expected decision and task")
	}
	done, owner := "done", "session-b"
	updated, err := b.Do(ctx, http.MethodPatch, "/v1/records/"+task.ID, nil, store.UpdateInput{Version: read.Tasks[0].Version, Status: &done, Owner: &owner}, runID+"-complete")
	if err != nil {
		return err
	}
	var complete store.Record
	if err = json.Unmarshal(updated, &complete); err != nil {
		return err
	}
	note, err := create(b, "note", store.CreateInput{Kind: "note", Title: "Handover verified", Body: "Session B retrieved session A's decision and task, then recorded completion using the version it read.", ProjectID: project.ID, Links: []string{decision.ID, task.ID}, Tags: []string{"demo"}})
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
		"result": "handover verified", "project_id": project.ID, "decision_id": decision.ID, "task_id": task.ID, "note_id": note.ID,
		"task_status": complete.Status, "task_version": complete.Version, "created_by": complete.CreatedBy, "completed_by": complete.UpdatedBy,
		"next_command": "herma context --project " + project.ID,
	})
}
