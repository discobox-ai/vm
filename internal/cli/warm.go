package cli

import (
	"errors"
	"fmt"
	osuser "os/user"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
)

func warmCommand(g *globals) *cobra.Command {
	var (
		count, cpus       int
		memory, user      string
		remove, localUser bool
		timeout           time.Duration
	)
	cmd := &cobra.Command{
		Use:   "warm [flags] IMAGE",
		Short: "Stage an image so instances of it skip the cold boot (fork on hcs, resume on vz)",
		Long: `Stage an image so instances of it skip the cold boot. The driver picks how:
hcs freezes a live template that forks any number of clones, and vz saves
two saved templates that each resume any number of clones. run and create use
the stage by default (--mode auto) and boot cold when it cannot serve.

--user creates an account in the stage and logs it in, so every clone starts
as that user; --local-user makes it your own account here, with the same name
and uid, so files on a shared directory have the same owner on both sides.

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
			account, err := warmUser(user, localUser)
			if err != nil {
				return err
			}
			if _, err := e.Warm(cmd.Context(), ref, engine.WarmOptions{Count: count, CPUs: cpus, Memory: mem, User: account, Timeout: timeout}); err != nil {
				return err
			}
			layer, err := e.Images.Resolve(ref)
			if err != nil {
				return err
			}
			stage, _ := e.Stage(cmd.Context(), layer)
			fmt.Printf("%s: warm, %s\n", ref, describeStage(stage))
			return nil
		},
	}
	cmd.Flags().IntVar(&count, "count", 1, "clones to stage where the stage is a pool used once per clone (fake); warming again tops up")
	cmd.Flags().IntVar(&cpus, "cpus", 0, "vCPUs of the staged machine, and so of its clones (default: the driver's)")
	cmd.Flags().StringVar(&memory, "memory", "", "memory of the staged machine, e.g. 8GiB (default: the driver's)")
	cmd.Flags().StringVar(&user, "user", "", "create this account in the stage and log it in: NAME or NAME:UID")
	cmd.Flags().BoolVar(&localUser, "local-user", false, "create your own account here in the stage (same name and uid) and log it in")
	cmd.Flags().BoolVar(&remove, "rm", false, "remove the image's stage instead")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long warming may take")
	return cmd
}

// describeStage says what a stage can serve and who it logs in, for warm and
// images: "2 resume clones, as darren", "used up (0 of 2), warm to refill",
// or "held: 2 staged, but a restore needs the screen unlocked".
func describeStage(s engine.StageInfo) string {
	var out string
	switch w := s.Warmth; {
	case w.Held != "":
		out = "held: " + w.Held
	case w.Clones == 0:
		out = fmt.Sprintf("used up (0 of %d), warm to refill", max(s.Count, 1))
	case w.Clones < 0:
		out = fmt.Sprintf("%s clones", w.Mode)
	case w.Clones == 1:
		out = fmt.Sprintf("1 %s clone", w.Mode)
	default:
		out = fmt.Sprintf("%d %s clones", w.Clones, w.Mode)
	}
	if s.User != nil {
		out += ", as " + s.User.Name
	}
	return out
}

// warmUser is the account --user or --local-user asks a stage to log in.
func warmUser(spec string, local bool) (*machine.User, error) {
	switch {
	case spec != "" && local:
		return nil, errors.New("--user and --local-user are exclusive")
	case local:
		me, err := osuser.Current()
		if err != nil {
			return nil, err
		}
		u := &machine.User{Name: me.Username, FullName: me.Name}
		// A uid is numeric on unix; Windows has SIDs, and its guests pick.
		if uid, err := strconv.Atoi(me.Uid); err == nil {
			u.UID = uid
		}
		return u, nil
	case spec != "":
		name, uid, hasUID := strings.Cut(spec, ":")
		if name == "" {
			return nil, fmt.Errorf("--user %q has no name", spec)
		}
		u := &machine.User{Name: name}
		if hasUID {
			n, err := strconv.Atoi(uid)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("--user %q: the uid must be a positive number", spec)
			}
			u.UID = n
		}
		return u, nil
	}
	return nil, nil
}
