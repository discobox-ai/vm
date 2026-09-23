package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/pkg/guest"
)

// splitGuestPath reads INSTANCE:PATH. A Windows drive letter ("C:\x") is a
// host path, not an instance named C, so a one-letter prefix does not count.
func splitGuestPath(arg string) (instance, path string, ok bool) {
	name, rest, found := strings.Cut(arg, ":")
	if !found || len(name) <= 1 {
		return "", arg, false
	}
	return name, rest, true
}

func cpCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "cp SRC DST",
		Short: "Copy files between the host and an instance (INSTANCE:PATH names the guest side)",
		Long: `Copy files between the host and a running instance, as docker cp does.

  disco-vm cp ./tools dev:C:\tools     host -> guest: tools lands inside C:\tools
  disco-vm cp dev:/Users/admin/out .   guest -> host`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			srcInst, srcPath, srcGuest := splitGuestPath(args[0])
			dstInst, dstPath, dstGuest := splitGuestPath(args[1])
			switch {
			case srcGuest == dstGuest:
				return errors.New("exactly one of SRC and DST must be INSTANCE:PATH")
			case dstGuest:
				inst, err := e.Get(dstInst)
				if err != nil {
					return err
				}
				client := e.Guest(inst)
				defer client.Close()
				reader, writer := io.Pipe()
				go func() { writer.CloseWithError(guest.Pack(writer, filepath.Clean(srcPath), false)) }()
				return client.CopyTo(cmd.Context(), dstPath, reader)
			default:
				inst, err := e.Get(srcInst)
				if err != nil {
					return err
				}
				client := e.Guest(inst)
				defer client.Close()
				stream, err := client.CopyFrom(cmd.Context(), srcPath)
				if err != nil {
					return err
				}
				defer stream.Close()
				if err := os.MkdirAll(dstPath, 0o755); err != nil {
					return err
				}
				return guest.Unpack(stream, dstPath)
			}
		},
	}
}
