package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Naming faces must work while an indexer (another process, so another
// connection) keeps saving batches. With deferred transactions, a
// read-then-write transaction failed at once with "database is locked"
// whenever another writer committed between its read and its write; the
// busy timeout does not help in that case.
func TestWritesWhileIndexing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	web, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer web.Close()
	indexer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer indexer.Close()

	face := func(p string) *Result {
		return &Result{
			Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, Version: SchemaVersion, FaceCount: 1},
			Faces: []Face{{Ordinal: 0, X1: .1, Y1: .1, X2: .3, Y2: .4, Embedding: make([]byte, 1024)}},
		}
	}
	r := face("/named.jpg")
	if err := web.Save([]*Result{r}); err != nil {
		t.Fatal(err)
	}
	faceID := r.Faces[0].ID

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the indexer: batches of 64, back to back
		defer wg.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			var batch []*Result
			for i := 0; i < 64; i++ {
				batch = append(batch, face(fmt.Sprintf("/batch%d/%d.jpg", n, i)))
			}
			if err := indexer.Save(batch); err != nil {
				t.Errorf("indexer save: %v", err)
				return
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		if _, err := web.LabelFaces(fmt.Sprintf("Person %d", i%3), []int64{faceID}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("naming a face while indexing (attempt %d): %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}
