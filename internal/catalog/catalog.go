// Package catalog ties the store, the in-memory vector indexes and the models
// together for searching, browsing and labelling people.
package catalog

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/auteursoft/archivis/internal/imageio"
	"github.com/auteursoft/archivis/internal/indexer"
	"github.com/auteursoft/archivis/internal/ml"
	"github.com/auteursoft/archivis/internal/store"
	"github.com/auteursoft/archivis/internal/vindex"
)

// Catalog is the searchable photo collection.
type Catalog struct {
	Store  *store.Store
	Layout indexer.Layout
	Eng    *indexer.Engines

	mu           sync.RWMutex    // guards the fields below
	photos       *vindex.Index   // CLIP embeddings by photo id
	faces        *vindex.Index   // ArcFace embeddings by face id
	facePhoto    map[int64]int64 // face id -> photo id
	maxFacePhoto int64           // highest photo id in facePhoto

	// owned by Refresh (under refreshMu)
	refreshMu   sync.Mutex
	lastPhotoID int64
	lastFaceID  int64
	lastSeq     int64 // store change number covered by the previous refresh
	loaded      bool  // the first (full) load has happened
}

// Open opens the catalogue in dataDir and loads the vector indexes.
func Open(dataDir string, eng *indexer.Engines) (*Catalog, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	l := indexer.Layout{Dir: dataDir}
	st, err := store.Open(l.DB())
	if err != nil {
		return nil, err
	}
	if eng == nil {
		eng = &indexer.Engines{}
	}
	// Register aesthetic models and score photos that the active ones have
	// not rated yet, from their stored CLIP embeddings (no re-indexing).
	if eng.Aesthetics, err = indexer.SyncAesthetics(st, ml.DefaultAesthetic()); err != nil {
		st.Close()
		return nil, fmt.Errorf("aesthetic models: %w", err)
	}
	c := &Catalog{Store: st, Layout: l, Eng: eng,
		photos: vindex.New(ml.EmbedDim), faces: vindex.New(ml.FaceEmbedDim), facePhoto: map[int64]int64{}}
	if err := c.Refresh(); err != nil {
		st.Close()
		return nil, err
	}
	return c, nil
}

func (c *Catalog) Close() error { return c.Store.Close() }

// PhotoIndex is the current CLIP index. A rebuild swaps in a new index, so
// fetch it per operation rather than keeping it.
func (c *Catalog) PhotoIndex() *vindex.Index {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.photos
}

// FaceIndex is the current face index (see PhotoIndex).
func (c *Catalog) FaceIndex() *vindex.Index {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.faces
}

// Refresh brings the indexes up to date: it loads new embeddings, reloads
// photos re-analysed in place (they keep their id, while their faces are
// reinserted and may reuse freed ids), and rebuilds if rows were deleted.
// A rebuild happens off to the side; searches keep using the old index until
// the new one is complete.
func (c *Catalog) Refresh() error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	photos, faces := c.PhotoIndex(), c.FaceIndex()
	// Read the change number first: anything saved while we load gets a
	// higher one and is picked up next time (reloading twice is harmless).
	top := c.Store.MaxSeq()
	if c.loaded {
		since := c.lastSeq
		if err := c.Store.EachChangedEmbedding(since, func(id int64, b []byte) {
			photos.Add(id, vindex.DecodeF16(b))
		}); err != nil {
			return err
		}
		if err := c.reloadChangedFaces(since, faces); err != nil {
			return err
		}
	}
	newFaces := map[int64]int64{}
	lastP, lastF, err := c.loadAfter(photos, faces, newFaces, c.lastPhotoID, c.lastFaceID)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.addFacePhotos(newFaces)
	c.mu.Unlock()
	c.lastPhotoID, c.lastFaceID = lastP, lastF
	consistent := func() bool {
		return int64(photos.Len()) == c.countWithClip() && int64(faces.Len()) == c.Store.Count(true)
	}
	if !consistent() {
		// rows were deleted (prune, person-less re-index): drop them in place
		if err := c.dropDeleted(photos, faces); err != nil {
			return err
		}
	}
	if !consistent() {
		// still out of step: rebuild into fresh indexes, then swap
		np, nf, nm := vindex.New(ml.EmbedDim), vindex.New(ml.FaceEmbedDim), map[int64]int64{}
		lastP, lastF, err := c.loadAfter(np, nf, nm, 0, 0)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.photos, c.faces, c.facePhoto, c.maxFacePhoto = np, nf, map[int64]int64{}, 0
		c.addFacePhotos(nm)
		c.mu.Unlock()
		c.lastPhotoID, c.lastFaceID = lastP, lastF
	}
	c.lastSeq, c.loaded = top, true
	return nil
}

// loadAfter adds embeddings with ids above the given ones.
func (c *Catalog) loadAfter(photos, faces *vindex.Index, facePhoto map[int64]int64, afterP, afterF int64) (int64, int64, error) {
	lastP, err := c.Store.EachEmbedding(false, afterP, func(id int64, b []byte) {
		photos.Add(id, vindex.DecodeF16(b))
	})
	if err != nil {
		return afterP, afterF, err
	}
	lastF, err := c.Store.EachEmbedding(true, afterF, func(id int64, b []byte) {
		faces.Add(id, vindex.DecodeF16(b))
	})
	if err != nil {
		return lastP, afterF, err
	}
	return lastP, lastF, c.Store.FacePhotoMap(afterF, facePhoto)
}

// dropDeleted removes vectors whose rows no longer exist.
func (c *Catalog) dropDeleted(photos, faces *vindex.Index) error {
	live := func(q string) (map[int64]struct{}, error) {
		rows, err := c.Store.DB.Query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[int64]struct{}{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			m[id] = struct{}{}
		}
		return m, rows.Err()
	}
	gone := func(ix *vindex.Index, alive map[int64]struct{}) []int64 {
		var out []int64
		var mu sync.Mutex // ForEach calls fn from several goroutines
		ix.ForEach(func(id int64, _ []int8, _ float32) {
			if _, ok := alive[id]; !ok {
				mu.Lock()
				out = append(out, id)
				mu.Unlock()
			}
		})
		for _, id := range out {
			ix.Remove(id)
		}
		return out
	}
	alive, err := live(`SELECT photo_id FROM photo_clip`)
	if err != nil {
		return err
	}
	gone(photos, alive)
	if alive, err = live(`SELECT id FROM faces`); err != nil {
		return err
	}
	deadFaces := gone(faces, alive)
	c.mu.Lock()
	for _, fid := range deadFaces {
		delete(c.facePhoto, fid)
	}
	c.mu.Unlock()
	return nil
}

// addFacePhotos merges m into facePhoto; c.mu must be held.
func (c *Catalog) addFacePhotos(m map[int64]int64) {
	for f, p := range m {
		c.facePhoto[f] = p
		c.maxFacePhoto = max(c.maxFacePhoto, p)
	}
}

// reloadChangedFaces replaces the faces of photos re-indexed since then:
// re-indexing deletes and reinserts faces, and SQLite reuses freed ids.
func (c *Catalog) reloadChangedFaces(since int64, faces *vindex.Index) error {
	type face struct {
		id, photo int64
		v         []float32
	}
	changed := map[int64]bool{}
	var cur []face
	if err := c.Store.EachChangedFace(since, func(pid, fid int64, b []byte) {
		changed[pid] = true
		if fid != 0 {
			cur = append(cur, face{fid, pid, vindex.DecodeF16(b)})
		}
	}); err != nil {
		return err
	}
	// Only photos at or below the highest known one can have old faces.
	var stale []int64
	c.mu.RLock()
	scan := false
	for pid := range changed {
		if pid <= c.maxFacePhoto {
			scan = true
			break
		}
	}
	if scan {
		for fid, pid := range c.facePhoto {
			if changed[pid] {
				stale = append(stale, fid)
			}
		}
	}
	c.mu.RUnlock()
	// Index updates happen outside c.mu: a search holds the index lock while
	// its filter may take c.mu (PhotoOfFace).
	for _, fid := range stale {
		faces.Remove(fid)
	}
	m := make(map[int64]int64, len(cur))
	for _, f := range cur {
		faces.Add(f.id, f.v)
		m[f.id] = f.photo
	}
	c.mu.Lock()
	for _, fid := range stale {
		delete(c.facePhoto, fid)
	}
	c.addFacePhotos(m)
	c.mu.Unlock()
	return nil
}

func (c *Catalog) countWithClip() int64 {
	var n int64
	c.Store.DB.QueryRow(`SELECT COUNT(*) FROM photo_clip`).Scan(&n)
	return n
}

// PhotoOfFace returns the photo id a face belongs to.
func (c *Catalog) PhotoOfFace(faceID int64) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.facePhoto[faceID]
}

// PhotoHit is a ranked photo.
type PhotoHit struct {
	store.Photo
	Score float32
}

// filtered runs a vector search and applies the SQL filter to the hits.
func (c *Catalog) filtered(hits []vindex.Hit, f store.Filter, k int) ([]PhotoHit, error) {
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	ok, err := c.Store.FilterIDs(f, ids)
	if err != nil {
		return nil, err
	}
	var keep []int64
	score := map[int64]float32{}
	for _, h := range hits {
		if ok[h.ID] {
			keep = append(keep, h.ID)
			score[h.ID] = h.Score
		}
		if len(keep) >= k {
			break
		}
	}
	ps, err := c.Store.Photos(keep)
	if err != nil {
		return nil, err
	}
	out := make([]PhotoHit, len(ps))
	for i, p := range ps {
		out[i] = PhotoHit{p, score[p.ID]}
	}
	return out, nil
}

// SortHits re-orders search results by one of the browse orders (see
// store.Filter.Sort); "" or "relevance" keeps the similarity ranking. It
// is applied to the best matches, e.g. "newest among the best matches".
func SortHits(hits []PhotoHit, by string) {
	if by == "" || by == "relevance" {
		return
	}
	aes := func(p store.Photo) float64 {
		if p.Aesthetic.Valid {
			return p.Aesthetic.Float64
		}
		return -1 // unscored last, as SQL sorts NULLs in DESC order
	}
	less := map[string]func(a, b store.Photo) bool{
		"technical": func(a, b store.Photo) bool { return a.Technical > b.Technical },
		"aesthetic": func(a, b store.Photo) bool { return aes(a) > aes(b) },
		"sharpness": func(a, b store.Photo) bool {
			if a.FocusScore != b.FocusScore {
				return a.FocusScore > b.FocusScore
			}
			return a.Sharpness > b.Sharpness
		},
		"newest": func(a, b store.Photo) bool { return a.TakenAt > b.TakenAt },
		"oldest": func(a, b store.Photo) bool {
			if (a.TakenAt > 0) != (b.TakenAt > 0) {
				return a.TakenAt > 0 // undated last
			}
			return a.TakenAt < b.TakenAt
		},
		"colorful": func(a, b store.Photo) bool { return a.Colorfulness > b.Colorfulness },
		"faces": func(a, b store.Photo) bool {
			if a.FaceCount != b.FaceCount {
				return a.FaceCount > b.FaceCount
			}
			return a.Overall > b.Overall
		},
		"worst": func(a, b store.Photo) bool { return a.Overall < b.Overall },
		"path":  func(a, b store.Photo) bool { return a.Path < b.Path },
	}[by]
	if less == nil { // "overall" and anything unknown, as in SQL
		less = func(a, b store.Photo) bool { return a.Overall > b.Overall }
	}
	sort.SliceStable(hits, func(i, j int) bool { return less(hits[i].Photo, hits[j].Photo) })
}

// SearchText finds photos matching a natural-language description.
func (c *Catalog) SearchText(q string, f store.Filter, k int) ([]PhotoHit, error) {
	if c.Eng.CLIP == nil || !c.Eng.CLIP.HasText() {
		return nil, errors.New("text search needs the CLIP text model (run `archivis setup`)")
	}
	e, err := c.Eng.CLIP.EmbedText(q)
	if err != nil {
		return nil, err
	}
	return c.SearchVector(e, f, k)
}

// eligibleMax is the largest filtered set searched directly; broader
// filters widen the candidate list instead (see SearchVector).
var eligibleMax = 300_000

// SearchVector ranks photos by CLIP similarity to e, returning the best k
// that match f. A selective filter (person, year, a rare tag) can exclude
// every one of the globally most similar photos, so when few photos match,
// only those are searched; when many match, the candidate list widens until
// k pass the filter or the whole index has been considered.
func (c *Catalog) SearchVector(e []float32, f store.Filter, k int) ([]PhotoHit, error) {
	ix := c.PhotoIndex()
	if !f.Restricts() {
		return c.filtered(ix.Search([][]float32{e}, vindex.SearchOpts{K: k}), f, k)
	}
	n, err := c.Store.FilterCount(f)
	if err != nil || n == 0 {
		return nil, err
	}
	if n <= eligibleMax {
		ok, err := c.Store.FilterSet(f)
		if err != nil {
			return nil, err
		}
		hits := ix.Search([][]float32{e}, vindex.SearchOpts{K: k, Filter: func(id int64) bool { return ok[id] }})
		return c.filtered(hits, f, k)
	}
	for want := k * 8; ; want *= 8 {
		hits := ix.Search([][]float32{e}, vindex.SearchOpts{K: want})
		out, err := c.filtered(hits, f, k)
		if err != nil || len(out) >= k || len(hits) < want {
			return out, err
		}
	}
}

// SimilarPhotos finds photos that look like photo id.
func (c *Catalog) SimilarPhotos(id int64, f store.Filter, k int) ([]PhotoHit, error) {
	v, ok := c.PhotoIndex().Vector(id)
	if !ok {
		return nil, fmt.Errorf("photo %d has no embedding", id)
	}
	hits, err := c.SearchVector(v, f, k+1)
	if err != nil {
		return nil, err
	}
	out := hits[:0]
	for _, h := range hits {
		if h.ID != id {
			out = append(out, h)
		}
	}
	return out, nil
}

// FaceHit is a ranked face with its photo.
type FaceHit struct {
	store.Face
	Score float32
	Photo store.Photo
}

// SearchFaces ranks faces by similarity to any of the query embeddings.
// skip excludes face ids (e.g. the query faces themselves).
func (c *Catalog) SearchFaces(qs [][]float32, k int, minScore float32, skip func(id int64) bool) ([]FaceHit, error) {
	hits := c.FaceIndex().Search(qs, vindex.SearchOpts{K: k, MinScore: minScore, Filter: func(id int64) bool {
		return skip == nil || !skip(id)
	}})
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	faces, err := c.Store.FacesByID(ids)
	if err != nil {
		return nil, err
	}
	byID := map[int64]store.Face{}
	var pids []int64
	for _, f := range faces {
		byID[f.ID] = f
		pids = append(pids, f.PhotoID)
	}
	photos, err := c.Store.Photos(pids)
	if err != nil {
		return nil, err
	}
	pByID := map[int64]store.Photo{}
	for _, p := range photos {
		pByID[p.ID] = p
	}
	var out []FaceHit
	for _, h := range hits {
		f, ok := byID[h.ID]
		if !ok {
			continue
		}
		out = append(out, FaceHit{Face: f, Score: h.Score, Photo: pByID[f.PhotoID]})
	}
	return out, nil
}

// SearchFacePhotos finds up to k photos matching f that contain a face like
// q (similarity >= minScore), each scored by its best face, best first.
// Filters work as in SearchVector: when few photos match, only their faces
// are searched; otherwise the candidate list widens until k photos are found
// or no more faces clear minScore.
func (c *Catalog) SearchFacePhotos(q []float32, f store.Filter, k int, minScore float32) ([]PhotoHit, error) {
	var keep func(fid int64) bool
	if f.Restricts() {
		n, err := c.Store.FilterCount(f)
		if err != nil || n == 0 {
			return nil, err
		}
		if n <= eligibleMax {
			ok, err := c.Store.FilterSet(f)
			if err != nil {
				return nil, err
			}
			keep = func(fid int64) bool { return ok[c.PhotoOfFace(fid)] }
		}
	}
	ix := c.FaceIndex()
	for want := k * 4; ; want *= 8 {
		hits := ix.Search([][]float32{q}, vindex.SearchOpts{K: want, MinScore: minScore, Filter: keep})
		// best face per photo, in score order
		var order []int64
		best := map[int64]float32{}
		for _, h := range hits {
			pid := c.PhotoOfFace(h.ID)
			if _, seen := best[pid]; !seen {
				order = append(order, pid)
				best[pid] = h.Score
			}
		}
		pass := order
		if f.Restricts() && keep == nil { // broad filter: apply it to the candidates
			ok, err := c.Store.FilterIDs(f, order)
			if err != nil {
				return nil, err
			}
			pass = pass[:0:0]
			for _, pid := range order {
				if ok[pid] {
					pass = append(pass, pid)
				}
			}
		}
		if len(pass) >= k || len(hits) < want {
			if len(pass) > k {
				pass = pass[:k]
			}
			ps, err := c.Store.Photos(pass)
			if err != nil {
				return nil, err
			}
			out := make([]PhotoHit, len(ps))
			for i, p := range ps {
				out[i] = PhotoHit{p, best[p.ID]}
			}
			sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
			return out, nil
		}
	}
}

// FaceVectors returns the embeddings of the given faces.
func (c *Catalog) FaceVectors(ids []int64) [][]float32 {
	var out [][]float32
	for _, id := range ids {
		if v, ok := c.FaceIndex().Vector(id); ok {
			out = append(out, v)
		}
	}
	return out
}

// QueryResult is the analysis of an uploaded query image.
type QueryResult struct {
	Analysis *indexer.Analysis
	Thumb    []byte   // JPEG of the query image
	FaceJPG  [][]byte // JPEG crops of detected faces
}

// AnalyzeQuery decodes and analyses an uploaded image (not stored). Any
// format the indexer understands is accepted, including RAW and HEIC.
func (c *Catalog) AnalyzeQuery(data []byte) (*QueryResult, error) {
	tmp, err := os.CreateTemp("", "archivis-query-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()
	return c.AnalyzeQueryFile(tmp.Name())
}

// AnalyzeQueryFile analyses an image file for searching (not stored).
func (c *Catalog) AnalyzeQueryFile(path string) (*QueryResult, error) {
	d, err := imageio.Load(path, indexer.WorkSize)
	if err != nil {
		return nil, fmt.Errorf("cannot decode query image: %w", err)
	}
	return c.analyzeDecoded(d)
}

func (c *Catalog) analyzeDecoded(d *imageio.Decoded) (*QueryResult, error) {
	a, err := indexer.AnalyzeImage(d, c.Eng)
	if err != nil {
		return nil, err
	}
	qr := &QueryResult{Analysis: a}
	qr.Thumb, _ = imageio.EncodeJPEG(imageio.Thumbnail(d.Img, 480), 85)
	for _, f := range a.Faces {
		qr.FaceJPG = append(qr.FaceJPG, faceCropJPEG(d.Img, f.Box))
	}
	return qr, nil
}

func faceCropJPEG(img *imageio.RGB, box [4]float32) []byte {
	cx, cy := float64(box[0]+box[2])/2, float64(box[1]+box[3])/2
	side := 1.6 * float64(max(box[2]-box[0], box[3]-box[1]))
	x0, y0 := int(cx-side/2), int(cy-side/2)
	b, _ := imageio.EncodeJPEG(imageio.Thumbnail(img.Crop(x0, y0, x0+int(side), y0+int(side)), 160), 85)
	return b
}

// ---- people -------------------------------------------------------------

// MatchOptions tune automatic person labelling.
type MatchOptions struct {
	Threshold    float32 // minimum similarity to a person's exemplar
	Margin       float32 // required lead over the second-best person
	PersonID     int64   // only this person (0 = everyone)
	MaxPerPerson int     // exemplars used per person
}

// DefaultMatch are conservative settings for ArcFace (w600k_r50).
var DefaultMatch = MatchOptions{Threshold: 0.45, Margin: 0.08, MaxPerPerson: 32}

// AutoMatch labels unconfirmed faces that clearly match a named person's
// confirmed faces. It returns the number of faces labelled.
func (c *Catalog) AutoMatch(o MatchOptions) (int, error) {
	// NaN would make every similarity comparison false and silently disable
	// the threshold and ambiguity checks; out-of-range values are nonsense.
	if !(o.Threshold >= -1 && o.Threshold <= 1) || !(o.Margin >= 0 && o.Margin <= 2) {
		return 0, fmt.Errorf("matching threshold must be in [-1, 1] and margin in [0, 2] (got %v, %v)", o.Threshold, o.Margin)
	}
	if o.MaxPerPerson <= 0 {
		o.MaxPerPerson = 32
	}
	labels, err := c.Store.AllManualLabels()
	if err != nil {
		return 0, err
	}
	byPerson := map[int64][]int64{}
	for f, p := range labels {
		byPerson[p] = append(byPerson[p], f)
	}
	type ex struct {
		person int64
		q      vindex.Quantized
	}
	var exemplars []ex
	for p, fs := range byPerson {
		sort.Slice(fs, func(i, j int) bool { return fs[i] < fs[j] })
		vecs := diverse(c.FaceVectors(fs), o.MaxPerPerson)
		for _, v := range vecs {
			exemplars = append(exemplars, ex{p, vindex.Quantize(v)})
		}
	}
	if len(exemplars) == 0 {
		return 0, nil
	}
	eligible, err := c.Store.UnlabeledOrAutoFaceIDs()
	if err != nil {
		return 0, err
	}
	type match struct {
		person int64
		sim    float32
	}
	var mu sync.Mutex
	assign := map[int64][]int64{}
	sims := map[int64]float32{}
	c.FaceIndex().ForEach(func(id int64, q []int8, scale float32) {
		if !eligible[id] {
			return
		}
		best := map[int64]float32{}
		for _, e := range exemplars {
			s := vindex.DotQ(e.q, q, scale)
			if s > best[e.person] {
				best[e.person] = s
			}
		}
		var b1, b2 match
		for p, s := range best {
			if s > b1.sim {
				b2 = b1
				b1 = match{p, s}
			} else if s > b2.sim {
				b2 = match{p, s}
			}
		}
		if b1.sim < o.Threshold || b1.sim-b2.sim < o.Margin {
			return
		}
		if o.PersonID > 0 && b1.person != o.PersonID {
			return
		}
		mu.Lock()
		assign[b1.person] = append(assign[b1.person], id)
		sims[id] = b1.sim
		mu.Unlock()
	})
	// Reset previous automatic labels so they reflect the current exemplars.
	if err := c.Store.ClearAutoLabels(o.PersonID); err != nil {
		return 0, err
	}
	n := 0
	for p, ids := range assign {
		if err := c.Store.AssignFaces(p, ids, "auto", sims); err != nil {
			return n, err
		}
		n += len(ids)
	}
	return n, nil
}

// Diverse picks up to k mutually dissimilar vectors (farthest-point sampling).
func Diverse(vs [][]float32, k int) [][]float32 { return diverse(vs, k) }

func diverse(vs [][]float32, k int) [][]float32 {
	if len(vs) <= k {
		return vs
	}
	out := [][]float32{vs[0]}
	minSim := make([]float32, len(vs))
	for i := range minSim {
		minSim[i] = dot(vs[i], vs[0])
	}
	for len(out) < k {
		bi := 0
		for i := range vs {
			if minSim[i] < minSim[bi] {
				bi = i
			}
		}
		out = append(out, vs[bi])
		for i := range vs {
			if s := dot(vs[i], vs[bi]); s > minSim[i] {
				minSim[i] = s
			}
		}
	}
	return out
}

func dot(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// Cluster is a group of similar unnamed faces (a probable recurring person).
type Cluster struct {
	Faces  []store.Face // a sample, best first
	Size   int          // faces in the cluster
	Photos int          // distinct photos those faces are in
}

// Discover clusters prominent, unlabelled faces to surface people who appear
// often but have not been named. It examines at most maxFaces faces and
// keeps groups seen in at least minPhotos different photos.
func (c *Catalog) Discover(maxFaces, minPhotos int, threshold float32) ([]Cluster, error) {
	rows, err := c.Store.DB.Query(`SELECT id FROM faces
		WHERE person_id IS NULL AND width_px >= 60 AND score >= 0.7 AND ABS(yaw) < 0.6
		ORDER BY width_px * score DESC LIMIT ?`, maxFaces)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()

	// Leader clustering: faces are visited best-quality first; each joins the
	// most similar existing cluster leader if similar enough, else founds a
	// new cluster. Leaders are compared in parallel.
	var clusters []*leader
	for _, id := range ids {
		v, ok := c.FaceIndex().Vector(id)
		if !ok {
			continue
		}
		qv := vindex.Quantize(v)
		bi, _ := bestLeader(qv, clusters, threshold)
		if bi >= 0 {
			clusters[bi].ids = append(clusters[bi].ids, id)
		} else {
			clusters = append(clusters, &leader{lq: qv.Q, ls: qv.Scale, ids: []int64{id}})
		}
	}
	// Rank by how many photos a person is in, not how many faces: two
	// detections in one photo (a mirror, a poster) are still one photo.
	photos := make([]int, len(clusters))
	for i, k := range clusters {
		seen := map[int64]bool{}
		for _, id := range k.ids {
			seen[c.PhotoOfFace(id)] = true
		}
		photos[i] = len(seen)
	}
	order := make([]int, len(clusters))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return photos[order[a]] > photos[order[b]] })
	var out []Cluster
	for _, ci := range order {
		k := clusters[ci]
		if photos[ci] < minPhotos {
			break
		}
		show := k.ids
		if len(show) > 12 {
			show = show[:12]
		}
		fs, err := c.Store.FacesByID(show)
		if err != nil {
			return nil, err
		}
		rank := map[int64]int{}
		for i, id := range show {
			rank[id] = i
		}
		sort.Slice(fs, func(i, j int) bool { return rank[fs[i].ID] < rank[fs[j].ID] })
		out = append(out, Cluster{Faces: fs, Size: len(k.ids), Photos: photos[ci]})
		if len(out) >= 100 {
			break
		}
	}
	return out, nil
}

type leader struct {
	lq  []int8
	ls  float32
	ids []int64
}

// bestLeader finds the most similar cluster leader above threshold,
// comparing in parallel once there are many leaders.
func bestLeader(q vindex.Quantized, cs []*leader, threshold float32) (int, float32) {
	const chunk = 2048
	if len(cs) < 2*chunk {
		bi, bs := -1, threshold
		for i, k := range cs {
			if s := vindex.DotQ(q, k.lq, k.ls); s > bs {
				bi, bs = i, s
			}
		}
		return bi, bs
	}
	n := (len(cs) + chunk - 1) / chunk
	bis := make([]int, n)
	bss := make([]float32, n)
	var wg sync.WaitGroup
	for c := 0; c < n; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			bi, bs := -1, threshold
			for i := c * chunk; i < min(len(cs), (c+1)*chunk); i++ {
				if s := vindex.DotQ(q, cs[i].lq, cs[i].ls); s > bs {
					bi, bs = i, s
				}
			}
			bis[c], bss[c] = bi, bs
		}(c)
	}
	wg.Wait()
	bi, bs := -1, threshold
	for c := range bis {
		if bis[c] >= 0 && bss[c] > bs {
			bi, bs = bis[c], bss[c]
		}
	}
	return bi, bs
}

// ClusterMembers returns all face ids in the discovered cluster led by
// leaderID (recomputed: faces similar to the leader above threshold that are
// still unlabelled).
func (c *Catalog) ClusterMembers(leaderID int64, threshold float32, max int) ([]int64, error) {
	v, ok := c.FaceIndex().Vector(leaderID)
	if !ok {
		return nil, fmt.Errorf("face %d not found", leaderID)
	}
	eligible, err := c.Store.UnlabeledOrAutoFaceIDs()
	if err != nil {
		return nil, err
	}
	hits := c.FaceIndex().Search([][]float32{v}, vindex.SearchOpts{K: max, MinScore: threshold, Filter: func(id int64) bool { return eligible[id] }})
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out, nil
}
