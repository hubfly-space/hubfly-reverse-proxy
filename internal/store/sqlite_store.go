package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/hubfly/hubfly-reverse-proxy/internal/models"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	httpDB *sql.DB
	tcpDB  *sql.DB
	mu     sync.RWMutex
}

func NewSQLiteStore(baseDir string) (*SQLiteStore, error) {
	httpDir := filepath.Join(baseDir, "data", "http")
	tcpDir := filepath.Join(baseDir, "data", "tcp")
	if err := os.MkdirAll(httpDir, 0755); err != nil {
		return nil, fmt.Errorf("create http data dir: %w", err)
	}
	if err := os.MkdirAll(tcpDir, 0755); err != nil {
		return nil, fmt.Errorf("create tcp data dir: %w", err)
	}

	httpDBPath := filepath.Join(httpDir, "sites.db")
	tcpDBPath := filepath.Join(tcpDir, "streams.db")

	httpDB, err := sql.Open("sqlite", httpDBPath)
	if err != nil {
		return nil, fmt.Errorf("open sites db: %w", err)
	}
	if err := configureSQLiteDurability(httpDB); err != nil {
		_ = httpDB.Close()
		return nil, fmt.Errorf("configure sites db: %w", err)
	}
	if err := initSQLiteDB(httpDB, "sites"); err != nil {
		_ = httpDB.Close()
		return nil, err
	}
	if err := initSQLiteDB(httpDB, "redirects"); err != nil {
		_ = httpDB.Close()
		return nil, err
	}

	tcpDB, err := sql.Open("sqlite", tcpDBPath)
	if err != nil {
		_ = httpDB.Close()
		return nil, fmt.Errorf("open streams db: %w", err)
	}
	if err := configureSQLiteDurability(tcpDB); err != nil {
		_ = httpDB.Close()
		_ = tcpDB.Close()
		return nil, fmt.Errorf("configure streams db: %w", err)
	}
	if err := initSQLiteDB(tcpDB, "streams"); err != nil {
		_ = httpDB.Close()
		_ = tcpDB.Close()
		return nil, err
	}

	return &SQLiteStore{
		httpDB: httpDB,
		tcpDB:  tcpDB,
	}, nil
}

func initSQLiteDB(db *sql.DB, table string) error {
	schema := fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
	id TEXT PRIMARY KEY,
	payload TEXT NOT NULL,
	updated_at TEXT NOT NULL
);`, table)
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("init table %s: %w", table, err)
	}
	return nil
}

func configureSQLiteDurability(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=FULL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA wal_autocheckpoint=1000;",
	}
	for _, stmt := range pragmas {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) ListSites() ([]models.Site, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.httpDB.Query(`SELECT payload FROM sites`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]models.Site, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var site models.Site
		if err := json.Unmarshal([]byte(payload), &site); err != nil {
			return nil, fmt.Errorf("decode site payload: %w", err)
		}
		out = append(out, site)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetSite(id string) (*models.Site, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var payload string
	err := s.httpDB.QueryRow(`SELECT payload FROM sites WHERE id = ?`, id).Scan(&payload)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("site not found: %s", id)
		}
		return nil, err
	}
	var site models.Site
	if err := json.Unmarshal([]byte(payload), &site); err != nil {
		return nil, fmt.Errorf("decode site payload: %w", err)
	}
	return &site, nil
}

func (s *SQLiteStore) SaveSite(site *models.Site) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload, err := json.Marshal(site)
	if err != nil {
		return err
	}
	_, err = s.httpDB.Exec(
		`INSERT INTO sites (id, payload, updated_at) VALUES (?, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		site.ID, string(payload),
	)
	return err
}

func (s *SQLiteStore) DeleteSite(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.httpDB.Exec(`DELETE FROM sites WHERE id = ?`, id)
	return err
}

func (s *SQLiteStore) ListRedirects() ([]models.Redirect, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.httpDB.Query(`SELECT payload FROM redirects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]models.Redirect, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var redirect models.Redirect
		if err := json.Unmarshal([]byte(payload), &redirect); err != nil {
			return nil, fmt.Errorf("decode redirect payload: %w", err)
		}
		out = append(out, redirect)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetRedirect(id string) (*models.Redirect, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var payload string
	err := s.httpDB.QueryRow(`SELECT payload FROM redirects WHERE id = ?`, id).Scan(&payload)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("redirect not found: %s", id)
		}
		return nil, err
	}
	var redirect models.Redirect
	if err := json.Unmarshal([]byte(payload), &redirect); err != nil {
		return nil, fmt.Errorf("decode redirect payload: %w", err)
	}
	return &redirect, nil
}

func (s *SQLiteStore) SaveRedirect(redirect *models.Redirect) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload, err := json.Marshal(redirect)
	if err != nil {
		return err
	}
	_, err = s.httpDB.Exec(
		`INSERT INTO redirects (id, payload, updated_at) VALUES (?, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		redirect.ID, string(payload),
	)
	return err
}

func (s *SQLiteStore) DeleteRedirect(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.httpDB.Exec(`DELETE FROM redirects WHERE id = ?`, id)
	return err
}

func (s *SQLiteStore) ListStreams() ([]models.Stream, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.tcpDB.Query(`SELECT payload FROM streams`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]models.Stream, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var stream models.Stream
		if err := json.Unmarshal([]byte(payload), &stream); err != nil {
			return nil, fmt.Errorf("decode stream payload: %w", err)
		}
		out = append(out, stream)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetStream(id string) (*models.Stream, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var payload string
	err := s.tcpDB.QueryRow(`SELECT payload FROM streams WHERE id = ?`, id).Scan(&payload)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("stream not found: %s", id)
		}
		return nil, err
	}
	var stream models.Stream
	if err := json.Unmarshal([]byte(payload), &stream); err != nil {
		return nil, fmt.Errorf("decode stream payload: %w", err)
	}
	return &stream, nil
}

func (s *SQLiteStore) SaveStream(stream *models.Stream) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	payload, err := json.Marshal(stream)
	if err != nil {
		return err
	}
	_, err = s.tcpDB.Exec(
		`INSERT INTO streams (id, payload, updated_at) VALUES (?, ?, datetime('now'))
		 ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		stream.ID, string(payload),
	)
	return err
}

func (s *SQLiteStore) DeleteStream(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.tcpDB.Exec(`DELETE FROM streams WHERE id = ?`, id)
	return err
}
