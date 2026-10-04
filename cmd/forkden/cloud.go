package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"time"

	"github.com/forkden/cli/api/cloud"
	"github.com/forkden/cli/internal/account"
)

type cloudApp struct {
	credentials account.Credentials
	openBrowser func(context.Context, string) error
	pause       func(context.Context, time.Duration) error
}

func defaultCloudApp() cloudApp {
	return cloudApp{credentials: account.Keyring{}, openBrowser: openBrowser, pause: pause}
}

func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

//nolint:gosec // The account client validates the URI as its pinned origin's fixed /device route; arguments never enter a shell.
func openBrowser(ctx context.Context, uri string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "/usr/bin/open", uri)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", uri)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", uri)
	}
	return cmd.Run()
}

func (app cloudApp) run(ctx context.Context, args []string, origin, dir string, out, progress io.Writer) (value any, retErr error) {
	if len(args) == 0 || args[0] == "help" || args[len(args)-1] == "--help" {
		_, err := fmt.Fprint(out, cloudUsage)
		return nil, errors.Join(flag.ErrHelp, err)
	}
	store, err := account.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, store.Close()) }()
	write := args[0] == "login" || args[0] == "logout" || (len(args) > 1 && args[0] == "project" && args[1] == "use")
	if write {
		if err := store.Lock(); err != nil {
			return nil, err
		}
		defer func() { retErr = errors.Join(retErr, store.Unlock()) }()
	}
	cfg, err := store.Load()
	if err != nil {
		return nil, err
	}
	if origin != "" {
		origin, err = cloud.Origin(origin)
		if err != nil {
			return nil, invalidInput("--api_url must be an HTTPS origin (loopback HTTP is allowed for development)")
		}
		if cfg.CredentialID != "" && origin != cfg.APIURL {
			return nil, invalidInput("API origin differs from the saved login; logout before switching servers, or use a separate --config_dir")
		}
	} else {
		origin = cfg.APIURL
	}
	if origin == "" {
		return nil, invalidInput("provide --api_url before login; cloud hosting is not configured yet")
	}
	if args[0] == "login" {
		return app.login(ctx, args[1:], origin, cfg, store, out, progress)
	}
	if cfg.CredentialID == "" {
		return nil, &cloud.Error{Code: "unauthorized", Message: "Run forkden login first."}
	}
	credential, err := app.credentials.Get(cfg.CredentialID)
	if err != nil {
		return nil, err
	}
	if credential.APIURL != origin {
		return nil, invalidInput("saved settings do not match the credential's API origin; restore the saved origin before using this login")
	}
	client, err := cloud.NewClient(origin, credential.Token)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	switch args[0] {
	case "whoami":
		if len(args) != 1 {
			return nil, invalidInput("use whoami")
		}
		return client.Session(ctx)
	case "logout":
		if len(args) != 1 {
			return nil, invalidInput("use logout")
		}
		return app.logout(ctx, client, cfg, store)
	case "org":
		if len(args) != 2 || args[1] != "list" {
			return nil, invalidInput("use cloud org list")
		}
		return client.Organizations(ctx)
	case "project":
		return app.project(ctx, client, args[1:], cfg, store, out)
	case "db", "profile":
		if len(args) != 2 || args[1] != "list" {
			return nil, invalidInput("cloud DB profiles are metadata; use cloud db list and configure connection references on the worker")
		}
		if err := selection(cfg); err != nil {
			return nil, err
		}
		return client.Profiles(ctx, cfg.OrgID, cfg.ProjectID)
	case "clone", "fork":
		if err := selection(cfg); err != nil {
			return nil, err
		}
		return app.resource(ctx, client, args[0], args[1:], cfg, out, progress)
	case "job":
		if err := selection(cfg); err != nil {
			return nil, err
		}
		return app.job(ctx, client, args[1:], cfg, out, progress)
	default:
		return nil, invalidInput("unknown cloud command; run forkden cloud help")
	}
}

func (app cloudApp) login(ctx context.Context, args []string, origin string, cfg account.Config, store *account.Store, out, progress io.Writer) (any, error) {
	flags := commandFlags("login")
	noBrowser := flags.Bool("no-browser", false, "show a browser URL and code for manual authorization")
	if err := parse(flags, args, out); err != nil {
		return nil, err
	}
	if cfg.CredentialID != "" {
		return nil, invalidInput("a login is already saved; use whoami or logout before logging in again")
	}
	client, err := cloud.NewClient(origin, "")
	if err != nil {
		return nil, err
	}
	defer client.Close()
	device, err := client.StartDevice(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(progress, "Open %s\nEnter code: %s\nConfirm this code only for the login you just started.\n", device.VerificationURI, device.UserCode); err != nil {
		return nil, err
	}
	if !*noBrowser {
		browserCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := app.openBrowser(browserCtx, device.VerificationURI)
		cancel()
		if err != nil {
			if _, writeErr := fmt.Fprintln(progress, "Browser could not open; use the URL above."); writeErr != nil {
				return nil, writeErr
			}
		}
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(device.ExpiresIn)*time.Second)
	defer cancel()
	interval := time.Duration(device.Interval) * time.Second
	for {
		if err := app.pause(pollCtx, interval); err != nil {
			return nil, err
		}
		grant, err := client.RedeemDevice(pollCtx, device.DeviceCode)
		var publicErr *cloud.Error
		if errors.As(err, &publicErr) {
			switch publicErr.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "transport_error":
				interval = min(10*time.Minute, interval*2)
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		if err := app.saveGrant(ctx, origin, grant, store); err != nil {
			return nil, err
		}
		return grant.Session, nil
	}
}

func (app cloudApp) saveGrant(ctx context.Context, origin string, grant cloud.Grant, store *account.Store) (retErr error) {
	credentialID := randomID("cred_")
	// Failed persistence must not leave a usable account token behind.
	defer func() {
		if retErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		client, err := cloud.NewClient(origin, grant.AccessToken)
		if err != nil {
			retErr = errors.Join(retErr, err)
			return
		}
		defer client.Close()
		if err := client.Logout(cleanupCtx); err != nil {
			retErr = errors.Join(retErr, errors.New("could not revoke the unsaved CLI session; it expires in 7 days"))
		}
		retErr = errors.Join(retErr, app.credentials.Delete(credentialID))
	}()
	if err := app.credentials.Set(credentialID, account.Credential{APIURL: origin, Token: grant.AccessToken}); err != nil {
		return err
	}
	return store.Save(account.Config{APIURL: origin, CredentialID: credentialID, UserID: grant.User.ID})
}

func (app cloudApp) logout(ctx context.Context, client *cloud.Client, cfg account.Config, store *account.Store) (any, error) {
	err := client.Logout(ctx)
	var publicErr *cloud.Error
	if err != nil {
		if !errors.As(err, &publicErr) || publicErr.Code != "unauthorized" {
			return nil, err
		}
	}
	if err := store.Save(account.Config{APIURL: cfg.APIURL}); err != nil {
		return nil, err
	}
	if err := app.credentials.Delete(cfg.CredentialID); err != nil {
		return nil, err
	}
	return map[string]bool{"signed_out": true}, nil
}

func (app cloudApp) project(ctx context.Context, client *cloud.Client, args []string, cfg account.Config, store *account.Store, out io.Writer) (any, error) {
	if len(args) == 1 && args[0] == "current" {
		return map[string]string{"api_url": cfg.APIURL, "org_id": cfg.OrgID, "project_id": cfg.ProjectID}, nil
	}
	if len(args) == 0 || (args[0] != "list" && args[0] != "use") {
		return nil, invalidInput("use cloud project list --org ID, project use ID --org ID, or project current")
	}
	flags := commandFlags("cloud project " + args[0])
	org := flags.String("org", cfg.OrgID, "organization ID")
	var projectID string
	flagArgs := args[1:]
	if args[0] == "use" {
		if len(args) < 2 || !cloud.ValidID(args[1]) {
			return nil, invalidInput("use cloud project use PROJECT_ID --org ORG_ID")
		}
		projectID = args[1]
		flagArgs = args[2:]
	}
	if err := parse(flags, flagArgs, out); err != nil {
		return nil, err
	}
	if !cloud.ValidID(*org) {
		return nil, invalidInput("--org requires an organization ID")
	}
	projects, err := client.Projects(ctx, *org)
	if err != nil {
		return nil, err
	}
	if args[0] == "list" {
		return projects, nil
	}
	for _, p := range projects {
		if p.ID == projectID && p.OrgID == *org {
			cfg.OrgID, cfg.ProjectID = *org, projectID
			if err := store.Save(cfg); err != nil {
				return nil, err
			}
			return p, nil
		}
	}
	return nil, &cloud.Error{Code: "not_found", Message: "Project is not accessible in this organization."}
}

func selection(cfg account.Config) error {
	if cfg.OrgID == "" || cfg.ProjectID == "" {
		return invalidInput("select a workspace with cloud project use PROJECT_ID --org ORG_ID")
	}
	return nil
}

func randomID(prefix string) string {
	var data [16]byte
	rand.Read(data[:]) // crypto/rand.Read fills the buffer or terminates on Go 1.26.
	return prefix + hex.EncodeToString(data[:])
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,80}$`)

func (app cloudApp) resource(ctx context.Context, client *cloud.Client, kind string, args []string, cfg account.Config, out, progress io.Writer) (any, error) {
	if len(args) == 0 {
		return nil, invalidInput("use cloud " + kind + " create NAME, list or show RESOURCE_ID")
	}
	if (args[0] == "list" && len(args) == 1) || (args[0] == "show" && len(args) == 2 && cloud.ValidID(args[1])) {
		resources, err := client.Resources(ctx, cfg.OrgID, cfg.ProjectID)
		if err != nil {
			return nil, err
		}
		items := []cloud.Resource{}
		for _, resource := range resources {
			if resource.Kind != kind {
				continue
			}
			if args[0] == "show" && resource.ID == args[1] {
				return resource, nil
			}
			if args[0] == "list" {
				items = append(items, resource)
			}
		}
		if args[0] == "list" {
			return items, nil
		}
		return nil, &cloud.Error{Code: "not_found", Message: "Resource is not available in this project."}
	}
	if kind == "clone" && len(args) == 2 && args[0] == "versions" && cloud.ValidID(args[1]) {
		return client.Versions(ctx, cfg.OrgID, cfg.ProjectID, args[1])
	}
	if (kind == "clone" && (args[0] == "refresh" || args[0] == "delete")) || (kind == "fork" && args[0] == "close") {
		return app.lifecycle(ctx, client, kind, args, cfg, out, progress)
	}
	if args[0] != "create" || len(args) < 2 || args[1] == "" {
		return nil, invalidInput("use cloud " + kind + " create NAME")
	}
	flags := commandFlags("cloud " + kind + " create")
	requestID := flags.String("request_id", "", "stable idempotency key for explicit retries")
	wait := flags.Bool("wait", false, "poll until the job reaches a terminal status")
	timeout := flags.Duration("timeout", 20*time.Minute, "maximum polling time; stopping polling does not cancel the job")
	input := cloud.JobInput{Kind: kind + ".create", Name: args[1]}
	var profile, clone *string
	var version *int
	var ttl *time.Duration
	if kind == "clone" {
		profile = flags.String("profile", "", "platform profile ID")
	} else {
		clone = flags.String("clone", "", "ready clone's platform resource ID")
		version = flags.Int("version", 1, "immutable ready snapshot version")
		ttl = flags.Duration("ttl", time.Hour, "fork lifetime, whole seconds from 1m to 24h")
	}
	if err := parse(flags, args[2:], out); err != nil {
		return nil, err
	}
	if *timeout < time.Second || *timeout > time.Hour {
		return nil, invalidInput("--timeout must be between 1s and 1h")
	}
	if kind == "clone" {
		if !cloud.ValidID(*profile) {
			return nil, invalidInput("--profile requires a platform profile ID")
		}
		input.ProfileID = *profile
	} else {
		if !cloud.ValidID(*clone) || (*version < 1 || *version > 10000) || *ttl < time.Minute || *ttl > 24*time.Hour || *ttl%time.Second != 0 {
			return nil, invalidInput("provide --clone RESOURCE_ID, a ready --version and a whole-second ttl between 1m and 24h")
		}
		input.CloneID, input.CloneVersion, input.TTLSeconds = *clone, *version, int(*ttl/time.Second)
	}
	if *requestID == "" {
		*requestID = randomID("req_")
	}
	if !requestIDPattern.MatchString(*requestID) {
		return nil, invalidInput("--request_id requires 16–80 letters, digits, underscores or hyphens")
	}
	input.IdempotencyKey = *requestID
	if _, err := fmt.Fprintf(progress, "Request ID: %s (reuse this ID for the same intent if the response is lost)\n", *requestID); err != nil {
		return nil, err
	}
	job, err := client.CreateJob(ctx, cfg.OrgID, cfg.ProjectID, input)
	if err != nil {
		return map[string]string{"request_id": *requestID}, err
	}
	if *wait {
		job, err = app.waitJob(ctx, client, cfg, job, *timeout, progress)
	}
	return map[string]any{"request_id": *requestID, "job": job}, err
}

func (app cloudApp) job(ctx context.Context, client *cloud.Client, args []string, cfg account.Config, out, progress io.Writer) (any, error) {
	if len(args) == 1 && args[0] == "list" {
		return client.Jobs(ctx, cfg.OrgID, cfg.ProjectID)
	}
	if len(args) < 2 || !cloud.ValidID(args[1]) {
		return nil, invalidInput("use cloud job list, status ID, wait ID or cancel ID")
	}
	switch args[0] {
	case "status", "cancel":
		if len(args) != 2 {
			return nil, invalidInput("unexpected job arguments")
		}
		if args[0] == "cancel" {
			return client.CancelJob(ctx, cfg.OrgID, cfg.ProjectID, args[1])
		}
		return client.Job(ctx, cfg.OrgID, cfg.ProjectID, args[1])
	case "wait":
		flags := commandFlags("cloud job wait")
		timeout := flags.Duration("timeout", 20*time.Minute, "maximum polling time")
		if err := parse(flags, args[2:], out); err != nil {
			return nil, err
		}
		if *timeout < time.Second || *timeout > time.Hour {
			return nil, invalidInput("--timeout must be between 1s and 1h")
		}
		job, err := client.Job(ctx, cfg.OrgID, cfg.ProjectID, args[1])
		if err != nil {
			return nil, err
		}
		return app.waitJob(ctx, client, cfg, job, *timeout, progress)
	default:
		return nil, invalidInput("unknown job command")
	}
}

func (app cloudApp) waitJob(ctx context.Context, client *cloud.Client, cfg account.Config, job cloud.Job, timeout time.Duration, progress io.Writer) (cloud.Job, error) {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	previous := ""
	for {
		if job.Status != previous {
			if _, err := fmt.Fprintf(progress, "%s · %s\n", job.ID, job.Status); err != nil {
				return job, err
			}
			previous = job.Status
		}
		if job.Terminal() {
			if job.Status != "succeeded" {
				return job, &cloud.Error{Code: "job_failed", Message: "Job ended without success; inspect its status and cleanup state."}
			}
			return job, nil
		}
		if job.Status != "queued" && job.Status != "running" && job.Status != "cancelling" {
			return job, &cloud.Error{Code: "unavailable", Message: "Invalid job status returned by Forkden."}
		}
		if err := app.pause(pollCtx, 5*time.Second); err != nil {
			return job, err
		}
		updated, err := client.Job(pollCtx, cfg.OrgID, cfg.ProjectID, job.ID)
		if err != nil {
			return job, err
		}
		job = updated
	}
}

const cloudUsage = `Forkden · account and copy jobs

  forkden --api_url ORIGIN login [--no-browser]
  forkden whoami / logout
  forkden cloud org list
  forkden cloud project list --org ORG_ID
  forkden cloud project use PROJECT_ID --org ORG_ID
  forkden cloud project current
  forkden cloud db list
  forkden cloud clone create NAME --profile PROFILE_ID [--wait]
  forkden cloud fork create NAME --clone RESOURCE_ID [--version N] [--ttl 1h] [--wait]
  forkden cloud clone list / show RESOURCE_ID
  forkden cloud fork list / show RESOURCE_ID
  forkden cloud fork close RESOURCE_ID [--wait]
  forkden cloud clone delete RESOURCE_ID [--wait]
  forkden cloud clone refresh RESOURCE_ID [--wait]
  forkden cloud clone versions RESOURCE_ID
  forkden cloud job list / status JOB_ID / wait JOB_ID / cancel JOB_ID

Use --request_id on create/close/delete/refresh to repeat the same intent safely. Polling uses --timeout
(default 20m). Stopping polling does not cancel work. Running copy cancellation waits for worker cleanup; dispatched close/delete must finish.
Resource lists show physical lifecycle; job list preserves operation history. DB connections
and env/secret references stay on the assigned worker. Cloud query access is pending.
Login uses browser consent and the OS keyring; plaintext token storage is unsupported.
Global --json, --api_url and --config_dir precede commands. Local engine commands
without the cloud prefix retain the Unix socket protocol.
`
