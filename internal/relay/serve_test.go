package relay

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestRelay_Serve_AcceptsOnPreOpenedListener regression-tests the startup-order
// fix: main.go now opens the TCP listener (net.Listen) and logs "listening"
// BEFORE running DB init/migrations, then hands the already-open listener to
// Relay.Serve once the heavy work is done. This verifies Serve actually serves
// correctly on a listener that was opened well before Serve was called —
// exactly the gap the fix introduces between bind and serve.
func TestRelay_Serve_AcceptsOnPreOpenedListener(t *testing.T) {
	r := testRelay(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	// Simulate heavy init happening between bind and Serve (the whole point of
	// the fix: the socket is already open and queuing connections here).
	time.Sleep(50 * time.Millisecond)

	serveErr := make(chan error, 1)
	go func() { serveErr <- r.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = r.Shutdown(ctx)
	})

	url := "http://" + ln.Addr().String() + "/api/projects"
	var resp *http.Response
	for i := 0; i < 50; i++ {
		resp, err = http.Get(url)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}
