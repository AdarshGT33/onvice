// Package router gives every app a stable URL, <name>.<domain>, and sends
// each request to whichever container is live for that app right now.
// Containers come and go with every deploy; the URL never changes.
package router

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/AdarshGT33/onvice/internal/store"
)

type Lookup interface {
	LiveHostPort(ctx context.Context, appName string) (int, error)
}

type Router struct {
	db     Lookup
	domain string // "localhost" locally, e.g. "apps.example.com" in production
}

func New(db Lookup, domain string) *Router {
	return &Router{db: db, domain: strings.ToLower(domain)}
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := appFromHost(r.Host, rt.domain)
	if !ok {
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}

	// Looked up on every request, not cached: right after a deploy commits,
	// the very next request goes to the new container.
	// ponytail: one indexed query per request; add a short cache if the
	// database becomes the bottleneck.
	port, err := rt.db.LiveHostPort(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrAppNotFound):
		http.Error(w, "no app named "+name, http.StatusNotFound)
		return
	case errors.Is(err, store.ErrAppNotLive):
		// The app exists but nothing healthy is running: first build still
		// in progress, or its deploy failed. Temporary from the visitor's view.
		http.Error(w, name+" is not running right now", http.StatusServiceUnavailable)
		return
	case err != nil:
		log.Printf("router: look up %s: %v", name, err)
		http.Error(w, "temporary error, please try again", http.StatusServiceUnavailable)
		return
	}

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The app sees its public hostname, not 127.0.0.1:<port>, so
			// redirects and absolute links it builds point at the real URL.
			pr.Out.Host = pr.In.Host
			// X-Forwarded-For/-Host/-Proto: the visitor's IP and the
			// original host and scheme, which the app can't see otherwise.
			pr.SetXForwarded()
		},
		// The default error handler answers 502 when the container doesn't
		// respond, e.g. it crashed and Docker is restarting it.
	}
	proxy.ServeHTTP(w, r)
}

// appFromHost turns "myapp.localhost:8000" into "myapp". Only a single
// label directly under the domain counts: "a.b.localhost" is not an app.
func appFromHost(host, domain string) (string, bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, ".")) // "myapp.localhost." is the same host
	name, ok := strings.CutSuffix(host, "."+domain)
	if !ok || name == "" || strings.Contains(name, ".") {
		return "", false
	}
	return name, true
}
