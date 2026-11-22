package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moncho/herma/internal/store"
)

func changesRequest(t *testing.T, h http.Handler, token, query string) store.ChangesResult {
	t.Helper()
	response := apiTestRequest(h, http.MethodGet, "/v1/changes"+query, "", "", "Bearer "+token, "")
	return decodeChanges(t, response)
}

func decodeChanges(t *testing.T, response *httptest.ResponseRecorder) store.ChangesResult {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("changes status %d: %s", response.Code, response.Body.String())
	}
	var result store.ChangesResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// waitingChanges starts a request that waits for changes and returns the
// channel its response arrives on.
func waitingChanges(ctx context.Context, h http.Handler, query string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodGet, "/v1/changes"+query, nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+apiTestToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		done <- w
	}()
	return done
}

func within(t *testing.T, done <-chan *httptest.ResponseRecorder, limit time.Duration, what string) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-done:
		return response
	case <-time.After(limit):
		t.Fatalf("%s: no response within %v", what, limit)
		return nil
	}
}

func stillWaiting(t *testing.T, done <-chan *httptest.ResponseRecorder, what string) {
	t.Helper()
	select {
	case response := <-done:
		t.Fatalf("%s: answered early with %s", what, response.Body.String())
	case <-time.After(150 * time.Millisecond):
	}
}

func TestChangesListsWritesAfterACursor(t *testing.T) {
	h, s := roleTestHandler(t)
	project := createContextRecord(t, s, store.CreateInput{Kind: "project", Title: "Mine"})
	task := createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Ship it", Body: "secret body text", ProjectID: project.ID})
	createContextRecord(t, s, store.CreateInput{Kind: "note", Title: "A note", Body: "b"})

	// Without a cursor the answer is where the feed is now, for a reader
	// that wants only what happens next.
	if now := changesRequest(t, h, apiTestToken, ""); len(now.Items) != 0 || now.Next != 3 {
		t.Errorf("no cursor: %+v, want nothing and next 3", now)
	}
	all := apiTestRequest(h, http.MethodGet, "/v1/changes?after=0", "", "", "Bearer "+readerToken, "")
	if result := decodeChanges(t, all); len(result.Items) != 3 || result.Next != 3 {
		t.Errorf("after=0 as a read-only identity: %+v", result)
	}
	if strings.Contains(all.Body.String(), "secret body text") {
		t.Errorf("the feed carries record bodies: %s", all.Body.String())
	}
	scoped := changesRequest(t, h, apiTestToken, "?after=0&kind=task&project_id="+project.ID)
	if len(scoped.Items) != 1 || scoped.Next != 3 {
		t.Fatalf("kind and project filter: %+v", scoped)
	}
	got := scoped.Items[0]
	if got.Seq != 2 || got.Action != "created" || got.Actor != "context-test-session" || got.Record.ID != task.ID || got.Record.Title != "Ship it" || got.Record.Version != 1 {
		t.Errorf("change: %+v", got)
	}
	if page := changesRequest(t, h, apiTestToken, "?after=0&limit=2"); len(page.Items) != 2 || page.Next != 2 {
		t.Errorf("limit=2: %+v", page)
	}
}

func TestChangesWaitReturnsAsSoonAsAMatchingWriteCommits(t *testing.T) {
	h, _ := roleTestHandler(t)
	done := waitingChanges(context.Background(), h, "?kind=task&wait=25")
	stillWaiting(t, done, "nothing written yet")
	post := func(body string) {
		t.Helper()
		if response := apiTestRequest(h, http.MethodPost, "/v1/records", body, "application/json", "Bearer "+apiTestToken, ""); response.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", response.Code, response.Body.String())
		}
	}
	// A write the filter excludes does not end the wait.
	post(`{"kind":"note","title":"A note","body":"b"}`)
	stillWaiting(t, done, "only a note written")
	post(`{"kind":"task","title":"Ship it"}`)
	result := decodeChanges(t, within(t, done, 5*time.Second, "after a task was written"))
	if len(result.Items) != 1 || result.Items[0].Record.Title != "Ship it" || result.Items[0].Seq != 2 || result.Next != 2 {
		t.Errorf("woken with %+v, want the task as change 2", result)
	}
}

func TestChangesWaitEndsEmptyAtTheDeadlineOrWhenToldToStop(t *testing.T) {
	h, s := roleTestHandler(t)
	createContextRecord(t, s, store.CreateInput{Kind: "task", Title: "Before"})

	started := time.Now()
	result := decodeChanges(t, within(t, waitingChanges(context.Background(), h, "?after=1&wait=1"), 5*time.Second, "one-second wait"))
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || len(result.Items) != 0 || result.Next != 1 {
		t.Errorf("wait=1 answered after %v with %+v, want an empty answer after about a second", elapsed, result)
	}

	// The reader going away ends the wait.
	ctx, cancel := context.WithCancel(context.Background())
	gone := waitingChanges(ctx, h, "?after=1&wait=25")
	stillWaiting(t, gone, "reader still connected")
	cancel()
	within(t, gone, 5*time.Second, "after the reader went away")

	// So does the server shutting down, and later waits end at once.
	stopping := waitingChanges(context.Background(), h, "?after=1&wait=25")
	stillWaiting(t, stopping, "server still running")
	h.StopWaiting()
	if result := decodeChanges(t, within(t, stopping, 5*time.Second, "after StopWaiting")); len(result.Items) != 0 || result.Next != 1 {
		t.Errorf("stopped wait: %+v", result)
	}
	decodeChanges(t, within(t, waitingChanges(context.Background(), h, "?after=1&wait=25"), 5*time.Second, "a wait begun after StopWaiting"))
}

func TestChangesRequestValidation(t *testing.T) {
	h, _ := roleTestHandler(t)
	for _, query := range []string{"?after=-1", "?after=x", "?limit=0", "?limit=201", "?wait=-1", "?wait=26", "?wait=soon", "?kind=Not-A-Kind", "?unknown=1", "?project_id=" + strings.Repeat("x", 101)} {
		assertAPIError(t, apiTestRequest(h, http.MethodGet, "/v1/changes"+query, "", "", "Bearer "+apiTestToken, ""), http.StatusBadRequest)
	}
	assertAPIError(t, apiTestRequest(h, http.MethodPost, "/v1/changes", "{}", "application/json", "Bearer "+apiTestToken, ""), http.StatusMethodNotAllowed)
	assertAPIError(t, apiTestRequest(h, http.MethodGet, "/v1/changes", "", "", "", ""), http.StatusUnauthorized)
	if result := changesRequest(t, h, apiTestToken, fmt.Sprintf("?after=%d&wait=0", 0)); len(result.Items) != 0 || result.Next != 0 {
		t.Errorf("empty feed: %+v", result)
	}
}
