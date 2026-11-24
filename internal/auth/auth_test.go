package auth

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AdarshGT33/onvice/internal/store"
	"github.com/gin-gonic/gin"
)

type fakeUsers struct{ hash []byte }

func (f fakeUsers) UserIDByTokenHash(_ context.Context, h []byte) (string, error) {
	if bytes.Equal(h, f.hash) {
		return "user-1", nil
	}
	return "", store.ErrInvalidToken
}

func TestMiddleware(t *testing.T) {
	token, hash, err := NewToken()
	if err != nil || !strings.HasPrefix(token, "onv_") || len(token) != 68 || !bytes.Equal(Hash(token), hash) {
		t.Fatalf("NewToken: %q %x %v", token, hash, err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/", Middleware(fakeUsers{hash}), func(c *gin.Context) { c.String(200, UserID(c)) })

	for header, want := range map[string]int{
		"":                         401,
		"Bearer " + token:          200,
		"Bearer " + token + "x":    401, // valid prefix, wrong key
		"Basic " + token:           401,
		"Bearer not-an-onvice-key": 401,
	} {
		req := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("%q: got %d, want %d", header, rec.Code, want)
		}
		if want == 200 && rec.Body.String() != "user-1" {
			t.Errorf("handler saw user %q", rec.Body.String())
		}
		if want == 401 && rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%q: missing WWW-Authenticate", header)
		}
	}
}
