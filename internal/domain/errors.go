package domain

import "errors"

// Typed domain errors (Unbreakable Rule 8 — fail-fast, explicit, typed).
// The API layer maps these to HTTP status codes (TRD §7.4) without leaking internals.
var (
	// ErrNotFound is returned when a sandbox does not exist (for the acting tenant).
	ErrNotFound = errors.New("sandbox not found")
	// ErrInvalidTransition is returned when a state transition violates the machine.
	ErrInvalidTransition = errors.New("invalid sandbox state transition")
	// ErrInvalidArgument is returned when input fails validation at the boundary.
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrAlreadyExists is returned when an operation would replace something that
	// exists and was not allowed to. The API layer maps it to 409.
	ErrAlreadyExists = errors.New("already exists")
	// ErrForbidden is returned when the caller is authenticated and lacks the
	// permission the operation needs. The API layer maps it to 403.
	ErrForbidden = errors.New("forbidden")
	// ErrTimeout is returned when an operation exceeds its deadline.
	ErrTimeout = errors.New("operation timed out")
	// ErrNoCapacity is returned when no ready node can host a new sandbox (M21). The API layer
	// maps it to 503 (a transient, retriable condition), never a 500.
	ErrNoCapacity = errors.New("no node has capacity")
)
