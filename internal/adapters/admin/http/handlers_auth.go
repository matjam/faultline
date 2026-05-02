package adminhttp

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/matjam/faultline/internal/adapters/auth/users"
)

// loginData drives the login template.
type loginData struct {
	Title         string
	Authenticated bool
	Username      string // pre-fill on retry
	Error         string
	CSRFToken     string // pre-session CSRF (form-only, not yet bound)
	Next          string
	UI            string
	Theme         string
	IsModern      bool
	IsMatrix      bool
}

// handleLoginGet renders the login form. Already-authenticated users
// are redirected straight to the dashboard so the form isn't
// reachable when there's nothing to do.
//
// We attach a *transient* CSRF token to the form via a short-lived
// cookie, so even pre-authentication forms cannot be cross-site
// posted. On successful login the real session takes over.
func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if sess := s.currentSession(r); sess != nil {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}

	csrf, err := s.ensureLoginCSRF(w, r)
	if err != nil {
		s.deps.Logger.Error("admin: issue login csrf", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	data := loginData{
		Title:         "Sign in",
		Authenticated: false,
		CSRFToken:     csrf,
		Next:          safeNext(r.URL.Query().Get("next")),
		UI:            s.deps.UI,
		Theme:         themeForUI(s.deps.UI),
		IsModern:      s.deps.UI == "modern",
		IsMatrix:      s.deps.UI == "matrix",
	}
	s.render(w, "login.html", data)
}

// handleLoginPost validates credentials and (on success) mints a
// session, sets the session cookie, and redirects to the requested
// `next` (defaulting to /admin). Failures re-render the form with a
// generic error — we never disclose whether the username existed.
func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !s.checkLoginCSRF(r) {
		s.deps.Logger.Warn("admin: login csrf rejected", "remote", r.RemoteAddr)
		http.Error(w, "csrf token mismatch", http.StatusForbidden)
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	next := safeNext(r.PostFormValue("next"))
	if next == "" {
		next = "/admin"
	}

	user, err := s.deps.Users.Verify(username, password)
	if err != nil {
		// Generic error message; do not leak whether user exists.
		s.deps.Logger.Info("admin: login failed",
			"username", username,
			"remote", r.RemoteAddr,
			"reason", classify(err))
		// Refresh (or mint, if missing) the login CSRF for the retry.
		csrf, ierr := s.ensureLoginCSRF(w, r)
		if ierr != nil {
			s.deps.Logger.Error("admin: issue login csrf", "error", ierr)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		data := loginData{
			Title:         "Sign in",
			Authenticated: false,
			Username:      username,
			Error:         "Invalid username or password.",
			CSRFToken:     csrf,
			Next:          next,
			UI:            s.deps.UI,
			Theme:         themeForUI(s.deps.UI),
			IsModern:      s.deps.UI == "modern",
			IsMatrix:      s.deps.UI == "matrix",
		}
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", data)
		return
	}

	sess, err := s.deps.Sessions.New(user.Name)
	if err != nil {
		s.deps.Logger.Error("admin: new session", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Set the session cookie. HttpOnly + Secure + SameSite=Lax covers
	// browser-side XSS, cleartext transit, and naive CSRF.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.Token,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		// Session cookie (no MaxAge): expires when the browser
		// closes, in addition to the server-side TTL eviction.
	})

	// Clear the transient login-CSRF cookie now that the real
	// session has taken over.
	clearLoginCSRF(w)

	s.deps.Logger.Info("admin: login ok", "username", user.Name, "remote", r.RemoteAddr)
	redirectSameOrigin(w, next)
}

// handleLogout deletes the session and redirects to the login form.
// Reachable only from within requireAuth, so the session is known
// to exist.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess := sessionFromContext(r.Context())
	s.deps.Sessions.Delete(sess.Token)

	// Clear the cookie regardless of session state.
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   true,
		MaxAge:   -1,
	})

	s.deps.Logger.Info("admin: logout", "username", sess.Username)
	redirectSameOrigin(w, "/admin/login")
}

// classify returns a short string for log purposes without leaking
// which credential half was wrong (the operator log file already
// captures everything; the network response says "invalid").
func classify(err error) string {
	switch {
	case errors.Is(err, users.ErrUserNotFound):
		return "no-such-user"
	case errors.Is(err, users.ErrPasswordMismatch):
		return "wrong-password"
	case errors.Is(err, users.ErrInvalidHash), errors.Is(err, users.ErrUnsupportedHash):
		return "corrupt-hash"
	default:
		return "other"
	}
}

// --- login-form CSRF ------------------------------------------------
//
// The login form has no session yet, so the standard per-session
// CSRF token doesn't apply. Instead we attach a short-lived random
// token in a separate cookie and require the form to echo it back.
//
// The cookie is *stable* across renders: we keep the same value as
// long as the browser still has the cookie, only refreshing its
// MaxAge. Rotating per render was tempting but turned out to race
// with concurrent fragment polling — a stale tab polling
// /admin/fragments/* gets 401-redirected to the login page (or a
// non-HTMX client follows a 303), each redirect-follow re-rendered
// the login form with a fresh cookie value, and any login form the
// user already had open lost its CSRF match. Stability removes the
// race; security is unchanged because the cookie is HttpOnly +
// SameSite=Strict (no cross-site read or set), so an attacker can't
// learn the value and the "form must echo cookie" check still does
// the work.

const loginCSRFCookie = "faultline_login_csrf"

const loginCSRFTTL = 10 * time.Minute

// ensureLoginCSRF returns the login-form CSRF token to embed in the
// rendered form. If the request already carries a valid login-CSRF
// cookie we reuse its value and refresh the cookie's MaxAge;
// otherwise we mint and set a fresh one.
func (s *Server) ensureLoginCSRF(w http.ResponseWriter, r *http.Request) (string, error) {
	if existing, err := r.Cookie(loginCSRFCookie); err == nil && existing.Value != "" {
		// Refresh MaxAge so an actively-used login page doesn't
		// time out mid-edit. Same value, same attributes — the
		// browser updates its TTL.
		setLoginCSRFCookie(w, existing.Value)
		return existing.Value, nil
	}
	tok, err := randomLoginToken()
	if err != nil {
		return "", err
	}
	setLoginCSRFCookie(w, tok)
	return tok, nil
}

func setLoginCSRFCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginCSRFCookie,
		Value:    value,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   true,
		MaxAge:   int(loginCSRFTTL.Seconds()),
	})
}

func (s *Server) checkLoginCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(loginCSRFCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	supplied := r.PostFormValue(csrfFormField)
	if supplied == "" {
		return false
	}
	// Constant-time compare via the helper we already use for
	// session CSRF; build a synthetic Session so we can reuse it.
	return users.VerifyCSRF(&users.Session{CSRFToken: cookie.Value}, supplied) == nil
}

func clearLoginCSRF(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     loginCSRFCookie,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   true,
		MaxAge:   -1,
	})
}

func redirectSameOrigin(w http.ResponseWriter, target string) {
	w.Header().Set("Location", safeNext(target))
	w.WriteHeader(http.StatusSeeOther)
}

// randomLoginToken matches the entropy of session tokens but lives
// in its own helper so the package's session/token surface can
// evolve independently.
func randomLoginToken() (string, error) {
	return users.NewToken()
}
