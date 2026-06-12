package drive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/CarriedWorldUniverse/porter/internal/drive/drivetest"
)

func newTestClient(t *testing.T, fake *drivetest.Server) *Client {
	t.Helper()
	c, err := New(context.Background(), nil,
		option.WithEndpoint(fake.URL()), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Small chunks + fast backoff so tests exercise multi-chunk resumable
	// uploads without multi-MB payloads or real sleeps.
	c.ChunkSize = 256 * 1024
	c.backoffBase = time.Millisecond
	return c
}

func TestEnsureFolderCreatesPathOnce(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()

	id, err := c.EnsureFolder(ctx, "CarriedWorld-Porter/backups/almanac")
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}
	if id == "" {
		t.Fatal("empty folder id")
	}
	// Idempotent: a second call resolves the SAME folder.
	id2, err := c.EnsureFolder(ctx, "CarriedWorld-Porter/backups/almanac")
	if err != nil {
		t.Fatalf("EnsureFolder(again): %v", err)
	}
	if id2 != id {
		t.Fatalf("EnsureFolder not idempotent: %s vs %s", id, id2)
	}
	// Exactly 3 folders exist (no duplicates).
	var folders int
	for _, f := range fake.Files() {
		if f.MimeType == drivetest.FolderMimeType {
			folders++
		}
	}
	if folders != 3 {
		t.Fatalf("got %d folders, want 3", folders)
	}
	// Sibling path shares the prefix folders.
	if _, err := c.EnsureFolder(ctx, "CarriedWorld-Porter/backups/herald"); err != nil {
		t.Fatalf("EnsureFolder(sibling): %v", err)
	}
	folders = 0
	for _, f := range fake.Files() {
		if f.MimeType == drivetest.FolderMimeType {
			folders++
		}
	}
	if folders != 4 {
		t.Fatalf("after sibling: got %d folders, want 4", folders)
	}
}

func TestUploadResumableChunks(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()

	folder, err := c.EnsureFolder(ctx, "backups/blobs")
	if err != nil {
		t.Fatal(err)
	}

	// 600KB > 2 chunks of 256KB → genuinely resumable (initiate + 3 PUTs).
	payload := make([]byte, 600*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	id, err := c.Upload(ctx, "snap.casket", folder, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	got, ok := fake.FileByName(folder, "snap.casket")
	if !ok {
		t.Fatal("uploaded file not on fake drive")
	}
	if got.ID != id {
		t.Fatalf("id mismatch: %s vs %s", got.ID, id)
	}
	if !bytes.Equal(got.Content, payload) {
		t.Fatalf("content mismatch: got %d bytes want %d", len(got.Content), len(payload))
	}
	if fake.ChunkCount < 3 {
		t.Fatalf("expected >=3 chunk PUTs for 600KB/256KB, got %d", fake.ChunkCount)
	}
}

func TestUploadSmallPayload(t *testing.T) {
	// Below one chunk the SDK uses a single multipart request — content must
	// still land intact.
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()
	folder, err := c.EnsureFolder(ctx, "backups/small")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("tiny sealed manifest")
	id, err := c.Upload(ctx, "m.casket", folder, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	got, ok := fake.FileByName(folder, "m.casket")
	if !ok || got.ID != id || !bytes.Equal(got.Content, payload) {
		t.Fatalf("small upload mismatch: ok=%v %+v", ok, got)
	}
}

func TestUploadRetriesOn429Initiate(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()
	folder, err := c.EnsureFolder(ctx, "backups/retry")
	if err != nil {
		t.Fatal(err)
	}

	fake.FailNextUploadInits(2, http.StatusTooManyRequests)
	payload := bytes.Repeat([]byte("x"), 300*1024)
	if _, err := c.Upload(ctx, "retry.casket", folder, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Upload after 429s: %v", err)
	}
	if fake.InitCount < 3 {
		t.Fatalf("expected >=3 initiate attempts (2 failed + success), got %d", fake.InitCount)
	}
	got, ok := fake.FileByName(folder, "retry.casket")
	if !ok || !bytes.Equal(got.Content, payload) {
		t.Fatal("payload corrupted across retries — reader not rewound?")
	}
}

func TestUploadRetriesOn403RateLimitChunk(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()
	folder, err := c.EnsureFolder(ctx, "backups/retry403")
	if err != nil {
		t.Fatal(err)
	}
	fake.FailNextChunks(1, http.StatusForbidden)
	payload := bytes.Repeat([]byte("y"), 300*1024)
	if _, err := c.Upload(ctx, "r403.casket", folder, bytes.NewReader(payload)); err != nil {
		t.Fatalf("Upload after 403 chunk: %v", err)
	}
	got, ok := fake.FileByName(folder, "r403.casket")
	if !ok || !bytes.Equal(got.Content, payload) {
		t.Fatal("payload corrupted after 403-chunk retry")
	}
}

func TestUploadGivesUpAfterMaxRetries(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()
	folder, err := c.EnsureFolder(ctx, "backups/fail")
	if err != nil {
		t.Fatal(err)
	}
	fake.FailNextUploadInits(100, http.StatusTooManyRequests)
	_, err = c.Upload(ctx, "doomed.casket", folder, strings.NewReader("zzz"))
	if err == nil {
		t.Fatal("Upload: want error when rate limiting never clears")
	}
}

func TestListDownloadDelete(t *testing.T) {
	fake := drivetest.New()
	defer fake.Close()
	c := newTestClient(t, fake)
	ctx := context.Background()
	folder, err := c.EnsureFolder(ctx, "backups/almanac")
	if err != nil {
		t.Fatal(err)
	}
	idA, err := c.Upload(ctx, "a.casket", folder, strings.NewReader("AAAA"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(ctx, "b.casket", folder, strings.NewReader("BBBB")); err != nil {
		t.Fatal(err)
	}

	files, err := c.List(ctx, folder)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("List: got %d files, want 2", len(files))
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
		if f.ID == "" {
			t.Fatalf("List: empty id for %s", f.Name)
		}
	}
	if !names["a.casket"] || !names["b.casket"] {
		t.Fatalf("List names: %v", names)
	}

	rc, err := c.Download(ctx, idA)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(got) != "AAAA" {
		t.Fatalf("Download: %q err=%v", got, err)
	}

	if err := c.Delete(ctx, idA); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	files, err = c.List(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "b.casket" {
		t.Fatalf("after delete: %+v", files)
	}

	if _, err := c.Download(ctx, idA); err == nil {
		t.Fatal("Download(deleted): want error")
	}
}

func TestOAuthTokenSourceExchangesRefreshToken(t *testing.T) {
	// The refresh→access exchange happens HERE in the consumer (per spec):
	// the token source must POST grant_type=refresh_token to token_uri with
	// the client id/secret and use the returned access token.
	var sawGrant, sawRefresh, sawID, sawSecret string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("token endpoint form: %v", err)
		}
		sawGrant = r.FormValue("grant_type")
		sawRefresh = r.FormValue("refresh_token")
		sawID, sawSecret, _ = clientCreds(r)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at-123", "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()

	o := OAuth{
		ClientID:     "cid",
		ClientSecret: "csec",
		RefreshToken: "rt-456",
		TokenURI:     tokenSrv.URL,
		Scope:        "https://www.googleapis.com/auth/drive.file",
	}
	tok, err := o.TokenSource(context.Background()).Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "at-123" {
		t.Fatalf("access token: %q", tok.AccessToken)
	}
	if sawGrant != "refresh_token" || sawRefresh != "rt-456" {
		t.Fatalf("exchange: grant=%q refresh=%q", sawGrant, sawRefresh)
	}
	if sawID != "cid" || sawSecret != "csec" {
		t.Fatalf("client creds not presented: id=%q secret=%q", sawID, sawSecret)
	}
}

// clientCreds extracts client id/secret from basic auth or form fields (the
// oauth2 package may use either).
func clientCreds(r *http.Request) (id, secret string, ok bool) {
	if id, secret, ok = r.BasicAuth(); ok {
		return id, secret, true
	}
	return r.FormValue("client_id"), r.FormValue("client_secret"), false
}
