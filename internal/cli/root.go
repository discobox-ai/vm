// Package cli is disco-vm's command line: a thin layer over pkg/engine.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"text/tabwriter"

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

// BaseEnv is the driver's base (machine.Config.Base), what a fresh VM starts
// from: the driver's, which no command takes as a flag.
const BaseEnv = machine.BaseEnv

type globals struct {
	root   string
	driver string
	pool   string
	debug  bool
	// config is --driver-config: the driver's whole configuration, as JSON
	// or @FILE.
	config string
	// driverFlags are the flags of every driver's fields (addDriverFlags).
	driverFlags []driverFlag
	// opened is the engine a command opened.
	opened *engine.Engine
}

// dir is where a driver on this host's disk keeps its state.
func (g *globals) dir() string {
	if g.root != "" {
		return g.root
	}
	return machine.DefaultDir()
}

// newDriver makes a driver of the name for a pool, keeping its state, if
// it keeps any on this host, under dir, with the key the environment holds
// for it and the configuration its flags or the environment give it.
func (g *globals) newDriver(name, pool string) (machine.Driver, error) {
	if name == "" {
		name = machine.DefaultName()
	}
	config, err := g.driverConfig(name)
	if err != nil {
		return nil, err
	}
	g.warnEnvIgnored(name, config)
	return machine.New(name, machine.Config{Dir: g.dir(), Pool: pool, Base: os.Getenv(BaseEnv), Keys: machine.KeyFromEnv(name), DriverConfig: config})
}

func (g *globals) engine() (*engine.Engine, error) {
	if g.opened != nil {
		return g.opened, nil
	}
	driver, err := g.newDriver(g.driver, g.pool)
	if err != nil {
		return nil, err
	}
	g.opened, err = engine.Open(driver, g.pool)
	return g.opened, err
}

// Main runs the command line and returns the process exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g := &globals{}
	root := &cobra.Command{
		Use:   "disco-vm",
		Short: "Run VMs, fresh or from committed ones, and commit them",
		Long: `Run VMs, fresh or from committed ones, and commit them.

Each driver takes a configuration of its own: its flags, environment
variables, and --driver-config keys are shown by --help=DRIVER, such as
` + "`disco-vm --help=docker`" + `. ` + "`disco-vm drivers`" + ` lists the drivers in this build.`,
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
	root.PersistentFlags().StringVar(&g.root, "root", "", "where a driver on this host's disk (hcs, vz, fake) keeps its VMs (default $"+machine.DirEnv+" or the per-OS data directory)")
	root.PersistentFlags().StringVar(&g.pool, "pool", envOr("DISCO_VM_POOL", machine.DefaultPool), "pool that scopes every VM and driver resource (also $DISCO_VM_POOL)")
	root.PersistentFlags().StringVar(&g.driver, "driver", os.Getenv("DISCO_VM_DRIVER"), "hypervisor driver (default "+machine.DefaultName()+"; also $DISCO_VM_DRIVER)")
	root.PersistentFlags().StringVar(&g.config, "driver-config", "", driverConfigUsage)
	g.addDriverFlags(root.PersistentFlags())
	g.installHelp(root)
	root.AddCommand(
		driversCommand(g),
		installCommand(g),
		commitCommand(g), upgradeCommand(g),
		createCommand(g), runCommand(g), startCommand(g), stopCommand(g), rmCommand(g),
		lsCommand(g), inspectCommand(g), urlCommand(g), dialCommand(g), guiCommand(g), execCommand(g), cpCommand(g),
		infoCommand(g), keyCommand(g), cacheCommand(g),
		guestCommand(), consoleHostCommand(), displayHostCommand(), pipeCommand(), shimCommand(g), dialHostCommand(), dockerInitCommand(), dockerVolumesCommand(),
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

func driversCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "drivers",
		Short: "List the hypervisor drivers in this build (--help=DRIVER for one's configuration)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			// The selected driver is named, not constructed: listing must not
			// fail on what a driver needs only to run.
			selected := g.driver
			if selected == "" {
				selected = machine.DefaultName()
			}
			if !slices.Contains(machine.Names(), selected) {
				return fmt.Errorf("machine: no driver %q in this build (have %v)", selected, machine.Names())
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "DRIVER\tSELECTED\tDESCRIPTION")
			for _, name := range machine.Names() {
				marker := ""
				if name == selected {
					marker = "*"
				}
				def, _ := machine.DefinitionOf(name)
				fmt.Fprintf(w, "%s\t%s\t%s\n", name, marker, def.Description)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Println("\nA driver's configuration: disco-vm --help=DRIVER")
			return nil
		},
	}
}

// envOr is $name, or def when it is empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
