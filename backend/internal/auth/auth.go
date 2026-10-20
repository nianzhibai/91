package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"log"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/video-site/backend/internal/catalog"
	"github.com/video-site/backend/internal/requestmeta"
)

const (
	sessionCookie      = "vs_admin"
	sessionTTL         = 7 * 24 * time.Hour
	sessionRenewBefore = sessionTTL / 2
	loginFailWindow    = 30 * time.Minute
	loginFailThreshold = 3
)

var ErrLoginIPBanned = errors.New("login ip banned")
var ErrUserBanned = errors.New("user is banned")

type Authenticator struct {
	Catalog *catalog.Catalog
	Now     func() time.Time
}

type sessionIdentityContextKey struct{}

// SessionIdentityFromContext returns an opaque, server-only identity for the
// authenticated login session. The raw session token is never exposed to
// downstream handlers or retained by their in-memory state.
func SessionIdentityFromContext(ctx context.Context) (string, bool) {
	identity, ok := ctx.Value(sessionIdentityContextKey{}).(string)
	return identity, ok && identity != ""
}

type userIDContextKey struct{}

// UserIDFromContext returns the authenticated account ID. Handlers use it for
// account-scoped data such as favorites; SessionIdentityFromContext above stays
// the opaque per-login-session handle used by per-session caches.
func UserIDFromContext(ctx context.Context) (int64, bool) {
	userID, ok := ctx.Value(userIDContextKey{}).(int64)
	return userID, ok && userID > 0
}

// WithUserID returns a context carrying the authenticated account ID, the same
// value UserIDFromContext reads. It lets callers outside this package build a
// request context for one account without replaying a login.
func WithUserID(ctx context.Context, userID int64) context.Context {
	if userID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, userIDContextKey{}, userID)
}

// CheckCurrentPassword re-authenticates the administrator represented by the
// request's current session against the stored bcrypt hash.
func (a *Authenticator) CheckCurrentPassword(r *http.Request, password string) (bool, error) {
	if a == nil || a.Catalog == nil || r == nil {
		return false, nil
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			return false, nil
		}
		return false, err
	}
	session, found, err := a.Catalog.GetSession(r.Context(), cookie.Value)
	if err != nil || !found {
		return false, err
	}
	if !a.now().Before(session.ExpiresAt) {
		return false, nil
	}
	if session.UserID <= 0 {
		return false, nil
	}
	user, err := a.Catalog.GetUserByID(r.Context(), session.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if user.Banned || user.Role != "admin" {
		return false, nil
	}
	return checkPassword(password, user.Password), nil
}

func (a *Authenticator) recordLoginAttempt(r *http.Request, ip string, successful bool) error {
	banned, err := a.Catalog.RecordLoginAttempt(r.Context(), ip, successful, a.now(), loginFailWindow, loginFailThreshold)
	if err != nil {
		return err
	}
	if banned {
		return ErrLoginIPBanned
	}
	return nil
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.Catalog.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:    sessionCookie,
		Value:   "",
		Path:    "/",
		Expires: time.Unix(0, 0),
	})
}

func (a *Authenticator) ValidateRequest(w http.ResponseWriter, r *http.Request) (bool, int64, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false, 0, nil
	}
	return a.validateSession(w, r, c.Value)
}

func (a *Authenticator) validateSession(w http.ResponseWriter, r *http.Request, token string) (bool, int64, error) {
	session, found, err := a.Catalog.GetSession(r.Context(), token)
	if err != nil || !found {
		return false, 0, err
	}
	if session.UserID <= 0 {
		return false, 0, nil
	}
	now := a.now()
	if !now.Before(session.ExpiresAt) {
		return false, 0, nil
	}
	if session.ExpiresAt.Sub(now) < sessionRenewBefore {
		expiresAt := now.Add(sessionTTL)
		if err := a.Catalog.UpdateSessionExpires(r.Context(), token, expiresAt); err != nil {
			return false, 0, err
		}
		setSessionCookie(w, token, expiresAt)
	}
	return true, session.UserID, nil
}

func (a *Authenticator) Required(next http.Handler) http.Handler {
	return a.require(next, false)
}

func withSessionIdentity(r *http.Request, userID int64) *http.Request {
	ctx := r.Context()
	if userID > 0 {
		ctx = context.WithValue(ctx, userIDContextKey{}, userID)
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return r.WithContext(ctx)
	}
	digest := sha256.Sum256([]byte(cookie.Value))
	identity := hex.EncodeToString(digest[:])
	return r.WithContext(context.WithValue(ctx, sessionIdentityContextKey{}, identity))
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// UserLogin authenticates a user (admin or regular) from the users table.
// Returns the role on success, empty string on failure.
func (a *Authenticator) UserLogin(w http.ResponseWriter, r *http.Request, user, pass string) (string, error) {
	ip := requestmeta.ClientIP(r)
	if ip != "" {
		banned, err := a.Catalog.IsLoginIPBanned(r.Context(), ip)
		if err != nil {
			return "", err
		}
		if banned {
			return "", ErrLoginIPBanned
		}
	}

	u, err := a.Catalog.GetUserByUsername(r.Context(), user)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if ip != "" {
			if err := a.recordLoginAttempt(r, ip, false); err != nil {
				return "", err
			}
		}
		return "", nil
	}

	if u.Banned {
		return "", ErrUserBanned
	}

	if !checkPassword(pass, u.Password) {
		if ip != "" {
			if err := a.recordLoginAttempt(r, ip, false); err != nil {
				return "", err
			}
		}
		return "", nil
	}

	if ip != "" {
		if err := a.recordLoginAttempt(r, ip, true); err != nil {
			return "", err
		}
	}

	token, err := randomToken()
	if err != nil {
		return "", err
	}
	expiresAt := a.now().Add(sessionTTL)
	created, err := a.Catalog.CreateVerifiedUserSession(r.Context(), token, expiresAt, u.ID, u.Password)
	if err != nil {
		return "", err
	}
	if !created {
		return "", nil
	}

	setSessionCookie(w, token, expiresAt)
	return u.Role, nil
}

// AdminRequired is like Required but additionally checks that the session
// belongs to a user with role="admin". Regular users get 403.
func (a *Authenticator) AdminRequired(next http.Handler) http.Handler {
	return a.require(next, true)
}

func (a *Authenticator) require(next http.Handler, adminOnly bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, userID, err := a.ValidateRequest(w, r)
		if err != nil {
			writeAuthUnavailable(w, r, "validate session", err)
			return
		}
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		u, err := a.Catalog.GetUserByID(r.Context(), userID)
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if err != nil {
			writeAuthUnavailable(w, r, "load session user", err)
			return
		}
		if u.Banned || (adminOnly && u.Role != "admin") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, withSessionIdentity(r, userID))
	})
}

func writeAuthUnavailable(w http.ResponseWriter, r *http.Request, operation string, err error) {
	// A canceled client is no longer waiting for a response. More importantly,
	// cancellation must not be translated into a misleading authentication
	// failure that makes the browser discard otherwise valid session state.
	if r.Context().Err() != nil {
		return
	}
	log.Printf("[auth] %s method=%s path=%s: %v", operation, r.Method, r.URL.Path, err)
	http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
}

func setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
	})
}

func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

func checkPassword(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}
