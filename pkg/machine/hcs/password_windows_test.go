package hcs

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
)

// answering is a guest whose every exec prints out and exits with code, or
// fails to start with err.
type answering struct {
	machine.Guest
	out  string
	code int
	err  error
	argv []string
}

func (g *answering) Exec(_ context.Context, req machine.ExecRequest) (machine.Process, error) {
	g.argv = req.Argv
	if g.err != nil {
		return nil, g.err
	}
	return &answer{g: g}, nil
}

type answer struct{ g *answering }

func (a *answer) Write(p []byte) (int, error) { return len(p), nil }
func (a *answer) CloseStdin() error           { return nil }
func (a *answer) Resize(uint16, uint16) error { return nil }
func (a *answer) Close() error                { return nil }
func (a *answer) Wait(stdout, _ io.Writer) (int, error) {
	_, _ = io.WriteString(stdout, a.g.out)
	return a.g.code, nil
}

// A window asks the guest's agent whether the account's password is still
// its name, and takes nothing but the agent's two answers for one: an older
// agent's unknown command, or a failure, is no answer.
func TestPasswordIsNameTakesOnlyTheAgentsAnswer(t *testing.T) {
	for _, c := range []struct {
		name        string
		g           answering
		same, known bool
	}{
		{"name", answering{out: "name\r\n"}, true, true},
		{"other", answering{out: "other\n"}, false, true},
		{"an older agent", answering{out: "Error: unknown command \"password-is-name\"\n", code: 1}, false, false},
		{"help on stdout", answering{out: "Usage:\n  disco-vm [command]\n"}, false, false},
		{"a failed check", answering{code: 1}, false, false},
		{"no exec", answering{err: errors.New("no agent")}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			same, known := passwordIsName(t.Context(), &c.g, "disco")
			if same != c.same || known != c.known {
				t.Fatalf("passwordIsName = %v, %v; want %v, %v", same, known, c.same, c.known)
			}
			if want := []string{agentPath, "password-is-name", "disco"}; !slices.Equal(c.g.argv, want) {
				t.Fatalf("ran %q, want %q", c.g.argv, want)
			}
		})
	}
}
