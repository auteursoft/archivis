package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Bursts must not hold its scan connection while loading photos: with more
// concurrent calls than pooled connections, that deadlocked.
func TestBurstsConcurrentNoDeadlock(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.DB.SetMaxOpenConns(2)
	var rs []*Result
	for i := 0; i < 30; i++ {
		p := fmt.Sprintf("/c/%d", i)
		rs = append(rs, &Result{Photo: Photo{Path: p, Size: 1, MTime: 1, Fingerprint: p, CameraModel: "X",
			TakenAt: 1_000_000 + int64(i/3)*100 + int64(i%3), Version: SchemaVersion}})
	}
	if err := st.Save(rs); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, err := st.Bursts(Filter{Limit: 5}, 3, 3); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Bursts calls deadlocked")
	}
}
