package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
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
		options, tags                         []string
		gui                                   bool
	)
	cmd := &cobra.Command{
		Use:   "install [-t NAME[:TAG]] [--os OS] [--media MEDIA]",
		Short: "Make an image by installing an OS from media (hcs, vz)",
		Long: `Make an image by installing an OS from installation media, on a driver
that installs (hcs: a Windows ISO; vz: a macOS IPSW, or "latest"). It prints
the image's ID. A driver that does not install has bases instead, which run
takes by name. Shape an image further by running it, changing the guest,
stopping it, and taking a ` + "`disco-vm snapshot`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			img, err := e.Install(cmd.Context(), engine.InstallOptions{
				GuestOS: machine.OS(guestOS), Media: media, Edition: edition,
				DiskBytes: int64(diskBytes), CPUs: cpus, Memory: memoryBytes,
				Options: opts, GUI: gui, Log: os.Stderr,
			}, tags...)
			if err != nil {
				return err
			}
			fmt.Println(img.ID)
			return nil
		},
	}
	cmd.Flags().StringArrayVarP(&tags, "tag", "t", nil, "name the image NAME[:TAG] (repeatable); without it the image is known by its ID")
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

func imagesCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "images",
		Short: "List images, and the driver's bases, which run takes by name with no image",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			entries, err := e.Images(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "IMAGE\tID\tOS\tDRIVER\tCREATED")
			for _, entry := range entries {
				ref := entry.Ref
				if ref == "" {
					ref = "<none>"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ref, engine.Short(entry.Image.ID), entry.Image.GuestOS, e.Driver.Name(), ago(entry.Image.Created))
			}
			for _, b := range bases(cmd.Context(), e) {
				fmt.Fprintf(w, "%s\t-\t%s\t%s\t%s\n", b.Name, b.GuestOS, e.Driver.Name(), ago(b.Created))
			}
			return w.Flush()
		},
	}
}

func rmiCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rmi IMAGE...",
		Short: "Untag images, or remove untagged ones by ID, and delete those nothing else tags or runs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			for _, ref := range args {
				// A base is the driver's, and disco-vm never deletes it.
				if _, err := e.Resolve(cmd.Context(), ref); err != nil {
					if err := e.NotABase(cmd.Context(), ref, err); errors.Is(err, engine.ErrBase) {
						return err
					}
				}
				r, err := e.RemoveImage(cmd.Context(), ref)
				if err != nil {
					return err
				}
				if r.Untagged != "" {
					fmt.Printf("untagged %s\n", r.Untagged)
				}
				if r.Deleted != "" {
					fmt.Printf("deleted %s\n", engine.Short(r.Deleted))
				}
				if r.Kept != nil {
					fmt.Fprintf(os.Stderr, "disco-vm: kept it, untagged: %v\n", r.Kept)
				}
			}
			return nil
		},
	}
}

func tagCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "tag SOURCE TARGET",
		Short: "Add a name:tag to an image",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			img, err := e.Resolve(cmd.Context(), args[0])
			if err != nil {
				return e.NotABase(cmd.Context(), args[0], err)
			}
			return e.Tag(cmd.Context(), args[1], img.ID)
		},
	}
}

func snapshotCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "snapshot INSTANCE [NAME[:TAG]]",
		Short: "Commit a stopped instance as an image, and remove the instance",
		Long: `Commit a stopped instance's disk as a new image, and remove the instance.
To go on from the snapshot, create an instance of it. Without NAME the image
is known by the ID it prints.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			inst, err := e.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			img, err := e.Snapshot(cmd.Context(), inst, args[1:]...)
			if err != nil {
				return err
			}
			fmt.Println(img.ID)
			return nil
		},
	}
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

// bases is the driver's bases, or none, with a warning, when it cannot list
// them.
func bases(ctx context.Context, e *engine.Engine) []machine.Base {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	list, err := e.Driver.Bases(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "disco-vm: cannot list %s's bases: %v\n", e.Driver.Name(), err)
		return nil
	}
	return list
}
