package admin

import (
	"fmt"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
)

// Content types of the admin API's request bodies. Clients set them from
// these constants so they cannot drift from what the server requires.
const (
	// ContentTypeJSON is required by every endpoint that reads a JSON body:
	// POST /ports/discover, POST /ports/push, POST /routes,
	// POST /transfer/pending/{id} and PUT /transfer/receive.
	ContentTypeJSON = "application/json"
	// ContentTypeFile is what PUT /transfer/send takes: the raw file bytes.
	ContentTypeFile = "application/octet-stream"
)

// CheckListenAddr reports whether addr (the admin_addr setting) is one the
// admin API may listen on: localhost or a loopback IP (127.0.0.0/8, ::1),
// with a port. The API has no authentication and can expose local services on
// the remote and accept files, so it must never be reachable from the network:
// an empty host (":9999"), 0.0.0.0, a LAN or Tailscale IP and any other
// hostname are refused.
func CheckListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("admin_addr %q is not a host:port address (e.g. 127.0.0.1:9999): %v", addr, err)
	}
	if isLoopbackHost(host) {
		return nil
	}
	what := "is not a loopback address"
	if host == "" {
		what = "listens on every network interface"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		what = "listens on every network interface"
	}
	return fmt.Errorf("admin_addr %q %s: the admin API is unauthenticated (it can expose local ports on the remote and accept files), "+
		"so it only listens on localhost, 127.0.0.1 or [::1] (e.g. admin_addr: 127.0.0.1:9999)", addr, what)
}

// isLoopbackHost reports whether host (no port, IPv6 without brackets) is
// "localhost" or a loopback IP literal. Other names are refused even when
// they resolve to a loopback address: that is exactly what DNS rebinding
// makes an attacker's name do.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostIsLoopback reports whether a request's Host header names this machine
// by loopback: localhost, 127.x.x.x or [::1], with or without a port.
func hostIsLoopback(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1] // "[::1]" without a port
	}
	return isLoopbackHost(host)
}

// guard keeps web browsers out of the admin API.
//
// The API has no authentication: whatever can connect to it can use it. It
// listens on loopback only (see CheckListenAddr), but every web page the user
// opens runs in a browser on this machine, which can reach loopback too:
//
//   - A page can send a cross-origin "simple request" (a POST with a
//     text/plain, form or multipart body) to http://127.0.0.1:9999 without a
//     CORS preflight. It cannot read the answer, but the write happens: it
//     could accept a pending incoming file or push a local port to the remote.
//   - With DNS rebinding a page on attacker.example makes its own name resolve
//     to 127.0.0.1, so its requests become same-origin: it can read /status
//     and the transfer log, and send any method.
//
// So guard refuses (403):
//
//   - a Host that is not localhost or a loopback IP literal: a rebound request
//     still carries the attacker's hostname;
//   - any request with an Origin header: browsers send one on every
//     cross-origin request and on every POST, PUT and DELETE, while the mole
//     CLI, the desktop app and other Go or curl clients never do;
//   - any Sec-Fetch-Site other than "none", which is a navigation the user
//     made (a URL typed in the address bar, so the JSON of /status can still
//     be looked at). "cross-site" and "same-site" are other pages;
//     "same-origin" would need a page served by this API, which serves no
//     HTML, so it is refused as well.
//
// The endpoints that read a body also require a content type a simple request
// cannot carry (see requireBody), which covers browsers too old to send Origin
// or Sec-Fetch-* headers.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostIsLoopback(r.Host) {
			http.Error(w, fmt.Sprintf("forbidden: Host %q is not localhost or a loopback address; "+
				"the mole admin API only answers requests made to localhost, 127.0.0.1 or [::1] (this blocks DNS rebinding)", r.Host),
				http.StatusForbidden)
			return
		}
		if origin := r.Header.Values("Origin"); len(origin) > 0 {
			http.Error(w, fmt.Sprintf("forbidden: requests from web pages (Origin %q) are refused; "+
				"use the mole CLI or the desktop app", origin[0]), http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "none" {
			http.Error(w, fmt.Sprintf("forbidden: requests from web pages (Sec-Fetch-Site %q) are refused; "+
				"use the mole CLI or the desktop app", site), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireBody wraps an endpoint that reads a request body so it only accepts
// the given media types (compared without parameters, so
// "application/json; charset=utf-8" is application/json). "" in accepted
// allows a request without a Content-Type. Anything else gets 415.
//
// A browser can send a body without a CORS preflight only as text/plain,
// application/x-www-form-urlencoded or multipart/form-data, or with no
// Content-Type at all on a POST, so requiring application/json makes a
// cross-site write impossible even without the checks in guard.
func requireBody(h http.HandlerFunc, accepted ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		media := ""
		if ct != "" {
			var err error
			if media, _, err = mime.ParseMediaType(ct); err != nil {
				media = "\x00invalid" // never in accepted
			}
		}
		if !slices.Contains(accepted, media) {
			got := "no Content-Type"
			if ct != "" {
				got = fmt.Sprintf("Content-Type %q", ct)
			}
			http.Error(w, fmt.Sprintf("unsupported media type: %s %s takes Content-Type %s, got %s. "+
				"If this came from the mole CLI or the desktop app, it is older than the running daemon: "+
				"upgrade the client to the daemon's version", r.Method, r.URL.Path, accepted[0], got),
				http.StatusUnsupportedMediaType)
			return
		}
		h(w, r)
	}
}
