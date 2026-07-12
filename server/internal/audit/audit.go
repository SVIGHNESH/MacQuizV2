// Package audit appends rows to the append-only audit_log table
// (docs/08-security.md section 7). Every module writes its audit row inside
// the same transaction as the mutation it records, so the change and its
// trail commit or roll back together. The table itself rejects UPDATE and
// DELETE via triggers, so a row, once committed, is permanent.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Change is one field's before/after pair. Update-type mutations put a
// map[string]Change (changed fields only) under the detail key "changes", so
// docs/08 section 7's promised diff has one shape across every module:
//
//	{"changes": {"title": {"from": "Old", "to": "New"}}}
//
// Set-valued mutations (audience, group membership) record added_*/removed_*
// id arrays instead: for a set, added and removed IS the diff, and dumping
// the whole membership from/to would bury it. Rows written before this
// convention keep their flat shape - audit_log is append-only, so the reader
// tolerates both.
type Change struct {
	From any `json:"from"`
	To   any `json:"to"`
}

// Diff records field's before/after in changes when the two values differ,
// and does nothing when they are equal - so a writer can hand every candidate
// field to it and the map ends up holding exactly what moved.
func Diff[T comparable](changes map[string]Change, field string, from, to T) {
	if from != to {
		changes[field] = Change{From: from, To: to}
	}
}

// DiffPointer is Diff for a nullable column: nil is a value (the field was or
// became unset), and two non-nil pointers are compared by what they point at,
// never by address.
func DiffPointer[T comparable](changes map[string]Change, field string, from, to *T) {
	switch {
	case from == nil && to == nil:
	case from == nil || to == nil:
		changes[field] = Change{From: derefOrNil(from), To: derefOrNil(to)}
	case *from != *to:
		changes[field] = Change{From: *from, To: *to}
	}
}

// derefOrNil unwraps a pointer into the value it holds, or an untyped nil.
// Storing the pointer itself would put a typed nil in the any field: it
// marshals to null all the same, but a reader comparing against nil would be
// wrong about it, and this is evidence code.
func derefOrNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// DiffTime is Diff for a timestamp column. Instants are compared with
// time.Time.Equal and recorded in UTC, so the same moment carried in two
// locations - the zone the driver hands back versus the one the request
// parsed - is not a change.
func DiffTime(changes map[string]Change, field string, from, to *time.Time) {
	switch {
	case from == nil && to == nil:
	case from == nil || to == nil:
		changes[field] = Change{From: utcOrNil(from), To: utcOrNil(to)}
	case !from.Equal(*to):
		changes[field] = Change{From: from.UTC(), To: to.UTC()}
	}
}

func utcOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// DiffJSON is Diff for a jsonb column. It compares canonical encodings: the
// stored value comes back from Postgres already normalized, while an incoming
// patch is raw client JSON, so a byte-wise compare would report a change for
// nothing more than reordered keys or added whitespace.
func DiffJSON(changes map[string]Change, field string, from, to json.RawMessage) {
	cFrom, cTo := canonicalJSON(from), canonicalJSON(to)
	if string(cFrom) == string(cTo) {
		return
	}
	changes[field] = Change{From: cFrom, To: cTo}
}

// canonicalJSON re-encodes a JSON value with sorted object keys and no
// insignificant whitespace. Invalid or absent JSON is passed through as-is:
// this is an audit detail, so a value that cannot be parsed is still worth
// recording verbatim.
func canonicalJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// Write appends one audit_log row inside the caller's transaction.
func Write(ctx context.Context, tx *sql.Tx, actorID, action, resourceType, resourceID string, detail map[string]any) error {
	var detailJSON any
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("marshal audit detail: %w", err)
		}
		detailJSON = b
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (actor_id, action, resource_type, resource_id, detail)
		 VALUES ($1, $2, $3, $4, $5)`,
		actorID, action, resourceType, resourceID, detailJSON); err != nil {
		return fmt.Errorf("write audit row: %w", err)
	}
	return nil
}

// DefaultPageSize and MaxPageSize bound the admin audit read (docs/04-api.md:
// "GET /audit - Filterable audit log (admin)"). The log is append-only and
// unbounded, so the read is always paginated; no other list endpoint needs it
// because their result sets are bounded by class size.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Entry is one audit_log row as returned by the read side. actor_id and
// resource_id are nullable in the schema, so they surface as omitted when the
// column is NULL; detail is the raw jsonb (null when the writer passed none).
type Entry struct {
	ID           int64           `json:"id"`
	ActorID      string          `json:"actor_id,omitempty"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id,omitempty"`
	Detail       json.RawMessage `json:"detail"`
	At           time.Time       `json:"at"`
}

// Filter narrows an audit read. Every field is optional; an empty Filter reads
// the newest DefaultPageSize rows across all actors and resources. The
// available filters mirror the two table indexes (actor_id+at,
// resource_type+resource_id) plus an exact action match and an at range.
type Filter struct {
	ActorID      string
	Action       string
	ResourceType string
	ResourceID   string
	From         *time.Time // at >= From
	To           *time.Time // at <  To
	Before       int64      // keyset cursor: id < Before (0 = from the newest)
	Limit        int        // clamped to [1, MaxPageSize], defaulting to DefaultPageSize
}

// Page is a keyset-paginated slice of the audit log, newest first. NextCursor
// is the id to pass back as Filter.Before to fetch the following page; it is
// nil once a page returns fewer than the requested limit (the last page).
type Page struct {
	Entries    []Entry `json:"entries"`
	NextCursor *int64  `json:"next_cursor"`
}

// List reads a page of the audit log newest-first, keyset-paginated on the
// bigserial primary key (stable under concurrent appends, unlike OFFSET). It
// runs on the *sql.DB pool: the read is outside any mutation transaction.
func List(ctx context.Context, db *sql.DB, f Filter) (Page, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	var conds []string
	var args []any
	add := func(clause string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(clause, len(args)))
	}
	if f.ActorID != "" {
		add("actor_id = $%d", f.ActorID)
	}
	if f.Action != "" {
		add("action = $%d", f.Action)
	}
	if f.ResourceType != "" {
		add("resource_type = $%d", f.ResourceType)
	}
	if f.ResourceID != "" {
		add("resource_id = $%d", f.ResourceID)
	}
	if f.From != nil {
		add("at >= $%d", *f.From)
	}
	if f.To != nil {
		add("at < $%d", *f.To)
	}
	if f.Before > 0 {
		add("id < $%d", f.Before)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit)
	query := fmt.Sprintf(
		`SELECT id, actor_id, action, resource_type, resource_id, detail, at
		 FROM audit_log %s
		 ORDER BY id DESC
		 LIMIT $%d`, where, len(args))

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()

	entries := make([]Entry, 0, limit)
	for rows.Next() {
		var (
			e          Entry
			actorID    sql.NullString
			resourceID sql.NullString
			detail     []byte // scan jsonb (incl. NULL) as bytes; direct *json.RawMessage rejects NULL
		)
		if err := rows.Scan(&e.ID, &actorID, &e.Action, &e.ResourceType, &resourceID, &detail, &e.At); err != nil {
			return Page{}, fmt.Errorf("scan audit row: %w", err)
		}
		e.ActorID = actorID.String
		e.ResourceID = resourceID.String
		e.Detail = json.RawMessage(detail) // nil bytes marshal to JSON null
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("iterate audit rows: %w", err)
	}

	page := Page{Entries: entries}
	if len(entries) == limit {
		next := entries[len(entries)-1].ID
		page.NextCursor = &next
	}
	return page, nil
}
