// Command disco-vm-release publishes a disco-vm release (ADR 26-11-13-679),
// run by the release:* Taskfile targets:
//
//	disco-vm-release tag -tag vX.Y.Z
//	    refuse a tag that is not a release version
//	disco-vm-release assets -tag vX.Y.Z -dir build/release
//	    write checksums.txt, install.sh, and install.ps1 beside the binaries
//	disco-vm-release publish -tag vX.Y.Z -dir build/release
//	    make the GitHub Release ($GITHUB_TOKEN, $GITHUB_REPOSITORY)
//	disco-vm-release tap -tag vX.Y.Z [-print]
//	    commit the Homebrew formula of a published full release to the tap
//	    ($HOMEBREW_TAP_TOKEN), or print it
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/discobox-ai/vm/internal/release"
)

const usage = "usage: disco-vm-release tag|assets|publish|tap -tag vX.Y.Z [flags]"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "disco-vm-release:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	tag := fs.String("tag", "", "the release's tag")
	dir := fs.String("dir", "build/release", "the directory holding the release's files")
	repo := fs.String("repo", env("GITHUB_REPOSITORY", "discobox-ai/vm"), "the repository released")
	tap := fs.String("tap", "discobox-ai/homebrew-tap", "the Homebrew tap")
	printOnly := fs.Bool("print", false, "tap: print the formula rather than commit it")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := release.CheckTag(*tag); err != nil {
		return err
	}
	gh := &release.GitHub{
		API:   env("GITHUB_API_URL", "https://api.github.com"),
		Token: os.Getenv("GITHUB_TOKEN"),
		Log:   os.Stderr,
	}

	switch cmd {
	case "tag":
		return nil
	case "assets":
		return release.Write(*dir, *tag)
	case "publish":
		if gh.Token == "" {
			return errors.New("publish needs GITHUB_TOKEN, a token that can write the repository's releases")
		}
		return gh.Publish(ctx, *repo, *tag, *dir)
	case "tap":
		if release.Prerelease(*tag) {
			fmt.Fprintf(os.Stderr, "%s is a prerelease, so the tap keeps the formula it has\n", *tag)
			return nil
		}
		// Checked before anything is read, so a full release without the
		// token fails rather than leave the tap behind with nobody told.
		tapToken := os.Getenv("HOMEBREW_TAP_TOKEN")
		if tapToken == "" && !*printOnly {
			return fmt.Errorf("HOMEBREW_TAP_TOKEN is not set, so %s cannot be told about %s", *tap, *tag)
		}
		// A fix on an older line leaves the formula on the newest release.
		newer, err := gh.NewerRelease(ctx, *repo, *tag)
		if err != nil {
			return err
		}
		if newer != "" {
			fmt.Fprintf(os.Stderr, "%s is newer than %s, so the tap keeps the formula it has\n", newer, *tag)
			return nil
		}
		digests, err := gh.Checksums(ctx, *repo, *tag)
		if err != nil {
			return err
		}
		formula, err := release.Formula(*repo, *tag, digests)
		if err != nil {
			return err
		}
		if *printOnly {
			_, err := os.Stdout.Write(formula)
			return err
		}
		tapGH := &release.GitHub{API: gh.API, Token: tapToken, Log: os.Stderr}
		_, err = tapGH.PutFile(ctx, *tap, release.FormulaPath, "disco-vm "+*tag, formula)
		return err
	default:
		return errors.New(usage)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
