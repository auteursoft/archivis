// Package store persists the photo catalogue in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// SchemaVersion is bumped when the analysis pipeline changes in a way that
// warrants re-processing photos (see Photo.Version).
const SchemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS photos (
	id               INTEGER PRIMARY KEY,
	path             TEXT NOT NULL UNIQUE,
	size             INTEGER NOT NULL,
	mtime            INTEGER NOT NULL,
	fingerprint      TEXT NOT NULL,
	format           TEXT,
	width            INTEGER,
	height           INTEGER,
	taken_at         INTEGER,
	camera_make      TEXT,
	camera_model     TEXT,
	lens             TEXT,
	focal_length     REAL,
	focal35          REAL,
	fnumber          REAL,
	exposure_time    REAL,
	iso              INTEGER,
	flash            INTEGER,
	gps_lat          REAL,
	gps_lon          REAL,
	artist           TEXT,
	phash            INTEGER,
	sharpness        REAL,
	sharpness_global REAL,
	face_sharpness   REAL,
	brightness       REAL,
	highlights       REAL,
	shadows          REAL,
	dynamic_range    REAL,
	contrast         REAL,
	cast_a           REAL,
	cast_b           REAL,
	cast_strength    REAL,
	colorfulness     REAL,
	saturation       REAL,
	monochrome       INTEGER,
	noise            REAL,
	focus_score      REAL,
	exposure_score   REAL,
	color_score      REAL,
	noise_score      REAL,
	technical        REAL,
	aesthetic        REAL,
	overall          REAL,
	face_count       INTEGER NOT NULL DEFAULT 0,
	blockiness       REAL NOT NULL DEFAULT 1,
	indexed_at       INTEGER NOT NULL,
	version          INTEGER NOT NULL,
	-- change counter: each Save takes the next value (see nextSeq), so a
	-- reader can find everything saved since it last looked
	seq              INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS photos_fp      ON photos(fingerprint);
CREATE INDEX IF NOT EXISTS photos_taken   ON photos(taken_at);
CREATE INDEX IF NOT EXISTS photos_overall ON photos(overall);
CREATE INDEX IF NOT EXISTS photos_phash   ON photos(phash);
CREATE INDEX IF NOT EXISTS photos_burst   ON photos(taken_at, camera_make, camera_model);
CREATE INDEX IF NOT EXISTS photos_seq     ON photos(seq);

-- CLIP embeddings live in their own table so that filtering and counting
-- photos scans small rows (the 1 KB blobs made full scans ~7x slower).
CREATE TABLE IF NOT EXISTS photo_clip (
	photo_id INTEGER PRIMARY KEY REFERENCES photos(id) ON DELETE CASCADE,
	clip     BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS people (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE COLLATE NOCASE,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS faces (
	id            INTEGER PRIMARY KEY,
	photo_id      INTEGER NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
	ordinal       INTEGER NOT NULL,
	x1 REAL, y1 REAL, x2 REAL, y2 REAL,
	score         REAL,
	width_px      REAL,
	sharpness     REAL,
	yaw           REAL,
	embedding     BLOB NOT NULL,
	person_id     INTEGER REFERENCES people(id) ON DELETE SET NULL,
	person_source TEXT,
	person_sim    REAL
);
CREATE INDEX IF NOT EXISTS faces_photo  ON faces(photo_id);
CREATE INDEX IF NOT EXISTS faces_person ON faces(person_id);

CREATE TABLE IF NOT EXISTS tags (
	photo_id INTEGER NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
	tag      TEXT NOT NULL,
	score    REAL NOT NULL,
	PRIMARY KEY (photo_id, tag)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS tags_tag ON tags(tag, score);

CREATE TABLE IF NOT EXISTS errors (
	path  TEXT PRIMARY KEY,
	size  INTEGER,
	mtime INTEGER,
	error TEXT,
	at    INTEGER
);

CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);

CREATE TABLE IF NOT EXISTS aesthetic_models (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	kind       TEXT NOT NULL,
	parent     TEXT,
	model      BLOB,
	active     INTEGER NOT NULL DEFAULT 0,
	weight     REAL NOT NULL DEFAULT 0,
	metrics    TEXT,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS aesthetic_scores (
	photo_id INTEGER NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
	model    TEXT NOT NULL,
	score    REAL NOT NULL,
	PRIMARY KEY (photo_id, model)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS aesthetic_scores_model ON aesthetic_scores(model);

CREATE TABLE IF NOT EXISTS aesthetic_feedback (
	id         INTEGER PRIMARY KEY,
	photo_id   INTEGER NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
	rater      TEXT NOT NULL,
	rating     REAL,
	verdict    TEXT,
	shown      REAL,
	models     TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS aesthetic_feedback_photo ON aesthetic_feedback(photo_id);
`

// Store is the catalogue database.
type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=30000&_foreign_keys=on&_cache_size=-131072"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if err := migrateClipColumn(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating catalogue: %w", err)
	}
	if err := addMissingColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating catalogue: %w", err)
	}
	// an earlier release created photos_burst with a different column order
	db.Exec(`DROP INDEX IF EXISTS photos_burst_v1`)
	var oldDef string
	db.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'photos_burst'`).Scan(&oldDef)
	if strings.Contains(oldDef, "camera_make, camera_model, taken_at") {
		db.Exec(`DROP INDEX photos_burst`)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialising schema: %w", err)
	}
	return &Store{DB: db}, nil
}

func (s *Store) Close() error { return s.DB.Close() }

// nextSeq takes the next change number inside a write transaction. SQLite
// has a single writer, so numbers become visible in commit order: a reader
// that remembers the highest number it has seen misses nothing, however
// long the save took.
func nextSeq(tx *sql.Tx) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('seq', '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + 1`); err != nil {
		return 0, err
	}
	var n int64
	err := tx.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'seq'`).Scan(&n)
	return n, err
}

// MaxSeq is the latest change number committed (0 if none).
func (s *Store) MaxSeq() int64 {
	var n int64
	s.DB.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'seq'`).Scan(&n)
	return n
}

// migrateClipColumn moves embeddings out of the photos table in catalogues
// created by earlier versions.
func migrateClipColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(photos)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
		if name == "clip" {
			has = true
		}
	}
	rows.Close()
	if !has {
		return nil
	}
	fmt.Fprintln(os.Stderr, "upgrading catalogue: moving embeddings to their own table (one-off, may take a few minutes)…")
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS photo_clip (photo_id INTEGER PRIMARY KEY REFERENCES photos(id) ON DELETE CASCADE, clip BLOB NOT NULL)`,
		`INSERT OR REPLACE INTO photo_clip (photo_id, clip) SELECT id, clip FROM photos WHERE clip IS NOT NULL`,
		`ALTER TABLE photos DROP COLUMN clip`,
	} {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	_, err = db.Exec(`VACUUM`)
	return err
}

// addMissingColumns brings tables created by older versions up to the
// current schema by adding every column the schema declares that the table
// lacks. Column definitions are read from the schema itself, so new columns
// are migrated without a hand-written list. SQLite cannot add PRIMARY KEY or
// UNIQUE columns, and NOT NULL columns need a default, which is supplied.
func addMissingColumns(db *sql.DB) error {
	for table, cols := range schemaColumns() {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue // fresh catalogue: the schema creates it
		}
		have := map[string]bool{}
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			have[name] = true
		}
		rows.Close()
		for _, c := range cols {
			if have[c.name] {
				continue
			}
			decl := c.decl
			up := strings.ToUpper(decl)
			if strings.Contains(up, "PRIMARY KEY") || strings.Contains(up, "UNIQUE") {
				return fmt.Errorf("table %s lacks key column %s and cannot be upgraded in place", table, c.name)
			}
			// Existing rows get a typed default: the Go code scans most
			// columns into plain strings/numbers, which cannot hold NULL.
			// Columns it reads as nullable keep NULL ("unknown").
			if !strings.Contains(up, "DEFAULT") && !nullableColumns[c.name] {
				if strings.HasPrefix(up, "TEXT") {
					decl += " DEFAULT ''"
				} else if !strings.HasPrefix(up, "BLOB") {
					decl += " DEFAULT 0"
				}
			}
			if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + c.name + ` ` + decl); err != nil {
				return fmt.Errorf("adding %s.%s: %w", table, c.name, err)
			}
		}
	}
	return nil
}

type columnDef struct{ name, decl string }

// nullableColumns are scanned into sql.Null* types, where NULL means unknown.
var nullableColumns = map[string]bool{
	"gps_lat": true, "gps_lon": true, "face_sharpness": true, "aesthetic": true,
	"person_id": true, "person_source": true, "person_sim": true,
}

// schemaColumns parses the column definitions of each CREATE TABLE in schema.
func schemaColumns() map[string][]columnDef {
	out := map[string][]columnDef{}
	var table string
	for _, line := range strings.Split(schema, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "CREATE TABLE IF NOT EXISTS "):
			f := strings.Fields(strings.TrimPrefix(t, "CREATE TABLE IF NOT EXISTS "))
			table = f[0]
		case table != "" && strings.HasPrefix(t, ")"):
			table = ""
		case table != "" && t != "" && !strings.HasPrefix(t, "--"):
			t = strings.TrimSuffix(t, ",")
			f := strings.Fields(t)
			if len(f) < 2 || strings.ToUpper(f[0]) == "PRIMARY" || strings.ToUpper(f[0]) == "UNIQUE" {
				continue // table constraints, not columns
			}
			// "x1 REAL, y1 REAL, ..." declares several columns on one line
			for _, part := range strings.Split(t, ",") {
				pf := strings.Fields(strings.TrimSpace(part))
				if len(pf) >= 2 {
					out[table] = append(out[table], columnDef{pf[0], strings.Join(pf[1:], " ")})
				}
			}
		}
	}
	return out
}

// CLIP returns the stored embedding of a photo (nil if none).
func (s *Store) CLIP(photoID int64) ([]byte, error) {
	var b []byte
	err := s.DB.QueryRow(`SELECT clip FROM photo_clip WHERE photo_id = ?`, photoID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// Photo is one catalogued image.
type Photo struct {
	ID              int64
	Path            string
	Size            int64
	MTime           int64
	Fingerprint     string
	Format          string
	Width, Height   int
	TakenAt         int64 // unix seconds; 0 = unknown
	CameraMake      string
	CameraModel     string
	Lens            string
	FocalLength     float64
	Focal35         float64
	FNumber         float64
	ExposureTime    float64
	ISO             int
	Flash           bool
	GPSLat, GPSLon  sql.NullFloat64
	Artist          string
	PHash           uint64
	Sharpness       float64
	SharpnessGlobal float64
	FaceSharpness   sql.NullFloat64
	Brightness      float64
	Highlights      float64
	Shadows         float64
	DynamicRange    float64
	Contrast        float64
	CastA, CastB    float64
	CastStrength    float64
	Colorfulness    float64
	Saturation      float64
	Monochrome      bool
	Noise           float64
	FocusScore      float64
	ExposureScore   float64
	ColorScore      float64
	NoiseScore      float64
	Technical       float64
	Aesthetic       sql.NullFloat64
	Overall         float64
	FaceCount       int
	Blockiness      float64 // JPEG block artefacts, 1 = none
	CLIP            []byte  // float16 embedding (stored in photo_clip; not filled by queries — see Store.CLIP)
	IndexedAt       int64
	Version         int
}

// Face is a detected face.
type Face struct {
	ID             int64
	PhotoID        int64
	Ordinal        int
	X1, Y1, X2, Y2 float64 // normalised 0..1
	Score          float64
	WidthPx        float64
	Sharpness      float64
	Yaw            float64
	Embedding      []byte // float16
	PersonID       sql.NullInt64
	PersonSource   sql.NullString
	PersonSim      sql.NullFloat64
}

// Tag is a classifier label.
type Tag struct {
	Tag   string
	Score float64
}

const photoCols = `id, path, size, mtime, fingerprint, format, width, height, taken_at,
	camera_make, camera_model, lens, focal_length, focal35, fnumber, exposure_time, iso, flash,
	gps_lat, gps_lon, artist, phash, sharpness, sharpness_global, face_sharpness, brightness,
	highlights, shadows, dynamic_range, contrast, cast_a, cast_b, cast_strength, colorfulness,
	saturation, monochrome, noise, focus_score, exposure_score, color_score, noise_score,
	technical, aesthetic, overall, face_count, blockiness, indexed_at, version`

func (p *Photo) scanArgs() []any {
	return []any{&p.ID, &p.Path, &p.Size, &p.MTime, &p.Fingerprint, &p.Format, &p.Width, &p.Height, &p.TakenAt,
		&p.CameraMake, &p.CameraModel, &p.Lens, &p.FocalLength, &p.Focal35, &p.FNumber, &p.ExposureTime, &p.ISO, &p.Flash,
		&p.GPSLat, &p.GPSLon, &p.Artist, (*int64ptr)(&p.PHash), &p.Sharpness, &p.SharpnessGlobal, &p.FaceSharpness, &p.Brightness,
		&p.Highlights, &p.Shadows, &p.DynamicRange, &p.Contrast, &p.CastA, &p.CastB, &p.CastStrength, &p.Colorfulness,
		&p.Saturation, &p.Monochrome, &p.Noise, &p.FocusScore, &p.ExposureScore, &p.ColorScore, &p.NoiseScore,
		&p.Technical, &p.Aesthetic, &p.Overall, &p.FaceCount, &p.Blockiness, &p.IndexedAt, &p.Version}
}

// int64ptr lets a uint64 phash round-trip through SQLite's signed integers.
type int64ptr uint64

func (u *int64ptr) Scan(v any) error {
	switch x := v.(type) {
	case int64:
		*u = int64ptr(uint64(x))
	case nil:
		*u = 0
	default:
		return fmt.Errorf("phash: unexpected %T", v)
	}
	return nil
}

func (p *Photo) insertArgs() []any {
	return []any{p.Path, p.Size, p.MTime, p.Fingerprint, p.Format, p.Width, p.Height, p.TakenAt,
		p.CameraMake, p.CameraModel, p.Lens, p.FocalLength, p.Focal35, p.FNumber, p.ExposureTime, p.ISO, p.Flash,
		p.GPSLat, p.GPSLon, p.Artist, int64(p.PHash), p.Sharpness, p.SharpnessGlobal, p.FaceSharpness, p.Brightness,
		p.Highlights, p.Shadows, p.DynamicRange, p.Contrast, p.CastA, p.CastB, p.CastStrength, p.Colorfulness,
		p.Saturation, p.Monochrome, p.Noise, p.FocusScore, p.ExposureScore, p.ColorScore, p.NoiseScore,
		p.Technical, p.Aesthetic, p.Overall, p.FaceCount, p.Blockiness, p.IndexedAt, p.Version}
}

var insertPhotoSQL = func() string {
	cols := strings.TrimPrefix(photoCols, "id, ")
	n := strings.Count(cols, ",") + 1
	return "INSERT INTO photos (" + cols + ") VALUES (" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}()

var updatePhotoSQL = func() string {
	cols := strings.Split(strings.TrimPrefix(photoCols, "id, "), ",")
	for i, c := range cols {
		cols[i] = strings.TrimSpace(c) + " = ?"
	}
	return "UPDATE photos SET " + strings.Join(cols, ", ") + " WHERE id = ?"
}()

// Known describes what the catalogue already has for a path.
type Known struct {
	ID      int64
	Size    int64
	MTime   int64
	Version int
}

// Lookup returns the catalogue state of a path, if any.
func (s *Store) Lookup(path string) (*Known, error) {
	var k Known
	err := s.DB.QueryRow(`SELECT id, size, mtime, version FROM photos WHERE path = ?`, path).Scan(&k.ID, &k.Size, &k.MTime, &k.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &k, err
}

// ErrorFor returns a recorded error for an unchanged file.
func (s *Store) ErrorFor(path string, size, mtime int64) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM errors WHERE path = ? AND size = ? AND mtime = ?`, path, size, mtime).Scan(&n)
	return n > 0, err
}

// ByFingerprint returns photos with the given content fingerprint.
func (s *Store) ByFingerprint(fp string) ([]Photo, error) {
	return s.queryPhotos(`SELECT `+photoCols+` FROM photos WHERE fingerprint = ?`, fp)
}

func (s *Store) queryPhotos(q string, args ...any) ([]Photo, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Photo
	for rows.Next() {
		var p Photo
		if err := rows.Scan(p.scanArgs()...); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Photo loads one photo by id.
func (s *Store) Photo(id int64) (*Photo, error) {
	ps, err := s.queryPhotos(`SELECT `+photoCols+` FROM photos WHERE id = ?`, id)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	return &ps[0], nil
}

// Photos loads several photos by id, preserving the input order.
func (s *Store) Photos(ids []int64) ([]Photo, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	byID := map[int64]Photo{}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		q := `SELECT ` + photoCols + ` FROM photos WHERE id IN (` + placeholders(len(chunk)) + `)`
		ps, err := s.queryPhotos(q, int64sToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			byID[p.ID] = p
		}
	}
	out := make([]Photo, 0, len(ids))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func int64sToAny(v []int64) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

// Result is everything the indexer produced for one file.
type Result struct {
	Photo Photo
	Faces []Face
	Tags  []Tag
	// AestheticScores holds each aesthetic model's rating of the photo, by
	// model ID; Photo.Aesthetic is their blend.
	AestheticScores map[string]float64
}

// Save writes results in a single transaction, replacing earlier versions of
// the same paths. It fills in the assigned photo and face IDs.
func (s *Store) Save(results []*Result) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ps := &prepared{tx: tx}
	find := ps.prep(`SELECT id, fingerprint FROM photos WHERE path = ?`)
	upd := ps.prep(updatePhotoSQL)
	clearF := ps.prep(`DELETE FROM faces WHERE photo_id = ?`)
	clearT := ps.prep(`DELETE FROM tags WHERE photo_id = ?`)
	clearC := ps.prep(`DELETE FROM photo_clip WHERE photo_id = ?`)
	clearA := ps.prep(`DELETE FROM aesthetic_scores WHERE photo_id = ?`)
	ins := ps.prep(insertPhotoSQL)
	insF := ps.prep(`INSERT INTO faces (photo_id, ordinal, x1, y1, x2, y2, score, width_px, sharpness, yaw, embedding, person_id, person_source, person_sim)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	insT := ps.prep(`INSERT OR REPLACE INTO tags (photo_id, tag, score) VALUES (?,?,?)`)
	insC := ps.prep(`INSERT INTO photo_clip (photo_id, clip) VALUES (?,?)`)
	insA := ps.prep(`INSERT OR REPLACE INTO aesthetic_scores (photo_id, model, score) VALUES (?,?,?)`)
	delE := ps.prep(`DELETE FROM errors WHERE path = ?`)
	setSeq := ps.prep(`UPDATE photos SET seq = ? WHERE id = ?`)
	if ps.err != nil {
		return ps.err
	}
	seq, err := nextSeq(tx)
	if err != nil {
		return err
	}
	for _, r := range results {
		p := &r.Photo
		if err := preserveLabels(tx, p.Path, r.Faces); err != nil {
			return err
		}
		// Re-indexing updates the row in place so photo IDs (and the URLs
		// built from them) stay stable; faces and tags are replaced.
		var existing int64
		var oldFP string
		if err := find.QueryRow(p.Path).Scan(&existing, &oldFP); err == nil {
			if _, err := upd.Exec(append(p.insertArgs(), existing)...); err != nil {
				return fmt.Errorf("update %s: %w", p.Path, err)
			}
			// Replace everything derived from the old analysis, including
			// its CLIP embedding (a re-index without one must not leave the
			// stale vector searchable). Aesthetic scores are kept when the
			// content is unchanged: they still hold, and those of inactive
			// or legacy models could not be recomputed.
			clear := []*sql.Stmt{clearF, clearT, clearC}
			if oldFP != p.Fingerprint {
				clear = append(clear, clearA)
			}
			for _, st := range clear {
				if _, err := st.Exec(existing); err != nil {
					return fmt.Errorf("clear %s: %w", p.Path, err)
				}
			}
			p.ID = existing
		} else {
			res, err := ins.Exec(p.insertArgs()...)
			if err != nil {
				return fmt.Errorf("insert %s: %w", p.Path, err)
			}
			p.ID, _ = res.LastInsertId()
		}
		if _, err := setSeq.Exec(seq, p.ID); err != nil {
			return err
		}
		if len(p.CLIP) > 0 {
			if _, err := insC.Exec(p.ID, p.CLIP); err != nil {
				return err
			}
		}
		for i := range r.Faces {
			f := &r.Faces[i]
			f.PhotoID = p.ID
			res, err := insF.Exec(f.PhotoID, f.Ordinal, f.X1, f.Y1, f.X2, f.Y2, f.Score, f.WidthPx, f.Sharpness, f.Yaw, f.Embedding, f.PersonID, f.PersonSource, f.PersonSim)
			if err != nil {
				return err
			}
			f.ID, _ = res.LastInsertId()
		}
		for _, t := range r.Tags {
			if _, err := insT.Exec(p.ID, t.Tag, t.Score); err != nil {
				return err
			}
		}
		for m, v := range r.AestheticScores {
			if _, err := insA.Exec(p.ID, m, v); err != nil {
				return err
			}
		}
		if _, err := delE.Exec(p.Path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// prepared collects the first error from a series of Prepare calls.
type prepared struct {
	tx  *sql.Tx
	err error
}

func (p *prepared) prep(q string) *sql.Stmt {
	if p.err != nil {
		return nil
	}
	st, err := p.tx.Prepare(q)
	if err != nil {
		p.err = fmt.Errorf("preparing statement: %w", err)
	}
	return st
}

// MovePath updates a photo's path in place (the file was moved/renamed).
func (s *Store) MovePath(id int64, newPath string, mtime int64) error {
	_, err := s.DB.Exec(`UPDATE photos SET path = ?, mtime = ? WHERE id = ?`, newPath, mtime, id)
	return err
}

// RecordError remembers that a file failed so unchanged files aren't retried.
func (s *Store) RecordError(path string, size, mtime int64, msg string) error {
	_, err := s.DB.Exec(`INSERT OR REPLACE INTO errors (path, size, mtime, error, at) VALUES (?,?,?,?,?)`,
		path, size, mtime, msg, time.Now().Unix())
	return err
}

// FacesForPhoto returns the faces of a photo.
func (s *Store) FacesForPhoto(photoID int64) ([]Face, error) {
	return s.queryFaces(`WHERE photo_id = ? ORDER BY ordinal`, photoID)
}

// FacesByID loads faces by id (order not preserved).
func (s *Store) FacesByID(ids []int64) ([]Face, error) {
	var out []Face
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		fs, err := s.queryFaces(`WHERE id IN (`+placeholders(len(chunk))+`)`, int64sToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	return out, nil
}

const faceCols = `id, photo_id, ordinal, x1, y1, x2, y2, score, width_px, sharpness, yaw, embedding, person_id, person_source, person_sim`

func (s *Store) queryFaces(where string, args ...any) ([]Face, error) {
	rows, err := s.DB.Query(`SELECT `+faceCols+` FROM faces `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Face
	for rows.Next() {
		var f Face
		if err := rows.Scan(&f.ID, &f.PhotoID, &f.Ordinal, &f.X1, &f.Y1, &f.X2, &f.Y2, &f.Score, &f.WidthPx, &f.Sharpness, &f.Yaw, &f.Embedding, &f.PersonID, &f.PersonSource, &f.PersonSim); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// TagsForPhoto returns a photo's tags, best first.
func (s *Store) TagsForPhoto(photoID int64) ([]Tag, error) {
	rows, err := s.DB.Query(`SELECT tag, score FROM tags WHERE photo_id = ? ORDER BY score DESC`, photoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		rows.Scan(&t.Tag, &t.Score)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReplaceTags rewrites the tags of many photos (used by re-classification).
func (s *Store) ReplaceTags(tags map[int64][]Tag) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ps := &prepared{tx: tx}
	del := ps.prep(`DELETE FROM tags WHERE photo_id = ?`)
	ins := ps.prep(`INSERT INTO tags (photo_id, tag, score) VALUES (?,?,?)`)
	if ps.err != nil {
		return ps.err
	}
	for id, ts := range tags {
		if _, err := del.Exec(id); err != nil {
			return err
		}
		for _, t := range ts {
			if _, err := ins.Exec(id, t.Tag, t.Score); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// Meta helpers ------------------------------------------------------------

func (s *Store) GetMeta(key string) string {
	var v string
	s.DB.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.DB.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, key, value)
	return err
}

// Stats summarises the catalogue.
type Stats struct {
	Photos, Faces, People, Labeled, Errors int64
}

func (s *Store) Stats() (Stats, error) {
	var st Stats
	err := s.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM photos), (SELECT COUNT(*) FROM faces), (SELECT COUNT(*) FROM people),
		(SELECT COUNT(*) FROM faces WHERE person_id IS NOT NULL), (SELECT COUNT(*) FROM errors)`).
		Scan(&st.Photos, &st.Faces, &st.People, &st.Labeled, &st.Errors)
	return st, err
}
