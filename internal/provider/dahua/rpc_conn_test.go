package dahua

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Field case: some recorders drop idle keep-alive connections without
// telling the client. The next request on the reused connection then gets
// no answer. CNM must not report that as a lost device: the request is
// repeated once on a fresh connection.
func TestRPCReusedConnectionDroppedByDevice(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var conns, answered atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				rd := bufio.NewReader(c)
				for n := 0; ; n++ {
					req, err := http.ReadRequest(rd)
					if err != nil {
						return
					}
					if n > 0 {
						return // the device forgot this connection: close without answer
					}
					_, _ = req.Body.Read(make([]byte, 4096))
					body := `{"id":1,"result":true,"params":{"timeout":300},"session":"s1"}`
					answered.Add(1)
					fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: keep-alive\r\n\r\n%s", len(body), body)
				}
			}(c)
		}
	}()
	c := newRPCClient("127.0.0.1", ln.Addr().(*net.TCPAddr).Port, "admin", "pw", VariantRPC2Classic, &env{})
	c.setSession([]byte(`"s1"`))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		r, err := c.call(ctx, "global.keepAlive", map[string]any{"timeout": 300}, nil)
		if err != nil || !r.ok() {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if answered.Load() != 3 || conns.Load() < 3 {
		t.Fatalf("answered %d on %d connections", answered.Load(), conns.Load())
	}
	if !strings.HasPrefix(c.base, "http://127.0.0.1:") {
		t.Fatal(c.base)
	}
}
