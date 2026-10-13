package media

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// fakeGo2rtc reproduces the go2rtc API semantics CNM relies on (verified
// against go2rtc v1.9.14 sources):
//   - PUT /api/streams replaces the stream *object*; an existing preload stays
//     attached to the old (orphaned) object
//   - DELETE /api/streams?src=name removes the definition only
//   - PUT /api/preload attaches a probe consumer to the current object
type fakeGo2rtc struct {
	mu        sync.Mutex
	nextID    int
	streams   map[string]*fakeStream
	preloads  map[string]int // name → stream object id
	noPreload bool           // simulate go2rtc without preload API
	// slowPut delays PUT /api/streams after executing it (timeout simulation)
	slowPut time.Duration
	// putConfigError answers PUT /api/streams with 400 after creating the
	// stream, like go2rtc when it cannot patch its config file
	putConfigError bool
	failDel        bool
	calls          map[string]int
	srv            *httptest.Server
}

type fakeStream struct {
	id      int
	sources []string
	bytes   int64
}

func newFakeGo2rtc() *fakeGo2rtc {
	f := &fakeGo2rtc{streams: map[string]*fakeStream{}, preloads: map[string]int{}, calls: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeGo2rtc) restart() {
	f.mu.Lock()
	f.streams = map[string]*fakeStream{}
	f.preloads = map[string]int{}
	f.mu.Unlock()
}

func (f *fakeGo2rtc) count(k string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[k]
}

func (f *fakeGo2rtc) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	f.calls[r.Method+" "+r.URL.Path]++
	switch {
	case r.URL.Path == "/api":
		f.mu.Unlock()
		w.Write([]byte(`{"version":"1.9.14-fake"}`))
		return
	case r.URL.Path == "/api/streams" && r.Method == http.MethodGet:
		out := map[string]any{}
		for name, s := range f.streams {
			s.bytes += 1000
			var prods []map[string]any
			attached := f.preloads[name] == s.id
			for _, src := range s.sources {
				if attached {
					prods = append(prods, map[string]any{"url": src, "format_name": "rtsp", "bytes_recv": s.bytes})
					attached = false // only first producer active
				} else {
					prods = append(prods, map[string]any{"url": src})
				}
			}
			var cons []map[string]any
			if id, ok := f.preloads[name]; ok && id == s.id {
				cons = append(cons, map[string]any{"format_name": "preload"})
			}
			out[name] = map[string]any{"producers": prods, "consumers": cons}
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(out)
		return
	case r.URL.Path == "/api/streams" && r.Method == http.MethodPut:
		f.nextID++
		f.streams[q.Get("name")] = &fakeStream{id: f.nextID, sources: q["src"]}
		slow, cfgErr := f.slowPut, f.putConfigError
		f.mu.Unlock()
		if slow > 0 {
			time.Sleep(slow) // executed, but the answer arrives too late
		}
		if cfgErr {
			http.Error(w, "yaml: line 1: did not find expected key", http.StatusBadRequest)
		}
		return
	case r.URL.Path == "/api/streams" && r.Method == http.MethodDelete:
		if f.failDel {
			f.mu.Unlock()
			http.Error(w, "boom", 500)
			return
		}
		delete(f.streams, q.Get("src"))
		f.mu.Unlock()
		return
	case r.URL.Path == "/api/preload":
		if f.noPreload {
			f.mu.Unlock()
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			out := map[string]any{}
			for n := range f.preloads {
				out[n] = map[string]any{"query": "video"}
			}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(out)
			return
		case http.MethodPut:
			s, ok := f.streams[q.Get("src")]
			if !ok {
				f.mu.Unlock()
				http.Error(w, "streams: stream not found", 500)
				return
			}
			f.preloads[q.Get("src")] = s.id
			f.mu.Unlock()
			return
		case http.MethodDelete:
			if _, ok := f.preloads[q.Get("src")]; !ok {
				f.mu.Unlock()
				http.Error(w, "streams: preload not found: "+q.Get("src"), 500)
				return
			}
			delete(f.preloads, q.Get("src"))
			f.mu.Unlock()
			return
		}
	}
	f.mu.Unlock()
	http.NotFound(w, r)
}
