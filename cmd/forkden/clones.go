package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	v1 "github.com/forkden/cli/api/v1"
)

func cloneCommand(ctx context.Context, client engine, args []string, out io.Writer) (any, error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprint(out, cloneUsage)
		return nil, errors.Join(flag.ErrHelp, err)
	}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return nil, invalidInput("use clone create NAME --source PROFILE")
		}
		flags := commandFlags("clone create")
		source := flags.String("source", "", "registered source profile")
		target := flags.String("target_env", "FORKDEN_TARGET_URL", "target administrator URL env on engine")
		if err := parse(flags, args[2:], out); err != nil {
			return nil, err
		}
		if args[1] == "" || *source == "" || *target == "" {
			return nil, invalidInput("clone name, --source and target env reference are required")
		}
		return execute(ctx, client, v1.CloneCreate, v1.CloneInput{Name: args[1], Source: *source, TargetEnv: *target})
	case "list", "prune":
		if len(args) != 1 {
			return nil, invalidInput("unexpected clone arguments")
		}
		action := v1.CloneList
		if args[0] == "prune" {
			action = v1.ClonePrune
		}
		return execute(ctx, client, action, struct{}{})
	case "show", "refresh", "remove":
		if len(args) != 2 || args[1] == "" {
			return nil, invalidInput("clone command requires one name or ID")
		}
		action := map[string]v1.Action{"show": v1.CloneShow, "refresh": v1.CloneRefresh, "remove": v1.CloneRemove}[args[0]]
		return execute(ctx, client, action, v1.CloneRef{Clone: args[1]})
	default:
		return nil, invalidInput("unknown clone command; run forkden clone help")
	}
}

const cloneUsage = `Reusable clones keep immutable snapshot versions on the engine's target.

  clone create NAME --source PROFILE [--target_env ENV]
  clone list / clone show NAME_OR_ID
  clone refresh NAME_OR_ID
  clone remove NAME_OR_ID
  clone prune
  fork create --clone NAME_OR_ID [--version N] [--ttl 1h]

Refresh creates a new version; existing forks keep their original version.
Remove requires every fork of the clone to be closed. Prune recovers interrupted
captures and removals; ready versions are retained until clone remove.
Snapshot payloads are PostgreSQL databases, not copy-on-write storage.
`
