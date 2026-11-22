package api

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/moncho/herma/internal/store"
)

// maxChangesWait keeps a waiting request inside the server's write timeout
// and the client's request timeout, both 30 seconds.
const maxChangesWait = 25

// StopWaiting ends every request that is waiting for changes, now and from
// here on, so shutting the server down does not have to wait them out.
func (h *Handler) StopWaiting() { h.stopOnce.Do(func() { close(h.stop) }) }

// changes lists record writes after a cursor. With wait it holds the request
// until a matching write commits, so a reader learns of it at once without
// the server having to call anyone.
func (h *Handler) changes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if err := validateQuery(q, "after", "kind", "project_id", "limit", "wait"); err != nil {
		badRequest(w, err)
		return
	}
	// Without a cursor the reader starts from now: a cursor past the newest
	// change is answered with where the feed is.
	options := store.ChangesOptions{After: math.MaxInt64, Kind: q.Get("kind"), ProjectID: q.Get("project_id")}
	if q.Has("after") {
		after, err := strconv.ParseInt(q.Get("after"), 10, 64)
		if err != nil {
			badRequest(w, errors.New("after must be an integer"))
			return
		}
		options.After = after
	}
	var err error
	if options.Limit, err = queryInt(q, "limit", 100); err != nil || options.Limit < 1 || options.Limit > 200 {
		badRequest(w, errors.New("limit must be an integer between 1 and 200"))
		return
	}
	wait, err := queryInt(q, "wait", 0)
	if err != nil || wait < 0 || wait > maxChangesWait {
		badRequest(w, errors.New("wait must be an integer between 0 and 25 seconds"))
		return
	}
	deadline := time.NewTimer(time.Duration(wait) * time.Second)
	defer deadline.Stop()
	for {
		// Taken before reading, so a write that commits between the read and
		// the wait still ends the wait.
		written := h.store.Changed()
		result, err := h.store.Changes(r.Context(), options)
		if err != nil {
			h.storeError(w, r, err)
			return
		}
		if len(result.Items) > 0 {
			writeJSON(w, http.StatusOK, result)
			return
		}
		options.After = result.Next
		// Without wait the deadline has already passed.
		select {
		case <-written:
		case <-deadline.C:
			writeJSON(w, http.StatusOK, result)
			return
		case <-h.stop:
			writeJSON(w, http.StatusOK, result)
			return
		case <-r.Context().Done():
			return
		}
	}
}
