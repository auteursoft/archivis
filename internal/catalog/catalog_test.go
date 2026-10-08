package catalog

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"archivis/internal/indexer"
	"archivis/internal/ml"
	"archivis/internal/store"
	"archivis/internal/vindex"
)

func unit(i int) []float32 {
	v := make([]float32, 512)
	v[i] = 1
	return v
}

func photo(path string, clip, face int) *store.Result {
	r := &store.Result{Photo: store.Photo{Path: path, Size: 1, MTime: time.Now().UnixNano(), Fingerprint: path + time.Now().String(),
		Version: store.SchemaVersion, IndexedAt: time.Now().Unix(), CLIP: vindex.EncodeF16(unit(clip))}}
	if face >= 0 {
		r.Photo.FaceCount = 1
		r.Faces = []store.Face{{X1: .1, Y1: .1, X2: .3, Y2: .4, Score: .9, Embedding: vindex.EncodeF16(unit(face))}}
	}
	return r
}

func near(t *testing.T, ix *vindex.Index, id int64, want int) {
	t.Helper()
	v, ok := ix.Vector(id)
	if !ok {
		t.Fatalf("id %d not in index", id)
	}
	if math.Abs(float64(v[want])-1) > 0.02 {
		t.Fatalf("id %d holds a stale vector (component %d = %.2f)", id, want, v[want])
	}
}

func open(t *testing.T) *Catalog {
	t.Helper()
	c, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// Re-indexing a photo deletes and reinserts its faces; SQLite reuses the
// highest freed face id, so the refresh must not keep the old vector.
func TestRefreshSeesReplacedFaces(t *testing.T) {
	c := open(t)
	a, b := photo("/a.jpg", 1, 10), photo("/b.jpg", 2, 20)
	if err := c.Store.Save([]*store.Result{a, b}); err != nil {
		t.Fatal(err)
	}
	c.Refresh()
	fid := b.Faces[0].ID
	near(t, c.FaceIndex(), fid, 20)
	b2 := photo("/b.jpg", 2, 30)
	if err := c.Store.Save([]*store.Result{b2}); err != nil {
		t.Fatal(err)
	}
	if b2.Faces[0].ID != fid {
		t.Skipf("face id not reused (%d -> %d); nothing to test", fid, b2.Faces[0].ID)
	}
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	near(t, c.FaceIndex(), fid, 30)
	if c.PhotoOfFace(fid) != b2.Photo.ID {
		t.Fatal("face→photo map is stale")
	}
	// a re-index that finds fewer faces must drop the old ones
	b3 := photo("/b.jpg", 2, -1)
	c.Store.Save([]*store.Result{b3})
	c.Refresh()
	if c.FaceIndex().Has(fid) {
		t.Fatal("face removed by re-index is still searchable")
	}
}

// Deleting the highest photo and indexing a new one reuses its id.
func TestRefreshSeesReusedPhotoID(t *testing.T) {
	c := open(t)
	a, b := photo("/a.jpg", 1, -1), photo("/b.jpg", 2, -1)
	c.Store.Save([]*store.Result{a, b})
	c.Refresh()
	if _, err := c.Store.DB.Exec(`DELETE FROM photos WHERE id = ?`, b.Photo.ID); err != nil {
		t.Fatal(err)
	}
	c.Store.DB.Exec(`DELETE FROM photo_clip WHERE photo_id = ?`, b.Photo.ID)
	n := photo("/new.jpg", 3, -1)
	c.Store.Save([]*store.Result{n})
	if n.Photo.ID != b.Photo.ID {
		t.Skipf("photo id not reused")
	}
	c.Refresh()
	near(t, c.PhotoIndex(), n.Photo.ID, 3)
}

// Searches running while a refresh rebuilds must never see a half-built
// index (run with -race to check the pointer swap too).
func TestSearchDuringRebuild(t *testing.T) {
	c := open(t)
	var rs []*store.Result
	for i := 0; i < 50; i++ {
		rs = append(rs, photo("/p"+string(rune('A'+i))+".jpg", i, 100+i))
	}
	c.Store.Save(rs)
	c.Refresh()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var short int
	var mu sync.Mutex
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				hits := c.PhotoIndex().Search([][]float32{unit(5)}, vindex.SearchOpts{K: 100})
				if len(hits) < 49 {
					mu.Lock()
					short++
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < 30; i++ {
		// delete one photo, forcing a full rebuild, then put it back
		victim := rs[i%50]
		c.Store.DB.Exec(`DELETE FROM photo_clip WHERE photo_id = ?`, victim.Photo.ID)
		c.Refresh()
		c.Store.Save([]*store.Result{photo(victim.Photo.Path, i%50, 100+i%50)})
		c.Refresh()
	}
	close(stop)
	wg.Wait()
	if short > 0 {
		t.Fatalf("%d searches saw a partially built index", short)
	}
}

// Deletions (e.g. prune) are removed from the live index without a rebuild.
func TestRefreshDropsDeletedInPlace(t *testing.T) {
	c := open(t)
	a, b := photo("/a.jpg", 1, 10), photo("/b.jpg", 2, 20)
	c.Store.Save([]*store.Result{a, b})
	c.Refresh()
	before := c.PhotoIndex()
	for _, q := range []string{`DELETE FROM faces WHERE photo_id = ?`, `DELETE FROM photo_clip WHERE photo_id = ?`, `DELETE FROM photos WHERE id = ?`} {
		c.Store.DB.Exec(q, a.Photo.ID)
	}
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	if c.PhotoIndex() != before {
		t.Fatal("a deletion triggered a full rebuild")
	}
	if c.PhotoIndex().Has(a.Photo.ID) || c.FaceIndex().Has(a.Faces[0].ID) || c.PhotoOfFace(a.Faces[0].ID) != 0 {
		t.Fatal("deleted photo or face still indexed")
	}
	near(t, c.PhotoIndex(), b.Photo.ID, 2)
	near(t, c.FaceIndex(), b.Faces[0].ID, 20)
}

// legacyCatalogue writes a catalogue as versions before the aesthetic model
// registry left it: one photo with an aesthetic score in photos.aesthetic,
// and the given meta aesthetic_model ("" for none).
func legacyCatalogue(t *testing.T, version string, aesthetic float64) (string, int64) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(indexer.Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := photo("/a.jpg", 7, -1)
	r.Photo.Technical = 60
	r.Photo.Aesthetic = sql.NullFloat64{Float64: aesthetic, Valid: true}
	r.Photo.Overall = indexer.Overall(60, r.Photo.Aesthetic)
	if err := st.Save([]*store.Result{r}); err != nil {
		t.Fatal(err)
	}
	if version != "" {
		st.SetMeta("aesthetic_model", version)
	}
	return dir, r.Photo.ID
}

// Scores from the current built-in model are attributed to it on upgrade,
// not recomputed.
func TestOpenAdoptsEvaScores(t *testing.T) {
	dir, id := legacyCatalogue(t, "eva-ridge-1", 6.25)
	c, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, _ := c.Store.Photo(id)
	if p.Aesthetic.Float64 != 6.25 {
		t.Fatalf("aesthetic %.3f: an existing score was recomputed", p.Aesthetic.Float64)
	}
	sc, _ := c.Store.AestheticScores(id)
	if len(sc) != 1 || sc[ml.BuiltinAestheticID] != 6.25 {
		t.Fatalf("scores %v, want 6.25 by %s", sc, ml.BuiltinAestheticID)
	}
	if len(c.Eng.Aesthetics) != 1 || c.Eng.Aesthetics[0].ID != ml.BuiltinAestheticID {
		t.Fatalf("active models %v", c.Eng.Aesthetics)
	}
}

// Scores from the LAION predictor are kept under laion-legacy and the
// built-in model scores the photo from its stored embedding.
func TestOpenKeepsLAIONScores(t *testing.T) {
	dir, id := legacyCatalogue(t, "", 9.9)
	c, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p, _ := c.Store.Photo(id)
	want := float64(ml.DefaultAesthetic().Score(unit(7)))
	if math.Abs(p.Aesthetic.Float64-want) > 1e-3 {
		t.Fatalf("aesthetic %.3f, want %.3f from the built-in model", p.Aesthetic.Float64, want)
	}
	if p.Overall != indexer.Overall(60, p.Aesthetic) {
		t.Fatalf("overall %.1f not updated", p.Overall)
	}
	sc, _ := c.Store.AestheticScores(id)
	if sc[indexer.LegacyLAION] != 9.9 || len(sc) != 2 {
		t.Fatalf("scores %v: the LAION score was not kept", sc)
	}
	ms, _ := c.Store.AestheticModels()
	for _, m := range ms {
		if m.ID == indexer.LegacyLAION && m.Active {
			t.Fatal("legacy LAION scores are blended")
		}
	}
}

// A model added to the blend scores only the photos it has not scored; the
// blend and overall follow the weights, and the next open changes nothing.
// A new built-in model takes over the old one's weight; user models keep
// theirs, and every earlier score is kept.
func TestAestheticModelsEvolve(t *testing.T) {
	dir, id := legacyCatalogue(t, "eva-ridge-1", 6.25)
	c, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := ml.DefaultAesthetic()
	w := make([]float32, len(b.W))
	user, _ := ml.NewAesthetic("user", w, 8) // rates everything 8
	if _, err := c.Store.AddAestheticModel(store.AestheticModel{ID: user.ID, Name: user.Name, Kind: "user", Model: user.JSON()}); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.SetAestheticWeights(map[string]float64{b.ID: 1, user.ID: 3}); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if c, err = Open(dir, nil); err != nil {
		t.Fatal(err)
	}
	p, _ := c.Store.Photo(id)
	if want := (6.25 + 3*8) / 4; math.Abs(p.Aesthetic.Float64-want) > 1e-6 || p.Overall != indexer.Overall(60, p.Aesthetic) {
		t.Fatalf("aesthetic %.4f overall %.1f, want blend %.4f", p.Aesthetic.Float64, p.Overall, want)
	}
	if sc, _ := c.Store.AestheticScores(id); sc[b.ID] != 6.25 || sc[user.ID] != 8 {
		t.Fatalf("scores %v", sc)
	}
	if n, _ := indexer.ScoreUnscored(c.Store, user); n != 0 {
		t.Fatalf("scored %d photos twice", n)
	}
	if n, _ := indexer.RefreshBlend(c.Store, c.Eng.Aesthetics); n != 0 {
		t.Fatalf("blend changed %d photos with nothing new", n)
	}

	w2 := append([]float32(nil), b.W...)
	nb, _ := ml.NewAesthetic(b.Name, w2, b.B+1) // a refit built-in model
	active, err := indexer.SyncAesthetics(c.Store, nb)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, m := range active {
		got[m.ID] = m.Weight
	}
	if len(got) != 2 || got[nb.ID] != 1 || got[user.ID] != 3 {
		t.Fatalf("active after a built-in update: %v", got)
	}
	sc, _ := c.Store.AestheticScores(id)
	if len(sc) != 3 || sc[b.ID] != 6.25 || math.Abs(sc[nb.ID]-float64(nb.Score(unit(7)))) > 1e-4 {
		t.Fatalf("scores after a built-in update %v", sc)
	}
	c.Close()
}

// A selective filter must still return k results when every matching photo
// ranks below the globally most similar ones.
func TestFilteredSearchFindsLowRankedMatches(t *testing.T) {
	c := open(t)
	var rs []*store.Result
	for i := 0; i < 100; i++ { // very similar to the query, but untagged
		rs = append(rs, photo(fmt.Sprintf("/near%03d.jpg", i), 0, -1))
	}
	for i := 0; i < 20; i++ { // dissimilar, tagged
		r := photo(fmt.Sprintf("/rare%02d.jpg", i), 1+i%5, -1)
		r.Tags = []store.Tag{{Tag: "rare", Score: 0.9}}
		rs = append(rs, r)
	}
	if err := c.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	c.Refresh()
	check := func(how string) {
		t.Helper()
		hits, err := c.SearchVector(unit(0), store.Filter{Tag: "rare"}, 5)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 5 {
			t.Fatalf("%s: %d results, want 5", how, len(hits))
		}
		for _, h := range hits {
			if !strings.HasPrefix(h.Path, "/rare") {
				t.Fatalf("%s: %s does not match the filter", how, h.Path)
			}
		}
	}
	check("small filtered set searched directly")
	old := eligibleMax
	eligibleMax = 5 // force the widening path
	defer func() { eligibleMax = old }()
	check("candidate list widened")
	// unfiltered search is unchanged: the best k overall
	hits, _ := c.SearchVector(unit(0), store.Filter{}, 5)
	if len(hits) != 5 || !strings.HasPrefix(hits[0].Path, "/near") {
		t.Fatalf("unfiltered: %v", hits)
	}
}

// Face search returns photos (one per photo, best face), honours filters,
// and fills the requested number when enough matching photos exist.
func TestSearchFacePhotosFiltersAndFills(t *testing.T) {
	c := open(t)
	y2018, y2019 := time.Date(2018, 6, 1, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC).Unix()
	var rs []*store.Result
	for i := 0; i < 30; i++ {
		r := photo(fmt.Sprintf("/p%02d.jpg", i), i%7, 10) // same person in every photo
		r.Photo.TakenAt = y2018
		if i >= 25 {
			r.Photo.TakenAt = y2019
		}
		if i == 0 { // seen three times in one photo
			f := r.Faces[0]
			for j := 1; j < 3; j++ {
				g := f
				g.X1, g.X2 = f.X1+0.3*float64(j), f.X2+0.3*float64(j)
				r.Faces = append(r.Faces, g)
			}
			r.Photo.FaceCount = 3
		}
		rs = append(rs, r)
	}
	if err := c.Store.Save(rs); err != nil {
		t.Fatal(err)
	}
	c.Refresh()
	distinct := func(hs []PhotoHit) bool {
		seen := map[int64]bool{}
		for _, h := range hs {
			if seen[h.ID] {
				return false
			}
			seen[h.ID] = true
		}
		return true
	}
	hits, err := c.SearchFacePhotos(unit(10), store.Filter{}, 10, 0.4)
	if err != nil || len(hits) != 10 || !distinct(hits) {
		t.Fatalf("unfiltered: %d hits, distinct %v, err %v", len(hits), distinct(hits), err)
	}
	check := func(how string) {
		t.Helper()
		hits, err := c.SearchFacePhotos(unit(10), store.Filter{Year: 2019}, 5, 0.4)
		if err != nil || len(hits) != 5 || !distinct(hits) {
			t.Fatalf("%s: %d hits, err %v", how, len(hits), err)
		}
		for _, h := range hits {
			if h.TakenAt != y2019 {
				t.Fatalf("%s: %s is not from 2019", how, h.Path)
			}
		}
	}
	check("filtered set searched directly")
	old := eligibleMax
	eligibleMax = 1
	defer func() { eligibleMax = old }()
	check("candidate list widened")
	if hits, _ := c.SearchFacePhotos(unit(10), store.Filter{Year: 2001}, 5, 0.4); len(hits) != 0 {
		t.Fatalf("no photos from 2001, got %d", len(hits))
	}
}

// A re-analysis that started long before it was committed (a slow RAW, a
// busy database) must still reach the running server's index.
func TestRefreshSeesSlowCommits(t *testing.T) {
	c := open(t)
	a := photo("/a.jpg", 1, 10)
	c.Store.Save([]*store.Result{a})
	c.Refresh()
	time.Sleep(1100 * time.Millisecond)
	c.Refresh() // the server refreshes; meanwhile a.jpg is being re-analysed
	slow := photo("/a.jpg", 2, 20)
	slow.Photo.IndexedAt = time.Now().Add(-time.Hour).Unix() // analysis began an hour ago
	if err := c.Store.Save([]*store.Result{slow}); err != nil {
		t.Fatal(err)
	}
	if err := c.Refresh(); err != nil {
		t.Fatal(err)
	}
	near(t, c.PhotoIndex(), a.Photo.ID, 2)
	near(t, c.FaceIndex(), slow.Faces[0].ID, 20)
}

func TestSortHits(t *testing.T) {
	mk := func(path string, taken int64, overall float64) PhotoHit {
		return PhotoHit{Photo: store.Photo{Path: path, TakenAt: taken, Overall: overall}}
	}
	base := []PhotoHit{mk("/b", 200, 50), mk("/a", 0, 90), mk("/c", 100, 70)} // relevance order
	order := func(by string) string {
		h := append([]PhotoHit(nil), base...)
		SortHits(h, by)
		var s string
		for _, x := range h {
			s += x.Path
		}
		return s
	}
	for by, want := range map[string]string{
		"": "/b/a/c", "relevance": "/b/a/c", // similarity ranking kept
		"newest": "/b/c/a", "oldest": "/c/b/a", // undated last
		"overall": "/a/c/b", "worst": "/b/c/a", "path": "/a/b/c",
	} {
		if got := order(by); got != want {
			t.Errorf("%q: %s, want %s", by, got, want)
		}
	}
}

func TestAutoMatchRejectsBadSettings(t *testing.T) {
	c := open(t)
	nan := float32(math.NaN())
	for _, o := range []MatchOptions{{Threshold: nan, Margin: 0.08}, {Threshold: 0.45, Margin: nan}, {Threshold: 2, Margin: 0.08}, {Threshold: 0.45, Margin: -1}} {
		if _, err := c.AutoMatch(o); err == nil {
			t.Errorf("%+v accepted", o)
		}
	}
	if _, err := c.AutoMatch(DefaultMatch); err != nil {
		t.Fatal(err)
	}
}

// A trained model that is better joins the blend: every photo gets its
// score and the blended scores follow; models that cannot score are refused.
func TestSaveTrainedActivates(t *testing.T) {
	dir, id := legacyCatalogue(t, "eva-ridge-1", 6.25)
	c, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := ml.DefaultAesthetic()
	user, _ := ml.NewAesthetic("user", b.W, b.B-2) // rates everything 2 lower
	res := &indexer.TrainResult{Model: user, Parent: indexer.DescribeBlend(c.Eng.Aesthetics), Samples: 30,
		Weights: map[string]float64{user.ID: 1}}
	if err := indexer.SaveTrained(c.Store, res, true); err != nil {
		t.Fatal(err)
	}
	p, _ := c.Store.Photo(id)
	want := float64(user.Score(unit(7)))
	if math.Abs(p.Aesthetic.Float64-want) > 1e-4 || p.Overall != indexer.Overall(60, p.Aesthetic) {
		t.Fatalf("aesthetic %.3f overall %.1f, want %.3f from the new model", p.Aesthetic.Float64, p.Overall, want)
	}
	if sc, _ := c.Store.AestheticScores(id); sc[b.ID] != 6.25 {
		t.Fatalf("the earlier score was not kept: %v", sc)
	}
	if err := indexer.Activate(c.Store, map[string]float64{indexer.LegacyLAION: 1}); err == nil {
		t.Fatal("activated a model without weights")
	}
	if err := indexer.Activate(c.Store, nil); err == nil {
		t.Fatal("deactivated every model")
	}
}
