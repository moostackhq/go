package password

import (
	"context"
	"errors"
)

// User is the password backend's view of a user row. Apps decide the
// shape of [User.ID] — see the README's UUIDv7 recommendation — and
// supply the persistence implementation via [Store].
//
// The shape is intentionally minimal: ID, Email, PassHash. There is
// no Name field — the password backend treats display names as out
// of scope and never populates [auth.Identity.Name]. Apps that want
// a display name keep it in their own user table and look it up by
// Subject after [auth.FromContext].
type User struct {
	ID       string // assigned by Store.Create
	Email    string
	PassHash []byte
}

// Store is the user table the password provider talks to. All
// methods take a context so callers can cancel and so timeouts
// propagate via the request's deadline.
//
// The provider holds the only references to PassHash; it never
// leaks the hash to handlers or templates.
type Store interface {
	// LookupByEmail returns the user matching email or
	// [ErrUserNotFound] if no such row exists. Other errors
	// (network, query) propagate as-is.
	LookupByEmail(ctx context.Context, email string) (User, error)

	// Create inserts a new user with the given email and
	// password hash. The implementation assigns User.ID and
	// returns the resulting row. Returns [ErrEmailTaken] if a
	// user with that email already exists.
	Create(ctx context.Context, email string, passHash []byte) (User, error)

	// SetPassword updates the hash for the user identified by ID.
	// Returns [ErrUserNotFound] if no such user exists.
	//
	// Reserved for app-side use — the password package itself does
	// not invoke SetPassword. It is part of the [Store] contract so
	// apps can wire their own change-password and admin-reset
	// endpoints against the same backing store, using
	// [BcryptHasher.Hash] (or whatever [Hasher] they configured) to
	// produce the passHash argument.
	SetPassword(ctx context.Context, userID string, passHash []byte) error
}

// ErrUserNotFound is the sentinel a [Store] returns when a lookup
// finds no matching user. The provider translates it to a generic
// auth failure on the wire — never reveals to the client whether
// the email was registered.
var ErrUserNotFound = errors.New("password: user not found")

// ErrEmailTaken is the sentinel [Store.Create] returns when a user
// with the requested email already exists. The provider translates
// it to a generic register failure.
var ErrEmailTaken = errors.New("password: email already registered")
