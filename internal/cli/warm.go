package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

func warmCommand(g *globals) *cobra.Command {
	var (
		count, cpus int
		memory      string
		remove      bool
		timeout     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "warm [flags] IMAGE",
		Short: "Stage an image so instances of it skip the cold boot (fork on hcs, resume on vz)",
		Long: `Stage an image so instances of it skip the cold boot. The driver picks how:
hcs freezes a live template that forks any number of clones, and vz saves
--count machine states that are each resumed once. run and create use the
stage by default (--mode auto) and boot cold when it is used up.

A stage holds memory or disk until it is removed with --rm.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			ref := args[0]
			if remove {
				if err := e.Cool(cmd.Context(), ref); err != nil {
					return err
				}
				fmt.Printf("%s: cooled\n", ref)
				return nil
			}
			mem, err := units.ParseBytes(memory)
			if err != nil {
				return err
			}
			w, err := e.Warm(cmd.Context(), ref, engine.WarmOptions{Count: count, CPUs: cpus, Memory: mem, Timeout: timeout})
			if err != nil {
				return err
			}
			fmt.Printf("%s: warm, %s\n", ref, describeWarmth(w))
			return nil
		},
	}
	cmd.Flags().IntVar(&count, "count", 1, "clones to stage where each is used once (resume); warming again tops up")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "vCPUs of the staged machine, and so of its clones (default: the driver's)")
	cmd.Flags().StringVar(&memory, "memory", "", "memory of the staged machine, e.g. 8GiB (default: the driver's)")
	cmd.Flags().BoolVar(&remove, "rm", false, "remove the image's stage instead")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long warming may take")
	return cmd
}

// describeWarmth says what a stage can serve, for warm and images.
func describeWarmth(w machine.Warmth) string {
	switch {
	case w.Clones == 0:
		return "nothing staged"
	case w.Clones < 0:
		return fmt.Sprintf("%s clones", w.Mode)
	case w.Clones == 1:
		return fmt.Sprintf("1 %s clone", w.Mode)
	default:
		return fmt.Sprintf("%d %s clones", w.Clones, w.Mode)
	}
}
