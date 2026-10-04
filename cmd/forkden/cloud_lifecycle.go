package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/forkden/cli/api/cloud"
	"github.com/forkden/cli/internal/account"
)

func (app cloudApp) lifecycle(ctx context.Context, client *cloud.Client, kind string, args []string, cfg account.Config, out, progress io.Writer) (any, error) {
	if len(args) < 2 || !cloud.ValidID(args[1]) {
		return nil, invalidInput("provide a platform RESOURCE_ID")
	}
	flags := commandFlags("cloud " + kind + " " + args[0])
	requestID := flags.String("request_id", "", "stable idempotency key for explicit retries")
	wait := flags.Bool("wait", false, "wait for worker-confirmed completion")
	timeout := flags.Duration("timeout", 20*time.Minute, "maximum polling time")
	if err := parse(flags, args[2:], out); err != nil {
		return nil, err
	}
	if *timeout < time.Second || *timeout > time.Hour {
		return nil, invalidInput("--timeout must be between 1s and 1h")
	}
	if *requestID == "" {
		*requestID = randomID("req_")
	}
	if !requestIDPattern.MatchString(*requestID) {
		return nil, invalidInput("--request_id requires 16–80 letters, digits, underscores or hyphens")
	}
	if _, err := fmt.Fprintf(progress, "Request ID: %s (reuse this ID if the response is lost)\n", *requestID); err != nil {
		return nil, err
	}
	j, err := client.CreateJob(ctx, cfg.OrgID, cfg.ProjectID, cloud.JobInput{Kind: kind + "." + args[0], ResourceID: args[1], IdempotencyKey: *requestID})
	if err != nil {
		return map[string]string{"request_id": *requestID}, err
	}
	if *wait {
		j, err = app.waitJob(ctx, client, cfg, j, *timeout, progress)
	}
	return map[string]any{"request_id": *requestID, "job": j}, err
}
