package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Snippet kinds.
const (
	KindText    = "text"    // plain text, e.g. an email address
	KindCommand = "command" // a shell command that can be run from the TUI
)

// Snippet is a named, reusable piece of text or shell command. Names are not
// unique: several snippets may share a name.
type Snippet struct {
	ID        int64
	Name      string
	Body      string
	Kind      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (sn *Snippet) normalise() error {
	sn.Name = strings.TrimSpace(sn.Name)
	sn.Body = strings.ToValidUTF8(sn.Body, "\uFFFD")
	if sn.Kind == "" {
		sn.Kind = KindText
	}
	switch {
	case sn.Name == "":
		return fmt.Errorf("%w: a name is required", ErrInvalid)
	case strings.TrimSpace(sn.Body) == "":
		return fmt.Errorf("%w: text is required", ErrInvalid)
	case sn.Kind != KindText && sn.Kind != KindCommand:
		return fmt.Errorf("%w: kind must be %q or %q", ErrInvalid, KindText, KindCommand)
	}
	return nil
}

// ListSnippets returns all snippets ordered by name (case-insensitive).
func (s *Store) ListSnippets(ctx context.Context) ([]Snippet, error) {
	return s.querySnippets(ctx, `ORDER BY name COLLATE NOCASE, id`)
}

// FindSnippets returns the snippets whose name equals name (case-insensitive).
func (s *Store) FindSnippets(ctx context.Context, name string) ([]Snippet, error) {
	return s.querySnippets(ctx, `WHERE name = ? COLLATE NOCASE ORDER BY id`, strings.TrimSpace(name))
}

func (s *Store) querySnippets(ctx context.Context, tail string, args ...any) ([]Snippet, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, body, kind, created_at, updated_at FROM snippets `+tail, args...)
	if err != nil {
		return nil, fmt.Errorf("list snippets: %w", err)
	}
	defer rows.Close()
	var out []Snippet
	for rows.Next() {
		var sn Snippet
		var created, updated int64
		if err := rows.Scan(&sn.ID, &sn.Name, &sn.Body, &sn.Kind, &created, &updated); err != nil {
			return nil, err
		}
		sn.CreatedAt, sn.UpdatedAt = fromMS(created), fromMS(updated)
		out = append(out, sn)
	}
	return out, rows.Err()
}

// AddSnippet validates and stores a new snippet, returning its id.
func (s *Store) AddSnippet(ctx context.Context, sn Snippet) (int64, error) {
	if err := sn.normalise(); err != nil {
		return 0, err
	}
	now := nowMS()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO snippets (name, body, kind, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		sn.Name, sn.Body, sn.Kind, now, now)
	if err != nil {
		return 0, fmt.Errorf("add snippet: %w", err)
	}
	return res.LastInsertId()
}

// UpdateSnippet validates and rewrites an existing snippet.
func (s *Store) UpdateSnippet(ctx context.Context, sn Snippet) error {
	if err := sn.normalise(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE snippets SET name = ?, body = ?, kind = ?, updated_at = ? WHERE id = ?`,
		sn.Name, sn.Body, sn.Kind, nowMS(), sn.ID)
	if err != nil {
		return fmt.Errorf("update snippet: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("snippet %d no longer exists", sn.ID)
	}
	return nil
}

// DeleteSnippet removes one snippet.
func (s *Store) DeleteSnippet(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM snippets WHERE id = ?`, id)
	return err
}

// GetSnippet reads one snippet.
func (s *Store) GetSnippet(ctx context.Context, id int64) (Snippet, error) {
	got, err := s.querySnippets(ctx, `WHERE id = ?`, id)
	if err != nil {
		return Snippet{}, err
	}
	if len(got) == 0 {
		return Snippet{}, fmt.Errorf("snippet %d no longer exists", id)
	}
	return got[0], nil
}

// RestoreSnippet puts a deleted snippet back with its original id (when
// still free) and timestamps, and returns the id it now has.
func (s *Store) RestoreSnippet(ctx context.Context, sn Snippet) (int64, error) {
	if err := sn.normalise(); err != nil {
		return 0, err
	}
	var id any = sn.ID
	var taken int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snippets WHERE id = ?`, sn.ID).Scan(&taken)
	if taken > 0 {
		id = nil
	}
	created, updated := sn.CreatedAt.UnixMilli(), sn.UpdatedAt.UnixMilli()
	if sn.CreatedAt.IsZero() {
		created = nowMS()
	}
	if sn.UpdatedAt.IsZero() {
		updated = created
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO snippets (id, name, body, kind, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, sn.Name, sn.Body, sn.Kind, created, updated)
	if err != nil {
		return 0, fmt.Errorf("restore snippet: %w", err)
	}
	return res.LastInsertId()
}
