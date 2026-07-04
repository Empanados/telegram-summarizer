package storage

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	db *sql.DB
}

type Channel struct {
	Username  string
	ChannelID int64
	Title     string
	AddedAt   time.Time
}

type Message struct {
	ID        int
	Username  string
	MessageID int
	Date      time.Time
	Text      string
}

func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	d := &DB{db: db}
	return d, d.migrate()
}

func (d *DB) Close() error { return d.db.Close() }

func (d *DB) migrate() error {
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS user_channels (
			user_id    INTEGER NOT NULL,
			username   TEXT    NOT NULL,
			channel_id INTEGER,
			title      TEXT,
			added_at   TEXT    NOT NULL,
			PRIMARY KEY (user_id, username)
		);
		CREATE TABLE IF NOT EXISTS messages (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			username   TEXT    NOT NULL,
			message_id INTEGER NOT NULL,
			date       TEXT    NOT NULL,
			text       TEXT    NOT NULL,
			UNIQUE (username, message_id)
		);
		CREATE TABLE IF NOT EXISTS active_channel (
			user_id    INTEGER PRIMARY KEY,
			username   TEXT    NOT NULL
		);
		CREATE TABLE IF NOT EXISTS message_embeddings (
			username   TEXT    NOT NULL,
			message_id INTEGER NOT NULL,
			embedding  BLOB    NOT NULL,
			PRIMARY KEY (username, message_id)
		);
		CREATE INDEX IF NOT EXISTS idx_msg_username ON messages(username);
		CREATE INDEX IF NOT EXISTS idx_msg_date     ON messages(date DESC);
	`)
	return err
}

// ── Channels ──────────────────────────────────────────────────────────────────

func (d *DB) AddChannel(userID int64, username string) (bool, error) {
	username = norm(username)
	res, err := d.db.Exec(
		`INSERT OR IGNORE INTO user_channels (user_id, username, added_at) VALUES (?, ?, ?)`,
		userID, username, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (d *DB) UpdateChannelInfo(userID int64, username string, channelID int64, title string) error {
	_, err := d.db.Exec(
		`UPDATE user_channels SET channel_id=?, title=? WHERE user_id=? AND username=?`,
		channelID, title, userID, norm(username),
	)
	return err
}

func (d *DB) RemoveChannel(userID int64, username string) (bool, error) {
	res, err := d.db.Exec(
		`DELETE FROM user_channels WHERE user_id=? AND username=?`,
		userID, norm(username),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (d *DB) GetUserChannels(userID int64) ([]Channel, error) {
	rows, err := d.db.Query(
		`SELECT username, COALESCE(channel_id,0), COALESCE(title,''), added_at
		 FROM user_channels WHERE user_id=? ORDER BY added_at`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []Channel
	for rows.Next() {
		var c Channel
		var addedAt string
		if err := rows.Scan(&c.Username, &c.ChannelID, &c.Title, &addedAt); err != nil {
			return nil, err
		}
		c.AddedAt, _ = time.Parse(time.RFC3339, addedAt)
		channels = append(channels, c)
	}
	return channels, rows.Err()
}

func (d *DB) ChannelBelongsToUser(userID int64, username string) (bool, error) {
	var n int
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM user_channels WHERE user_id=? AND username=?`,
		userID, norm(username),
	).Scan(&n)
	return n > 0, err
}

// ── Messages ──────────────────────────────────────────────────────────────────

func (d *DB) SaveMessages(username string, msgs []Message) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	username = norm(username)
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO messages (username, message_id, date, text) VALUES (?, ?, ?, ?)`,
	)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	saved := 0
	for _, m := range msgs {
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		res, err := stmt.Exec(username, m.MessageID, m.Date.UTC().Format(time.RFC3339), m.Text)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			saved++
		}
	}
	return saved, tx.Commit()
}

func (d *DB) GetLastMessageID(username string) (int, error) {
	var id sql.NullInt64
	err := d.db.QueryRow(
		`SELECT MAX(message_id) FROM messages WHERE username=?`, norm(username),
	).Scan(&id)
	return int(id.Int64), err
}

func (d *DB) MessageCount(username string) (int, error) {
	var n int
	err := d.db.QueryRow(
		`SELECT COUNT(*) FROM messages WHERE username=?`, norm(username),
	).Scan(&n)
	return n, err
}

// SearchMessages returns messages relevant to query from the given channels.
// Keyword-matched messages come first; padded with recent messages if fewer than limit.
func (d *DB) SearchMessages(usernames []string, query string, limit int) ([]Message, error) {
	if len(usernames) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(usernames))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, len(usernames))
	for i, u := range usernames {
		args[i] = norm(u)
	}

	rows, err := d.db.Query(
		fmt.Sprintf(`SELECT username, message_id, date, text
		             FROM messages
		             WHERE username IN (%s) AND text != ''
		             ORDER BY date DESC LIMIT 300`, placeholders),
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var all []Message
	for rows.Next() {
		var m Message
		var dateStr string
		if err := rows.Scan(&m.Username, &m.MessageID, &dateStr, &m.Text); err != nil {
			return nil, err
		}
		m.Date, _ = time.Parse(time.RFC3339, dateStr)
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	words := relevantWords(query)
	var matched, rest []Message
	for _, m := range all {
		lower := strings.ToLower(m.Text)
		if hasAny(lower, words) {
			matched = append(matched, m)
		} else {
			rest = append(rest, m)
		}
	}

	result := matched
	if len(result) < limit {
		need := limit - len(result)
		if need > len(rest) {
			need = len(rest)
		}
		result = append(result, rest[:need]...)
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// ── Active Channel ────────────────────────────────────────────────────────────

func (d *DB) SetActiveChannel(userID int64, username string) error {
	_, err := d.db.Exec(
		`INSERT OR REPLACE INTO active_channel (user_id, username) VALUES (?, ?)`,
		userID, norm(username),
	)
	return err
}

func (d *DB) GetActiveChannel(userID int64) (string, error) {
	var username string
	err := d.db.QueryRow(
		`SELECT username FROM active_channel WHERE user_id = ?`,
		userID,
	).Scan(&username)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return username, err
}

func (d *DB) ClearActiveChannel(userID int64) error {
	_, err := d.db.Exec(
		`DELETE FROM active_channel WHERE user_id = ?`,
		userID,
	)
	return err
}

func (d *DB) GetRecentMessages(username string, limit int) ([]Message, error) {
	rows, err := d.db.Query(
		`SELECT username, message_id, date, text
		 FROM messages
		 WHERE username = ? AND text != ''
		 ORDER BY date DESC LIMIT ?`,
		norm(username), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var dateStr string
		if err := rows.Scan(&m.Username, &m.MessageID, &dateStr, &m.Text); err != nil {
			return nil, err
		}
		m.Date, _ = time.Parse(time.RFC3339, dateStr)
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Reverse to chronological order (oldest first)
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}

	return msgs, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func norm(s string) string {
	return strings.ToLower(strings.TrimPrefix(s, "@"))
}

func relevantWords(q string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if len(w) > 2 {
			out = append(out, w)
		}
	}
	return out
}

func hasAny(text string, words []string) bool {
	for _, w := range words {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

func (d *DB) SaveEmbeddings(username string, messageIDs []int, embeddings [][]float32) error {
	if len(messageIDs) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO message_embeddings (username, message_id, embedding) VALUES (?, ?, ?)`,
	)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for i, id := range messageIDs {
		embBytes := float32ToBytes(embeddings[i])
		_, err = stmt.Exec(norm(username), id, embBytes)
		if err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) GetMessagesWithoutEmbeddings(username string, limit int) ([]Message, error) {
	rows, err := d.db.Query(
		`SELECT m.username, m.message_id, m.date, m.text
		 FROM messages m
		 LEFT JOIN message_embeddings e ON m.username = e.username AND m.message_id = e.message_id
		 WHERE m.username = ? AND m.text != '' AND e.message_id IS NULL
		 ORDER BY m.date DESC LIMIT ?`,
		norm(username), limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var dateStr string
		if err := rows.Scan(&m.Username, &m.MessageID, &dateStr, &m.Text); err != nil {
			return nil, err
		}
		m.Date, _ = time.Parse(time.RFC3339, dateStr)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func (d *DB) GetMessageEmbeddings(username string) (map[int][]float32, error) {
	rows, err := d.db.Query(
		`SELECT message_id, embedding FROM message_embeddings WHERE username = ?`,
		norm(username),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	embs := make(map[int][]float32)
	for rows.Next() {
		var id int
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		embs[id] = bytesToFloat32(b)
	}
	return embs, rows.Err()
}

func (d *DB) GetMessagesByIDs(username string, ids []int) ([]Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, len(ids)+1)
	args[0] = norm(username)
	for i, id := range ids {
		args[i+1] = id
	}

	rows, err := d.db.Query(
		fmt.Sprintf(`SELECT username, message_id, date, text
		             FROM messages
		             WHERE username = ? AND message_id IN (%s)
		             ORDER BY date DESC`, placeholders),
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		var dateStr string
		if err := rows.Scan(&m.Username, &m.MessageID, &dateStr, &m.Text); err != nil {
			return nil, err
		}
		m.Date, _ = time.Parse(time.RFC3339, dateStr)
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func float32ToBytes(vec []float32) []byte {
	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.LittleEndian, vec)
	return buf.Bytes()
}

func bytesToFloat32(b []byte) []float32 {
	buf := bytes.NewReader(b)
	vec := make([]float32, len(b)/4)
	_ = binary.Read(buf, binary.LittleEndian, &vec)
	return vec
}
