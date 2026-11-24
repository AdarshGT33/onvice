package router

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/AdarshGT33/onvice/internal/store"
)

type fakeLookup map[string]int // app name -> host port; 0 = not live

func (f fakeLookup) LiveHostPort(_ context.Context, name string) (int, error) {
	port, ok := f[name]
	switch {
	case !ok:
		return 0, store.ErrAppNotFound
	case port == 0:
		return 0, store.ErrAppNotLive
	}
	return port, nil
}

func TestRouter(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.Host+" "+r.Header.Get("X-Forwarded-Host")+" "+r.URL.Path)
	}))
	defer app.Close()
	_, portText, _ := net.SplitHostPort(app.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	rt := New(fakeLookup{"shop": port, "sleepy": 0}, "localhost")

	for _, c := range []struct {
		host, wantBody string
		wantCode       int
	}{
		{"shop.localhost:8000", "shop.localhost:8000 shop.localhost:8000 /cart", 200},
		{"SHOP.localhost.", "SHOP.localhost. SHOP.localhost. /cart", 200},
		{"ghost.localhost", "no app named ghost\n", 404},
		{"sleepy.localhost", "sleepy is not running right now\n", 503},
		{"a.shop.localhost", "unknown host\n", 404},
		{"shop.example.com", "unknown host\n", 404},
		{"localhost", "unknown host\n", 404},
	} {
		req := httptest.NewRequest("GET", "http://"+c.host+"/cart", nil)
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		if rec.Code != c.wantCode || rec.Body.String() != c.wantBody {
			t.Errorf("%s: got %d %q, want %d %q", c.host, rec.Code, rec.Body.String(), c.wantCode, c.wantBody)
		}
	}
}
