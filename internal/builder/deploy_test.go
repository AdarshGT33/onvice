package builder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWaitHealthy(t *testing.T) {
	for status, wantOK := range map[int]bool{200: true, 404: true, 500: false} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		err := waitHealthy(context.Background(), srv.URL, 1500*time.Millisecond)
		srv.Close()
		if (err == nil) != wantOK {
			t.Errorf("HTTP %d: got err %v, want healthy=%v", status, err, wantOK)
		}
	}

	// Nothing listening at all.
	if err := waitHealthy(context.Background(), "http://127.0.0.1:1/", time.Second); err == nil {
		t.Error("closed port: want error")
	}
}
