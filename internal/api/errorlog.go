package api

import (
	"context"
	"errors"
	"io"
	"log"

	"github.com/moncho/herma/internal/safetext"
)

// requestInfo names a request in the error log. It leaves out the query
// string, which can hold search words, and anything from the headers.
type requestInfo struct{ method, path, actor string }

type requestInfoKey struct{}

// SetErrorLog sends the causes of internal errors to w, one line each. Until
// it is called they are discarded. Call it before serving requests.
func (h *Handler) SetErrorLog(w io.Writer) { h.errors = log.New(w, "", log.LstdFlags|log.LUTC) }

// logFailure records why a request failed inside the server, or why something
// the server does alongside a request did. The caller is told only that the
// request could not be completed; this line is where the cause goes. A request
// its caller abandoned is not a failure of the server and is not logged.
func (h *Handler) logFailure(ctx context.Context, what string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	// Error text can include text a caller chose; keep it on this line.
	cause := safetext.Line(err.Error())
	if info, ok := ctx.Value(requestInfoKey{}).(requestInfo); ok {
		h.errors.Printf("%s: %s %s (%s): %s", what, info.method, info.path, info.actor, cause)
		return
	}
	h.errors.Printf("%s: %s", what, cause)
}
