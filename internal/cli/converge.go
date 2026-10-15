package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/pkg/converge"
)

// convergeCommand makes a running instance match state files, from here: the
// state files and what they name are read on this side, and the guest is
// reached through exec and copy, so nothing has to be installed in it.
func convergeCommand(g *globals) *cobra.Command {
	var lease time.Duration
	cmd := &cobra.Command{
		Use:   "converge INSTANCE [STATE...]",
		Short: "Make an instance match state files, or show how its last converge left it",
		Long: `Make a running instance match state files: the packages, files, services,
and scripts they declare. Each STATE is a NAME.yaml or a directory of them, on
this machine, with the files they name. Only what differs from what the
instance already has is applied. See docs/state-file.md.

  disco-vm converge dev harness/claude-code/converge   apply, and exit 1 with the error
  disco-vm converge dev                                the status of the last run, as JSON

The instance is leased to the run while it lasts, so two cannot converge it
at once: a second is refused until the first ends, or its lease runs out
because its client went away.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			inst, err := e.Get(args[0])
			if err != nil {
				return err
			}
			if len(args) == 1 {
				status, err := e.ConvergeStatus(cmd.Context(), inst)
				if err != nil {
					return err
				}
				if status.State == "" {
					return errors.New(inst.Name + " has not been converged")
				}
				out := json.NewEncoder(cmd.OutOrStdout())
				out.SetIndent("", "  ")
				return out.Encode(status)
			}
			err = e.Converge(cmd.Context(), inst, converge.Options{Paths: args[1:], Out: cmd.OutOrStdout(), Lease: lease})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "converged")
			return nil
		},
	}
	cmd.Flags().DurationVar(&lease, "lease", converge.DefaultLease, "how long the instance stays locked to this run after its last heartbeat")
	return cmd
}
