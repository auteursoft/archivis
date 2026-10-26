package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Apple Photos marks screenshots itself: in a library's database
// (database/Photos.sqlite), an asset whose kind subtype is 10 is a
// screenshot (Photos 5 and later; macOS 13 and later also flag detected
// ones). MarkLibraryScreenshots reads that flag for the catalogued
// libraries and marks those photos as memes and screenshots, whatever their
// own analysis said: Apple knows how each was made.

// photosScreenshot is ZKINDSUBTYPE for a screenshot.
const photosScreenshot = 10

// MarkLibraryScreenshots marks the photos Apple Photos calls screenshots,
// in every catalogued Photos library (as the folders last found them). A
// library whose database cannot be read (Photos 4 and older, no access) is
// skipped and reported in the error; the others are still marked. It
// returns how many photos it changed.
func (s *Store) MarkLibraryScreenshots(ctx context.Context) (int, error) {
	libs, err := s.PhotosLibraries()
	if err != nil {
		return 0, err
	}
	n := 0
	var failed []string
	for _, lib := range libs {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		paths, err := LibraryScreenshots(lib.Path)
		if err != nil {
			failed = append(failed, err.Error())
			continue
		}
		got, err := s.markScreenshots(paths)
		n += got
		if err != nil {
			return n, err
		}
	}
	if len(failed) > 0 {
		return n, fmt.Errorf("%s", strings.Join(failed, "; "))
	}
	return n, nil
}

// LibraryScreenshots lists the paths of the originals Apple Photos calls
// screenshots in the library at lib, spelled as the catalogue spells them
// (lib, then originals/<directory>/<file name>).
func LibraryScreenshots(lib string) ([]string, error) {
	dbPath := filepath.Join(lib, "database", "Photos.sqlite")
	db, err := openReadOnly(dbPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dbPath, err)
	}
	defer db.Close()
	// the assets' table: ZASSET since macOS 11, ZGENERICASSET in Photos 5
	table := ""
	for _, t := range []string{"ZASSET", "ZGENERICASSET"} {
		var ok bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, t).Scan(&ok); err != nil {
			return nil, fmt.Errorf("reading %s: %w", dbPath, err)
		}
		if ok {
			table = t
			break
		}
	}
	if table == "" {
		return nil, fmt.Errorf("reading %s: no assets table (a Photos library older than macOS 10.15?)", dbPath)
	}
	cols := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dbPath, err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return nil, err
		}
		cols[c] = true
	}
	rows.Close()
	for _, c := range []string{"ZDIRECTORY", "ZFILENAME", "ZKINDSUBTYPE"} {
		if !cols[c] {
			return nil, fmt.Errorf("reading %s: %s has no %s column", dbPath, table, c)
		}
	}
	cond := `ZKINDSUBTYPE = ?`
	if cols["ZISDETECTEDSCREENSHOT"] {
		cond += ` OR ZISDETECTEDSCREENSHOT = 1`
	}
	rows, err = db.Query(`SELECT ZDIRECTORY, ZFILENAME FROM `+table+` WHERE (`+cond+`) AND ZFILENAME IS NOT NULL`, photosScreenshot)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dbPath, err)
	}
	defer rows.Close()
	sep := "/" // the library's own separator
	if !strings.HasPrefix(lib, "/") && strings.Contains(lib, `\`) {
		sep = `\`
	}
	var out []string
	for rows.Next() {
		var dir sql.NullString
		var name string
		if err := rows.Scan(&dir, &name); err != nil {
			return nil, err
		}
		p := lib + sep + "originals" + sep
		if dir.String != "" {
			p += dir.String + sep
		}
		out = append(out, p+name)
	}
	return out, rows.Err()
}

// openReadOnly opens another program's SQLite database without writing to
// it. Photos may have it open: a read-only connection reads its write-ahead
// log; if that cannot be done, the database file alone is read (what
// Photos last checkpointed).
func openReadOnly(path string) (*sql.DB, error) {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // C:/… becomes file:///C:/… (file://C:/… would make C: an authority)
	}
	u := (&url.URL{Scheme: "file", Path: p}).String()
	var err error
	for _, opts := range []string{"?mode=ro", "?mode=ro&immutable=1"} {
		var db *sql.DB
		if db, err = sql.Open("sqlite3", u+opts); err != nil {
			continue
		}
		if err = db.Ping(); err == nil {
			if _, err = db.Exec(`SELECT COUNT(*) FROM sqlite_master`); err == nil {
				return db, nil
			}
		}
		db.Close()
	}
	return nil, err
}

// markScreenshots marks the catalogued photos at these paths as memes and
// screenshots, in short transactions, marking the folders for working out
// again (their covers) if any changed.
func (s *Store) markScreenshots(paths []string) (int, error) {
	n := 0
	for start := 0; start < len(paths); start += 500 {
		chunk := paths[start:min(len(paths), start+500)]
		args := []any{SourceMeme, SourceMeme}
		for _, p := range chunk {
			args = append(args, p)
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return n, err
		}
		res, err := tx.Exec(`UPDATE photos SET source = ? WHERE source != ? AND path IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			tx.Rollback()
			return n, err
		}
		got, _ := res.RowsAffected()
		if got > 0 {
			if _, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value)
				VALUES ('folders_dirty', COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'folders_dirty'), 0) + 1)`); err != nil {
				tx.Rollback()
				return n, err
			}
		}
		if err := tx.Commit(); err != nil {
			return n, err
		}
		n += int(got)
	}
	return n, nil
}
