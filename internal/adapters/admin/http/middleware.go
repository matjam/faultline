package adminhttp

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/matjam/faultline/internal/adapters/auth/users"
)

const (
	// sessionCookieName is the cookie name carrying the session
	// token. Prefixed with "__Host-" would be ideal but requires
	// HTTPS, which we deliberately don't terminate ourselves.
	sessionCookieName = "faultline_session"

	// csrfFormField is the form/multipart name we look up for the
	// CSRF token on every state-changing request.
	csrfFormField = "_csrf"
)

// ctxKey is the unexported key type for context values. Avoids
// collisions with anything else stuffing values into the same
// context.
type ctxKey int

const (
	ctxKeySession ctxKey = iota
)

// sessionFromContext returns the session attached by requireAuth, or
// nil if none. Handlers behind requireAuth can rely on a non-nil
// return.
func sessionFromContext(ctx context.Context) *users.Session {
	v, _ := ctx.Value(ctxKeySession).(*users.Session)
	return v
}

// requireAuth wraps a handler so it only fires for authenticated
// requests. Unauthenticated callers are redirected to /admin/login
// (preserving their target via the `next` query parameter when the
// method is GET) or rejected with 401 for non-GET. State-changing
// methods additionally have their CSRF token validated.
//
// HTMX requests (header `HX-Request: true`) get an HX-Redirect
// response instead of a 303 to /admin/login. HTMX honors
// HX-Redirect on any response and triggers a full-page navigation
// client-side. This avoids a subtle race: stale fragment polling
// from a tab whose session expired would otherwise XHR-follow the
// 303 to /admin/login, and the login GET handler would be invoked
// in the background — visible in the access log and (historically)
// rotating the login-CSRF cookie out from under any login form the
// user had open. The HX-Redirect path keeps polling redirects from
// touching the login page at all.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.currentSession(r)
		if sess == nil {
			switch {
			case isHTMXRequest(r):
				w.Header().Set("HX-Redirect", htmxLoginTarget(r))
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			case r.Method == http.MethodGet:
				redirectToLogin(w, r)
			default:
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
			return
		}

		// CSRF guard: enforce on every method that mutates state.
		// GET / HEAD / OPTIONS are exempt (no side effects).
		if !isSafeMethod(r.Method) {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			supplied := r.PostFormValue(csrfFormField)
			if err := users.VerifyCSRF(sess, supplied); err != nil {
				s.deps.Logger.Warn("admin: csrf rejected",
					"path", r.URL.Path,
					"remote", r.RemoteAddr,
					"error", err)
				http.Error(w, "csrf token mismatch", http.StatusForbidden)
				return
			}
		}

		ctx := context.WithValue(r.Context(), ctxKeySession, sess)
		next(w, r.WithContext(ctx))
	}
}

// currentSession returns the session matching the request's session
// cookie, or nil if absent / expired.
func (s *Server) currentSession(r *http.Request) *users.Session {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	sess, ok := s.deps.Sessions.Get(cookie.Value)
	if !ok {
		return nil
	}
	return sess
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	target := "/admin/login"
	if r.URL.Path != "" && r.URL.Path != "/admin/login" {
		// Best-effort: preserve the post-login destination so the
		// operator lands where they tried to go after sign-in.
		// safeNext rejects anything that isn't a pure absolute
		// same-origin path; the open-redirect guard in
		// handleLoginPost rechecks before consuming `next`.
		target = "/admin/login?next=" + url.QueryEscape(safeNext(r.URL.Path))
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// isHTMXRequest reports whether the caller is HTMX (or a stand-in
// that mirrors its convention). HTMX sets `HX-Request: true` on every
// XHR it issues; the header is absent on plain navigations.
func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// htmxLoginTarget builds the value for the HX-Redirect header when an
// HTMX caller is unauthenticated. We preserve a same-origin GET path
// in `next` so the operator lands back where they were after sign-in;
// for non-GET HTMX calls (toggles, saves) we just send them to the
// dashboard root.
func htmxLoginTarget(r *http.Request) string {
	if r.Method == http.MethodGet && r.URL.Path != "" && r.URL.Path != "/admin/login" {
		return "/admin/login?next=" + url.QueryEscape(safeNext(r.URL.Path))
	}
	return "/admin/login"
}

// safeNext URL-escapes the path and rejects anything that isn't a
// pure absolute path beginning with "/". Returns "/admin" as a safe
// fallback otherwise.
func safeNext(p string) string {
	if len(p) == 0 || p[0] != '/' {
		return "/admin"
	}
	// Disallow protocol-relative URLs ("//evil.example/...").
	if len(p) >= 2 && p[1] == '/' {
		return "/admin"
	}
	return p
}

// requestLogger wraps the mux to emit one structured log line per
// request. Routed to deps.RequestLogger when wired so the access log
// — which can be very spammy due to 1-2s fragment polling — lives in
// its own daily-rotated file rather than drowning out the main log.
// Falls back to deps.Logger when no dedicated request logger is set
// (test harnesses, embedded scenarios).
//
// Static asset requests are demoted to debug regardless of which sink
// is active, in case operators raise the request log to debug for
// triage.
func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		dur := time.Since(start)

		sink := s.deps.RequestLogger
		if sink == nil {
			sink = s.deps.Logger
		}

		args := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"bytes", rw.bytes,
			"duration", dur,
			"remote", r.RemoteAddr,
		}
		if r.URL.Path == "/admin/static/" || hasPrefix(r.URL.Path, "/admin/static/") {
			sink.Debug("admin request", args...)
		} else {
			sink.Info("admin request", args...)
		}
	})
}

// statusRecorder is a tiny ResponseWriter wrapper that captures the
// status code and byte count for logging. No timing data — that's
// computed by the caller.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// hasPrefix is a tiny stdlib-substitute. Not strings.HasPrefix to
// avoid a 1-line import for a hot middleware path.
func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
