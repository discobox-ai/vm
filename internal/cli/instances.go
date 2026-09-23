package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

type createFlags struct {
	name, memory, mode string
	cpus               int
}

func (f *createFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.name, "name", "", "instance name (default: its ID)")
	cmd.Flags().IntVar(&f.cpus, "cpus", 0, "vCPUs (default: the driver's)")
	cmd.Flags().StringVar(&f.memory, "memory", "", "memory, e.g. 8GiB (default: the driver's)")
	cmd.Flags().StringVar(&f.mode, "mode", "cold", "clone mode: cold, resume (vz), or fork (hcs)")
}

func (f *createFlags) create(ctx context.Context, e *engine.Engine, ref string) (*engine.Instance, error) {
	memory, err := units.ParseBytes(f.memory)
	if err != nil {
		return nil, err
	}
	return e.Create(ctx, ref, engine.CreateOptions{
		Name: f.name, CPUs: f.cpus, Memory: memory, Mode: machine.CloneMode(f.mode),
	})
}

func createCommand(g *globals) *cobra.Command {
	var flags createFlags
	cmd := &cobra.Command{
		Use:   "create IMAGE",
		Short: "Create an instance without starting it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			inst, err := flags.create(cmd.Context(), e, args[0])
			if err != nil {
				return err
			}
			fmt.Println(inst.ID)
			return nil
		},
	}
	flags.register(cmd)
	return cmd
}

func runCommand(g *globals) *cobra.Command {
	var (
		flags       createFlags
		remove, tty bool
		timeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "run [flags] IMAGE [-- COMMAND...]",
		Short: "Create and start an instance, and optionally run a command in it",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			inst, err := flags.create(ctx, e, args[0])
			if err != nil {
				return err
			}
			if err := e.Start(ctx, inst, engine.StartOptions{Timeout: timeout}); err != nil {
				if remove {
					_ = e.Remove(context.Background(), inst, true)
				}
				return err
			}
			if len(args) == 1 {
				fmt.Println(inst.ID)
				return nil
			}
			runErr := execIn(ctx, e, inst, execOptions{argv: args[1:], tty: tty, stdin: tty})
			if remove {
				if err := e.Remove(context.Background(), inst, true); err != nil && runErr == nil {
					runErr = err
				}
			}
			return runErr
		},
	}
	flags.register(cmd)
	cmd.Flags().BoolVar(&remove, "rm", false, "remove the instance when the command exits")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "run the command in a terminal")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the guest agent")
	return cmd
}

func startCommand(g *globals) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "start INSTANCE...",
		Short: "Start instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return eachInstance(g, args, func(e *engine.Engine, inst *engine.Instance) error {
				return e.Start(cmd.Context(), inst, engine.StartOptions{Timeout: timeout})
			})
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the guest agent")
	return cmd
}

func stopCommand(g *globals) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "stop INSTANCE...",
		Short: "Shut instances down in order, forcing them off after a timeout",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return eachInstance(g, args, func(e *engine.Engine, inst *engine.Instance) error {
				return e.Stop(cmd.Context(), inst, timeout)
			})
		},
	}
	cmd.Flags().DurationVarP(&timeout, "time", "t", time.Minute, "how long to wait before forcing the guest off")
	return cmd
}

func rmCommand(g *globals) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm INSTANCE...",
		Short: "Remove instances",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return eachInstance(g, args, func(e *engine.Engine, inst *engine.Instance) error {
				return e.Remove(cmd.Context(), inst, force)
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "stop a running instance first")
	return cmd
}

func eachInstance(g *globals, refs []string, fn func(*engine.Engine, *engine.Instance) error) error {
	e, err := g.engine()
	if err != nil {
		return err
	}
	var errs []error
	for _, ref := range refs {
		inst, err := e.Get(ref)
		if err == nil {
			err = fn(e, inst)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ref, err))
			continue
		}
		fmt.Println(inst.Name)
	}
	return errors.Join(errs...)
}

func psCommand(g *globals) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List instances",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			instances, err := e.List()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tIMAGE\tOS\tSTATE\tCREATED")
			for _, inst := range instances {
				state := e.State(cmd.Context(), inst)
				if state != engine.Running && !all {
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", inst.ID, inst.Name, inst.Image, inst.GuestOS, state, ago(inst.Created))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include stopped instances")
	return cmd
}

func inspectCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect INSTANCE",
		Short: "Show an instance, and its guest when it is running",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			inst, err := e.Get(args[0])
			if err != nil {
				return err
			}
			out := struct {
				*engine.Instance
				State engine.State `json:"state"`
				Guest *guest.Info  `json:"guest,omitempty"`
			}{Instance: inst, State: e.State(cmd.Context(), inst)}
			if out.State == engine.Running {
				client := e.Guest(inst)
				defer client.Close()
				if info, err := client.Info(cmd.Context()); err == nil {
					out.Guest = &info
				}
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		},
	}
}

type execOptions struct {
	argv       []string
	env        []string
	dir, user  string
	tty, stdin bool
}

func execCommand(g *globals) *cobra.Command {
	var opts execOptions
	cmd := &cobra.Command{
		Use:   "exec [flags] INSTANCE COMMAND...",
		Short: "Run a command in a running instance",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			inst, err := e.Get(args[0])
			if err != nil {
				return err
			}
			opts.argv = args[1:]
			return execIn(cmd.Context(), e, inst, opts)
		},
	}
	cmd.Flags().BoolVarP(&opts.tty, "tty", "t", false, "allocate a terminal")
	cmd.Flags().BoolVarP(&opts.stdin, "interactive", "i", false, "keep stdin open")
	cmd.Flags().StringArrayVarP(&opts.env, "env", "e", nil, "set an environment variable (KEY=VALUE)")
	cmd.Flags().StringVarP(&opts.dir, "workdir", "w", "", "working directory in the guest")
	cmd.Flags().StringVarP(&opts.user, "user", "u", "", "run as this guest account")
	// Everything after the instance belongs to the guest command.
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// execIn runs a command in an instance and turns its exit code into ours.
func execIn(ctx context.Context, e *engine.Engine, inst *engine.Instance, opts execOptions) error {
	client := e.Guest(inst)
	defer client.Close()
	req := guest.ExecRequest{Argv: opts.argv, Env: opts.env, Dir: opts.dir, User: opts.user, TTY: opts.tty}

	if !opts.tty {
		var stdin io.Reader
		if opts.stdin {
			stdin = os.Stdin
		}
		code, err := client.Run(ctx, req, stdin, os.Stdout, os.Stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			return exitError{code}
		}
		return nil
	}

	inFD, outFD := int(os.Stdin.Fd()), int(os.Stdout.Fd())
	if cols, rows, err := term.GetSize(outFD); err == nil {
		req.Rows, req.Cols = uint16(rows), uint16(cols)
	}
	proc, err := client.Exec(ctx, req)
	if err != nil {
		return err
	}
	defer proc.Close()
	if term.IsTerminal(inFD) {
		state, err := term.MakeRaw(inFD)
		if err != nil {
			return err
		}
		defer term.Restore(inFD, state)
	}
	// TODO: follow terminal resizes (SIGWINCH on unix, console events on
	// Windows); the size is set once, at start.
	go func() {
		_, _ = io.Copy(proc, os.Stdin)
		_ = proc.CloseStdin()
	}()
	code, err := proc.Wait(os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func infoCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show the driver, its capabilities, and whether this host can run it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			check := "ok"
			if err := e.Driver.Check(cmd.Context()); err != nil {
				check = err.Error()
			}
			caps, _ := json.MarshalIndent(e.Driver.Capabilities(), "  ", "  ")
			fmt.Printf("version:  %s\nroot:     %s\ndriver:   %s (available: %s)\ncheck:    %s\ncapabilities:\n  %s\n",
				Version, e.Root, e.Driver.Name(), strings.Join(machine.Names(), ", "), check, caps)
			return nil
		},
	}
}
