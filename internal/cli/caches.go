package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

func cacheCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "List, save, and remove the project's cache keys",
		Long: `A cache key names a project's cache: an immutable generation that an
instance created with --cache KEY starts from, as a private clone at
/.discobox/cache. Only "cache save" publishes one, from a stopped instance.`,
	}
	cmd.AddCommand(cacheLsCommand(g), cacheSaveCommand(g), cacheRmCommand(g))
	return cmd
}

func cacheLsCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the project's cache keys and their current generations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			caches, err := e.Caches(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				if caches == nil {
					caches = []machine.Cache{}
				}
				return json.NewEncoder(os.Stdout).Encode(caches)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "KEY\tGENERATION\tSIZE\tSAVED")
			for _, c := range caches {
				size := "-"
				if c.Bytes > 0 {
					size = units.FormatBytes(uint64(c.Bytes))
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Key, c.Generation, size, c.Saved.Local().Format(time.RFC3339))
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func cacheSaveCommand(g *globals) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "save INSTANCE [KEY]",
		Short: "Publish a stopped instance's cache volume as a key's new generation",
		Long: `Snapshot a stopped instance's cache volume as a new generation and move
KEY to it, the key the instance started from by default. The instance keeps
its volume. A save to the key the instance started from is refused once that
key holds a newer generation than the one the instance was cloned from;
--force saves anyway.`,
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
			opts := engine.SaveOptions{Force: force}
			if len(args) == 2 {
				opts.Key = args[1]
			}
			c, err := e.SaveCache(cmd.Context(), inst, opts)
			if errors.Is(err, machine.ErrCacheMoved) {
				return fmt.Errorf("%w (--force saves anyway)", err)
			}
			if err != nil {
				return err
			}
			fmt.Println(c.Generation)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "save even if the key has moved since the instance was created")
	return cmd
}

func cacheRmCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rm KEY...",
		Short: "Remove cache keys and their generations; instances keep their own volumes",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			var errs []error
			for _, key := range args {
				errs = append(errs, e.DeleteCache(cmd.Context(), key))
			}
			return errors.Join(errs...)
		},
	}
}
