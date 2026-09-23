package cli

import (
	"fmt"
	"os"

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
			return server.Serve(listener)
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
	var warm bool
	cmd := &cobra.Command{
		Use:    "shim INSTANCE | shim --warm LAYER",
		Short:  "Own one running instance or warm stage (started by disco-vm itself)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			if warm {
				return e.RunWarmShim(cmd.Context(), args[0])
			}
			return e.RunShim(cmd.Context(), args[0])
		},
	}
	cmd.Flags().BoolVar(&warm, "warm", false, "stage LAYER and hold the stage")
	return cmd
}
