package machine

import (
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	debugMu    sync.Mutex
	debugOut   io.Writer
	debugStart time.Time
)

// SetDebug sends a line to w for every call a driver makes to a service
// outside this process (boxd's API, the Docker daemon), with how long the
// answer took. nil turns it off. It is `disco-vm --debug`.
func SetDebug(w io.Writer) {
	debugMu.Lock()
	defer debugMu.Unlock()
	debugOut, debugStart = w, time.Now()
}

// Debugging reports whether SetDebug is on, so a driver can skip building a
// line nobody reads.
func Debugging() bool {
	debugMu.Lock()
	defer debugMu.Unlock()
	return debugOut != nil
}

// DebugCall reports one finished call: how long it took, what it was, and how
// it ended. Each line starts with the time since SetDebug at which the call
// finished, so a gap between two lines is time spent outside any call.
func DebugCall(started time.Time, err error, format string, args ...any) {
	debugMu.Lock()
	defer debugMu.Unlock()
	if debugOut == nil {
		return
	}
	result := ""
	if err != nil {
		result = ": " + err.Error()
	}
	fmt.Fprintf(debugOut, "[+%8.3fs] %7s  %s%s\n", time.Since(debugStart).Seconds(),
		time.Since(started).Round(time.Millisecond), fmt.Sprintf(format, args...), result)
}
