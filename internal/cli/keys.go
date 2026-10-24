package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

func keyCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "key",
		Short: "Issue, list, and revoke keys another host runs this project's machines with",
	}
	cmd.AddCommand(keyIssueCommand(g), keyLsCommand(g), keyRevokeCommand(g))
	return cmd
}

func keyIssueCommand(g *globals) *cobra.Command {
	var (
		name string
		ttl  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "issue --ttl DURATION",
		Short: "Issue a key that expires, and print it as JSON, its secret included",
		Long: `Issue the narrowest key the driver can for another host to run this
project's machines with, and print it as JSON on stdout. The key expires
after --ttl and cannot itself issue keys. Its secret is shown only here.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			if name == "" {
				name = "key-" + fsutil.RandomHex(4)
			}
			key, err := e.Driver.IssueKey(cmd.Context(), machine.IssueRequest{Name: name, TTL: ttl})
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(key) //nolint:gosec // G117: printing the secret, once, is what key issue is for
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "what the key is issued as, which `key ls` finds it by (default: a random one)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "how long the key lasts, e.g. 24h (required)")
	_ = cmd.MarkFlagRequired("ttl")
	return cmd
}

func keyLsCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ls [NAME]",
		Short: "List the keys the driver issued for this project, without their secrets",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			keys, err := e.Driver.IssuedKeys(cmd.Context(), name)
			if err != nil {
				return err
			}
			if asJSON {
				if keys == nil {
					keys = []machine.Key{}
				}
				return json.NewEncoder(os.Stdout).Encode(keys) //nolint:gosec // G117: a listed key has no secret
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSCOPE\tEXPIRES")
			for _, k := range keys {
				expires := "never"
				if !k.Expires.IsZero() {
					expires = k.Expires.Local().Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.ID, k.Name, k.Scope, expires)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func keyRevokeCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke ID...",
		Short: "Revoke keys the driver issued for this project; an ID it did not issue is an error",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := g.engine()
			if err != nil {
				return err
			}
			issued, err := e.Driver.IssuedKeys(cmd.Context(), "")
			if err != nil {
				return err
			}
			var errs []error
			for _, id := range args {
				i := slices.IndexFunc(issued, func(k machine.Key) bool { return k.ID == id })
				if i < 0 {
					// A typo would otherwise read as revoked.
					errs = append(errs, fmt.Errorf("no key %s issued for project %s: %w", id, e.Project, machine.ErrNotFound))
					continue
				}
				if err := e.Driver.RevokeKey(cmd.Context(), issued[i]); err != nil {
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		},
	}
}
