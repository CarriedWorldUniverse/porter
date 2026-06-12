// porter-backup is the backup (cold) slice of porter: per-source consistent
// snapshots → multi-recipient casket envelopes → a Google Drive app folder,
// with per-run sealed manifests, a restore path that works from a single
// recovery key, and 30-day/monthly-keeper retention.
//
// Commands:
//
//	porter-backup sync [--once]
//	porter-backup restore <timestamp> [--source name] --key <privkey file> --out <dir>
//	porter-backup keygen --out <privkey file>
//
// Configuration is environment-driven; see env.go for the full surface.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CarriedWorldUniverse/porter/internal/snapshot"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "error", err.Error())
		os.Exit(1)
	}
}

func usage() error {
	return fmt.Errorf("usage: porter-backup <sync [--once] | restore <timestamp> [--source name] --key <file> --out <dir> | keygen --out <file>>")
}

func run(log *slog.Logger) error {
	if len(os.Args) < 2 {
		return usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "sync":
		fs := flag.NewFlagSet("sync", flag.ExitOnError)
		once := fs.Bool("once", false, "run a single sync pass and exit")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		return cmdSync(ctx, *once, log)

	case "restore":
		fs := flag.NewFlagSet("restore", flag.ExitOnError)
		source := fs.String("source", "", "restore only this source")
		keyFile := fs.String("key", "", "recipient private key file (keygen output)")
		outDir := fs.String("out", "", "output directory (never a live service path)")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 1 || *keyFile == "" || *outDir == "" {
			return usage()
		}
		return cmdRestore(ctx, fs.Arg(0), *source, *keyFile, *outDir, log)

	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ExitOnError)
		out := fs.String("out", "", "private key output file (written 0600)")
		if err := fs.Parse(os.Args[2:]); err != nil {
			return err
		}
		if *out == "" {
			return usage()
		}
		return runKeygen(*out, os.Stdout)

	default:
		return usage()
	}
}

// buildSyncEnv assembles the production sync environment from env config.
func buildSyncEnv(ctx context.Context, log *slog.Logger) (syncEnv, error) {
	sources, err := loadSources(ctx)
	if err != nil {
		return syncEnv{}, err
	}
	recipients, err := parseRecipients(os.Getenv(envRecipients))
	if err != nil {
		return syncEnv{}, err
	}
	kube, err := kubeClientIfNeeded(sources)
	if err != nil {
		return syncEnv{}, err
	}
	d, err := newDriveClient(ctx)
	if err != nil {
		return syncEnv{}, err
	}
	return syncEnv{
		Drive:      d,
		Runner:     snapshot.Runner{Kube: kube},
		Sources:    sources,
		Recipients: recipients,
		Folder:     envOr(envDriveFolder, defaultDriveFolder),
		Now:        time.Now,
		Log:        log,
	}, nil
}

func cmdSync(ctx context.Context, once bool, log *slog.Logger) error {
	pass := func() error {
		// Rebuild per pass: credentials are brokered at use-time (lazy
		// connection — a failed pass retries cleanly on the next tick).
		env, err := buildSyncEnv(ctx, log)
		if err != nil {
			return err
		}
		start := time.Now()
		m, err := runSyncPass(ctx, env)
		if err != nil {
			return err
		}
		log.Info("sync pass complete",
			"timestamp", m.Timestamp,
			"sources", len(m.Sources),
			"duration", time.Since(start).Round(time.Millisecond).String(),
		)
		return nil
	}

	if once {
		return pass()
	}
	every, err := interval()
	if err != nil {
		return err
	}
	log.Info("porter-backup sync loop starting", "interval", every.String())
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := pass(); err != nil {
			// Keep the loop alive: one failed pass must not kill the pod.
			log.Error("sync pass failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		case <-ticker.C:
		}
	}
}

func cmdRestore(ctx context.Context, ts, source, keyFile, outDir string, log *slog.Logger) error {
	privKey, err := readPrivateKey(keyFile)
	if err != nil {
		return err
	}
	d, err := newDriveClient(ctx)
	if err != nil {
		return err
	}
	folder := envOr(envDriveFolder, defaultDriveFolder)
	if err := runRestore(ctx, d, folder, ts, source, privKey, outDir, log); err != nil {
		return err
	}
	log.Info("restore complete", "timestamp", ts, "out", outDir)
	return nil
}
