package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// PreviewChars is how much of each entry ListClips returns; the full text is
// fetched on demand with ClipText so a few huge entries never slow the UI.
const PreviewChars = 8192

// Clip kinds.
const (
	ClipText  = "text"  // plain text
	ClipImage = "image" // image bytes; Text is a generated file name
	ClipFile  = "file"  // copied files; Text holds the absolute paths, one per line
)

// ignoreWindow is how long a text that suchi itself put on the clipboard is
// kept out of the history.
const ignoreWindow = 15 * time.Second

// Clip is one clipboard history entry (text truncated to PreviewChars; image
// bytes are loaded separately with ClipData).
type Clip struct {
	ID        int64
	Kind      string
	Preview   string
	Mime      string
	Size      int64 // bytes of the text or of the image data
	Length    int   // length of the full text in characters
	Multiline bool
	Pinned    bool
	CopiedAt  time.Time
	UsedAt    time.Time
}

// LastActive is when the entry was last copied by an application or chosen in suchi.
func (c Clip) LastActive() time.Time {
	if c.UsedAt.After(c.CopiedAt) {
		return c.UsedAt
	}
	return c.CopiedAt
}

// NewClip is an item to record.
type NewClip struct {
	Kind string // ClipText (default), ClipImage or ClipFile
	Text string // the text, the image's file name, or the file paths
	Mime string
	Data []byte // image bytes
}

// Limits bound what is stored.
type Limits struct {
	Count       int   // max entries kept; pinned entries are exempt
	MaxText     int   // skip text / file lists larger than this many bytes
	MaxMedia    int   // skip images larger than this many bytes
	MediaBudget int64 // total image bytes kept; the oldest unpinned images go first
}

// AddClip records text (see AddItem). Kept for callers that only have text.
func (s *Store) AddClip(ctx context.Context, text string, limit, maxBytes int) (bool, error) {
	return s.AddItem(ctx, NewClip{Text: text}, Limits{Count: limit, MaxText: maxBytes})
}

// AddItem records an item, bumping it to the top instead of duplicating it.
// Text is stored exactly as copied. Blank text, oversized items and texts
// registered with IgnoreNext are not recorded (added == false). Unpinned
// entries beyond Limits.Count are pruned, oldest first.
func (s *Store) AddItem(ctx context.Context, it NewClip, lim Limits) (added bool, err error) {
	if it.Kind == "" {
		it.Kind = ClipText
	}
	it.Text = strings.ToValidUTF8(it.Text, "\uFFFD")
	hash := ""
	switch it.Kind {
	case ClipText, ClipFile:
		if strings.TrimSpace(it.Text) == "" || (lim.MaxText > 0 && len(it.Text) > lim.MaxText) {
			return false, nil
		}
	case ClipImage:
		if len(it.Data) == 0 || (lim.MaxMedia > 0 && len(it.Data) > lim.MaxMedia) {
			return false, nil
		}
		hash = sum(it.Data)
		if it.Text == "" {
			it.Text = "image-" + time.Now().Format("20060102-150405") + "." + ExtForMime(it.Mime)
		}
	default:
		return false, fmt.Errorf("%w: unknown clip kind %q", ErrInvalid, it.Kind)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	now := nowMS()
	if it.Kind == ClipText {
		skip, err := consumeIgnore(ctx, tx, sum([]byte(it.Text)), now)
		if err != nil {
			return false, err
		}
		if skip { // commit so the single-use marker is really consumed
			return false, tx.Commit()
		}
	}

	var res sql.Result
	if it.Kind == ClipImage {
		res, err = tx.ExecContext(ctx, `UPDATE clips SET copied_at = ? WHERE kind = 'image' AND hash = ?`, now, hash)
	} else {
		res, err = tx.ExecContext(ctx, `UPDATE clips SET copied_at = ? WHERE kind = ? AND text = ?`, now, it.Kind, it.Text)
	}
	if err != nil {
		return false, fmt.Errorf("bump clip: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		size := int64(len(it.Text))
		var data any
		if it.Kind == ClipImage {
			size, data = int64(len(it.Data)), it.Data
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO clips (kind, text, mime, data, size, hash, pinned, copied_at, used_at)
			 VALUES (?, ?, ?, ?, ?, ?, 0, ?, 0)`,
			it.Kind, it.Text, it.Mime, data, size, hash, now); err != nil {
			return false, fmt.Errorf("insert clip: %w", err)
		}
	}
	if lim.MediaBudget > 0 {
		if err := pruneMedia(ctx, tx, lim.MediaBudget); err != nil {
			return false, err
		}
	}
	if err := prune(ctx, tx, lim.Count); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// IgnoreNext tells the daemon not to record text the next time it appears on
// the clipboard (used when suchi copies a snippet, which is not "history").
func (s *Store) IgnoreNext(ctx context.Context, text string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO ignored (hash, expires_at) VALUES (?, ?)`,
		sum([]byte(strings.ToValidUTF8(text, "\uFFFD"))), nowMS()+ignoreWindow.Milliseconds())
	return err
}

func consumeIgnore(ctx context.Context, tx *sql.Tx, hash string, now int64) (bool, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM ignored WHERE expires_at < ?`, now); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM ignored WHERE hash = ?`, hash)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ListClips returns all entries: pinned first, then most recently active.
func (s *Store) ListClips(ctx context.Context) ([]Clip, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, substr(text, 1, ?), mime, size, length(text), instr(text, char(10)) > 0,
		       pinned, copied_at, used_at
		FROM clips ORDER BY pinned DESC, max(copied_at, used_at) DESC, id DESC`, PreviewChars)
	if err != nil {
		return nil, fmt.Errorf("list clips: %w", err)
	}
	defer rows.Close()
	var out []Clip
	for rows.Next() {
		var c Clip
		var ml, pin int
		var copied, used int64
		if err := rows.Scan(&c.ID, &c.Kind, &c.Preview, &c.Mime, &c.Size, &c.Length, &ml, &pin, &copied, &used); err != nil {
			return nil, err
		}
		c.Multiline, c.Pinned = ml != 0, pin != 0
		c.CopiedAt, c.UsedAt = fromMS(copied), fromMS(used)
		if !utf8.ValidString(c.Preview) {
			c.Preview = strings.ToValidUTF8(c.Preview, "\uFFFD")
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClipText returns the full text of an entry (for images: its file name).
func (s *Store) ClipText(ctx context.Context, id int64) (string, error) {
	var t string
	if err := s.db.QueryRowContext(ctx, `SELECT text FROM clips WHERE id = ?`, id).Scan(&t); err != nil {
		return "", fmt.Errorf("read clip %d: %w", id, err)
	}
	return t, nil
}

// ClipData returns the bytes of an image entry.
func (s *Store) ClipData(ctx context.Context, id int64) (data []byte, mime string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT data, mime FROM clips WHERE id = ? AND kind = 'image'`, id).Scan(&data, &mime)
	if err != nil {
		return nil, "", fmt.Errorf("read image %d: %w", id, err)
	}
	return data, mime, nil
}

// TouchClip marks an entry as just used so it moves to the top.
func (s *Store) TouchClip(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clips SET used_at = ? WHERE id = ?`, nowMS(), id)
	return err
}

// TogglePin flips the pinned flag of an entry.
func (s *Store) TogglePin(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE clips SET pinned = 1 - pinned WHERE id = ?`, id)
	return err
}

// DeleteClip removes one entry.
func (s *Store) DeleteClip(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM clips WHERE id = ?`, id)
	return err
}

// ClearClips deletes history. Pinned entries survive unless includePinned is set.
func (s *Store) ClearClips(ctx context.Context, includePinned bool) (int64, error) {
	q := `DELETE FROM clips WHERE pinned = 0`
	if includePinned {
		q = `DELETE FROM clips`
	}
	res, err := s.db.ExecContext(ctx, q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// prune deletes the oldest unpinned entries beyond limit; pinned entries are
// exempt from (and shrink) the allowance.
func prune(ctx context.Context, q execer, limit int) error {
	if limit <= 0 {
		return nil
	}
	var pinned int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM clips WHERE pinned = 1`).Scan(&pinned); err != nil {
		return err
	}
	keep := limit - pinned
	if keep < 0 {
		keep = 0
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM clips WHERE pinned = 0 AND id NOT IN (
		SELECT id FROM clips WHERE pinned = 0 ORDER BY max(copied_at, used_at) DESC, id DESC LIMIT ?)`, keep); err != nil {
		return fmt.Errorf("prune clips: %w", err)
	}
	return nil
}

// pruneMedia deletes the oldest unpinned images until the stored image bytes
// fit the budget.
func pruneMedia(ctx context.Context, q execer, budget int64) error {
	for i := 0; i < 1000; i++ {
		var total int64
		if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM clips WHERE kind = 'image'`).Scan(&total); err != nil {
			return err
		}
		if total <= budget {
			return nil
		}
		res, err := q.ExecContext(ctx, `DELETE FROM clips WHERE id = (
			SELECT id FROM clips WHERE kind = 'image' AND pinned = 0
			ORDER BY max(copied_at, used_at) ASC, id ASC LIMIT 1)`)
		if err != nil {
			return fmt.Errorf("prune media: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // only pinned images remain
		}
	}
	return nil
}

// ExtForMime maps an image MIME type to a file extension.
func ExtForMime(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0])) {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "image/bmp", "image/x-ms-bmp":
		return "bmp"
	case "image/tiff":
		return "tiff"
	case "image/svg+xml":
		return "svg"
	case "image/avif":
		return "avif"
	}
	if i := strings.IndexByte(mime, '/'); i >= 0 && i+1 < len(mime) {
		return strings.TrimPrefix(mime[i+1:], "x-")
	}
	return "img"
}
