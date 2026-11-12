package guest

import (
	"net/http"

	"github.com/discobox-ai/vm/pkg/machine"
)

// errorCodeHeader adds sentinel identity without changing the text body or
// status older clients read. Missing and unknown codes carry no identity.
const errorCodeHeader = "Disco-Vm-Error-Code"

type remoteError struct {
	msg string
	err error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.err }

func responseError(resp *http.Response, msg string) error {
	return &remoteError{msg: msg, err: machine.ErrorSentinel(resp.Header.Get(errorCodeHeader))}
}
