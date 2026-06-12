package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc/metadata"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/CarriedWorldUniverse/porter/internal/custodian"
	"github.com/CarriedWorldUniverse/porter/internal/drive"
	"github.com/CarriedWorldUniverse/porter/internal/snapshot"
)

// Environment surface (MASON-style env config):
//
//	PORTER_SOURCES_FILE       local sources YAML path (v1 primary)
//	PORTER_ALMANAC_GRPC_ADDR  almanac ConfigService addr (alternative source
//	                          of the same YAML, parameter PORTER_ALMANAC_PARAM)
//	PORTER_ALMANAC_PARAM      almanac parameter path (default
//	                          cwb/porter/backup/sources)
//	PORTER_RECIPIENTS         comma-separated base64 X25519 public keys
//	PORTER_DRIVE_FOLDER       Drive base folder (default CarriedWorld-Porter/backups)
//	PORTER_INTERVAL           sync interval (default 6h)
//	CUSTODIAN_GRPC_ADDR       custodian vault addr (oauth bundle broker)
//	CUSTODIAN_ORG             tenant org presented to custodian/almanac
//	PORTER_TLS_CERT/_KEY/_CA  client mTLS material for custodian + almanac
//	PORTER_DRIVE_OAUTH_FILE   raw oauth bundle JSON file — the bare-metal
//	                          restore path (no cluster, no custodian)
const (
	envSourcesFile    = "PORTER_SOURCES_FILE"
	envAlmanacAddr    = "PORTER_ALMANAC_GRPC_ADDR"
	envAlmanacParam   = "PORTER_ALMANAC_PARAM"
	envRecipients     = "PORTER_RECIPIENTS"
	envDriveFolder    = "PORTER_DRIVE_FOLDER"
	envInterval       = "PORTER_INTERVAL"
	envCustodianAddr  = "CUSTODIAN_GRPC_ADDR"
	envCustodianOrg   = "CUSTODIAN_ORG"
	envTLSCert        = "PORTER_TLS_CERT"
	envTLSKey         = "PORTER_TLS_KEY"
	envTLSCA          = "PORTER_TLS_CA"
	envDriveOAuthFile = "PORTER_DRIVE_OAUTH_FILE"

	defaultAlmanacParam = "cwb/porter/backup/sources"
	defaultDriveFolder  = "CarriedWorld-Porter/backups"
	defaultInterval     = 6 * time.Hour
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// loadSources reads the backup sources config: PORTER_SOURCES_FILE if set,
// else the almanac parameter via ConfigService over mTLS.
func loadSources(ctx context.Context) ([]snapshot.Source, error) {
	if path := os.Getenv(envSourcesFile); path != "" {
		return snapshot.LoadConfigFile(path)
	}
	addr := os.Getenv(envAlmanacAddr)
	if addr == "" {
		return nil, fmt.Errorf("no sources config: set %s or %s", envSourcesFile, envAlmanacAddr)
	}
	conn, err := custodian.Dial(addr, os.Getenv(envTLSCert), os.Getenv(envTLSKey), os.Getenv(envTLSCA))
	if err != nil {
		return nil, fmt.Errorf("almanac: %w", err)
	}
	defer conn.Close()
	mdCtx := metadata.AppendToOutgoingContext(ctx,
		"cwb-subject", custodian.Subject,
		"cwb-org", os.Getenv(envCustodianOrg),
		"cwb-scopes", "config:read",
	)
	param := envOr(envAlmanacParam, defaultAlmanacParam)
	resp, err := cwbv1.NewConfigServiceClient(conn).GetConfig(mdCtx, &cwbv1.GetConfigRequest{Path: param})
	if err != nil {
		return nil, fmt.Errorf("almanac: get %s: %w", param, err)
	}
	return snapshot.ParseConfig([]byte(resp.GetItem().GetValue()))
}

// driveOAuth obtains the Google oauth bundle: from a local raw-bundle JSON
// file (bare-metal restores) or brokered from custodian (the normal pod
// path). The bundle is held in memory only.
func driveOAuth(ctx context.Context) (drive.OAuth, error) {
	if path := os.Getenv(envDriveOAuthFile); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return drive.OAuth{}, fmt.Errorf("oauth bundle file: %w", err)
		}
		var b struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			RefreshToken string `json:"refresh_token"`
			TokenURI     string `json:"token_uri"`
			Scope        string `json:"scope"`
		}
		if err := json.Unmarshal(data, &b); err != nil {
			return drive.OAuth{}, fmt.Errorf("oauth bundle file %s: %w", path, err)
		}
		return drive.OAuth{
			ClientID: b.ClientID, ClientSecret: b.ClientSecret,
			RefreshToken: b.RefreshToken, TokenURI: b.TokenURI, Scope: b.Scope,
		}, nil
	}

	addr := os.Getenv(envCustodianAddr)
	if addr == "" {
		return drive.OAuth{}, fmt.Errorf("no Drive credentials: set %s or %s", envCustodianAddr, envDriveOAuthFile)
	}
	conn, err := custodian.Dial(addr, os.Getenv(envTLSCert), os.Getenv(envTLSKey), os.Getenv(envTLSCA))
	if err != nil {
		return drive.OAuth{}, err
	}
	defer conn.Close()
	bundle, err := custodian.New(conn, os.Getenv(envCustodianOrg)).FetchOAuth(ctx, custodian.NameGoogleDrive)
	if err != nil {
		return drive.OAuth{}, err
	}
	return drive.OAuth{
		ClientID: bundle.ClientID, ClientSecret: bundle.ClientSecret,
		RefreshToken: bundle.RefreshToken, TokenURI: bundle.TokenURI, Scope: bundle.Scope,
	}, nil
}

// newDriveClient builds the production Drive client (custodian-brokered or
// file-supplied oauth bundle → token source → drive.Client).
func newDriveClient(ctx context.Context) (*drive.Client, error) {
	oauth, err := driveOAuth(ctx)
	if err != nil {
		return nil, err
	}
	return drive.New(ctx, oauth.TokenSource(ctx))
}

// kubeClientIfNeeded builds a k8s client only when a secrets source exists
// (lazy connection principle): in-cluster config first, kubeconfig fallback
// for dev.
func kubeClientIfNeeded(sources []snapshot.Source) (kubernetes.Interface, error) {
	needed := false
	for _, s := range sources {
		if s.Type == snapshot.TypeSecrets {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("secrets source configured but no k8s config (in-cluster or kubeconfig): %w", err)
		}
	}
	return kubernetes.NewForConfig(cfg)
}

// interval parses PORTER_INTERVAL (default 6h).
func interval() (time.Duration, error) {
	v := os.Getenv(envInterval)
	if v == "" {
		return defaultInterval, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: bad duration %q", envInterval, v)
	}
	return d, nil
}
