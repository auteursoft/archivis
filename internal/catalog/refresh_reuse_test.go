package catalog

import (
	"testing"

	"github.com/auteursoft/archivis/internal/store"
)

// SQLite reuses the id of a deleted newest row. A refresh must load the new
// photo, whether the reuse happens between refreshes or during one (the
// count check stops at the ids loaded; changed rows are found separately).
func TestRefreshIDReuseWithWatermark(t *testing.T) {
	// 1: newest photo deleted, its id reused by the next photo, between refreshes
	c := open(t)
	a, b := photo("/a.jpg", 1, 10), photo("/b.jpg", 2, 20)
	c.Store.Save([]*store.Result{a, b})
	c.Refresh()
	c.Store.DB.Exec(`DELETE FROM photos WHERE id = ?`, b.Photo.ID)
	n := photo("/n.jpg", 3, 30)
	c.Store.Save([]*store.Result{n})
	t.Logf("deleted id %d, new photo got id %d", b.Photo.ID, n.Photo.ID)
	c.Refresh()
	near(t, c.PhotoIndex(), n.Photo.ID, 3)
	near(t, c.FaceIndex(), n.Faces[0].ID, 30)
	if c.PhotoIndex().Len() != 2 || c.FaceIndex().Len() != 2 {
		t.Fatalf("index sizes %d/%d", c.PhotoIndex().Len(), c.FaceIndex().Len())
	}

	// 2: the same, but the reuse happens mid-refresh, after loading
	c2 := open(t)
	x, y := photo("/x.jpg", 4, 40), photo("/y.jpg", 5, 50)
	c2.Store.Save([]*store.Result{x, y})
	c2.Refresh()
	z := photo("/z.jpg", 6, 60)
	c2.afterLoad = func() {
		c2.Store.DB.Exec(`DELETE FROM photos WHERE id = ?`, y.Photo.ID)
		c2.Store.Save([]*store.Result{z})
	}
	c2.Refresh()
	c2.afterLoad = nil
	c2.Refresh()
	t.Logf("mid-refresh: deleted id %d, new photo got id %d", y.Photo.ID, z.Photo.ID)
	near(t, c2.PhotoIndex(), z.Photo.ID, 6)
	near(t, c2.FaceIndex(), z.Faces[0].ID, 60)
	if c2.PhotoIndex().Len() != 2 || c2.FaceIndex().Len() != 2 {
		t.Fatalf("index sizes %d/%d", c2.PhotoIndex().Len(), c2.FaceIndex().Len())
	}
}
