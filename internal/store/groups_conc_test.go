package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Reading pages of groups must not hold a connection while asking for
// another: with more concurrent pages than pooled connections, that
// deadlocks. Every path is tried: unfiltered, a filter checked group by
// group, and one found from the photos.
func TestGroupPagesConcurrentNoDeadlock(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var rs []*Result
	for i := 0; i < 2500; i++ { // more groups than one batch of member checks
		p := fmt.Sprintf("/c/%d", i)
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: "X",
			TakenAt: 1_000_000 + int64(i/2)*100 + int64(i%2), PHash: uint64(1 + i/2), Overall: float64(i % 20), Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RebuildGroups(context.Background(), GroupOptions{Gap: 3, Hamming: 6}); err != nil {
		t.Fatal(err)
	}
	st.DB.SetMaxOpenConns(2)
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 3 {
					for _, f := range []Filter{{Limit: 5}, {Limit: 5, MinOverall: 100}, {Limit: 5, Tag: "x"}} {
						for _, kind := range []int{GroupBurst, GroupDuplicate} {
							if _, _, err := st.GroupPage(kind, f, 2, false); err != nil {
								t.Error(err)
							}
						}
					}
				}
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent GroupPage calls deadlocked")
	}
}
