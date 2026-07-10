package s3

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
)

// do builds, signs, and performs one request against u, sending body (nil
// for none) with any extraHeaders set before signing (so e.g. If-None-Match
// is included in the signed header set). The caller must close the
// response body.
func (c *Client) do(ctx context.Context, method string, u *url.URL, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	req := &http.Request{
		Method: method,
		URL:    u,
		Host:   u.Host,
		Header: make(http.Header),
	}
	req = req.WithContext(ctx)
	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	payloadHash := sha256Hex(body)
	if err := c.applySigning(req, payloadHash); err != nil {
		return nil, fmt.Errorf("signing request: %w", err)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("performing request: %w", err)
	}
	return resp, nil
}

// PutObject writes key's contents. When ifNoneMatch is true, the write is
// conditional on the key not already existing (If-None-Match: "*"); a 412
// Precondition Failed response is wrapped as ErrPreconditionFailed.
func (c *Client) PutObject(ctx context.Context, key string, data []byte, ifNoneMatch bool) error {
	u, err := c.objectURL(key)
	if err != nil {
		return err
	}
	var headers map[string]string
	if ifNoneMatch {
		headers = map[string]string{"If-None-Match": "*"}
	}
	resp, err := c.do(ctx, http.MethodPut, u, data, headers)
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		io.Copy(io.Discard, resp.Body)
		return nil
	case http.StatusPreconditionFailed:
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("s3: put %s: %w", key, ErrPreconditionFailed)
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("s3: put %s: unexpected status %d: %s", key, resp.StatusCode, respBody)
	}
}

// GetObject reads key's full contents. A 404 response is wrapped as
// ErrNotFound.
func (c *Client) GetObject(ctx context.Context, key string) ([]byte, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodGet, u, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("s3: get %s: %w", key, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("s3: get %s: reading body: %w", key, err)
		}
		return data, nil
	case http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("s3: get %s: %w", key, ErrNotFound)
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("s3: get %s: unexpected status %d: %s", key, resp.StatusCode, respBody)
	}
}

// DeleteObject removes key. S3 delete is idempotent: both a 204 (deleted)
// and a 404 (already absent) are treated as success.
func (c *Client) DeleteObject(ctx context.Context, key string) error {
	u, err := c.objectURL(key)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodDelete, u, nil, nil)
	if err != nil {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent, http.StatusNotFound:
		io.Copy(io.Discard, resp.Body)
		return nil
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("s3: delete %s: unexpected status %d: %s", key, resp.StatusCode, respBody)
	}
}

// listBucketResult is the subset of a ListObjectsV2 XML response body this
// client needs.
type listBucketResult struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	IsTruncated           bool     `xml:"IsTruncated"`
	NextContinuationToken string   `xml:"NextContinuationToken"`
	Contents              []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// ListObjects returns the lexicographically sorted keys of every object
// under prefix, paginating with ListObjectsV2's continuation token until
// IsTruncated is false.
func (c *Client) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	token := ""
	for {
		params := map[string]string{"list-type": "2"}
		if prefix != "" {
			params["prefix"] = prefix
		}
		if token != "" {
			params["continuation-token"] = token
		}
		u, err := c.bucketURL(params)
		if err != nil {
			return nil, err
		}
		resp, err := c.do(ctx, http.MethodGet, u, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("s3: list %q: %w", prefix, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3: list %q: reading body: %w", prefix, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("s3: list %q: unexpected status %d: %s", prefix, resp.StatusCode, body)
		}
		var result listBucketResult
		if err := xml.Unmarshal(body, &result); err != nil {
			return nil, fmt.Errorf("s3: list %q: parsing response: %w", prefix, err)
		}
		for _, c := range result.Contents {
			out = append(out, c.Key)
		}
		if !result.IsTruncated {
			break
		}
		token = result.NextContinuationToken
	}
	sort.Strings(out)
	return out, nil
}
