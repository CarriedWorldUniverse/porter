package custodian

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	cwbv1 "github.com/CarriedWorldUniverse/cwb-proto/gen/go/cwb/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// fakeCredService records the request + metadata and serves a canned bundle.
type fakeCredService struct {
	cwbv1.UnimplementedCredentialServiceServer

	gotMD      metadata.MD
	gotReq     *cwbv1.FetchRequest
	resp       *cwbv1.FetchResponse
	deny       bool
	emptyOneof bool
}

func (f *fakeCredService) Fetch(ctx context.Context, req *cwbv1.FetchRequest) (*cwbv1.FetchResponse, error) {
	f.gotMD, _ = metadata.FromIncomingContext(ctx)
	f.gotReq = req
	if f.deny {
		return nil, status.Error(codes.PermissionDenied, "scope cred:read required")
	}
	if f.emptyOneof {
		return &cwbv1.FetchResponse{Kind: req.GetKind(), Name: req.GetName()}, nil
	}
	return f.resp, nil
}

func dialFake(t *testing.T, f *fakeCredService) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	cwbv1.RegisterCredentialServiceServer(srv, f)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestFetchOAuth(t *testing.T) {
	f := &fakeCredService{resp: &cwbv1.FetchResponse{
		Kind: "oauth",
		Name: "google-drive",
		Bundle: &cwbv1.FetchResponse_OauthBundle{OauthBundle: &cwbv1.OAuthBundle{
			ClientId:     "cid",
			ClientSecret: "csec",
			RefreshToken: "rtok",
			TokenUri:     "https://oauth2.googleapis.com/token",
			Scope:        "https://www.googleapis.com/auth/drive.file",
		}},
	}}
	c := New(dialFake(t, f), "cwb")

	b, err := c.FetchOAuth(context.Background(), "google-drive")
	if err != nil {
		t.Fatalf("FetchOAuth: %v", err)
	}
	if b.ClientID != "cid" || b.ClientSecret != "csec" || b.RefreshToken != "rtok" ||
		b.TokenURI != "https://oauth2.googleapis.com/token" || !strings.Contains(b.Scope, "drive.file") {
		t.Fatalf("bundle: %+v", b)
	}

	// The request shape custodian's gate audits.
	if f.gotReq.GetKind() != "oauth" || f.gotReq.GetName() != "google-drive" {
		t.Fatalf("request: %+v", f.gotReq)
	}
	if f.gotReq.GetIdentity() != Subject {
		t.Fatalf("identity: %q want %q", f.gotReq.GetIdentity(), Subject)
	}

	// The cwb-* metadata custodian's identity gate reads (mirrors mason's
	// almanac source pattern).
	for k, want := range map[string]string{
		"cwb-subject": Subject,
		"cwb-org":     "cwb",
		"cwb-scopes":  "cred:read",
	} {
		got := f.gotMD.Get(k)
		if len(got) != 1 || got[0] != want {
			t.Fatalf("metadata %s: got %v want %q", k, got, want)
		}
	}
}

func TestFetchOAuthDenied(t *testing.T) {
	c := New(dialFake(t, &fakeCredService{deny: true}), "cwb")
	if _, err := c.FetchOAuth(context.Background(), "google-drive"); err == nil {
		t.Fatal("want error on denial")
	}
}

func TestFetchOAuthEmptyBundle(t *testing.T) {
	c := New(dialFake(t, &fakeCredService{emptyOneof: true}), "cwb")
	_, err := c.FetchOAuth(context.Background(), "google-drive")
	if err == nil || !strings.Contains(err.Error(), "bundle") {
		t.Fatalf("want empty-bundle error, got %v", err)
	}
}

func TestFetchOAuthWrongKindBundle(t *testing.T) {
	f := &fakeCredService{resp: &cwbv1.FetchResponse{
		Kind: "git", Name: "github.com",
		Bundle: &cwbv1.FetchResponse_GitBundle{GitBundle: &cwbv1.GitBundle{Username: "u"}},
	}}
	c := New(dialFake(t, f), "cwb")
	if _, err := c.FetchOAuth(context.Background(), "google-drive"); err == nil {
		t.Fatal("want error when response carries a non-oauth bundle")
	}
}

func TestBundleNeverInStringForm(t *testing.T) {
	// Defense against accidental %v logging: the bundle's String/Format must
	// not reveal secret material.
	b := Bundle{ClientID: "cid", ClientSecret: "SECRET-cs", RefreshToken: "SECRET-rt", TokenURI: "https://t", Scope: "s"}
	for _, rendered := range []string{fmt.Sprintf("%v", b), fmt.Sprintf("%+v", b), fmt.Sprint(b), b.String()} {
		if strings.Contains(rendered, "SECRET-cs") || strings.Contains(rendered, "SECRET-rt") {
			t.Fatalf("secret material leaked via string formatting: %s", rendered)
		}
	}
}

func TestDialRequiresCerts(t *testing.T) {
	if _, err := Dial("localhost:1", "/nope/cert.pem", "/nope/key.pem", "/nope/ca.pem"); err == nil {
		t.Fatal("want error for missing client cert")
	}
}
