package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/internal/version"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine/docker"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

// guestCommand is the agent. It is the same binary, built for the guest's OS
// and started at boot: a SYSTEM service on Windows, a launchd daemon on macOS.
func guestCommand() *cobra.Command {
	var listen, addrFile, root string
	var fake, protocol bool
	cmd := &cobra.Command{
		Use:   "guest",
		Short: "Run the guest agent (inside a VM)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if protocol {
				// What a host moving this agent into a guest runs first, to
				// see the binary it uploaded is an agent that speaks its
				// protocol (vz's moveAgent).
				_, err := fmt.Fprintln(cmd.OutOrStdout(), guest.ServedProtocol())
				return err
			}
			listener, err := guest.Listen(listen)
			if err != nil {
				return err
			}
			if addrFile != "" {
				addr := listener.Addr().String()
				if err := os.WriteFile(addrFile+".tmp", []byte(addr), 0o600); err != nil {
					return err
				}
				if err := os.Rename(addrFile+".tmp", addrFile); err != nil {
					return err
				}
			}
			if root != "" {
				if err := os.MkdirAll(root, 0o755); err != nil {
					return err
				}
			}
			server := &guest.Server{Version: version.String(), Root: root, Fake: fake}
			if !fake {
				// The desktop, for a viewer on the host: a Windows guest's
				// only; elsewhere it returns at once. Failing to serve it
				// leaves the agent's own port serving.
				go func() {
					if err := guest.ServeDisplay(); err != nil {
						fmt.Fprintln(os.Stderr, "disco-vm guest: display:", err)
					}
				}()
			}
			// The root command turns SIGTERM into a canceled context. The
			// agent must still exit on it: the guest's init sends it at
			// shutdown, and waits out its stop timeout for an agent that
			// does not go.
			go func() {
				<-cmd.Context().Done()
				_ = listener.Close()
			}()
			serve := func() error {
				if err := server.Serve(listener); cmd.Context().Err() == nil {
					return err
				}
				return nil
			}
			return guest.RunAgent(serve, func() { _ = listener.Close() })
		},
	}
	cmd.Flags().StringVar(&listen, "listen", fmt.Sprintf("vsock:%d", guest.AgentPort), "listen address (vsock:PORT, tcp:ADDR, or unix:PATH)")
	cmd.Flags().StringVar(&addrFile, "addr-file", "", "write the bound address here once listening")
	cmd.Flags().StringVar(&root, "root", "", "confine file paths and processes to this directory (fake driver)")
	cmd.Flags().BoolVar(&fake, "fake", false, "shutdown exits the agent instead of powering off (fake driver)")
	cmd.Flags().BoolVar(&protocol, "protocol", false, "print the agent protocol this binary speaks and exit")
	for _, name := range []string{"addr-file", "root", "fake", "protocol"} {
		_ = cmd.Flags().MarkHidden(name)
	}
	return cmd
}

// pipeCommand relays its stdin and stdout to a port inside the guest. A driver
// with no hypervisor socket to the guest (boxd) dials a guest port by running
// this through its own authenticated exec stream.
func pipeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "pipe tcp:ADDR|unix:PATH",
		Short:  "Relay stdin and stdout to an address inside the guest",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return guest.Pipe(cmd.Context(), args[0], os.Stdin, os.Stdout)
		},
	}
}

// consoleHostCommand makes a Windows pseudo console as the account it runs as
// and runs a command on it. The agent starts it for a TTY session as another
// account, whose console must be that account's own (guest.ConsoleHost).
func consoleHostCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "console-host ROWS COLS APP CMDLINE",
		Short:  "Run a command on a pseudo console of this account's own (started by the guest agent)",
		Hidden: true,
		Args:   cobra.ExactArgs(4),
		RunE: func(_ *cobra.Command, args []string) error {
			rows, err := strconv.ParseUint(args[0], 10, 16)
			if err != nil {
				return fmt.Errorf("rows %q: %w", args[0], err)
			}
			cols, err := strconv.ParseUint(args[1], 10, 16)
			if err != nil {
				return fmt.Errorf("cols %q: %w", args[1], err)
			}
			code, err := guest.ConsoleHost(uint16(rows), uint16(cols), args[2], args[3], os.Stdin, os.Stdout)
			if err != nil {
				return err
			}
			if code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
}

// displayHostCommand shows the desktop it runs on to a viewer on its stdin
// and stdout. The agent starts it in the session of whoever is logged on to
// a Windows guest's console (guest.ServeDisplay).
func displayHostCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "display-host",
		Short:  "Stream this desktop over stdin and stdout (started by the guest agent)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return guest.DisplayHost(os.Stdin, os.Stdout)
		},
	}
}

// dockerInitCommand is a docker guest's PID 1, which starts systemd in a user
// namespace of the guest's own; with --shift it is the docker driver's install
// step that moves the base image's files into the guest's uids.
func dockerInitCommand() *cobra.Command {
	var shift bool
	cmd := &cobra.Command{
		Use:    "docker-init [--shift]",
		Short:  "Boot a docker guest's systemd in its own user namespace (inside the container)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if shift {
				return docker.Shift()
			}
			return docker.Init()
		},
	}
	cmd.Flags().BoolVar(&shift, "shift", false, "shift the root filesystem's owners into the guest's uids")
	return cmd
}

// dockerVolumesCommand is one step of the docker driver's directory work,
// in a helper container on the daemon's host that mounts the driver's root.
func dockerVolumesCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "docker-volumes REQUEST",
		Short:  "Make, copy, and remove the docker driver's directories (inside a helper container)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return docker.Dirs(args[0])
		},
	}
}

// shimCommand is one instance's supervisor, which a driver on this host's
// disk starts for `start` and `run`.
func shimCommand(g *globals) *cobra.Command {
	var gui bool
	cmd := &cobra.Command{
		Use:    "shim [--gui] INSTANCE",
		Short:  "Own one running instance (started by disco-vm itself)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			driver, err := g.newDriver(g.driver, g.pool)
			if err != nil {
				return err
			}
			shimArgs := args
			if gui {
				shimArgs = append([]string{"--gui"}, shimArgs...)
			}
			return local.ShimMain(cmd.Context(), driver, shimArgs)
		},
	}
	cmd.Flags().BoolVar(&gui, "gui", false, "show the guest's display in a window")
	return cmd
}

// dialHostCommand connects to the host from inside a guest and joins the
// connection to stdin and stdout, for shells and tests: it is what a guest
// program does with guest.DialHost.
func dialHostCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "dial-host PORT",
		Short:  "Connect to the host on PORT from inside a guest, over stdin and stdout",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			port, err := strconv.ParseUint(args[0], 10, 32)
			if err != nil {
				return fmt.Errorf("port %q: %w", args[0], err)
			}
			conn, err := guest.DialHost(uint32(port))
			if err != nil {
				return err
			}
			defer conn.Close()
			go func() {
				_, _ = io.Copy(conn, os.Stdin)
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			}()
			_, err = io.Copy(os.Stdout, conn)
			return err
		},
	}
}
