package main

import (
	"context"
	"io"
	"os"

	"github.com/forkden/cli/internal/review"
)

// runReview parses `forkden review SUBCOMMAND NAME [flags]` and runs it locally.
// args starts at the subcommand; orderCommandArgs has already put NAME first.
func runReview(ctx context.Context, args []string, out, progress io.Writer) (any, error) {
	if len(args) < 2 || args[1] == "--" || len(args[1]) > 0 && args[1][0] == '-' {
		return nil, invalidInput("review " + args[0] + " needs a review name, for example: forkden review " + args[0] + " demo")
	}
	cmd := review.Command{Action: args[0], Name: args[1], Root: reviewRoot()}
	flags := commandFlags("review " + args[0])
	switch cmd.Action {
	case "fork":
		flags.StringVar(&cmd.DSN, "dsn", "", "source database")
		flags.StringVar(&cmd.To, "to", "", "target server")
		flags.BoolVar(&cmd.Force, "force", false, "drop an existing review first")
		flags.BoolVar(&cmd.Template, "template", false, "copy with CREATE DATABASE TEMPLATE")
	case "diff":
		flags.StringVar(&cmd.To, "to", "", "target server")
		flags.StringVar(&cmd.Base, "base", "", "base database (DSN mode)")
		flags.StringVar(&cmd.Work, "work", "", "work database (DSN mode)")
	case "check":
		flags.StringVar(&cmd.File, "file", "", "check file")
		flags.StringVar(&cmd.To, "to", "", "target server")
		flags.StringVar(&cmd.Work, "work", "", "work database (DSN mode)")
	case "export":
		flags.StringVar(&cmd.Output, "output", "", "new directory")
	case "drop":
		flags.StringVar(&cmd.To, "to", "", "target server")
	}
	if err := parse(flags, args[2:], out); err != nil {
		return nil, err
	}
	return review.Run(ctx, cmd, progress)
}

func reviewRoot() string {
	if dir := os.Getenv("FORKDEN_REVIEW_DIR"); dir != "" {
		return dir
	}
	return "forkden-review"
}
