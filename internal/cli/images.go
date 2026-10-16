package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/pkg/build"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/image"
	"github.com/discobox-ai/vm/pkg/machine"
)

func buildCommand(g *globals) *cobra.Command {
	var (
		file, contextDir, agent string
		buildArgs, tags         []string
		noCache, gui            bool
	)
	cmd := &cobra.Command{
		Use:   "build [-f spec.yaml] [context]",
		Short: "Build an image from a YAML build spec",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			if len(args) == 1 {
				contextDir = args[0]
			}
			parsed, err := build.ParseArgs(buildArgs)
			if err != nil {
				return err
			}
			builder := &build.Builder{Engine: e, Out: os.Stdout}
			result, err := builder.Build(cmd.Context(), build.Options{
				File: file, Context: contextDir, Args: parsed, Tags: tags, NoCache: noCache, Agent: agent, GUI: gui,
			})
			if err != nil {
				return err
			}
			fmt.Println(result.Layer)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "disco-vm.yaml", "build spec")
	cmd.Flags().StringArrayVar(&buildArgs, "build-arg", nil, "override a spec arg (KEY=VALUE)")
	cmd.Flags().StringArrayVarP(&tags, "tag", "t", nil, "name the result NAME[:TAG] (repeatable); without it the build is known by its ID")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "rebuild every layer")
	cmd.Flags().BoolVar(&gui, "gui", false, "show each build VM's display in a native window")
	cmd.Flags().StringVar(&agent, "agent", "", "disco-vm binary for the guest OS (default: this one)")
	return cmd
}

func imagesCommand(g *globals) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "images",
		Short: "List images",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			images, err := e.Images.Images(all)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "IMAGE\tLAYER\tOS\tDRIVER\tCREATED")
			for _, img := range images {
				ref := img.Ref
				if ref == "" {
					ref = "<none>"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ref, image.Short(img.Layer.ID), img.Layer.GuestOS, img.Layer.Driver, ago(img.Layer.Created))
			}
			for _, o := range orphans(cmd.Context(), e, machine.OrphanLayer) {
				fmt.Fprintf(w, "<orphan>\t%s\t-\t%s\t%s\n", o.Name, e.Driver.Name(), ago(o.Created))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include intermediate layers, the parents of other layers, untagged")
	return cmd
}

func rmiCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rmi IMAGE...",
		Short: "Untag images, or remove untagged ones by ID, and delete layers nothing else uses; or delete orphaned layers by the name images shows",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			for _, ref := range args {
				_, err := e.Images.Resolve(ref)
				if errors.Is(err, image.ErrNotFound) {
					// Not an image: an orphaned layer of the project.
					o, err := e.RemoveOrphan(cmd.Context(), machine.OrphanLayer, ref, false)
					if errors.Is(err, engine.ErrNotOrphan) {
						return fmt.Errorf("no image or orphaned layer %q", ref)
					}
					if err != nil {
						return err
					}
					fmt.Printf("deleted orphan %s\n", o.Name)
					continue
				}
				untagged, removed, err := e.Images.Remove(cmd.Context(), ref, e.Driver, e.LayerInUse)
				if err != nil {
					return err
				}
				if untagged != "" {
					fmt.Printf("untagged %s\n", untagged)
				}
				for _, id := range removed {
					fmt.Printf("deleted %s\n", image.Short(id))
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
			id, err := e.Images.Resolve(args[0])
			if err != nil {
				return err
			}
			return e.Images.Tag(cmd.Context(), args[1], id, e.Driver)
		},
	}
}

func snapshotCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "snapshot INSTANCE [NAME[:TAG]]",
		Short: "Commit a stopped instance as an image, and remove the instance",
		Long: `Commit a stopped instance's disk as a new image layer on its image, and
remove the instance. To go on from the snapshot, create an instance of it.
Without NAME the image is known by the layer ID it prints.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			inst, err := e.Get(args[0])
			if err != nil {
				return err
			}
			ref := ""
			if len(args) == 2 {
				ref = args[1]
			}
			layer, err := e.Snapshot(cmd.Context(), inst, ref)
			if err != nil {
				return err
			}
			fmt.Println(layer.ID)
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

// orphans is what the driver holds of one kind that the store has no record
// of. A driver that cannot be asked is a warning, not a failed listing: the
// store's own rows still list.
func orphans(ctx context.Context, e *engine.Engine, kind machine.OrphanKind) []machine.Orphan {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	all, err := e.Orphans(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "disco-vm: cannot list what %s holds outside this store: %v\n", e.Driver.Name(), err)
		return nil
	}
	var out []machine.Orphan
	for _, o := range all {
		if o.Kind == kind {
			out = append(out, o)
		}
	}
	if len(out) > 0 {
		remove := "rmi"
		if kind == machine.OrphanMachine {
			remove = "rm"
		}
		fmt.Fprintf(os.Stderr, "disco-vm: <orphan> rows are %s's, named as disco-vm names them, and this store has no record of them; `disco-vm %s NAME` deletes one\n", e.Driver.Name(), remove)
	}
	return out
}
