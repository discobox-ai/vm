package cli

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/pkg/build"
	"github.com/discobox-ai/vm/pkg/image"
)

func buildCommand(g *globals) *cobra.Command {
	var (
		file, contextDir, agent string
		buildArgs, tags         []string
		noCache                 bool
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
				File: file, Context: contextDir, Args: parsed, Tags: tags, NoCache: noCache, Agent: agent,
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
	cmd.Flags().StringArrayVarP(&tags, "tag", "t", nil, "extra name:tag for the result")
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "rebuild every layer")
	cmd.Flags().StringVar(&agent, "agent", "", "disco-vm binary for the guest OS (default: this one)")
	return cmd
}

func imagesCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "images",
		Short: "List images",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			images, err := e.Images.Images()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "IMAGE\tLAYER\tOS\tDRIVER\tCREATED")
			for _, img := range images {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", img.Ref, image.Short(img.Layer.ID), img.Layer.GuestOS, img.Layer.Driver, ago(img.Layer.Created))
			}
			return w.Flush()
		},
	}
}

func rmiCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rmi IMAGE...",
		Short: "Untag images and delete layers nothing else uses",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			for _, ref := range args {
				removed, err := e.Images.Remove(cmd.Context(), ref, e.Driver, e.LayerInUse)
				if err != nil {
					return err
				}
				fmt.Printf("untagged %s\n", image.NormalizeRef(ref))
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
		RunE: func(_ *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			id, err := e.Images.Resolve(args[0])
			if err != nil {
				return err
			}
			return e.Images.Tag(args[1], id)
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
