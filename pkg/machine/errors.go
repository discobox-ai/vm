package machine

import "errors"

// errorCodes names the machine sentinels on the wire. Codes are stable
// across both the served driver's JSON and the guest relay's headers.
var errorCodes = []struct {
	code string
	err  error
}{
	{"not-found", ErrNotFound},
	{"exists", ErrExists},
	{"in-use", ErrInUse},
	{"committed", ErrCommitted},
	{"capacity", ErrCapacity},
	{"not-running", ErrNotRunning},
	{"stale", ErrStale},
	{"unsupported", ErrUnsupported},
	{"not-implemented", ErrNotImplemented},
	{"no-key", ErrNoKey},
}

// ErrorCode returns the wire code of the first machine sentinel err wraps,
// or an empty string if none matches.
func ErrorCode(err error) string {
	for _, s := range errorCodes {
		if errors.Is(err, s.err) {
			return s.code
		}
	}
	return ""
}

// ErrorSentinel returns the machine sentinel named by code, or nil for an
// unknown code. Unknown codes remain readable as the remote error's message.
func ErrorSentinel(code string) error {
	for _, s := range errorCodes {
		if s.code == code {
			return s.err
		}
	}
	return nil
}
