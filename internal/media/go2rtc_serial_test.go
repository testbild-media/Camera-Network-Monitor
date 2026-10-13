package media

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// go2rtc crashes on overlapping changes (and on listings during a change):
// the client must never send them at the same time, even when the reconciler
// runs several operations in parallel.
func TestClientSerialisesChanges(t *testing.T) {
	var writers, readers atomic.Int32
	var overlap atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			readers.Add(1)
			defer readers.Add(-1)
			if writers.Load() > 0 {
				overlap.Store("listing during a change")
			}
			time.Sleep(2 * time.Millisecond)
			w.Write([]byte(`{}`))
			return
		}
		if writers.Add(1) > 1 {
			overlap.Store("two changes at once")
		}
		if readers.Load() > 0 {
			overlap.Store("change during a listing")
		}
		time.Sleep(2 * time.Millisecond)
		writers.Add(-1)
	}))
	defer srv.Close()
	c := NewClient(srv.URL)
	defer c.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _ = c.PutStream(ctx, fmt.Sprintf("s%d", i), []string{"rtsp://x/1"}) }()
		go func() { defer wg.Done(); _ = c.PutPreload(ctx, fmt.Sprintf("s%d", i)) }()
		go func() { defer wg.Done(); _, _ = c.Streams(ctx) }()
	}
	wg.Wait()
	if v := overlap.Load(); v != nil {
		t.Fatal(v)
	}
}
