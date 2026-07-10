// Package s3 is a minimal, dependency-free Amazon S3 (and S3-compatible,
// e.g. Cloudflare R2) REST client: exactly the four operations
// packstore's s3backend needs — PutObject, GetObject, ListObjects,
// DeleteObject — hand-signed with AWS Signature Version 4 over the stdlib
// net/http. No SDK, no new module dependency.
//
// See sigv4.go for the signing math, pinned against the canonical AWS
// documentation worked example in sigv4_test.go, and s3test for an
// in-process fake server that independently re-derives and checks every
// signature it receives.
package s3

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ErrNotFound is the sentinel GetObject wraps when the object does not
// exist (HTTP 404).
var ErrNotFound = errors.New("s3: object not found")

// ErrPreconditionFailed is the sentinel PutObject wraps when a conditional
// (If-None-Match: *) write loses because the object already exists (HTTP
// 412). packstore/s3backend maps this to packstore.ErrExists.
var ErrPreconditionFailed = errors.New("s3: precondition failed: object already exists")

// Credentials is the brokered bundle needed to talk to one S3-compatible
// bucket: an access key pair, the service endpoint (path-style, e.g.
// Cloudflare R2's https://<account>.r2.cloudflarestorage.com), the bucket
// name, and the signing region ("auto" for R2).
type Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
}

// CredentialsFromFile loads a raw JSON credentials bundle file (the
// bare-metal, no-cluster path used by one-shot tools like porterpack, same
// posture as drive.OAuthFromBundleFile). The bundle holds the secret access
// key, so the file must not be group/world-readable — CredentialsFromFile
// refuses any mode with perm&0o077 != 0. All five fields are required.
func CredentialsFromFile(path string) (Credentials, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("s3 credentials file: %w", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Credentials{}, fmt.Errorf("s3 credentials file %s has loose permissions %04o: it holds the secret access key, chmod it to 0600", path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("s3 credentials file: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return Credentials{}, fmt.Errorf("s3 credentials file %s: %w", path, err)
	}
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.Endpoint == "" || c.Bucket == "" || c.Region == "" {
		return Credentials{}, fmt.Errorf("s3 credentials file %s: access_key_id, secret_access_key, endpoint, bucket, and region are all required", path)
	}
	return c, nil
}

// Client is the minimal SigV4-signed S3 REST client.
type Client struct {
	creds Credentials
	hc    *http.Client

	// now is the clock used to timestamp requests; tests pin it to a fixed
	// time so signatures are reproducible.
	now func() time.Time
}

// New builds a Client for creds. If httpClient is nil, http.DefaultClient
// is used.
func New(c Credentials, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{creds: c, hc: httpClient, now: time.Now}
}
