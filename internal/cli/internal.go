package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/pkg/guest"
)

// guestCommand is the agent. It is the same binary, built for the guest's OS
// and started at boot: a SYSTEM service on Windows, a launchd daemon on macOS.
func guestCommand() *cobra.Command {
	var listen, addrFile, root string
	var fake bool
	cmd := &cobra.Command{
		Use:   "guest",
		Short: "Run the guest agent (inside a VM)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
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
			server := &guest.Server{Version: Version, Root: root, Fake: fake}
			return guest.RunAgent(func() error { return server.Serve(listener) }, func() { _ = listener.Close() })
		},
	}
	cmd.Flags().StringVar(&listen, "listen", fmt.Sprintf("vsock:%d", guest.AgentPort), "listen address (vsock:PORT or tcp:ADDR)")
	cmd.Flags().StringVar(&addrFile, "addr-file", "", "write the bound address here once listening")
	cmd.Flags().StringVar(&root, "root", "", "confine file paths and processes to this directory (fake driver)")
	cmd.Flags().BoolVar(&fake, "fake", false, "shutdown exits the agent instead of powering off (fake driver)")
	for _, name := range []string{"addr-file", "root", "fake"} {
		_ = cmd.Flags().MarkHidden(name)
	}
	return cmd
}

// shimCommand is one instance's supervisor, started by `start` and `run`, or
// with --warm one image's stage, started by `warm`.
func shimCommand(g *globals) *cobra.Command {
	var warm, gui bool
	cmd := &cobra.Command{
		Use:    "shim [--gui] INSTANCE | shim --warm LAYER",
		Short:  "Own one running instance or warm stage (started by disco-vm itself)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			shimArgs := args
			if warm {
				shimArgs = append([]string{"--warm"}, shimArgs...)
			}
			if gui {
				shimArgs = append([]string{"--gui"}, shimArgs...)
			}
			return e.ShimMain(cmd.Context(), shimArgs)
		},
	}
	cmd.Flags().BoolVar(&warm, "warm", false, "stage LAYER and hold the stage")
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
