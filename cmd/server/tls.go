package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// certReloader serves the certificate in certFile/keyFile and re-reads it
// when certFile changes on disk. Let's Encrypt certificates last 90 days
// and renewal tools (certbot, etc.) rewrite the files in place; without
// this, every renewal would need a platform restart, which pauses routing.
type certReloader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	modTime time.Time
}

// newCertReloader loads the certificate once up front, so a wrong path or
// a key that doesn't match the certificate stops startup, not the first visitor.
func newCertReloader(certFile, keyFile string) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile}
	if _, err := r.getCertificate(nil); err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	return r, nil
}

// getCertificate is tls.Config.GetCertificate: called on every TLS
// handshake. The check is one stat() call; the files are only re-read
// when certFile's modification time changes.
func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	info, statErr := os.Stat(r.certFile)

	r.mu.Lock()
	defer r.mu.Unlock()

	if statErr == nil && !info.ModTime().Equal(r.modTime) {
		cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
		switch {
		case err == nil:
			r.cert, r.modTime = &cert, info.ModTime()
		case r.cert == nil:
			return nil, err
		default:
			// Mid-renewal the cert may be written before the key, so they
			// briefly don't match. Keep serving the old certificate;
			// modTime is unchanged, so the next handshake tries again.
			log.Printf("reload TLS certificate (still serving the previous one): %v", err)
		}
	}
	if r.cert == nil {
		return nil, fmt.Errorf("no TLS certificate: %v", statErr)
	}
	return r.cert, nil
}

// httpsRedirect sends app visitors who typed http:// to the https:// URL.
// Only for app hosts (<name>.<appsDomain>): redirecting arbitrary Host
// headers would turn this listener into a redirector for any domain.
// Deliberately not used for the API: by the time an http:// API request
// arrives, its key has already crossed the network unencrypted.
func httpsRedirect(appsDomain, httpsPort string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !strings.HasSuffix(strings.ToLower(host), "."+appsDomain) {
			http.NotFound(w, r)
			return
		}
		if httpsPort != "443" { // 443 is https's default; the URL needs no port
			host = net.JoinHostPort(host, httpsPort)
		}
		// 308, not 301: a POST stays a POST instead of turning into a GET.
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	})
}

// defaultAppURL builds the URL pattern users are shown, leaving out the
// port when it is the scheme's default (443 for https, 80 for http).
func defaultAppURL(scheme, appsDomain, port string) string {
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return scheme + "://{name}." + appsDomain
	}
	return scheme + "://{name}." + appsDomain + ":" + port
}
