// Command demo wires the auth library end-to-end against an
// in-memory user store and an in-memory session backend.
//
//	go run ./example/demo
//
// Then:
//
//	# Anonymous request — Optional lets it through, /me returns null.
//	curl -i localhost:8080/me
//
//	# Login. Pre-seeded user: alice@example.com / password123.
//	curl -i -c jar.txt -H 'Content-Type: application/json' \
//	     -d '{"email":"alice@example.com","password":"password123"}' \
//	     localhost:8080/auth/user/login
//
//	# Authenticated request — the cookie carries the session.
//	curl -i -b jar.txt localhost:8080/api/me
//
//	# Register a new user.
//	curl -i -c jar.txt -H 'Content-Type: application/json' \
//	     -d '{"email":"new@example.com","password":"longenough"}' \
//	     localhost:8080/auth/user/register
//
//	# Logout — destroys the session.
//	curl -i -b jar.txt -X POST localhost:8080/auth/user/logout
//
//	# Forward-auth bypass — header bypasses session lookup entirely.
//	curl -i -H 'X-Remote-User: from-proxy@example.com' localhost:8080/api/me
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/moostackhq/go/auth"
	"github.com/moostackhq/go/auth/forwardauth"
	"github.com/moostackhq/go/auth/password"
	"github.com/moostackhq/go/router"
	"github.com/moostackhq/go/router/middleware"
	"github.com/moostackhq/go/session"
)

// AppSession is the app-owned session payload. Embedding auth.Identity
// by value lets *AppSession pick up auth.Identifiable via Go's method
// promotion — no method on AppSession itself. Everything else in the
// struct is the app's business.
type AppSession struct {
	auth.Identity
	Locale   string
	CartSize int
}

func main() {
	// --- session backend -----------------------------------------
	sessMgr, err := session.New(session.Config[AppSession]{
		Store:          session.NewMemoryStore[AppSession](),
		Token:          session.Cookie{Name: "auth_session", HttpOnly: true, SameSite: http.SameSiteLaxMode},
		AbsoluteExpiry: 24 * time.Hour,
		IdleExpiry:     time.Hour,
	})
	if err != nil {
		panic(err)
	}

	// --- user store with a seeded account ------------------------
	users := newUserStore()
	if _, err := users.Create(context.Background(), "alice@example.com", mustHash("password123")); err != nil {
		panic(err)
	}

	// --- backends ------------------------------------------------
	// password takes the session manager directly. T = AppSession,
	// PT = *AppSession both inferred from sessMgr's type.
	ph := password.New(users, sessMgr, password.Options{
		RegisterEnabled:   true,
		MinPasswordLength: 10,
	})
	fa := forwardauth.New(forwardauth.Options{UserHeader: "X-Remote-User"})

	// --- chain composition ---------------------------------------
	// Header-based authenticators run before the session-reading
	// backend so an API client with a trusted header isn't mistaken
	// for a stale browser session. ph is itself an Authenticator —
	// its Authenticate method reads identity from the session.
	authChain := auth.Chain(fa, ph)

	// --- router --------------------------------------------------
	r := router.New()
	r.Use(
		middleware.RequestID(),
		middleware.Logger(),
		middleware.Compress(),
		middleware.Recover(),
		sessMgr.Middleware,
	)

	// Password's three routes are mounted via r.Group at the prefix
	// the app chooses — here /auth/user, so the resulting routes are
	// /auth/user/login, /auth/user/register, /auth/user/logout.
	// Group bakes the prefix into the registered patterns at the mux
	// level; no Mount gymnastics, no dispatch-time stripping.
	r.Group("/auth/user", ph.RegisterRoutes)

	// Optional populates identity in context if present, doesn't 401.
	r.Use(auth.Optional(authChain))

	// Public endpoint — shows identity if there is one.
	r.Get("/me", func(w http.ResponseWriter, req *http.Request) {
		id, ok := auth.FromContext(req.Context())
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"identity": nil, "note": "anonymous"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"identity": id})
	})

	// Protected API — Required 401s on miss.
	r.Group("/api", func(api *router.Router) {
		api.Use(auth.Required(authChain))
		api.Get("/me", func(w http.ResponseWriter, req *http.Request) {
			id, _ := auth.FromContext(req.Context())
			writeJSON(w, http.StatusOK, id)
		})
	})

	fmt.Println("listening on :8080 — see curl examples in the package doc")
	if err := http.ListenAndServe(":8080", r); err != nil {
		panic(err)
	}
}

// --- in-memory user store -------------------------------------------

type userStore struct {
	mu    sync.Mutex
	users map[string]password.User // key: lowercased email
}

func newUserStore() *userStore { return &userStore{users: map[string]password.User{}} }

func (s *userStore) LookupByEmail(_ context.Context, email string) (password.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[strings.ToLower(email)]
	if !ok {
		return password.User{}, password.ErrUserNotFound
	}
	return u, nil
}

func (s *userStore) Create(_ context.Context, email string, hash []byte) (password.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email = strings.ToLower(email)
	if _, exists := s.users[email]; exists {
		return password.User{}, password.ErrEmailTaken
	}
	u := password.User{ID: newID(), Email: email, PassHash: hash}
	s.users[email] = u
	return u, nil
}

func (s *userStore) SetPassword(_ context.Context, userID string, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, u := range s.users {
		if u.ID == userID {
			u.PassHash = hash
			s.users[k] = u
			return nil
		}
	}
	return password.ErrUserNotFound
}

// --- helpers ---------------------------------------------------------

func mustHash(plain string) []byte {
	h, err := password.BcryptHasher{Cost: password.DefaultBcryptCost}.Hash(plain)
	if err != nil {
		panic(err)
	}
	return h
}

func newID() string {
	// Production apps use UUIDv7 — see the README. Random hex keeps
	// the demo dependency-free.
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(errors.New("rand.Read failed"))
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
