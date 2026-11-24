package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCert writes a self-signed certificate with the given serial number.
func writeCert(t *testing.T, certFile, keyFile string, serial int64, mod time.Time) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.Chtimes(certFile, mod, mod) // explicit, so the test doesn't depend on clock resolution
}

func serialOf(t *testing.T, r *certReloader) int64 {
	t.Helper()
	c, err := r.getCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(c.Certificate[0])
	return leaf.SerialNumber.Int64()
}

func TestCertReloader(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	t0 := time.Now().Add(-time.Minute)

	if _, err := newCertReloader(certFile, keyFile); err == nil {
		t.Fatal("missing files: want startup error")
	}

	writeCert(t, certFile, keyFile, 1, t0)
	r, err := newCertReloader(certFile, keyFile)
	if err != nil || serialOf(t, r) != 1 {
		t.Fatalf("initial load: %v", err)
	}

	// Renewal: new files on disk, picked up without a restart.
	writeCert(t, certFile, keyFile, 2, t0.Add(time.Second))
	if got := serialOf(t, r); got != 2 {
		t.Fatalf("after renewal: serial %d, want 2", got)
	}

	// Half-written renewal (cert doesn't match key): keep the old one.
	os.WriteFile(keyFile, []byte("garbage"), 0o600)
	os.Chtimes(certFile, t0.Add(2*time.Second), t0.Add(2*time.Second))
	if got := serialOf(t, r); got != 2 {
		t.Fatalf("broken renewal: serial %d, want previous 2", got)
	}
}

func TestHTTPSRedirect(t *testing.T) {
	for _, c := range []struct {
		port, host, wantCode, wantLoc string
	}{
		{"8443", "shop.localhost:8001", "308", "https://shop.localhost:8443/cart?x=1"},
		{"443", "shop.localhost", "308", "https://shop.localhost/cart?x=1"},
		{"443", "evil.example.com", "404", ""},
	} {
		rec := httptest.NewRecorder()
		httpsRedirect("localhost", c.port).ServeHTTP(rec, httptest.NewRequest("GET", "http://"+c.host+"/cart?x=1", nil))
		if got := rec.Result().Status[:3]; got != c.wantCode || rec.Header().Get("Location") != c.wantLoc {
			t.Errorf("%s: got %s %q, want %s %q", c.host, got, rec.Header().Get("Location"), c.wantCode, c.wantLoc)
		}
	}

	if got := defaultAppURL("https", "apps.example.com", "443"); got != "https://{name}.apps.example.com" {
		t.Errorf("defaultAppURL: %s", got)
	}
}
