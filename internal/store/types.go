package store

import (
	"errors"
	"time"
)

var (
	ErrNotFound    = errors.New("record not found")
	ErrConflict    = errors.New("record version changed")
	ErrIdempotency = errors.New("idempotency key was already used for a different request")
)

type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// Record is a versioned piece of knowledge. Authenticated identities, rather
// than client-provided attribution, populate CreatedBy and UpdatedBy.
type Record struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	ProjectID string    `json:"project_id,omitempty"`
	Status    string    `json:"status"`
	Priority  int       `json:"priority"`
	Owner     string    `json:"owner,omitempty"`
	Tags      []string  `json:"tags"`
	Links     []string  `json:"links"`
	Sources   []string  `json:"sources"`
	Archived  bool      `json:"archived"`
	CreatedBy string    `json:"created_by"`
	UpdatedBy string    `json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int64     `json:"version"`
}

type CreateInput struct {
	Kind      string   `json:"kind"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	ProjectID string   `json:"project_id,omitempty"`
	Status    string   `json:"status,omitempty"`
	Priority  int      `json:"priority"`
	Owner     string   `json:"owner,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Links     []string `json:"links,omitempty"`
	Sources   []string `json:"sources,omitempty"`
}

// An explicit version prevents a stale session from overwriting newer work.
type UpdateInput struct {
	Version   int64     `json:"version"`
	Title     *string   `json:"title,omitempty"`
	Body      *string   `json:"body,omitempty"`
	ProjectID *string   `json:"project_id,omitempty"`
	Status    *string   `json:"status,omitempty"`
	Priority  *int      `json:"priority,omitempty"`
	Owner     *string   `json:"owner,omitempty"`
	Tags      *[]string `json:"tags,omitempty"`
	Links     *[]string `json:"links,omitempty"`
	Sources   *[]string `json:"sources,omitempty"`
	Archived  *bool     `json:"archived,omitempty"`
}

type ListOptions struct {
	Kind      string
	ProjectID string
	Global    bool // Only records with no project.
	Status    string
	Owner     string
	Tag       string
	Query     string
	Archived  bool // Include archived records when true.
	Limit     int
	Offset    int
}

type ListResult struct {
	Items  []Record `json:"items"`
	Total  int      `json:"total"`
	Limit  int      `json:"limit"`
	Offset int      `json:"offset"`
}

type Revision struct {
	Version int64     `json:"version"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	At      time.Time `json:"at"`
	Record  Record    `json:"record"`
}
