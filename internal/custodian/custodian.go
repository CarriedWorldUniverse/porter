// Package custodian is porter-backup's tiny mTLS client for the custodian
// credential vault: it fetches ONE thing — the Google OAuth bundle
// (kind=oauth, name=google-drive) — via cwb-proto's CredentialService.Fetch,
// presenting the cwb-subject/cwb-org/cwb-scopes metadata custodian's identity
// gate reads (the same pattern as mason's almanac source). The refresh token
// returned here lives only in memory for the sync pass; it is never written
// to env, logs, or manifests.
package custodian

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// Subject is the identity porter-backup presents to custodian.
const Subject = "porter-backup"

// KindOAuth / NameGoogleDrive are the credential coordinates of the brokered
// Drive bundle.
const (
	KindOAuth       = "oauth"
	NameGoogleDrive = "google-drive"
)

// Bundle is the decrypted oauth credential bundle: {client_id, client_secret,
// refresh_token, token_uri, scope}. ClientSecret and RefreshToken are secret
// material — see String.
type Bundle struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
	TokenURI     string
	Scope        string
}

// String renders the bundle WITHOUT secret material, so an accidental %v/%s
// log line never leaks the refresh token or client secret.
func (b Bundle) String() string {
	return fmt.Sprintf("oauth bundle{client_id: %s, token_uri: %s, scope: %s, client_secret: [redacted], refresh_token: [redacted]}",
		b.ClientID, b.TokenURI, b.Scope)
}

// Client fetches credentials from custodian over an established (mTLS) gRPC
// connection.
type Client struct {
	svc cwbv1.CredentialServiceClient
	org string
}

// New builds a Client over an existing connection (see Dial). org is the
// tenant org porter-backup presents (CUSTODIAN_ORG).
func New(conn grpc.ClientConnInterface, org string) *Client {
	return &Client{svc: cwbv1.NewCredentialServiceClient(conn), org: org}
}

// FetchOAuth fetches the oauth bundle named name (NameGoogleDrive in
// production). Every call is audited by custodian — success and denial.
func (c *Client) FetchOAuth(ctx context.Context, name string) (Bundle, error) {
	ctx = metadata.AppendToOutgoingContext(ctx,
		"cwb-subject", Subject,
		"cwb-org", c.org,
		"cwb-scopes", "cred:read",
	)
	resp, err := c.svc.Fetch(ctx, &cwbv1.FetchRequest{
		Identity: Subject,
		Kind:     KindOAuth,
		Name:     name,
	})
	if err != nil {
		return Bundle{}, fmt.Errorf("custodian: fetch oauth %q: %w", name, err)
	}
	ob := resp.GetOauthBundle()
	if ob == nil {
		return Bundle{}, fmt.Errorf("custodian: fetch oauth %q: response carries no oauth bundle (kind %q)", name, resp.GetKind())
	}
	return Bundle{
		ClientID:     ob.GetClientId(),
		ClientSecret: ob.GetClientSecret(),
		RefreshToken: ob.GetRefreshToken(),
		TokenURI:     ob.GetTokenUri(),
		Scope:        ob.GetScope(),
	}, nil
}

// Dial connects to custodian with client mTLS (TLS 1.3, cwb-ca trust) — the
// transport custodian requires before it will return plaintext bundles.
func Dial(addr, certFile, keyFile, caFile string) (*grpc.ClientConn, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("custodian client tls: load cert/key: %w", err)
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("custodian client tls: read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("custodian client tls: no certs parsed from CA file %s", caFile)
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("custodian: dial %s: %w", addr, err)
	}
	return conn, nil
}
