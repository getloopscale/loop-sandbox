package domain

import (
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// Limits on the caller-supplied attributes of a sandbox. They bound the row size
// and what an exec has to carry; the values follow what comparable APIs accept.
const (
	MaxMetadataEntries  = 32
	MaxMetadataKeyLen   = 64
	MaxMetadataValueLen = 256
	MaxEnvEntries       = 64
	MaxEnvNameLen       = 128
	MaxEnvValueLen      = 4096
)

// ValidationError says which input field was rejected and why. It is an
// ErrInvalidArgument whose message is written for the API caller, so the HTTP
// layer can return it as is.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

// Is makes errors.Is(err, ErrInvalidArgument) hold for a ValidationError.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalidArgument }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

var (
	metadataKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidateMetadata checks caller-supplied sandbox labels. Keys are restricted to a
// safe alphabet because they appear in list filters and in logs.
func ValidateMetadata(metadata map[string]string) error {
	if len(metadata) > MaxMetadataEntries {
		return invalid("metadata", "has %d entries, the limit is %d", len(metadata), MaxMetadataEntries)
	}
	for k, v := range metadata {
		if len(k) > MaxMetadataKeyLen || !metadataKeyRE.MatchString(k) {
			return invalid("metadata", "key %q must be 1 to %d characters of letters, digits, '.', '_', '/' or '-', starting with a letter or digit", k, MaxMetadataKeyLen)
		}
		if len(v) > MaxMetadataValueLen || !utf8.ValidString(v) {
			return invalid("metadata", "value of %q must be valid UTF-8 of at most %d bytes", k, MaxMetadataValueLen)
		}
	}
	return nil
}

// ValidateEnv checks the default environment of a sandbox. Names follow the POSIX
// rule for portable environment variable names.
func ValidateEnv(env map[string]string) error {
	if len(env) > MaxEnvEntries {
		return invalid("env", "has %d variables, the limit is %d", len(env), MaxEnvEntries)
	}
	for name, v := range env {
		if len(name) > MaxEnvNameLen || !envNameRE.MatchString(name) {
			return invalid("env", "name %q must be at most %d characters of letters, digits or '_', not starting with a digit", name, MaxEnvNameLen)
		}
		if len(v) > MaxEnvValueLen {
			return invalid("env", "value of %q is %d bytes, the limit is %d", name, len(v), MaxEnvValueLen)
		}
		for i := 0; i < len(v); i++ {
			if v[i] == 0 {
				return invalid("env", "value of %q contains a NUL byte", name)
			}
		}
	}
	return nil
}

// SetAttributes sets the metadata and default environment of a new sandbox, after
// validating both. The maps are copied, so later changes by the caller do not
// reach the sandbox.
func (s *Sandbox) SetAttributes(metadata, env map[string]string) error {
	if err := ValidateMetadata(metadata); err != nil {
		return err
	}
	if err := ValidateEnv(env); err != nil {
		return err
	}
	s.Metadata = copyMap(metadata)
	s.Env = copyMap(env)
	return nil
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SetTTL makes the sandbox expire ttl from now, replacing its current expiry. It
// can shorten or extend the lifetime. Only a running or paused sandbox has a
// lifetime to change.
func (s *Sandbox) SetTTL(ttl time.Duration, now time.Time) error {
	if ttl <= 0 {
		return invalid("ttl_seconds", "must be positive")
	}
	if s.State != StateRunning && s.State != StatePaused {
		return ErrInvalidTransition
	}
	s.ExpiresAt = now.Add(ttl)
	s.UpdatedAt = now
	return nil
}

// MatchesMetadata reports whether the sandbox carries every key of want with the
// same value. An empty want matches every sandbox.
func (s *Sandbox) MatchesMetadata(want map[string]string) bool {
	for k, v := range want {
		if got, ok := s.Metadata[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// SandboxCursor identifies a position in a tenant's sandbox list: the creation
// time and id of the last sandbox of the previous page.
type SandboxCursor struct {
	CreatedAt time.Time
	ID        string
}

// SandboxQuery selects a page of a tenant's sandboxes, newest first.
type SandboxQuery struct {
	// States keeps only sandboxes in one of these states; empty keeps all.
	States []State
	// Metadata keeps only sandboxes that carry all of these labels.
	Metadata map[string]string
	// After, when set, returns sandboxes that come after this position.
	After *SandboxCursor
	// Limit is the maximum number of sandboxes to return; it must be positive.
	Limit int
}

// ComesAfter reports whether s sorts after cursor in the newest-first order
// (creation time descending, then id descending).
func (s *Sandbox) ComesAfter(cursor SandboxCursor) bool {
	if !s.CreatedAt.Equal(cursor.CreatedAt) {
		return s.CreatedAt.Before(cursor.CreatedAt)
	}
	return s.ID < cursor.ID
}

// ValidState reports whether state is one a sandbox can be in.
func ValidState(state State) bool {
	_, ok := validTransitions[state]
	return ok
}
