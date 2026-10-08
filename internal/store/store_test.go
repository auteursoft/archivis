package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveBrowseAndPreserveLabels(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mk := func(path string, overall float64) *Result {
		return &Result{
			Photo: Photo{Path: path, Size: 10, MTime: 1, Fingerprint: "abc" + path, Overall: overall, FocusScore: overall / 100, PHash: 1 << 63, Version: SchemaVersion, FaceCount: 1},
			Faces: []Face{{Ordinal: 0, X1: .1, Y1: .1, X2: .3, Y2: .4, Embedding: []byte{1, 2}}},
			Tags:  []Tag{{"portrait", 0.9}},
		}
	}
	if err := st.Save([]*Result{mk("/a.jpg", 80), mk("/b.jpg", 40)}); err != nil {
		t.Fatal(err)
	}
	ps, total, err := st.Browse(Filter{MinFocus: 0.5})
	if err != nil || total != 1 || ps[0].Path != "/a.jpg" || ps[0].PHash != 1<<63 {
		t.Fatalf("browse: %v %d %+v", err, total, ps)
	}
	if tagged, _, _ := st.Browse(Filter{Tag: "portrait", Sort: "worst"}); len(tagged) != 2 || tagged[0].Path != "/b.jpg" {
		t.Fatal("tag filter / sort")
	}
	faces, _ := st.FacesForPhoto(ps[0].ID) // ps is the MinFocus browse above: /a.jpg
	pid, _ := st.EnsurePerson("Ada")
	if err := st.AssignFaces(pid, []int64{faces[0].ID}, "manual", nil); err != nil {
		t.Fatal(err)
	}
	// Re-index /a.jpg with a slightly shifted face: the label must survive.
	r := mk("/a.jpg", 82)
	r.Faces[0].X1 = .11
	if err := st.Save([]*Result{r}); err != nil {
		t.Fatal(err)
	}
	if !r.Faces[0].PersonID.Valid || r.Faces[0].PersonID.Int64 != pid {
		t.Fatalf("label not preserved: %+v", r.Faces[0].PersonID)
	}
	// auto labels never override manual ones
	st.AssignFaces(999, []int64{r.Faces[0].ID}, "auto", nil)
	f, _ := st.FacesByID([]int64{r.Faces[0].ID})
	if f[0].PersonID != (sql.NullInt64{Int64: pid, Valid: true}) {
		t.Fatal("auto overrode manual")
	}
	people, _ := st.People()
	if len(people) != 1 || people[0].Faces != 1 || people[0].Manual != 1 {
		t.Fatalf("people %+v", people)
	}
	if err := st.RenamePerson(pid, "Ada Lovelace"); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.PersonByName("ada lovelace"); p == nil {
		t.Fatal("case-insensitive lookup")
	}
}

func TestReindexKeepsPhotoID(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Result{Photo: Photo{Path: "/x.jpg", Size: 1, MTime: 1, Fingerprint: "f", Overall: 10, Version: SchemaVersion},
		Faces: []Face{{X1: .1, Y1: .1, X2: .2, Y2: .2, Embedding: []byte{0, 0}}}, Tags: []Tag{{"a", 1}},
		AestheticScores: map[string]float64{"m-1": 5}}
	if err := st.Save([]*Result{r}); err != nil {
		t.Fatal(err)
	}
	id := r.Photo.ID
	r2 := &Result{Photo: Photo{Path: "/x.jpg", Size: 2, MTime: 2, Fingerprint: "g", Overall: 90, Version: SchemaVersion},
		Faces:           []Face{{X1: .5, Y1: .5, X2: .6, Y2: .6, Embedding: []byte{0, 0}}},
		AestheticScores: map[string]float64{"n-2": 6}}
	if err := st.Save([]*Result{r2}); err != nil {
		t.Fatal(err)
	}
	if r2.Photo.ID != id {
		t.Fatalf("photo id changed %d -> %d", id, r2.Photo.ID)
	}
	p, _ := st.Photo(id)
	fs, _ := st.FacesForPhoto(id)
	tags, _ := st.TagsForPhoto(id)
	if p.Overall != 90 || p.Fingerprint != "g" || len(fs) != 1 || fs[0].X1 != .5 || len(tags) != 0 {
		t.Fatalf("not replaced: %+v %+v %+v", p.Overall, fs, tags)
	}
	if sc, _ := st.AestheticScores(id); len(sc) != 1 || sc["n-2"] != 6 {
		t.Fatalf("aesthetic scores not replaced: %v", sc)
	}
	if st.Count(false) != 1 {
		t.Fatal("duplicate row")
	}
}

func TestAestheticFeedback(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Result{Photo: Photo{Path: "/x.jpg", Size: 1, MTime: 1, Fingerprint: "f", Version: SchemaVersion, CLIP: []byte{1, 2}}}
	if err := st.Save([]*Result{r}); err != nil {
		t.Fatal(err)
	}
	id := r.Photo.ID
	rating := func(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
	for _, bad := range []Feedback{
		{PhotoID: id, Rater: "a"},                        // nothing said
		{PhotoID: id, Rater: "a", Verdict: "meh"},        // unknown verdict
		{PhotoID: id, Rater: "a", Rating: rating(11)},    // off the scale
		{PhotoID: id, Rater: "a", Rating: rating(-1)},    // off the scale
		{PhotoID: id + 9, Rater: "a", Rating: rating(5)}, // no such photo
	} {
		if _, err := st.AddFeedback(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if _, err := st.AddFeedback(Feedback{PhotoID: id + 9, Rater: "a", Verdict: "agree"}); !errors.Is(err, ErrNoPhoto) {
		t.Errorf("unknown photo: %v", err)
	}
	st.AddFeedback(Feedback{PhotoID: id, Rater: "a", Rating: rating(3), Shown: 6})
	st.AddFeedback(Feedback{PhotoID: id, Rater: "a", Verdict: "agree", Shown: 6}) // a later, weaker judgement
	st.AddFeedback(Feedback{PhotoID: id, Rater: "b", Rating: rating(7), Shown: 6})
	st.AddFeedback(Feedback{PhotoID: id, Rater: "c", Verdict: "low", Shown: 6})
	st.AddFeedback(Feedback{PhotoID: id, Rater: "c", Verdict: "high", Shown: 6}) // c changes their mind
	fb, embs, err := st.LatestFeedback("")
	if err != nil || len(fb) != 3 || len(embs) != 3 {
		t.Fatalf("latest feedback: %v %+v", err, fb)
	}
	// a's rating outranks their later verdict; c's latest verdict wins
	if fb[0].Rater != "a" || fb[0].Rating.Float64 != 3 || fb[1].Rating.Float64 != 7 || fb[2].Verdict != "high" {
		t.Fatalf("latest feedback %+v", fb)
	}
	sum, err := st.FeedbackSummary()
	if err != nil || sum.Photos != 1 || sum.NoEmbedding != 0 || len(sum.Raters) != 3 || sum.Raters[0] != (RaterCount{"a", 1}) {
		t.Fatalf("summary %+v %v", sum, err)
	}
	if fb, _, _ := st.LatestFeedback("b"); len(fb) != 1 || fb[0].Rater != "b" {
		t.Fatalf("rater filter %+v", fb)
	}
	if f, _ := st.PhotoFeedback(id, "a"); f == nil || f.Verdict != "agree" { // what they said last
		t.Fatalf("photo feedback %+v", f)
	}
	if f, _ := st.PhotoFeedback(id, "nobody"); f != nil {
		t.Fatalf("feedback from nobody %+v", f)
	}
}

func TestMigratesClipColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	// minimal photos table as written by the first release
	_, err = db.Exec(`CREATE TABLE photos (id INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE, size INTEGER NOT NULL,
		mtime INTEGER NOT NULL, fingerprint TEXT NOT NULL, clip BLOB, indexed_at INTEGER NOT NULL, version INTEGER NOT NULL);
		INSERT INTO photos (path, size, mtime, fingerprint, clip, indexed_at, version) VALUES ('/a', 1, 1, 'f', X'0102', 1, 1);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("old catalogue must open after migration: %v", err)
	}
	defer st.Close()
	b, err := st.CLIP(1)
	if err != nil || len(b) != 2 || b[1] != 2 {
		t.Fatalf("clip not migrated: %v %v", b, err)
	}
	// every query over the upgraded table must work
	p, err := st.Photo(1)
	if err != nil || p == nil || p.Path != "/a" || p.Blockiness != 1 {
		t.Fatalf("photo query after migration: %+v %v", p, err)
	}
	if _, _, err := st.Browse(Filter{MinFocus: 0.1, Sort: "sharpness"}); err != nil {
		t.Fatalf("browse after migration: %v", err)
	}
}

func TestBursts(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	add := func(path, cam string, at int64, overall float64) {
		rs = append(rs, &Result{Photo: Photo{Path: path, Size: 1, MTime: 1, Fingerprint: path, CameraModel: cam, TakenAt: at, Overall: overall, Version: SchemaVersion}})
	}
	// burst A: 4 frames, camera X; burst B: 3 frames camera Y interleaved in time; singles
	for i := 0; i < 4; i++ {
		add(fmt.Sprintf("/a%d", i), "X", 1000+int64(i), float64(10*i))
	}
	for i := 0; i < 3; i++ {
		add(fmt.Sprintf("/b%d", i), "Y", 1001+int64(i), 50)
	}
	add("/single", "X", 5000, 99)
	add("/old1", "Z", 10, 1)
	add("/old2", "Z", 100, 1)
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	bs, err := st.Bursts(Filter{Limit: 10}, 3, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 2 {
		t.Fatalf("want 2 bursts, got %d", len(bs))
	}
	sizes := map[int]bool{len(bs[0].Photos): true, len(bs[1].Photos): true}
	if !sizes[4] || !sizes[3] {
		t.Fatalf("burst sizes %d,%d", len(bs[0].Photos), len(bs[1].Photos))
	}
	for _, b := range bs {
		if len(b.Photos) == 4 && b.Photos[0].Path != "/a3" {
			t.Fatalf("best frame first: got %s", b.Photos[0].Path)
		}
	}
}

func TestSchemaColumnsParsed(t *testing.T) {
	cols := schemaColumns()
	want := map[string][]string{
		"photos":             {"path", "blockiness", "face_count", "aesthetic", "indexed_at"},
		"faces":              {"photo_id", "x1", "y1", "x2", "y2", "embedding", "person_sim"},
		"tags":               {"photo_id", "tag", "score"},
		"aesthetic_scores":   {"photo_id", "model", "score"},
		"aesthetic_feedback": {"photo_id", "rater", "rating", "verdict", "shown"},
	}
	for table, names := range want {
		have := map[string]string{}
		for _, c := range cols[table] {
			have[c.name] = c.decl
		}
		for _, n := range names {
			if _, ok := have[n]; !ok {
				t.Errorf("%s.%s not parsed from schema (got %v)", table, n, have)
			}
		}
		if _, bad := have["PRIMARY"]; bad {
			t.Errorf("%s: table constraint parsed as a column", table)
		}
	}
}

func TestMissingPersonAndStaleClip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.RenamePerson(999, "Nobody"); !errors.Is(err, ErrNoPerson) {
		t.Fatalf("rename missing person: want ErrNoPerson, got %v", err)
	}
	if err := st.DeletePerson(999); !errors.Is(err, ErrNoPerson) {
		t.Fatalf("delete missing person: want ErrNoPerson, got %v", err)
	}
	// renaming a missing person to an existing name is a merge from nobody
	ada, _ := st.EnsurePerson("Ada")
	if err := st.RenamePerson(999, "Ada"); !errors.Is(err, ErrNoPerson) {
		t.Fatalf("rename missing person onto existing name: want ErrNoPerson, got %v", err)
	}
	if err := st.MergePeople(999, ada); !errors.Is(err, ErrNoPerson) {
		t.Fatalf("merge into missing person: want ErrNoPerson, got %v", err)
	}
	if p, _ := st.Person(ada); p == nil {
		t.Fatal("merge into a missing person deleted the source")
	}
	// a re-index without a CLIP embedding must drop the old one
	r := &Result{Photo: Photo{Path: "/c.jpg", Size: 1, MTime: 1, Fingerprint: "c", CLIP: []byte{1, 2}, Version: SchemaVersion}}
	if err := st.Save([]*Result{r}); err != nil {
		t.Fatal(err)
	}
	r2 := &Result{Photo: Photo{Path: "/c.jpg", Size: 2, MTime: 2, Fingerprint: "c2", Version: SchemaVersion}}
	if err := st.Save([]*Result{r2}); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.CLIP(r2.Photo.ID); b != nil {
		t.Fatalf("stale CLIP embedding survived re-index: %v", b)
	}
	// and re-indexing with a new embedding replaces it
	r3 := &Result{Photo: Photo{Path: "/c.jpg", Size: 3, MTime: 3, Fingerprint: "c3", CLIP: []byte{9, 9}, Version: SchemaVersion}}
	if err := st.Save([]*Result{r3}); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.CLIP(r3.Photo.ID); len(b) != 2 || b[0] != 9 {
		t.Fatalf("new CLIP embedding not stored: %v", b)
	}
}

// An offline drive leaves its mount point missing or empty; prune must not
// read that as "every photo was deleted".
func TestPruneHoldsBackOfflineFolders(t *testing.T) {
	root := t.TempDir()
	st, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	online := filepath.Join(root, "online")
	mount := filepath.Join(root, "drive") // empty mount point
	os.MkdirAll(online, 0o755)
	os.MkdirAll(mount, 0o755)
	os.WriteFile(filepath.Join(online, "keep.jpg"), []byte("x"), 0o644)
	var rs []*Result
	for i, p := range []string{
		filepath.Join(online, "keep.jpg"),
		filepath.Join(online, "deleted.jpg"),           // really deleted
		filepath.Join(mount, "a.jpg"),                  // drive offline
		filepath.Join(mount, "2019", "b.jpg"),          // drive offline, nested
		filepath.Join(root, "removed-folder", "c.jpg"), // folder deleted
	} {
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	res, err := st.Prune([]string{root}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gone) != 1 || res.Gone[0] != filepath.Join(online, "deleted.jpg") {
		t.Fatalf("pruned %v, want only online/deleted.jpg", res.Gone)
	}
	if len(res.Held) != 3 || st.Count(false) != 4 {
		t.Fatalf("held %v, %d photos left", res.Held, st.Count(false))
	}
	// the mount point itself missing: refuse outright
	if _, err := st.Prune([]string{filepath.Join(root, "nope")}, false, false); err == nil {
		t.Fatal("pruned a root that does not exist")
	}
	// the user confirms the folders really are gone
	res, err = st.Prune([]string{root}, false, true)
	if err != nil || len(res.Gone) != 3 || st.Count(false) != 1 {
		t.Fatalf("--all-missing: %v gone %v, %d left", err, res.Gone, st.Count(false))
	}
}

// A person detected twice in one photo (a mirror, a poster) is still in one
// photo: photo counts must be distinct photos, not labelled faces.
func TestPersonCountsPhotosNotFaces(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	face := func(x float64) Face {
		return Face{X1: x, Y1: .1, X2: x + .1, Y2: .3, Score: .9, Embedding: []byte{1, 2}}
	}
	a := &Result{Photo: Photo{Path: "/a.jpg", Size: 1, MTime: 1, Fingerprint: "a", Version: SchemaVersion, FaceCount: 2}, Faces: []Face{face(.1), face(.5)}}
	b := &Result{Photo: Photo{Path: "/b.jpg", Size: 1, MTime: 1, Fingerprint: "b", Version: SchemaVersion, FaceCount: 1}, Faces: []Face{face(.1)}}
	if err := st.Save([]*Result{a, b}); err != nil {
		t.Fatal(err)
	}
	id, _ := st.EnsurePerson("Ada")
	st.AssignFaces(id, []int64{a.Faces[0].ID, a.Faces[1].ID}, "manual", nil) // both faces in photo a
	st.AssignFaces(id, []int64{b.Faces[0].ID}, "auto", nil)
	ps, err := st.People()
	if err != nil || len(ps) != 1 {
		t.Fatal(err, ps)
	}
	p, _ := st.Person(id)
	for _, got := range []Person{ps[0], *p} {
		if got.Photos != 2 || got.ManualPhotos != 1 || got.Faces != 3 || got.Manual != 2 {
			t.Fatalf("photos %d (confirmed %d), faces %d (confirmed %d); want 2 (1), 3 (2)",
				got.Photos, got.ManualPhotos, got.Faces, got.Manual)
		}
	}
}

// Labelling with a face id that doesn't exist changes nothing: no empty
// person, no partial labels.
func TestLabelFacesIsAllOrNothing(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Result{Photo: Photo{Path: "/a.jpg", Size: 1, MTime: 1, Fingerprint: "a", Version: SchemaVersion, FaceCount: 1},
		Faces: []Face{{X1: .1, Y1: .1, X2: .3, Y2: .4, Score: .9, Embedding: []byte{1, 2}}}}
	st.Save([]*Result{r})
	fid := r.Faces[0].ID
	if _, err := st.LabelFaces("Newcomer", []int64{fid, 99999}); !errors.Is(err, ErrNoFace) {
		t.Fatalf("want ErrNoFace, got %v", err)
	}
	if p, _ := st.PersonByName("Newcomer"); p != nil {
		t.Fatal("an empty person was created")
	}
	if fs, _ := st.FacesForPhoto(r.Photo.ID); fs[0].PersonID.Valid {
		t.Fatal("the valid face was labelled although the request failed")
	}
	pid, err := st.LabelFaces("Newcomer", []int64{fid, fid})
	if err != nil {
		t.Fatal(err)
	}
	if fs, _ := st.FacesForPhoto(r.Photo.ID); fs[0].PersonID.Int64 != pid || fs[0].PersonSource.String != "manual" {
		t.Fatalf("face not labelled: %+v", fs[0])
	}
	if again, _ := st.LabelFaces("newcomer", []int64{fid}); again != pid {
		t.Fatal("names should match case-insensitively, as everywhere else")
	}
	if _, err := st.LabelFaces("  ", []int64{fid}); err == nil {
		t.Fatal("empty name accepted")
	}
}

// Prune must find photos under folders with non-ASCII names, including a
// decomposed (NFD, as macOS may store it) spelling of the same name.
func TestPruneNonASCIIFolders(t *testing.T) {
	root := t.TempDir()
	st, _ := Open(filepath.Join(t.TempDir(), "p.db"))
	defer st.Close()
	nfc := filepath.Join(root, "Ålesund")  // Å composed
	nfd := filepath.Join(root, "Ålesund") // Å decomposed
	os.MkdirAll(nfc, 0o755)
	os.WriteFile(filepath.Join(nfc, "keep.jpg"), []byte("x"), 0o644)
	var rs []*Result
	for i, p := range []string{filepath.Join(nfc, "keep.jpg"), filepath.Join(nfc, "gone.jpg"), filepath.Join(nfd, "keep2.jpg")} {
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: fmt.Sprint(i), Version: SchemaVersion}})
	}
	st.Save(rs)
	res, err := st.Prune([]string{nfc}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nfd); err == nil {
		// macOS (APFS, HFS+): both spellings name the same, existing folder,
		// so keep2.jpg is genuinely missing from a folder that is there.
		if len(res.Gone) != 2 || len(res.Held) != 0 {
			t.Fatalf("normalisation-insensitive filesystem: gone %v, held %v", res.Gone, res.Held)
		}
		return
	}
	if len(res.Gone) != 1 || res.Gone[0] != filepath.Join(nfc, "gone.jpg") {
		t.Fatalf("gone %v", res.Gone)
	}
	if len(res.Held) != 1 { // the NFD folder doesn't exist on this filesystem: held as offline
		t.Fatalf("held %v", res.Held)
	}
}

// Re-indexing unchanged content keeps every aesthetic score (those of
// inactive and legacy models cannot be recomputed); changed content drops
// them.
func TestReindexKeepsScoresOfUnchangedContent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	save := func(fp string, scores map[string]float64) int64 {
		r := &Result{Photo: Photo{Path: "/x.jpg", Size: 1, MTime: 1, Fingerprint: fp, Version: SchemaVersion}, AestheticScores: scores}
		if err := st.Save([]*Result{r}); err != nil {
			t.Fatal(err)
		}
		return r.Photo.ID
	}
	id := save("f", map[string]float64{"laion-legacy": 9, "m-1": 5})
	save("f", map[string]float64{"m-1": 6}) // --force: same content
	if sc, _ := st.AestheticScores(id); len(sc) != 2 || sc["laion-legacy"] != 9 || sc["m-1"] != 6 {
		t.Fatalf("same content: %v", sc)
	}
	save("g", map[string]float64{"m-1": 4}) // the file was edited
	if sc, _ := st.AestheticScores(id); len(sc) != 1 || sc["m-1"] != 4 {
		t.Fatalf("changed content: %v", sc)
	}
}

// Writes computed from a photo that has since been re-saved are skipped.
func TestStaleScoreWritesSkipped(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := &Result{Photo: Photo{Path: "/x.jpg", Size: 1, MTime: 1, Fingerprint: "f", Version: SchemaVersion, Overall: 10}}
	st.Save([]*Result{r})
	var seq int64
	st.DB.QueryRow(`SELECT seq FROM photos WHERE id = ?`, r.Photo.ID).Scan(&seq)
	st.Save([]*Result{r}) // re-saved: seq moves on
	stale := sql.NullFloat64{Float64: 1, Valid: true}
	st.SetBlendedScores([]BlendUpdate{{ID: r.Photo.ID, Seq: seq, Aesthetic: stale, Overall: 99}})
	st.AddFreshAestheticScores("m-1", []SeqScore{{ID: r.Photo.ID, Seq: seq, Score: 1}})
	p, _ := st.Photo(r.Photo.ID)
	sc, _ := st.AestheticScores(r.Photo.ID)
	if p.Overall != 10 || p.Aesthetic.Valid || len(sc) != 0 {
		t.Fatalf("stale writes applied: overall %.0f, aesthetic %v, scores %v", p.Overall, p.Aesthetic, sc)
	}
	st.DB.QueryRow(`SELECT seq FROM photos WHERE id = ?`, r.Photo.ID).Scan(&seq)
	st.SetBlendedScores([]BlendUpdate{{ID: r.Photo.ID, Seq: seq, Aesthetic: stale, Overall: 50}})
	st.AddFreshAestheticScores("m-1", []SeqScore{{ID: r.Photo.ID, Seq: seq, Score: 1}})
	p, _ = st.Photo(r.Photo.ID)
	sc, _ = st.AestheticScores(r.Photo.ID)
	if p.Overall != 50 || sc["m-1"] != 1 {
		t.Fatalf("current writes not applied: overall %.0f, scores %v", p.Overall, sc)
	}
}

func TestBlendWeightsMustBeFinite(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.AddAestheticModel(AestheticModel{ID: "m-1", Name: "m", Kind: "user", Model: []byte("{}")})
	for _, w := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if err := st.SetAestheticWeights(map[string]float64{"m-1": w}); err == nil {
			t.Errorf("weight %v accepted", w)
		}
	}
}

// Camera, lens and path filters match their text literally.
func TestTextFiltersAreLiteral(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for _, p := range []string{"/a/IMG_1.jpg", "/a/IMGX1.jpg", "/a/50%.jpg", "/a/500.jpg", `/a/b\c.jpg`} {
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Version: SchemaVersion, Lens: "EF 24-70mm f/2.8L_II"}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]int{"IMG_1": 1, "50%": 1, `b\c`: 1, "/a/": 5} {
		if n, _ := st.FilterCount(Filter{PathContains: q}); n != want {
			t.Errorf("path contains %q: %d photos, want %d", q, n, want)
		}
	}
	if n, _ := st.FilterCount(Filter{Lens: "L_II"}); n != 5 {
		t.Errorf("lens filter: %d", n)
	}
	if n, _ := st.FilterCount(Filter{Lens: "L%II"}); n != 0 {
		t.Errorf("lens wildcard matched %d", n)
	}
}
