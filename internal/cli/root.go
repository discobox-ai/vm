// Package cli is disco-vm's command line: a thin layer over pkg/engine and
// pkg/build.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	_ "github.com/discobox-ai/vm/pkg/machine/drivers"
)

// Version is set at link time.
var Version = "dev"

// exitError carries a guest process's exit code out as disco-vm's own.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

type globals struct {
	root   string
	driver string
}

func (g *globals) engine() (*engine.Engine, error) {
	driver, err := machine.New(g.driver)
	if err != nil {
		return nil, err
	}
	return engine.Open(g.root, driver)
}

// Main runs the command line and returns the process exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g := &globals{}
	root := &cobra.Command{
		Use:           "disco-vm",
		Short:         "Build OS images and run them as VMs, on Windows and macOS",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.PersistentFlags().StringVar(&g.root, "root", "", "state directory (default $"+engine.RootEnv+" or the per-OS data directory)")
	root.PersistentFlags().StringVar(&g.driver, "driver", os.Getenv("DISCO_VM_DRIVER"), "hypervisor driver (default "+machine.DefaultName()+"; also $DISCO_VM_DRIVER)")
	root.AddCommand(
		buildCommand(g),
		imagesCommand(g), rmiCommand(g), tagCommand(g),
		createCommand(g), runCommand(g), startCommand(g), stopCommand(g), rmCommand(g),
		psCommand(g), inspectCommand(g), execCommand(g), cpCommand(g),
		infoCommand(g),
		guestCommand(), shimCommand(g),
	)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	var exit exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.code
	default:
		fmt.Fprintln(os.Stderr, "disco-vm:", err)
		return 1
	}
}
