package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/forkden/cli/api/cloud"
	"github.com/forkden/cli/internal/account"
)

func (app cloudApp) workStore() account.WorkCredentials {
	if app.workCredentials != nil {
		return app.workCredentials
	}
	return account.WorkKeyring{}
}

func (app cloudApp) workSession(ctx context.Context, client *cloud.Client, args []string, cfg account.Config, store *account.Store) (any, error) {
	if err := selection(cfg); err != nil {
		return nil, err
	}
	if len(args) == 0 {
		return nil, invalidInput("use cloud session create FORK_ID, show or revoke")
	}
	switch args[0] {
	case "create":
		flags := commandFlags("cloud session create")
		ttl := flags.Duration("ttl", 10*time.Minute, "short-lived session lifetime")
		rows := flags.Bool("allow_rows", false, "explicitly permit transient result rows and review samples")
		if err := flags.Parse(args[1:]); err != nil {
			return nil, invalidInput(err.Error())
		}
		if flags.NArg() != 1 || *ttl < 30*time.Second || *ttl > 10*time.Minute || *ttl%time.Second != 0 {
			return nil, invalidInput("use cloud session create [--ttl 10m] [--allow_rows] FORK_ID")
		}
		if cfg.WorkSessionID != "" {
			if err := app.clearWork(ctx, client, &cfg); err != nil {
				return nil, err
			}
			if err := store.Save(cfg); err != nil {
				return nil, err
			}
		}
		grant, err := client.CreateWorkSession(ctx, cfg.OrgID, cfg.ProjectID, flags.Arg(0), int(*ttl/time.Second), *rows)
		if err != nil {
			return nil, err
		}
		if err := app.workStore().Set(grant.Session.ID, account.Credential{APIURL: cfg.APIURL, Token: grant.Token}); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return nil, errors.Join(err, client.RevokeWorkSession(cleanupCtx, cfg.OrgID, cfg.ProjectID, grant.Session.ID))
		}
		cfg.WorkSessionID, cfg.WorkCredentialID, cfg.WorkResourceID = grant.Session.ID, grant.Session.ID, grant.Session.ResourceID
		if err := store.Save(cfg); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			revokeErr := client.RevokeWorkSession(cleanupCtx, cfg.OrgID, cfg.ProjectID, grant.Session.ID)
			deleteErr := app.workStore().Delete(grant.Session.ID)
			return nil, errors.Join(err, revokeErr, deleteErr)
		}
		return grant.Session, nil
	case "show":
		if len(args) != 1 {
			return nil, invalidInput("use cloud session show")
		}
		client, err := app.workClient(ctx, cfg)
		if err != nil {
			return nil, err
		}
		defer client.Close()
		return client.Session(ctx)
	case "revoke":
		if len(args) != 1 || cfg.WorkSessionID == "" {
			return nil, invalidInput("no selected work session")
		}
		if err := client.RevokeWorkSession(ctx, cfg.OrgID, cfg.ProjectID, cfg.WorkSessionID); err != nil {
			return nil, err
		}
		if err := app.workStore().Delete(cfg.WorkCredentialID); err != nil {
			return nil, err
		}
		cfg.WorkSessionID, cfg.WorkResourceID, cfg.WorkCredentialID = "", "", ""
		return nil, store.Save(cfg)
	default:
		return nil, invalidInput("unknown cloud session command")
	}
}

func (app cloudApp) workClient(ctx context.Context, cfg account.Config) (*cloud.WorkClient, error) {
	if cfg.WorkCredentialID == "" {
		return nil, invalidInput("create a fork-bound work session first")
	}
	credential, err := app.workStore().Get(cfg.WorkCredentialID)
	if err != nil {
		return nil, err
	}
	if credential.APIURL != cfg.APIURL {
		return nil, invalidInput("work credential is bound to another origin")
	}
	client, err := cloud.NewWorkClient(cfg.APIURL, credential.Token)
	if err != nil {
		return nil, err
	}
	session, err := client.Session(ctx)
	if err != nil {
		client.Close()
		return nil, err
	}
	if session.ID != cfg.WorkSessionID || session.OrgID != cfg.OrgID || session.ProjectID != cfg.ProjectID || session.ResourceID != cfg.WorkResourceID {
		client.Close()
		return nil, invalidInput("work session differs from the selected workspace/fork")
	}
	return client, nil
}

func (app cloudApp) clearWork(ctx context.Context, client *cloud.Client, cfg *account.Config) error {
	if cfg.WorkSessionID == "" {
		return nil
	}
	if err := client.RevokeWorkSession(ctx, cfg.OrgID, cfg.ProjectID, cfg.WorkSessionID); err != nil {
		var public *cloud.Error
		if !errors.As(err, &public) || (public.Code != "unauthorized" && public.Code != "not_found") {
			return err
		}
	}
	if err := app.workStore().Delete(cfg.WorkCredentialID); err != nil {
		return err
	}
	cfg.WorkSessionID, cfg.WorkResourceID, cfg.WorkCredentialID = "", "", ""
	return nil
}

func (app cloudApp) work(ctx context.Context, kind string, args []string, cfg account.Config, _ io.Writer, progress io.Writer) (any, error) {
	flags := commandFlags("cloud " + kind)
	requestID := flags.String("request_id", "", "explicit request ID; execution is never automatically retried")
	outputDir := flags.String("output_dir", "", "new export directory")
	var sqlFlag, fileFlag string
	if kind == "query" || kind == "check" {
		flags.StringVar(&sqlFlag, "sql", "", "one SQL statement")
		flags.StringVar(&fileFlag, "file", "", "file containing one SQL statement")
	}
	if err := flags.Parse(args); err != nil {
		return nil, invalidInput(err.Error())
	}
	var sql string
	if kind == "query" || kind == "check" {
		if flags.NArg() > 1 || (flags.NArg() == 1 && (sqlFlag != "" || fileFlag != "")) {
			return nil, invalidInput("provide SQL once: a quoted statement, --sql or --file")
		}
		if flags.NArg() == 1 {
			sqlFlag = flags.Arg(0)
		}
		var err error
		sql, err = statementInput(sqlFlag, fileFlag)
		if err != nil {
			return nil, err
		}
	} else if flags.NArg() != 0 {
		return nil, invalidInput("this work command takes no SQL")
	}
	if kind == "export" && *outputDir == "" {
		return nil, invalidInput("cloud export requires --output_dir for a new directory")
	}
	if *requestID == "" {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		*requestID = "wrq_" + hex.EncodeToString(random[:])
	}
	if _, err := fmt.Fprintln(progress, "Request ID:", *requestID); err != nil {
		return nil, err
	}
	client, err := app.workClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	result, executionErr := client.Execute(ctx, cloud.WorkInput{ID: *requestID, Kind: kind, SQL: sql})
	if executionErr != nil {
		return result, executionErr
	}
	if kind == "export" {
		if err := os.Mkdir(*outputDir, 0o700); err != nil {
			return nil, errors.New("export requires a new directory; existing paths are preserved")
		}
		if err := os.WriteFile(filepath.Join(*outputDir, "changes.sql"), []byte(result.SQL), 0o600); err != nil {
			return nil, errors.New("cannot write exported SQL")
		}
		var report any
		if err := json.Unmarshal(result.Report, &report); err != nil {
			return nil, errors.New("invalid export report")
		}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(*outputDir, "report.json"), data, 0o600); err != nil {
			return nil, errors.New("cannot write exported report")
		}
		return map[string]any{"request_id": *requestID, "output_dir": *outputDir, "status": result.Status}, nil
	}
	return result, nil
}
