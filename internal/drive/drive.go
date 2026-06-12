// Package drive is porter-backup's minimal Google Drive client: folder
// resolution, resumable chunked uploads (8MB chunks) with exponential backoff
// on rate limiting (403 rateLimitExceeded / 429), listing, download, and
// delete — exactly the surface a backup pod needs, over the official
// google.golang.org/api/drive/v3 client.
//
// Auth: the consumer performs the OAuth refresh→access exchange itself (per
// the backup-MVP spec — custodian only vaults the bundle). OAuth.TokenSource
// builds a golang.org/x/oauth2 token source from the brokered
// {client_id, client_secret, refresh_token, token_uri} bundle.
package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/oauth2"
	drivev3 "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// FolderMimeType is Drive's folder mime type.
const FolderMimeType = "application/vnd.google-apps.folder"

// DefaultChunkSize is the resumable-upload chunk size (8MB per the spec).
const DefaultChunkSize = 8 << 20

// maxAttempts bounds the rate-limit retry loop (per logical call).
const maxAttempts = 5

// OAuth is the brokered Google OAuth bundle (custodian kind=oauth). The
// refresh token never appears in env or logs — it lives in this struct only
// for the lifetime of a sync pass.
type OAuth struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
	TokenURI     string
	Scope        string
}

// TokenSource returns an oauth2 token source that exchanges the long-lived
// refresh token for short-lived access tokens against TokenURI (caching and
// re-exchanging on expiry).
func (o OAuth) TokenSource(ctx context.Context) oauth2.TokenSource {
	cfg := &oauth2.Config{
		ClientID:     o.ClientID,
		ClientSecret: o.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: o.TokenURI},
		Scopes:       strings.Fields(o.Scope),
	}
	return cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: o.RefreshToken})
}

// File is a listed Drive file.
type File struct {
	ID   string
	Name string
	Size int64
}

// Client is the minimal Drive client.
type Client struct {
	svc *drivev3.Service
	// ChunkSize is the resumable-upload chunk size in bytes.
	ChunkSize int
	// backoffBase is the first retry delay (doubles per attempt); tests
	// shrink it.
	backoffBase time.Duration
}

// New builds a Client. ts is the OAuth token source (see OAuth.TokenSource);
// pass ts == nil only with an explicit auth option (tests use
// option.WithoutAuthentication + option.WithEndpoint).
func New(ctx context.Context, ts oauth2.TokenSource, opts ...option.ClientOption) (*Client, error) {
	if ts != nil {
		opts = append([]option.ClientOption{option.WithTokenSource(ts)}, opts...)
	}
	svc, err := drivev3.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("building drive service: %w", err)
	}
	return &Client{svc: svc, ChunkSize: DefaultChunkSize, backoffBase: 500 * time.Millisecond}, nil
}

// retryable reports whether err is Drive rate limiting (429, or 403 with a
// rate-limit reason).
func retryable(err error) bool {
	var ge *googleapi.Error
	if !errors.As(err, &ge) {
		return false
	}
	if ge.Code == 429 {
		return true
	}
	if ge.Code == 403 {
		for _, e := range ge.Errors {
			switch e.Reason {
			case "rateLimitExceeded", "userRateLimitExceeded":
				return true
			}
		}
	}
	return false
}

// withRetry runs fn with exponential backoff on rate-limit errors.
func (c *Client) withRetry(ctx context.Context, fn func() error) error {
	delay := c.backoffBase
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = fn(); err == nil || !retryable(err) {
			return err
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
		delay *= 2
	}
	return fmt.Errorf("giving up after %d rate-limited attempts: %w", maxAttempts, err)
}

// escapeQuery escapes a string literal for Drive's query language.
func escapeQuery(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// EnsureFolder resolves a slash-separated folder path under the Drive root,
// creating missing segments, and returns the leaf folder id. Idempotent.
func (c *Client) EnsureFolder(ctx context.Context, path string) (string, error) {
	parent := "root"
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		id, err := c.findChildFolder(ctx, parent, seg)
		if err != nil {
			return "", err
		}
		if id == "" {
			var created *drivev3.File
			err := c.withRetry(ctx, func() error {
				var err error
				created, err = c.svc.Files.Create(&drivev3.File{
					Name:     seg,
					MimeType: FolderMimeType,
					Parents:  []string{parent},
				}).Fields("id").Context(ctx).Do()
				return err
			})
			if err != nil {
				return "", fmt.Errorf("creating folder %q under %s: %w", seg, parent, err)
			}
			id = created.Id
		}
		parent = id
	}
	return parent, nil
}

func (c *Client) findChildFolder(ctx context.Context, parent, name string) (string, error) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType = '%s' and trashed = false",
		escapeQuery(name), escapeQuery(parent), FolderMimeType)
	var list *drivev3.FileList
	err := c.withRetry(ctx, func() error {
		var err error
		list, err = c.svc.Files.List().Q(q).Fields("files(id)").PageSize(2).Context(ctx).Do()
		return err
	})
	if err != nil {
		return "", fmt.Errorf("looking up folder %q under %s: %w", name, parent, err)
	}
	if len(list.Files) == 0 {
		return "", nil
	}
	return list.Files[0].Id, nil
}

// Upload uploads r as name under the parent folder and returns the new file
// id. Uploads are resumable and chunked (ChunkSize); rate-limit errors are
// retried with backoff — across whole-call retries the reader is rewound
// when it is an io.Seeker (snapshot artifacts are files, so it always is in
// practice). Mid-upload chunk rate limiting is additionally retried inside
// the google-api client itself.
func (c *Client) Upload(ctx context.Context, name, parentID string, r io.Reader) (string, error) {
	var f *drivev3.File
	first := true
	err := c.withRetry(ctx, func() error {
		if !first {
			s, ok := r.(io.Seeker)
			if !ok {
				return fmt.Errorf("cannot retry upload of %q: reader is not seekable", name)
			}
			if _, err := s.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("rewinding %q for retry: %w", name, err)
			}
		}
		first = false
		var err error
		f, err = c.svc.Files.Create(&drivev3.File{
			Name:    name,
			Parents: []string{parentID},
		}).Media(r, googleapi.ChunkSize(c.ChunkSize)).Fields("id").Context(ctx).Do()
		return err
	})
	if err != nil {
		return "", fmt.Errorf("uploading %q: %w", name, err)
	}
	return f.Id, nil
}

// List returns the (non-trashed) files directly under a folder.
func (c *Client) List(ctx context.Context, folderID string) ([]File, error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", escapeQuery(folderID))
	var out []File
	pageToken := ""
	for {
		var list *drivev3.FileList
		err := c.withRetry(ctx, func() error {
			var err error
			list, err = c.svc.Files.List().Q(q).
				Fields("nextPageToken", "files(id, name, size)").
				PageToken(pageToken).Context(ctx).Do()
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("listing folder %s: %w", folderID, err)
		}
		for _, f := range list.Files {
			out = append(out, File{ID: f.Id, Name: f.Name, Size: f.Size})
		}
		pageToken = list.NextPageToken
		if pageToken == "" {
			return out, nil
		}
	}
}

// Download streams a file's content. The caller closes the reader.
func (c *Client) Download(ctx context.Context, id string) (io.ReadCloser, error) {
	var body io.ReadCloser
	err := c.withRetry(ctx, func() error {
		resp, err := c.svc.Files.Get(id).Context(ctx).Download()
		if err != nil {
			return err
		}
		body = resp.Body
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", id, err)
	}
	return body, nil
}

// Delete permanently deletes a file by id.
func (c *Client) Delete(ctx context.Context, id string) error {
	err := c.withRetry(ctx, func() error {
		return c.svc.Files.Delete(id).Context(ctx).Do()
	})
	if err != nil {
		return fmt.Errorf("deleting %s: %w", id, err)
	}
	return nil
}
