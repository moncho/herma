package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moncho/herma/internal/store"
)

// A gate signals the exact point at which simulated client I/O has stalled.
// Tests release it explicitly; the timeout below is only a deadlock guard.
type concurrencyGate struct {
	started     chan struct{}
	released    chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

func newConcurrencyGate() *concurrencyGate {
	return &concurrencyGate{started: make(chan struct{}), released: make(chan struct{})}
}

func (g *concurrencyGate) block() {
	g.startedOnce.Do(func() { close(g.started) })
	<-g.released
}

func (g *concurrencyGate) release() {
	g.releaseOnce.Do(func() { close(g.released) })
}

type gatedRequestBody struct {
	gate   *concurrencyGate
	reader io.Reader
}

func (b *gatedRequestBody) Read(p []byte) (int, error) {
	b.gate.block()
	return b.reader.Read(p)
}

func (b *gatedRequestBody) Close() error {
	b.gate.release()
	return nil
}

type gatedResponseWriter struct {
	*httptest.ResponseRecorder
	gate *concurrencyGate
}

func (w *gatedResponseWriter) WriteHeader(status int) {
	w.gate.block()
	w.ResponseRecorder.WriteHeader(status)
}

func (w *gatedResponseWriter) Write(p []byte) (int, error) {
	w.gate.block()
	return w.ResponseRecorder.Write(p)
}

func awaitConcurrencySignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func concurrencyRecord(t *testing.T, h http.Handler, body string) store.Record {
	t.Helper()
	w := apiTestRequest(h, http.MethodPost, "/v1/records", body, "application/json", "Bearer "+apiTestToken, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("seed create: status = %d, body = %s", w.Code, w.Body.String())
	}
	var record store.Record
	if err := json.Unmarshal(w.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// All requests begin while the first client remains blocked. Cleanup releases
// that client before joining workers, including when this assertion fails.
func assertOtherRequestsProgress(t *testing.T, h http.Handler, gate *concurrencyGate, writeBody string) {
	t.Helper()
	type result struct {
		name     string
		response *httptest.ResponseRecorder
		want     int
	}
	requests := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"list records", http.MethodGet, "/v1/records", "", http.StatusOK},
		{"read schema", http.MethodGet, "/v1/schema", "", http.StatusOK},
		{"another write", http.MethodPost, "/v1/records", writeBody, http.StatusCreated},
	}
	results := make(chan result, len(requests))
	finished := make(chan struct{})
	var workers sync.WaitGroup
	for _, request := range requests {
		workers.Add(1)
		go func() {
			defer workers.Done()
			w := apiTestRequest(h, request.method, request.path, request.body, "application/json", "Bearer "+apiTestToken, "")
			results <- result{name: request.name, response: w, want: request.want}
		}()
	}
	go func() {
		workers.Wait()
		close(finished)
	}()
	t.Cleanup(func() {
		gate.release()
		awaitConcurrencySignal(t, finished, "unblocked concurrent requests to finish")
	})
	pending := map[string]bool{"list records": true, "read schema": true, "another write": true}
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for range requests {
		select {
		case result := <-results:
			delete(pending, result.name)
			if result.response.Code != result.want {
				t.Errorf("%s: status = %d, want %d, body = %s", result.name, result.response.Code, result.want, result.response.Body.String())
			}
		case <-timeout.C:
			t.Fatalf("stalled client prevented other requests from completing: %v", pending)
		}
	}
}

func TestStalledRequestBodyDoesNotBlockOtherRequests(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			h, _ := apiTestHandler(t, nil)
			path := "/v1/records"
			body := `{"kind":"note","title":"Slowly uploaded note"}`
			wantStatus := http.StatusCreated
			if method == http.MethodPatch {
				task := concurrencyRecord(t, h, `{"kind":"task","title":"Task before slow upload"}`)
				path += "/" + task.ID
				body = `{"version":1,"title":"Task after slow upload"}`
				wantStatus = http.StatusOK
			}
			gate := newConcurrencyGate()
			r := httptest.NewRequest(method, path, nil)
			r.Body = &gatedRequestBody{gate: gate, reader: strings.NewReader(body)}
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+apiTestToken)
			w := httptest.NewRecorder()
			finished := make(chan struct{})
			go func() {
				h.ServeHTTP(w, r)
				close(finished)
			}()
			t.Cleanup(func() {
				gate.release()
				awaitConcurrencySignal(t, finished, "slow upload request to finish")
			})
			awaitConcurrencySignal(t, gate.started, "the first request to start reading its body")
			assertOtherRequestsProgress(t, h, gate, `{"kind":"note","title":"Independent write during slow upload"}`)
			gate.release()
			awaitConcurrencySignal(t, finished, "released upload request to finish")
			if w.Code != wantStatus {
				t.Errorf("released upload: status = %d, want %d, body = %s", w.Code, wantStatus, w.Body.String())
			}
		})
	}
}

func TestStalledResponseDoesNotBlockOtherRequests(t *testing.T) {
	for _, endpoint := range []string{"create", "update", "context", "export"} {
		t.Run(endpoint, func(t *testing.T) {
			h, _ := apiTestHandler(t, nil)
			project := concurrencyRecord(t, h, `{"kind":"project","title":"Snapshot project"}`)
			task := concurrencyRecord(t, h, `{"kind":"task","title":"Snapshot task","project_id":"`+project.ID+`"}`)
			method, path, body := http.MethodGet, "/v1/export", ""
			wantStatus := http.StatusOK
			switch endpoint {
			case "create":
				method, path, body = http.MethodPost, "/v1/records", `{"kind":"note","title":"Slow response after create"}`
				wantStatus = http.StatusCreated
			case "update":
				method, path, body = http.MethodPatch, "/v1/records/"+task.ID, `{"version":1,"title":"Slow response after update"}`
			case "context":
				path = "/v1/context?project_id=" + project.ID
			}
			gate := newConcurrencyGate()
			w := &gatedResponseWriter{ResponseRecorder: httptest.NewRecorder(), gate: gate}
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+apiTestToken)
			finished := make(chan struct{})
			go func() {
				h.ServeHTTP(w, r)
				close(finished)
			}()
			t.Cleanup(func() {
				gate.release()
				awaitConcurrencySignal(t, finished, "slow response request to finish")
			})
			awaitConcurrencySignal(t, gate.started, "the first request to start writing its response")
			assertOtherRequestsProgress(t, h, gate, `{"kind":"note","title":"Written after snapshot","project_id":"`+project.ID+`"}`)
			gate.release()
			awaitConcurrencySignal(t, finished, "released response request to finish")
			if w.Code != wantStatus {
				t.Fatalf("released response: status = %d, want %d, body = %s", w.Code, wantStatus, w.Body.String())
			}
			switch endpoint {
			case "context":
				var snapshot projectContext
				if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.Project.ID != project.ID || len(snapshot.Tasks) != 1 || snapshot.Tasks[0].ID != task.ID || len(snapshot.Notes) != 0 {
					t.Errorf("context changed after snapshot construction: %+v", snapshot)
				}
			case "export":
				var snapshot struct {
					Records []store.Record              `json:"records"`
					History map[string][]store.Revision `json:"history"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
					t.Fatal(err)
				}
				if len(snapshot.Records) != 2 || len(snapshot.History) != 2 {
					t.Fatalf("export included a write after snapshot construction: %+v", snapshot)
				}
				for _, record := range snapshot.Records {
					revisions := snapshot.History[record.ID]
					if len(revisions) != 1 || revisions[0].Version != record.Version || revisions[0].Record.Title != record.Title {
						t.Errorf("export record and history snapshots disagree: record = %+v; history = %+v", record, revisions)
					}
				}
			}
		})
	}
}
