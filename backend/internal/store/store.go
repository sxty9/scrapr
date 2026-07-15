// Package store is scrapr's metadata index: an embedded pure-Go SQLite database (one writer,
// WAL) holding scraper configs, run history and document metadata. The scraped BYTES live in
// the graveyard (lakearch); this index only holds the metadata + the graveyard Ref, and every
// row is scoped to its owner (the server-stamped holistic username) so users never see each
// other's scrapers or documents. The schema mirrors studiq's Scraper/Document/ScraperRun types
// so the HTTP layer maps them 1:1. Pattern follows notify/backend/internal/store.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned for a missing scraper or document (scoped to the caller).
var ErrNotFound = errors.New("not found")

// Scraper is one configured crawl. Goal/Allow/StoreRef are scrapr-internal (not part of
// studiq's UI shape); the LastRun* columns memoize the latest run for the list view.
type Scraper struct {
	ID             string
	Owner          string
	Name           string
	Model          string
	Source         string
	ScheduleKind   string
	ScheduleCustom string
	Enabled        bool
	Goal           string
	Allow          []string
	StoreRef       string
	LastRunAt      int64 // unix; 0 => never
	LastRunStatus  string
	LastRunAdded   int
	Created        int64
	Updated        int64
}

// ScraperPatch is a partial update; only non-nil fields are applied.
type ScraperPatch struct {
	Name           *string
	Model          *string
	Source         *string
	ScheduleKind   *string
	ScheduleCustom *string
	Enabled        *bool
	Goal           *string
	Allow          *[]string
	StoreRef       *string
}

// Run is one execution of a scraper.
type Run struct {
	ID        string
	ScraperID string
	Owner     string
	Started   int64
	Finished  int64
	Status    string
	Added     int
	Pages     int
	Bytes     int64
	Error     string
}

// Document is one scraped (or manually added) piece of content. Bytes live in the graveyard
// under LakearchRef; this is the metadata studiq shows and links to.
type Document struct {
	ID          string
	RunID       string
	ScraperID   string
	Owner       string
	Title       string
	Kategorie   string
	Source      string // "scraper" | "manual"
	ModuleID    string
	URL         string
	MediaType   string
	Bytes       int64
	LakearchRef string
	Added       int64
}

const schema = `
CREATE TABLE IF NOT EXISTS scrapers(
  id              TEXT PRIMARY KEY,
  owner           TEXT NOT NULL,
  name            TEXT NOT NULL,
  model           TEXT NOT NULL DEFAULT 'website',
  source          TEXT NOT NULL DEFAULT '',
  schedule_kind   TEXT NOT NULL DEFAULT 'manual',
  schedule_custom TEXT NOT NULL DEFAULT '',
  enabled         INTEGER NOT NULL DEFAULT 1,
  goal            TEXT NOT NULL DEFAULT '',
  allow_json      TEXT NOT NULL DEFAULT '[]',
  store_ref       TEXT NOT NULL DEFAULT '',
  last_run_at     INTEGER NOT NULL DEFAULT 0,
  last_run_status TEXT NOT NULL DEFAULT 'never',
  last_run_added  INTEGER NOT NULL DEFAULT 0,
  created         INTEGER NOT NULL,
  updated         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS scrapers_owner ON scrapers(owner, created);

CREATE TABLE IF NOT EXISTS runs(
  id         TEXT PRIMARY KEY,
  scraper_id TEXT NOT NULL,
  owner      TEXT NOT NULL,
  started    INTEGER NOT NULL,
  finished   INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL DEFAULT 'ok',
  added      INTEGER NOT NULL DEFAULT 0,
  pages      INTEGER NOT NULL DEFAULT 0,
  bytes      INTEGER NOT NULL DEFAULT 0,
  error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_scraper ON runs(scraper_id, started);

CREATE TABLE IF NOT EXISTS documents(
  id           TEXT PRIMARY KEY,
  run_id       TEXT NOT NULL DEFAULT '',
  scraper_id   TEXT NOT NULL DEFAULT '',
  owner        TEXT NOT NULL,
  title        TEXT NOT NULL,
  kategorie    TEXT NOT NULL DEFAULT 'Quellen',
  source       TEXT NOT NULL DEFAULT 'scraper',
  module_id    TEXT NOT NULL DEFAULT '',
  url          TEXT NOT NULL DEFAULT '',
  media_type   TEXT NOT NULL DEFAULT '',
  bytes        INTEGER NOT NULL DEFAULT 0,
  lakearch_ref TEXT NOT NULL DEFAULT '',
  added        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS docs_owner ON documents(owner, added);
`

// Store owns the SQLite database. One mutex serialises mutations (max-open-conns is 1).
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// Open initialises the data root and the SQLite index at <root>/index.db.
func Open(path string) (*Store, error) {
	if path == "" {
		path = "/var/lib/scrapr/index.db"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func randID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func marshalAllow(allow []string) string {
	if len(allow) == 0 {
		return "[]"
	}
	b, err := json.Marshal(allow)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalAllow(s string) []string {
	var out []string
	if s == "" {
		return nil
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// ── scrapers ──────────────────────────────────────────────────────────────────────

// AddScraper inserts a new scraper for owner, assigning an id and timestamps.
func (s *Store) AddScraper(sc Scraper) (Scraper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc.ID = randID()
	now := time.Now().Unix()
	sc.Created, sc.Updated = now, now
	if sc.Model == "" {
		sc.Model = "website"
	}
	if sc.ScheduleKind == "" {
		sc.ScheduleKind = "manual"
	}
	sc.LastRunStatus = "never"
	_, err := s.db.Exec(
		`INSERT INTO scrapers(id,owner,name,model,source,schedule_kind,schedule_custom,enabled,goal,allow_json,store_ref,last_run_at,last_run_status,last_run_added,created,updated)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,0,'never',0,?,?)`,
		sc.ID, sc.Owner, sc.Name, sc.Model, sc.Source, sc.ScheduleKind, sc.ScheduleCustom,
		boolToInt(sc.Enabled), sc.Goal, marshalAllow(sc.Allow), sc.StoreRef, sc.Created, sc.Updated,
	)
	if err != nil {
		return Scraper{}, err
	}
	return sc, nil
}

// Scrapers returns the caller's scrapers, newest first.
func (s *Store) Scrapers(owner string) ([]Scraper, error) {
	rows, err := s.db.Query(`SELECT `+scraperCols+` FROM scrapers WHERE owner=? ORDER BY created DESC`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Scraper
	for rows.Next() {
		sc, err := scanScraper(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

// Scraper returns one scraper scoped to the caller.
func (s *Store) Scraper(owner, id string) (Scraper, error) {
	row := s.db.QueryRow(`SELECT `+scraperCols+` FROM scrapers WHERE owner=? AND id=?`, owner, id)
	sc, err := scanScraper(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Scraper{}, ErrNotFound
	}
	return sc, err
}

// UpdateScraper applies a partial patch to the caller's scraper and returns the updated row.
func (s *Store) UpdateScraper(owner, id string, p ScraperPatch) (Scraper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, err := s.Scraper(owner, id)
	if err != nil {
		return Scraper{}, err
	}
	if p.Name != nil {
		sc.Name = *p.Name
	}
	if p.Model != nil {
		sc.Model = *p.Model
	}
	if p.Source != nil {
		sc.Source = *p.Source
	}
	if p.ScheduleKind != nil {
		sc.ScheduleKind = *p.ScheduleKind
	}
	if p.ScheduleCustom != nil {
		sc.ScheduleCustom = *p.ScheduleCustom
	}
	if p.Enabled != nil {
		sc.Enabled = *p.Enabled
	}
	if p.Goal != nil {
		sc.Goal = *p.Goal
	}
	if p.Allow != nil {
		sc.Allow = *p.Allow
	}
	if p.StoreRef != nil {
		sc.StoreRef = *p.StoreRef
	}
	sc.Updated = time.Now().Unix()
	_, err = s.db.Exec(
		`UPDATE scrapers SET name=?,model=?,source=?,schedule_kind=?,schedule_custom=?,enabled=?,goal=?,allow_json=?,store_ref=?,updated=? WHERE owner=? AND id=?`,
		sc.Name, sc.Model, sc.Source, sc.ScheduleKind, sc.ScheduleCustom, boolToInt(sc.Enabled),
		sc.Goal, marshalAllow(sc.Allow), sc.StoreRef, sc.Updated, owner, id,
	)
	if err != nil {
		return Scraper{}, err
	}
	return sc, nil
}

// ── runs ──────────────────────────────────────────────────────────────────────────

// StartRun records a run in progress and returns its id.
func (s *Store) StartRun(owner, scraperID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := randID()
	_, err := s.db.Exec(
		`INSERT INTO runs(id,scraper_id,owner,started,status) VALUES(?,?,?,?,'ok')`,
		id, scraperID, owner, time.Now().Unix(),
	)
	return id, err
}

// FinishRun records a run's outcome and memoizes it on the scraper row (last_run_*).
func (s *Store) FinishRun(r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	if _, err := s.db.Exec(
		`UPDATE runs SET finished=?,status=?,added=?,pages=?,bytes=?,error=? WHERE id=? AND owner=?`,
		now, r.Status, r.Added, r.Pages, r.Bytes, r.Error, r.ID, r.Owner,
	); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`UPDATE scrapers SET last_run_at=?,last_run_status=?,last_run_added=?,updated=? WHERE id=? AND owner=?`,
		now, r.Status, r.Added, now, r.ScraperID, r.Owner,
	)
	return err
}

// ── documents ─────────────────────────────────────────────────────────────────────

// InsertDocument stores one document's metadata, assigning an id + timestamp when unset.
func (s *Store) InsertDocument(d Document) (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.ID == "" {
		d.ID = randID()
	}
	if d.Added == 0 {
		d.Added = time.Now().Unix()
	}
	if d.Source == "" {
		d.Source = "scraper"
	}
	if d.Kategorie == "" {
		d.Kategorie = "Quellen"
	}
	_, err := s.db.Exec(
		`INSERT INTO documents(id,run_id,scraper_id,owner,title,kategorie,source,module_id,url,media_type,bytes,lakearch_ref,added)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.RunID, d.ScraperID, d.Owner, d.Title, d.Kategorie, d.Source, d.ModuleID, d.URL, d.MediaType, d.Bytes, d.LakearchRef, d.Added,
	)
	if err != nil {
		return Document{}, err
	}
	return d, nil
}

// Documents returns the caller's documents, newest first (capped at limit).
func (s *Store) Documents(owner string, limit int) ([]Document, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT `+docCols+` FROM documents WHERE owner=? ORDER BY added DESC, id DESC LIMIT ?`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DocumentsForRun returns the documents produced by one run, newest first.
func (s *Store) DocumentsForRun(owner, runID string) ([]Document, error) {
	rows, err := s.db.Query(`SELECT `+docCols+` FROM documents WHERE owner=? AND run_id=? ORDER BY added DESC, id DESC`, owner, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Document
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Document returns one document scoped to the caller (for the content fetch → lakearch Ref).
func (s *Store) Document(owner, id string) (Document, error) {
	row := s.db.QueryRow(`SELECT `+docCols+` FROM documents WHERE owner=? AND id=?`, owner, id)
	d, err := scanDoc(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	return d, err
}

// ── scanning helpers ────────────────────────────────────────────────────────────────

const scraperCols = `id,owner,name,model,source,schedule_kind,schedule_custom,enabled,goal,allow_json,store_ref,last_run_at,last_run_status,last_run_added,created,updated`

const docCols = `id,run_id,scraper_id,owner,title,kategorie,source,module_id,url,media_type,bytes,lakearch_ref,added`

type scanner interface {
	Scan(dest ...any) error
}

func scanScraper(row scanner) (Scraper, error) {
	var sc Scraper
	var enabled int
	var allowJSON string
	err := row.Scan(&sc.ID, &sc.Owner, &sc.Name, &sc.Model, &sc.Source, &sc.ScheduleKind, &sc.ScheduleCustom,
		&enabled, &sc.Goal, &allowJSON, &sc.StoreRef, &sc.LastRunAt, &sc.LastRunStatus, &sc.LastRunAdded, &sc.Created, &sc.Updated)
	if err != nil {
		return Scraper{}, err
	}
	sc.Enabled = enabled != 0
	sc.Allow = unmarshalAllow(allowJSON)
	return sc, nil
}

func scanDoc(row scanner) (Document, error) {
	var d Document
	err := row.Scan(&d.ID, &d.RunID, &d.ScraperID, &d.Owner, &d.Title, &d.Kategorie, &d.Source, &d.ModuleID, &d.URL, &d.MediaType, &d.Bytes, &d.LakearchRef, &d.Added)
	if err != nil {
		return Document{}, err
	}
	return d, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
