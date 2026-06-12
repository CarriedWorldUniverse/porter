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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/CarriedWorldUniverse/porter/internal/snapshot"
	"github.com/CarriedWorldUniverse/porter/internal/status"
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
	// holder records pass outcomes for the BackupStatusService (loop mode
	// only — --once exits after one pass, nothing would serve the state).
	var holder *status.Holder
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
		if holder != nil {
			srcs := make([]status.Source, 0, len(m.Sources))
			for _, s := range m.Sources {
				srcs = append(srcs, status.Source{Name: s.Name, SizeBytes: s.Size})
			}
			holder.RecordSuccess(time.Now().UTC(), srcs)
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
	holder = status.NewHolder(every)
	if err := serveStatus(ctx, holder, log); err != nil {
		return err
	}
	log.Info("porter-backup sync loop starting", "interval", every.String())
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if err := pass(); err != nil {
			// Keep the loop alive: one failed pass must not kill the pod.
			holder.RecordFailure(time.Now().UTC(), err.Error())
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

// serveStatus starts the cwb.v1.BackupStatusService gRPC server (the Strata
// map's backup clock) on PORTER_GRPC_ADDR, serving in a goroutine and
// stopping gracefully on ctx cancellation. Startup failure is fatal to the
// sync loop: a porter that can't report its clock should not run silently.
func serveStatus(ctx context.Context, h *status.Holder, log *slog.Logger) error {
	opts, err := statusServerOptions(log)
	if err != nil {
		return err
	}
	srv := grpc.NewServer(opts...)
	cwbv1.RegisterBackupStatusServiceServer(srv, status.NewServer(h))

	healthSrv := health.NewServer()
	grpc_health_v1.RegisterHealthServer(srv, healthSrv)
	healthSrv.SetServingStatus("cwb.v1.BackupStatusService", grpc_health_v1.HealthCheckResponse_SERVING)

	addr := envOr(envGRPCAddr, defaultGRPCAddr)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("status server: listen %s: %w", addr, err)
	}
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Error("status server exited", "error", err.Error())
		}
	}()
	log.Info("backup status gRPC listening", "addr", addr)
	return nil
}

// statusServerOptions builds the status server's gRPC options. When the
// PORTER_SERVER_TLS_* env vars are set the server enforces mTLS
// (RequireAndVerifyClientCert). Insecure mode requires an explicit
// PORTER_DEV_INSECURE=1 opt-in; missing certs without the opt-in are a
// startup error (fail-closed). These are the SERVER certs — distinct from
// PORTER_TLS_* (porter's client identity toward custodian/almanac).
func statusServerOptions(log *slog.Logger) ([]grpc.ServerOption, error) {
	if os.Getenv(envDevInsecure) == "1" {
		log.Warn("PORTER_DEV_INSECURE=1 — status server starting WITHOUT mTLS (dev only)")
		return nil, nil
	}
	certFile := os.Getenv(envServerTLSCert)
	keyFile := os.Getenv(envServerTLSKey)
	caFile := os.Getenv(envServerTLSCA)
	if certFile == "" || keyFile == "" || caFile == "" {
		return nil, fmt.Errorf("status server: mTLS required — set %s/%s/%s (or %s=1 for local dev)",
			envServerTLSCert, envServerTLSKey, envServerTLSCA, envDevInsecure)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("status server: tls: load cert/key: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("status server: tls: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("status server: tls: no certs parsed from CA file %s", caFile)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}
	return []grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsCfg))}, nil
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
