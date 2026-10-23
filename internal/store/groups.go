package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Groups: bursts (frames one camera shot within a few seconds of each
// other), stacks (near-identical frames within a burst, shown as one card)
// and sets of identical images (same perceptual hash). RebuildGroups works
// them out after indexing and stores them, so pages only read an index:
// per photo in the photos table's group columns, per group in photo_groups.

// Group kinds (photo_groups.kind).
const (
	GroupBurst     = 1
	GroupDuplicate = 2
)

// groupsVersion changes when the grouping rules do, forcing a rebuild.
const groupsVersion = "1"

// GroupOptions are the rules RebuildGroups groups by.
type GroupOptions struct {
	Gap     int64 // most seconds between frames of one burst
	Hamming int   // most differing perceptual-hash bits for frames to stack
}

// GroupOptions are the catalogue's grouping rules (from its Settings).
func (s *Store) GroupOptions() GroupOptions {
	v := s.GetSettings()
	return GroupOptions{Gap: int64(v.BurstGap), Hamming: v.StackHamming}
}

// RebuildStats reports what RebuildGroups found and wrote.
type RebuildStats struct {
	Bursts, Stacks, Duplicates int // groups found (bursts of 2+ frames)
	PhotoWrites, GroupWrites   int // rows changed
	Took                       time.Duration
}

// scanPage is the rows per read: short reads let the WAL checkpoint.
var scanPage = 50000

const (
	writeBatch = 500 // rows per write transaction ...
	writeSlice = 5 * time.Millisecond
)

// GroupsStale reports whether the stored groups may be out of date: photos
// changed or were deleted since, or the rules changed.
func (s *Store) GroupsStale(o GroupOptions) bool {
	if s.GroupRulesChanged(o) || s.GetMeta("groups_dirty") != s.GetMeta("groups_clean") {
		return true
	}
	seq, err := strconv.ParseInt(s.GetMeta("groups_seq"), 10, 64)
	return err != nil || s.MaxSeq() > seq
}

// markGroupsDirty notes, in tx, that groups need working out again though
// no photo's seq changed (scores rewritten in place). The photos_ungroup
// trigger does the same for deletes.
func markGroupsDirty(tx *sql.Tx) error {
	_, err := tx.Exec(`INSERT OR REPLACE INTO meta (key, value)
		VALUES ('groups_dirty', COALESCE((SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'groups_dirty'), 0) + 1)`)
	return err
}

// GroupRulesChanged reports whether the groups were worked out by other
// rules (or never), so they should be worked out again at once.
func (s *Store) GroupRulesChanged(o GroupOptions) bool {
	return s.GetMeta("groups_version") != groupsVersion ||
		s.GetMeta("groups_gap") != strconv.FormatInt(o.Gap, 10) ||
		s.GetMeta("groups_hamming") != strconv.Itoa(o.Hamming)
}

// GroupsRebuilding reports whether a rebuild is in progress (for a note on
// the pages; a crashed one counts for an hour).
func (s *Store) GroupsRebuilding() bool {
	t, err := strconv.ParseInt(s.GetMeta("groups_running"), 10, 64)
	return err == nil && time.Since(time.Unix(t, 0)) < time.Hour
}

// gphoto is what grouping needs of a photo, and its current group columns.
type gphoto struct {
	id                                         int64
	at                                         int64
	camera                                     string
	phash                                      uint64
	overall, technical, aesthetic, focus, colr float64
	faces                                      int
	cur                                        [5]int64 // burst_id, stack_top, stack_size, stacked, dup_id
}

type groupRow struct {
	kind, size                                        int
	id, start, end, best                              int64
	camera                                            string
	bestOverall, minOverall                           float64
	maxTechnical, maxAesthetic, maxFocus, maxColorful float64
	maxFaces                                          int
	members                                           string // JSON ids, ascending
}

// rebuild holds one RebuildGroups run's state.
type rebuild struct {
	s       *Store
	ctx     context.Context
	o       GroupOptions
	st      RebuildStats
	w       *batchWriter
	pending []groupRow // groups found, not yet compared with the stored ones
	ids     [3][]int64 // new group ids by kind (burst, dup)
	members [3][]int64 // new groups' members by kind
	left    [3][]int64 // changed or deleted groups' old members, by kind
}

// RebuildGroups works out every burst, stack and set of identical images
// and stores them, writing only what changed, in short transactions. It
// does not change photos' seq, so it does not make anything reload.
// Callers serialise runs (indexer.LockGroups).
func (s *Store) RebuildGroups(ctx context.Context, o GroupOptions) (RebuildStats, error) {
	start := time.Now()
	r := &rebuild{s: s, ctx: ctx, o: o, w: &batchWriter{s: s}}
	// read before scanning: changes during the scan count as unseen
	seq := s.MaxSeq()
	dirty := s.GetMeta("groups_dirty")
	if err := s.SetMeta("groups_running", strconv.FormatInt(time.Now().Unix(), 10)); err != nil {
		return r.st, err
	}
	defer s.SetMeta("groups_running", "")

	steps := []func() error{r.bursts, r.duplicates, r.deleteGone, r.resetOld}
	for _, step := range steps {
		if err := step(); err != nil {
			r.w.rollback()
			return r.st, err
		}
	}
	if err := r.w.commit(); err != nil {
		return r.st, err
	}
	for k, v := range map[string]string{
		"groups_seq": strconv.FormatInt(seq, 10), "groups_gap": strconv.FormatInt(o.Gap, 10),
		"groups_hamming": strconv.Itoa(o.Hamming), "groups_clean": dirty, "groups_version": groupsVersion,
	} {
		if err := s.SetMeta(k, v); err != nil {
			return r.st, err
		}
	}
	r.st.Took = time.Since(start)
	return r.st, nil
}

// bursts scans dated photos in time order (the photos_burst index covers
// the scan), keeping one open run per camera; a run ends when its camera's
// next frame is more than the gap later.
func (r *rebuild) bursts() error {
	type run struct {
		ids  []int64
		last int64
	}
	open := map[string]*run{}
	var pending [][]int64 // ended runs of 2+ frames, awaiting their details
	pendingN := 0
	flush := func() error {
		if err := r.burstDetails(pending); err != nil {
			return err
		}
		pending, pendingN = pending[:0], 0
		return nil
	}
	end := func(cam string, x *run) error {
		delete(open, cam)
		if len(x.ids) >= 2 {
			pending = append(pending, x.ids)
			pendingN += len(x.ids)
			if pendingN >= 2000 {
				return flush()
			}
		}
		return nil
	}
	type frame struct {
		at  int64
		cam string
		id  int64
	}
	// read in pages by time alone (the index is in time order, then camera):
	// a page ends at a whole second, so its next page starts at the next one
	read := func(q string, args ...any) ([]frame, error) {
		rows, err := r.query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []frame
		for rows.Next() {
			var f frame
			var mk, md string
			if err := rows.Scan(&f.at, &mk, &md, &f.id); err != nil {
				return nil, err
			}
			f.cam = mk + "\x00" + md
			out = append(out, f)
		}
		return out, rows.Err()
	}
	const cols = `SELECT taken_at, COALESCE(camera_make, ''), COALESCE(camera_model, ''), id FROM photos`
	at := int64(math.MinInt64)
	for {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		batch, err := read(cols+` WHERE taken_at > ? AND taken_at != 0 ORDER BY taken_at LIMIT ?`, at, scanPage)
		if err != nil {
			return err
		}
		n := len(batch)
		if n == scanPage {
			last := batch[n-1].at
			i := n
			for i > 0 && batch[i-1].at == last {
				i--
			}
			if i > 0 {
				batch = batch[:i] // the last second's frames come next page
			} else if batch, err = read(cols+` WHERE taken_at = ?`, last); err != nil {
				return err // a page of one second: read all of it
			}
		}
		if len(batch) > 0 {
			at = batch[len(batch)-1].at
		}
		for _, f := range batch {
			x := open[f.cam]
			if x != nil && f.at-x.last > r.o.Gap {
				if err := end(f.cam, x); err != nil {
					return err
				}
				x = nil
			}
			if x == nil {
				x = &run{}
				open[f.cam] = x
			}
			x.ids = append(x.ids, f.id)
			x.last = f.at
		}
		// end runs no later frame can join (keeps open runs few)
		cams := make([]string, 0, len(open))
		for c := range open {
			cams = append(cams, c)
		}
		sort.Strings(cams)
		for _, c := range cams {
			if x := open[c]; n < scanPage || at-x.last > r.o.Gap {
				if err := end(c, x); err != nil {
					return err
				}
			}
		}
		if n < scanPage {
			break
		}
	}
	return flush()
}

// burstDetails loads the runs' frames, stacks them, and queues the changes.
func (r *rebuild) burstDetails(runs [][]int64) error {
	if len(runs) == 0 {
		return nil
	}
	var ids []int64
	for _, x := range runs {
		ids = append(ids, x...)
	}
	byID, err := r.details(ids)
	if err != nil {
		return err
	}
	for _, x := range runs {
		var frames []*gphoto
		for _, id := range x {
			if p := byID[id]; p != nil {
				frames = append(frames, p)
			}
		}
		if len(frames) < 2 {
			continue // deleted meanwhile
		}
		burst := frames[0].id
		for _, f := range frames {
			burst = min(burst, f.id)
		}
		r.st.Bursts++
		want := stackFrames(frames, r.o.Hamming)
		for _, f := range frames {
			w := want[f.id]
			if w[0] == f.id {
				r.st.Stacks++
			}
			r.members[GroupBurst] = append(r.members[GroupBurst], f.id)
			if f.cur[0] != burst || f.cur[1] != w[0] || f.cur[2] != w[1] || f.cur[3] != w[2] {
				if err := r.w.exec(`UPDATE photos SET burst_id = ?, stack_top = ?, stack_size = ?, stacked = ? WHERE id = ?`,
					burst, w[0], w[1], w[2], f.id); err != nil {
					return err
				}
				r.st.PhotoWrites++
			}
		}
		r.addGroup(GroupBurst, frames)
	}
	return r.writePending()
}

// stackFrames clusters a burst's frames into stacks of near-identical
// frames: taking frames best first (overall, then lower id), each frame not
// yet in a stack starts one and takes every later frame not yet in a stack
// whose perceptual hash is within the threshold of its own. So the top is
// the best frame, and a slow pan does not chain into one stack. It returns
// each frame's (stack_top, stack_size, stacked); frames alone get zeros.
func stackFrames(frames []*gphoto, hamming int) map[int64][3]int64 {
	order := append([]*gphoto(nil), frames...)
	sort.Slice(order, func(i, j int) bool {
		if order[i].overall != order[j].overall {
			return order[i].overall > order[j].overall
		}
		return order[i].id < order[j].id
	})
	out := make(map[int64][3]int64, len(frames))
	taken := make([]bool, len(order))
	for i, top := range order {
		if taken[i] || top.phash == 0 {
			continue
		}
		stack := []int{i}
		for j := i + 1; j < len(order); j++ {
			if !taken[j] && order[j].phash != 0 && bits.OnesCount64(top.phash^order[j].phash) <= hamming {
				stack = append(stack, j)
			}
		}
		if len(stack) < 2 {
			continue
		}
		for _, j := range stack {
			taken[j] = true
			hidden := int64(0)
			if j != i {
				hidden = 1
			}
			out[order[j].id] = [3]int64{top.id, int64(len(stack)), hidden}
		}
	}
	return out
}

// duplicates scans photos by perceptual hash (the photos_phash index covers
// it); each hash shared by 2+ photos is a set of identical images.
func (r *rebuild) duplicates() error {
	var pending [][]int64
	pendingN := 0
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		var ids []int64
		for _, g := range pending {
			ids = append(ids, g...)
		}
		byID, err := r.details(ids)
		if err != nil {
			return err
		}
		for _, g := range pending {
			var ps []*gphoto
			for _, id := range g {
				if p := byID[id]; p != nil {
					ps = append(ps, p)
				}
			}
			if len(ps) < 2 {
				continue
			}
			dup := ps[0].id
			for _, p := range ps {
				dup = min(dup, p.id)
			}
			r.st.Duplicates++
			for _, p := range ps {
				r.members[GroupDuplicate] = append(r.members[GroupDuplicate], p.id)
				if p.cur[4] != dup {
					if err := r.w.exec(`UPDATE photos SET dup_id = ? WHERE id = ?`, dup, p.id); err != nil {
						return err
					}
					r.st.PhotoWrites++
				}
			}
			r.addGroup(GroupDuplicate, ps)
		}
		pending, pendingN = pending[:0], 0
		return r.writePending()
	}
	var cur []int64
	var curHash int64
	h, id := int64(math.MinInt64), int64(0)
	for {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		rows, err := r.query(`SELECT phash, id FROM photos WHERE phash != 0 AND (phash, id) > (?, ?)
			ORDER BY phash, id LIMIT ?`, h, id, scanPage)
		if err != nil {
			return err
		}
		n := 0
		var batch [][2]int64
		for rows.Next() {
			if err := rows.Scan(&h, &id); err != nil {
				rows.Close()
				return err
			}
			n++
			batch = append(batch, [2]int64{h, id})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, x := range batch {
			if len(cur) > 0 && x[0] != curHash {
				if len(cur) >= 2 {
					pending = append(pending, cur)
					pendingN += len(cur)
				}
				cur = nil
			}
			curHash = x[0]
			cur = append(cur, x[1])
		}
		if pendingN >= 2000 {
			if err := flush(); err != nil {
				return err
			}
		}
		if n < scanPage {
			break
		}
	}
	if len(cur) >= 2 {
		pending = append(pending, cur)
	}
	return flush()
}

// query reads, first committing pending writes: a write transaction must
// not stay open (blocking other writers) while the rebuild reads.
func (r *rebuild) query(q string, args ...any) (*sql.Rows, error) {
	if err := r.w.commit(); err != nil {
		return nil, err
	}
	return r.s.DB.Query(q, args...)
}

// details loads what grouping needs of the given photos, by id.
func (r *rebuild) details(ids []int64) (map[int64]*gphoto, error) {
	out := make(map[int64]*gphoto, len(ids))
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		rows, err := r.query(`SELECT id, taken_at, TRIM(COALESCE(camera_make, '') || ' ' || COALESCE(camera_model, '')), phash, COALESCE(overall, 0),
			COALESCE(technical, 0), COALESCE(aesthetic, 0), COALESCE(focus_score, 0), COALESCE(colorfulness, 0), face_count,
			burst_id, stack_top, stack_size, stacked, dup_id FROM photos WHERE id IN (`+placeholders(len(chunk))+`)`, int64sToAny(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			p := &gphoto{}
			if err := rows.Scan(&p.id, &p.at, &p.camera, (*int64ptr)(&p.phash), &p.overall, &p.technical, &p.aesthetic,
				&p.focus, &p.colr, &p.faces, &p.cur[0], &p.cur[1], &p.cur[2], &p.cur[3], &p.cur[4]); err != nil {
				rows.Close()
				return nil, err
			}
			out[p.id] = p
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// addGroup queues a group's photo_groups row.
func (r *rebuild) addGroup(kind int, ps []*gphoto) {
	g := groupRow{kind: kind, size: len(ps), minOverall: math.Inf(1), start: math.MaxInt64, end: math.MinInt64}
	ids := make([]int64, 0, len(ps))
	var best *gphoto
	for _, p := range ps {
		ids = append(ids, p.id)
		if best == nil || p.overall > best.overall || (p.overall == best.overall && p.id < best.id) {
			best = p
		}
		g.minOverall = math.Min(g.minOverall, p.overall)
		g.maxTechnical = math.Max(g.maxTechnical, p.technical)
		g.maxAesthetic = math.Max(g.maxAesthetic, p.aesthetic)
		g.maxFocus = math.Max(g.maxFocus, p.focus)
		g.maxColorful = math.Max(g.maxColorful, p.colr)
		g.maxFaces = max(g.maxFaces, p.faces)
		g.start, g.end = min(g.start, p.at), max(g.end, p.at)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	g.id, g.best, g.bestOverall, g.camera = ids[0], best.id, best.overall, best.camera
	b, _ := json.Marshal(ids)
	g.members = string(b)
	r.pending = append(r.pending, g)
	r.ids[kind] = append(r.ids[kind], g.id)
}

const groupCols = `kind, id, size, start_at, end_at, camera, best_id, best_overall, min_overall,
	max_technical, max_aesthetic, max_focus, max_colorful, max_faces, members`

// writePending writes the queued groups that are new or changed, noting the
// old members of changed ones.
func (r *rebuild) writePending() error {
	for kind := GroupBurst; kind <= GroupDuplicate; kind++ {
		var ids []int64
		for _, g := range r.pending {
			if g.kind == kind {
				ids = append(ids, g.id)
			}
		}
		old := map[int64]groupRow{}
		for start := 0; start < len(ids); start += 500 {
			chunk := ids[start:min(len(ids), start+500)]
			rows, err := r.query(`SELECT `+groupCols+` FROM photo_groups WHERE kind = ? AND id IN (`+placeholders(len(chunk))+`)`,
				append([]any{kind}, int64sToAny(chunk)...)...)
			if err != nil {
				return err
			}
			for rows.Next() {
				var g groupRow
				if err := rows.Scan(&g.kind, &g.id, &g.size, &g.start, &g.end, &g.camera, &g.best, &g.bestOverall, &g.minOverall,
					&g.maxTechnical, &g.maxAesthetic, &g.maxFocus, &g.maxColorful, &g.maxFaces, &g.members); err != nil {
					rows.Close()
					return err
				}
				old[g.id] = g
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		for _, g := range r.pending {
			if g.kind != kind {
				continue
			}
			o, ok := old[g.id]
			if ok && o == g {
				continue
			}
			if ok {
				r.noteLeft(kind, o.members)
			}
			if err := r.w.exec(`INSERT OR REPLACE INTO photo_groups (`+groupCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				g.kind, g.id, g.size, g.start, g.end, g.camera, g.best, g.bestOverall, g.minOverall,
				g.maxTechnical, g.maxAesthetic, g.maxFocus, g.maxColorful, g.maxFaces, g.members); err != nil {
				return err
			}
			r.st.GroupWrites++
		}
	}
	r.pending = r.pending[:0]
	return nil
}

func (r *rebuild) noteLeft(kind int, members string) {
	var ids []int64
	if json.Unmarshal([]byte(members), &ids) == nil {
		r.left[kind] = append(r.left[kind], ids...)
	}
}

// deleteGone deletes the stored groups that no longer exist, noting their
// members.
func (r *rebuild) deleteGone() error {
	for kind := GroupBurst; kind <= GroupDuplicate; kind++ {
		keep := dedupe(r.ids[kind])
		id := int64(math.MinInt64)
		for {
			if err := r.ctx.Err(); err != nil {
				return err
			}
			type gone struct {
				id      int64
				members string
			}
			var batch []gone
			n := 0
			rows, err := r.query(`SELECT id, members FROM photo_groups WHERE kind = ? AND id > ? ORDER BY id LIMIT ?`, kind, id, scanPage)
			if err != nil {
				return err
			}
			for rows.Next() {
				var g gone
				if err := rows.Scan(&g.id, &g.members); err != nil {
					rows.Close()
					return err
				}
				n++
				id = g.id
				if !contains(keep, g.id) {
					batch = append(batch, g)
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, g := range batch {
				r.noteLeft(kind, g.members)
				if err := r.w.exec(`DELETE FROM photo_groups WHERE kind = ? AND id = ?`, kind, g.id); err != nil {
					return err
				}
				r.st.GroupWrites++
			}
			if n < scanPage {
				break
			}
		}
	}
	return nil
}

// contains reports whether sorted ids holds id.
func contains(ids []int64, id int64) bool {
	i := sort.Search(len(ids), func(i int) bool { return ids[i] >= id })
	return i < len(ids) && ids[i] == id
}

// resetOld clears the group columns of photos no longer in a group: the
// old members of changed and deleted groups, and every hidden stack member
// (found through the photos_stacked index, so a run cut short cannot leave
// photos hidden).
func (r *rebuild) resetOld() error {
	newBurst, newDup := dedupe(r.members[GroupBurst]), dedupe(r.members[GroupDuplicate])
	hidden := r.left[GroupBurst]
	rows, err := r.query(`SELECT id FROM photos WHERE stacked = 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		hidden = append(hidden, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// frames still in a group were brought up to date when it was found
	for _, id := range dedupe(hidden) {
		if !contains(newBurst, id) {
			if err := r.w.exec(`UPDATE photos SET burst_id = 0, stack_top = 0, stack_size = 0, stacked = 0 WHERE id = ?`, id); err != nil {
				return err
			}
			r.st.PhotoWrites++
		}
	}
	for _, id := range dedupe(r.left[GroupDuplicate]) {
		if !contains(newDup, id) {
			if err := r.w.exec(`UPDATE photos SET dup_id = 0 WHERE id = ?`, id); err != nil {
				return err
			}
			r.st.PhotoWrites++
		}
	}
	return nil
}

// dedupe sorts ids and drops repeats.
func dedupe(ids []int64) []int64 {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := ids[:0]
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			out = append(out, id)
		}
	}
	return out
}

// batchWriter runs statements in short transactions: at most writeBatch
// statements or writeSlice of time each, so other writers (the web
// interface naming a face) wait at most that long.
type batchWriter struct {
	s     *Store
	tx    *sql.Tx
	stmts map[string]*sql.Stmt // prepared in tx
	n     int
	began time.Time
}

func (w *batchWriter) exec(q string, args ...any) error {
	if w.tx == nil {
		tx, err := w.s.DB.Begin()
		if err != nil {
			return err
		}
		w.tx, w.stmts, w.n, w.began = tx, map[string]*sql.Stmt{}, 0, time.Now()
	}
	st := w.stmts[q]
	if st == nil {
		var err error
		if st, err = w.tx.Prepare(q); err != nil {
			return err
		}
		w.stmts[q] = st
	}
	if _, err := st.Exec(args...); err != nil {
		return err
	}
	w.n++
	if w.n >= writeBatch || time.Since(w.began) >= writeSlice {
		return w.commit()
	}
	return nil
}

func (w *batchWriter) commit() error {
	if w.tx == nil {
		return nil
	}
	err := w.tx.Commit() // closes the statements
	w.tx, w.stmts = nil, nil
	return err
}

func (w *batchWriter) rollback() {
	if w.tx != nil {
		w.tx.Rollback()
		w.tx, w.stmts = nil, nil
	}
}

// ---- reading groups ---------------------------------------------------

// GroupPageInfo says what lies beyond a page of groups.
type GroupPageInfo struct {
	More    bool // further groups follow
	Partial bool // the page stopped at maxGroupWalk groups checked; more may match
}

// Group is a burst or a set of identical images, as a page shows it.
type Group struct {
	Kind    int
	ID      int64
	Size    int // members in all
	StartAt int64
	EndAt   int64
	Camera  string
	Members []Photo // the members matching the page's filter, best first
}

// groupSort maps a sort name to the photo_groups column and direction a
// page of groups is ordered by (ties: newer group id first).
func groupSort(name string) string {
	switch name {
	case "worst":
		return "min_overall ASC"
	case "newest":
		return "end_at DESC"
	case "oldest":
		return "start_at = 0, start_at ASC"
	case "technical":
		return "max_technical DESC"
	case "aesthetic":
		return "max_aesthetic DESC"
	case "sharpness":
		return "max_focus DESC"
	case "colorful":
		return "max_colorful DESC"
	case "faces":
		return "max_faces DESC"
	}
	return "best_overall DESC"
}

// photoFirst reports whether a filter picks out few photos through an
// index, so matching groups are best found from the photos (then ordered)
// rather than by walking groups in order and checking their members.
func (f Filter) photoFirst() bool {
	return f.PersonID > 0 || f.Tag != ""
}

// maxGroupWalk bounds how many groups a page with a quality-style filter
// checks; past it the page says it is partial.
var maxGroupWalk = 200000 // a variable so tests can reach it

// GroupPage returns a page of groups of a kind with at least minSize
// members (f.Offset and f.Limit count groups), ordered by f.Sort over each
// group's best (or, for "worst", worst) member, and whether more follow.
// With a filter, a group is listed if any member matches, with only its
// matching members.
func (s *Store) GroupPage(kind int, f Filter, minSize int) ([]Group, GroupPageInfo, error) {
	var info GroupPageInfo
	limit := max(1, f.Limit)
	offset := max(0, f.Offset)
	order := groupSort(f.Sort) + ", id DESC"
	where, args := f.where()
	var ids []int64
	var err error
	switch {
	case where == "":
		ids, err = s.groupIDs(`SELECT id FROM photo_groups WHERE kind = ? AND size >= ? ORDER BY `+order+` LIMIT ? OFFSET ?`,
			kind, minSize, limit+1, offset)
	case f.photoFirst():
		ids, err = s.groupsFromPhotos(kind, minSize, order, where, args, offset+limit+1)
		if len(ids) > offset {
			ids = ids[offset:]
		} else {
			ids = nil
		}
	default:
		ids, info.Partial, err = s.walkGroups(kind, minSize, f, order, where, args, offset, limit+1)
	}
	if err != nil {
		return nil, info, err
	}
	info.More = len(ids) > limit
	if info.More {
		ids = ids[:limit]
	}
	groups, err := s.loadGroups(kind, ids, where, args)
	return groups, info, err
}

func (s *Store) groupIDs(q string, args ...any) ([]int64, error) {
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// groupsFromPhotos finds the groups with a matching member from the
// matching photos, then orders them; it returns up to n ids.
func (s *Store) groupsFromPhotos(kind, minSize int, order, where string, args []any, n int) ([]int64, error) {
	col := "burst_id"
	if kind == GroupDuplicate {
		col = "dup_id"
	}
	gids, err := s.groupIDs(`SELECT DISTINCT `+col+` FROM photos`+where+` AND `+col+` != 0`, args...)
	if err != nil {
		return nil, err
	}
	return s.orderGroups(kind, minSize, gids, order, n)
}

// orderGroups returns the first n of the given groups with at least
// minSize members, in page order, through a temporary table.
func (s *Store) orderGroups(kind, minSize int, ids []int64, order string, n int) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	conn, err := s.DB.Conn(context.Background())
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS page_groups (id INTEGER PRIMARY KEY)`); err != nil {
		return nil, err
	}
	defer conn.ExecContext(ctx, `DELETE FROM page_groups`)
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO page_groups (id) VALUES `+strings.TrimSuffix(strings.Repeat("(?),", len(chunk)), ","),
			int64sToAny(chunk)...); err != nil {
			return nil, err
		}
	}
	rows, err := conn.QueryContext(ctx, `SELECT g.id FROM photo_groups g JOIN page_groups p ON p.id = g.id
		WHERE g.kind = ? AND g.size >= ? ORDER BY `+qualify(order, "g.")+` LIMIT ?`, kind, minSize, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// qualify prefixes the column names in an ORDER BY clause.
func qualify(order, prefix string) string {
	parts := strings.Split(order, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// walkGroups goes through groups in page order, checking members against
// the filter in chunks, until it has skipped offset and found n matching
// groups, or checked maxGroupWalk groups (partial: more may match beyond).
// A date range first rules out
// groups whose span misses it.
func (s *Store) walkGroups(kind, minSize int, f Filter, order, where string, args []any, offset, n int) (out []int64, partial bool, err error) {
	q := `SELECT id, members FROM photo_groups WHERE kind = ? AND size >= ?`
	qa := []any{kind, minSize}
	from, to := f.From, f.To
	if yf, yt := f.years(); yf != 0 || yt != 0 { // the same bounds where() uses
		if yf != 0 && (from == 0 || YearStart(yf) > from) {
			from = YearStart(yf)
		}
		if yt != 0 && (to == 0 || YearStart(yt+1) < to) {
			to = YearStart(yt + 1)
		}
	}
	if from != 0 {
		q += ` AND end_at >= ?`
		qa = append(qa, from)
	}
	if to != 0 {
		q += ` AND start_at < ?`
		qa = append(qa, to)
	}
	type gm struct {
		id      int64
		members []int64
	}
	// Read groups in batches, each read whole before its members are checked
	// (a cursor held open while checking would take a second connection,
	// and enough pages at once would wait on each other for ever). Batches
	// grow, so a page found early costs little and a long walk re-skips
	// little.
	skipped := 0
	for at, size := 0, min(1000, maxGroupWalk); at < maxGroupWalk; at, size = at+size, min(size*4, maxGroupWalk-at-size) {
		rows, err := s.DB.Query(q+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(qa, size, at)...)
		if err != nil {
			return nil, false, err
		}
		var batch []gm
		for rows.Next() {
			var g gm
			var members string
			if err := rows.Scan(&g.id, &members); err != nil {
				rows.Close()
				return nil, false, err
			}
			json.Unmarshal([]byte(members), &g.members)
			batch = append(batch, g)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, false, err
		}
		for start := 0; start < len(batch); start += 1000 {
			chunk := batch[start:min(len(batch), start+1000)]
			var all []int64
			for _, g := range chunk {
				all = append(all, g.members...)
			}
			match, err := s.matching(all, where, args)
			if err != nil {
				return nil, false, err
			}
			for _, g := range chunk {
				for _, id := range g.members {
					if match[id] {
						if skipped < offset {
							skipped++
						} else {
							out = append(out, g.id)
						}
						break
					}
				}
				if len(out) >= n {
					return out, false, nil
				}
			}
		}
		if len(batch) < size {
			return out, false, nil // every group checked
		}
	}
	return out, true, nil
}

// matching returns which of ids pass the filter (where/args from where()).
func (s *Store) matching(ids []int64, where string, args []any) (map[int64]bool, error) {
	out := map[int64]bool{}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(len(ids), start+500)]
		got, err := s.groupIDs(`SELECT id FROM photos`+where+` AND id IN (`+placeholders(len(chunk))+`)`,
			append(append([]any{}, args...), int64sToAny(chunk)...)...)
		if err != nil {
			return nil, err
		}
		for _, id := range got {
			out[id] = true
		}
	}
	return out, nil
}

// loadGroups loads groups by id, in order, with their members matching the
// filter (all members without one), best first.
func (s *Store) loadGroups(kind int, ids []int64, where string, args []any) ([]Group, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.DB.Query(`SELECT id, size, start_at, end_at, camera, members FROM photo_groups WHERE kind = ? AND id IN (`+
		placeholders(len(ids))+`)`, append([]any{kind}, int64sToAny(ids)...)...)
	if err != nil {
		return nil, err
	}
	byID := map[int64]*Group{}
	members := map[int64][]int64{}
	var all []int64
	for rows.Next() {
		g := &Group{Kind: kind}
		var m string
		if err := rows.Scan(&g.ID, &g.Size, &g.StartAt, &g.EndAt, &g.Camera, &m); err != nil {
			rows.Close()
			return nil, err
		}
		var ms []int64
		json.Unmarshal([]byte(m), &ms)
		byID[g.ID], members[g.ID] = g, ms
		all = append(all, ms...)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keep := map[int64]bool(nil)
	if where != "" {
		if keep, err = s.matching(all, where, args); err != nil {
			return nil, err
		}
	}
	ps, err := s.Photos(all)
	if err != nil {
		return nil, err
	}
	photo := make(map[int64]Photo, len(ps))
	for _, p := range ps {
		photo[p.ID] = p
	}
	out := make([]Group, 0, len(ids))
	for _, id := range ids {
		g := byID[id]
		if g == nil {
			continue
		}
		for _, m := range members[id] {
			if p, ok := photo[m]; ok && (keep == nil || keep[m]) {
				g.Members = append(g.Members, p)
			}
		}
		sort.SliceStable(g.Members, func(i, j int) bool {
			a, b := g.Members[i], g.Members[j]
			if a.Overall != b.Overall {
				return a.Overall > b.Overall
			}
			return a.ID < b.ID
		})
		if len(g.Members) > 0 {
			out = append(out, *g)
		}
	}
	return out, nil
}

// String describes the stats for logs.
func (st RebuildStats) String() string {
	return fmt.Sprintf("%d bursts (%d stacks), %d sets of identical images; %d photos and %d groups updated in %s",
		st.Bursts, st.Stacks, st.Duplicates, st.PhotoWrites, st.GroupWrites, st.Took.Round(time.Millisecond))
}
