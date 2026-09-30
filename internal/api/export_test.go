package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moncho/herma/internal/store"
)

func TestExportDeclaresCompleteResponseLength(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	r, _, err := s.Create(context.Background(), "writer", "", store.CreateInput{Kind: "knowledge", Title: "A decision", Body: "Keep the source."})
	if err != nil {
		t.Fatal(err)
	}
	w := apiTestRequest(h, http.MethodGet, "/v1/export", "", "", "Bearer "+apiTestToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
		t.Fatalf("Content-Length=%q, actual bytes=%d", w.Header().Get("Content-Length"), w.Body.Len())
	}
	var snapshot exportSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 1 || snapshot.Records[0].ID != r.ID || len(snapshot.History[r.ID]) != 1 {
		t.Fatalf("incomplete export: %+v", snapshot)
	}
}

func TestOversizedExportReturnsErrorBeforeAnySuccessBody(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	body := strings.Repeat("x", 64<<10)
	// Current records plus their first revisions exceed 16 MiB. Exercise the
	// actual public endpoint, rather than weakening its limit for the test.
	for i := 0; i < 129; i++ {
		if _, _, err := s.Create(context.Background(), "writer", "", store.CreateInput{Kind: "knowledge", Title: fmt.Sprintf("Decision %d", i), Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	w := apiTestRequest(h, http.MethodGet, "/v1/export", "", "", "Bearer "+apiTestToken, "")
	assertAPIError(t, w, http.StatusRequestEntityTooLarge)
	if !strings.Contains(w.Body.String(), `"code":"export_too_large"`) || !strings.Contains(w.Body.String(), "backup") {
		t.Fatalf("missing recovery guidance: %s", w.Body.String())
	}
	if w.Body.Len() > 1024 || strings.Contains(w.Body.String(), `"records"`) {
		t.Fatalf("oversized export emitted snapshot data instead of a compact failure")
	}
	// A failed snapshot must release its read lock and leave the store usable.
	created := apiTestRequest(h, http.MethodPost, "/v1/records", `{"kind":"task","title":"After rejected export"}`, "application/json", "Bearer "+apiTestToken, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("write after export failure: %d %s", created.Code, created.Body.String())
	}
}

func TestExpiredExportReturnsStructuredTimeout(t *testing.T) {
	h, _ := apiTestHandler(t, nil)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/v1/export", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+apiTestToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertAPIError(t, w, http.StatusServiceUnavailable)
	if !strings.Contains(w.Body.String(), `"code":"export_timeout"`) || !strings.Contains(w.Body.String(), "backup") {
		t.Fatalf("missing timeout guidance: %s", w.Body.String())
	}
}

func TestExportIncludesEveryPageOfRecords(t *testing.T) {
	h, s := apiTestHandler(t, nil)
	ctx := context.Background()
	const count = 401 // two full export pages and a partial one
	for i := 0; i < count; i++ {
		r, _, err := s.Create(ctx, "writer", "", store.CreateInput{Kind: "task", Title: fmt.Sprintf("Task %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, _, err := s.Update(ctx, r.ID, "writer", "", store.UpdateInput{Version: 1, Archived: pointerTo(true)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	w := apiTestRequest(h, http.MethodGet, "/v1/export", "", "", "Bearer "+apiTestToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var snapshot exportSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, record := range snapshot.Records {
		seen[record.ID] = true
	}
	if len(snapshot.Records) != count || len(seen) != count || len(snapshot.History) != count {
		t.Fatalf("export has %d records (%d unique) and %d histories, want %d", len(snapshot.Records), len(seen), len(snapshot.History), count)
	}
}

func pointerTo[T any](value T) *T { return &value }
