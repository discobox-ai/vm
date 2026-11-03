package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

func installCommand(g *globals) *cobra.Command {
	var (
		guestOS, media, edition, disk, memory string
		cpus                                  int
		options                               []string
		gui                                   bool
	)
	cmd := &cobra.Command{
		Use:   "install NAME [--os OS] [--media MEDIA]",
		Short: "Make a committed VM by installing an OS from media (hcs, vz)",
		Long: `Make the VM NAME by installing an OS from installation media, on a driver
that installs (hcs: a Windows ISO; vz: a macOS IPSW, or "latest"). It is
committed: create VMs from it with ` + "`--from NAME`" + `, and never start it. A
driver that does not install makes fresh VMs from its base instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			diskBytes, err := units.ParseBytes(disk)
			if err != nil {
				return fmt.Errorf("--disk: %w", err)
			}
			memoryBytes, err := units.ParseBytes(memory)
			if err != nil {
				return fmt.Errorf("--memory: %w", err)
			}
			opts := map[string]string{}
			for _, kv := range options {
				key, value, ok := strings.Cut(kv, "=")
				if !ok || key == "" {
					return fmt.Errorf("--option %q is not KEY=VALUE", kv)
				}
				opts[key] = value
			}
			inst, err := e.Install(cmd.Context(), args[0], engine.InstallOptions{
				GuestOS: machine.OS(guestOS), Media: media, Edition: edition,
				DiskBytes: int64(diskBytes), CPUs: cpus, Memory: memoryBytes,
				Options: opts, GUI: gui, Log: os.Stderr,
			})
			if err != nil {
				return err
			}
			fmt.Println(inst.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&guestOS, "os", "", "guest OS: windows, darwin, linux (default: the driver's only one)")
	cmd.Flags().StringVar(&media, "media", "latest", `installer: an ISO or IPSW path, or "latest" where the driver fetches one`)
	cmd.Flags().StringVar(&edition, "edition", "", "image inside multi-image media (install.wim)")
	cmd.Flags().StringVar(&disk, "disk", "", "disk size, e.g. 128GiB (default: the driver's)")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "vCPUs for the install (default: the driver's)")
	cmd.Flags().StringVar(&memory, "memory", "", "memory for the install, e.g. 8GiB (default: the driver's)")
	cmd.Flags().StringArrayVar(&options, "option", nil, "driver option KEY=VALUE (repeatable)")
	cmd.Flags().BoolVar(&gui, "gui", false, "show each of the install's boots in a native window")
	return cmd
}

func commitCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "commit VM...",
		Short: "Freeze stopped VMs under their names, to create VMs from",
		Long: `Freeze a stopped VM under its name: its data volume and cache clone are
removed, and VMs are created from it with ` + "`create --from VM`" + `. Never start
it again; to change it, create a VM from it, change that, and commit it under
a new name. Some drivers cannot tell a committed VM from a stopped one, so
keep your own record of which VMs you committed. A driver that cannot commit
(` + "`disco-vm info`" + ` reports "commit": false) refuses it, and refuses --from on
create and run too.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return eachInstance(cmd.Context(), g, args, func(e *engine.Engine, inst engine.Instance) error {
				_, err := e.Commit(cmd.Context(), inst)
				return err
			})
		},
	}
}

func upgradeCommand(g *globals) *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "upgrade VM [--from COMMITTED]",
		Short: "Give a stopped VM a new root, keeping its name, settings, and volumes",
		Long: `Give the stopped VM a new root: a copy of the committed VM --from, or a
fresh one from the driver's base. It keeps its name, its settings, its data
volume, and its cache clone; what it wrote to its old root is removed for
good, so commit it first to keep a way back. An upgrade that was interrupted
is finished by running it again with the same --from. Only a driver that
attaches a data volume upgrades a VM: ` + "`disco-vm info`" + ` lists its volumes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			inst, err := e.Upgrade(cmd.Context(), args[0], engine.UpgradeOptions{From: from})
			if err != nil {
				return err
			}
			fmt.Println(inst.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "the committed VM the new root is a copy of (default: a fresh root from the driver's base)")
	return cmd
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
