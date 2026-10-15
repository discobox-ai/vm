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

	"github.com/discobox-ai/vm/internal/version"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"

	// Registers this host OS's drivers with pkg/machine.
	_ "github.com/discobox-ai/vm/pkg/machine/drivers"
)

// exitError carries a guest process's exit code out as disco-vm's own.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

type globals struct {
	root   string
	driver string
	debug  bool
	// opened is the engine a command opened, which Main closes.
	opened *engine.Engine
}

func (g *globals) engine() (*engine.Engine, error) {
	if g.opened != nil {
		return g.opened, nil
	}
	driver, err := machine.New(g.driver)
	if err != nil {
		return nil, err
	}
	g.opened, err = engine.Open(g.root, driver, nil)
	return g.opened, err
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
		Version:       version.String(),
	}
	root.PersistentFlags().BoolVar(&g.debug, "debug", os.Getenv("DISCO_VM_DEBUG") != "", "print every call to the driver's API (boxd, docker) and how long it took, on stderr (also $DISCO_VM_DEBUG)")
	root.PersistentPreRun = func(*cobra.Command, []string) {
		if g.debug {
			machine.SetDebug(os.Stderr)
		}
	}
	root.PersistentFlags().StringVar(&g.root, "root", "", "state directory (default $"+engine.RootEnv+" or the per-OS data directory)")
	root.PersistentFlags().StringVar(&g.driver, "driver", os.Getenv("DISCO_VM_DRIVER"), "hypervisor driver (default "+machine.DefaultName()+"; also $DISCO_VM_DRIVER)")
	root.AddCommand(
		buildCommand(g),
		imagesCommand(g), rmiCommand(g), tagCommand(g),
		warmCommand(g),
		createCommand(g), runCommand(g), startCommand(g), stopCommand(g), rmCommand(g),
		lsCommand(g), inspectCommand(g), urlCommand(g), execCommand(g), cpCommand(g),
		convergeCommand(g),
		infoCommand(g),
		guestCommand(), pipeCommand(), shimCommand(g), dialHostCommand(), dockerInitCommand(),
	)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if g.opened != nil {
		_ = g.opened.Close()
	}
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
