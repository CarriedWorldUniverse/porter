package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// sha256Hex returns the lowercase hex SHA-256 digest of data (data == nil is
// the hash of the empty string, used as the payload hash for GET/DELETE/LIST
// requests).
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// hmacSHA256 is the HMAC-SHA256 primitive the SigV4 signing-key chain and
// final signature are both built from.
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// deriveSigningKey runs the AWS4 HMAC chain: AWS4+secret -> date -> region
// -> service -> "aws4_request".
func deriveSigningKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// signature computes the final hex SigV4 signature over stringToSign.
func signature(secret, dateStamp, region, service, strToSign string) string {
	key := deriveSigningKey(secret, dateStamp, region, service)
	return hex.EncodeToString(hmacSHA256(key, []byte(strToSign)))
}

// isUnreservedByte reports whether b is one of SigV4's "unreserved
// characters" (RFC 3986 unreserved set: ALPHA / DIGIT / "-" / "." / "_" /
// "~"), which AWS's URI-encoding rule leaves untouched.
func isUnreservedByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
		b == '-' || b == '_' || b == '.' || b == '~'
}

// awsURIEncode URI-encodes s per the AWS SigV4 rule: percent-encode every
// byte except the unreserved set, encoding each byte of a multi-byte UTF-8
// sequence separately. When encodeSlash is false, '/' path separators are
// left alone (canonical URI); when true (canonical query keys/values) '/'
// is also percent-encoded.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreservedByte(c) || (c == '/' && !encodeSlash) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalQuery builds SigV4's canonical query string from a set of
// (possibly multi-valued) query parameters: sorted by key, then by value,
// each key and value independently URI-encoded (with '/' encoded).
func canonicalQuery(params map[string][]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), params[k]...)
		sort.Strings(vals)
		encKey := awsURIEncode(k, true)
		for _, v := range vals {
			parts = append(parts, encKey+"="+awsURIEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// canonicalQueryFromMap builds a canonical query string from single-valued
// params, skipping any parameter whose value is empty (so an unset -prefix
// / -continuation-token is simply omitted, matching a real client).
func canonicalQueryFromMap(m map[string]string) string {
	multi := make(map[string][]string, len(m))
	for k, v := range m {
		if v != "" {
			multi[k] = []string{v}
		}
	}
	return canonicalQuery(multi)
}

// canonicalRequestString builds a SigV4 canonical request. headerNames need
// not be pre-sorted; headerValue is consulted for each (already-trimmed
// values are re-trimmed defensively).
func canonicalRequestString(method, canonicalURI, canonicalQueryStr string, headerNames []string, headerValue func(string) string, payloadHash string) string {
	sorted := append([]string(nil), headerNames...)
	sort.Strings(sorted)

	var headers strings.Builder
	for _, name := range sorted {
		headers.WriteString(name)
		headers.WriteByte(':')
		headers.WriteString(strings.TrimSpace(headerValue(name)))
		headers.WriteByte('\n')
	}
	signedHeaders := strings.Join(sorted, ";")

	return strings.Join([]string{
		method,
		canonicalURI,
		canonicalQueryStr,
		headers.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
}

// stringToSign builds SigV4's string-to-sign from the request timestamp,
// credential scope, and the hex SHA-256 of the canonical request.
func stringToSign(amzDate, scope, canonicalRequest string) string {
	return strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
}

// signedHeaderNames returns the (unsorted) set of headers this client signs
// on every request: host, x-amz-content-sha256, x-amz-date, plus
// if-none-match when the request carries one.
func signedHeaderNames(req *http.Request) []string {
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	if req.Header.Get("If-None-Match") != "" {
		names = append(names, "if-none-match")
	}
	return names
}

// ExpectedSignature computes the SigV4 signature a request SHOULD carry,
// given creds and payloadHash, deriving its timestamp from the request's
// own x-amz-date header (already set) rather than any wall clock. It reads
// only req.Method, req.URL (via EscapedPath/Query), req.Host (falling back
// to req.URL.Host), and the small header set signedHeaderNames names.
//
// This single routine is shared by Client (which calls it while building
// the Authorization header on the way out) and s3test (which calls it
// again on the request it actually received off the wire, and rejects any
// mismatch with the Authorization header presented) — so every round trip
// through s3test is also an independent signature check.
func ExpectedSignature(req *http.Request, creds Credentials, payloadHash string) (string, error) {
	amzDate := req.Header.Get("x-amz-date")
	if len(amzDate) < 8 {
		return "", fmt.Errorf("s3: missing or malformed x-amz-date header %q", amzDate)
	}
	dateStamp := amzDate[:8]

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headerValue := func(name string) string {
		if name == "host" {
			return host
		}
		return req.Header.Get(name)
	}

	canonicalURI := req.URL.EscapedPath()
	if canonicalURI == "" {
		canonicalURI = "/"
	}
	canonicalQueryStr := canonicalQuery(map[string][]string(req.URL.Query()))

	cr := canonicalRequestString(req.Method, canonicalURI, canonicalQueryStr, signedHeaderNames(req), headerValue, payloadHash)
	scope := dateStamp + "/" + creds.Region + "/s3/aws4_request"
	sts := stringToSign(amzDate, scope, cr)
	return signature(creds.SecretAccessKey, dateStamp, creds.Region, "s3", sts), nil
}

// applySigning timestamps req, sets its payload-hash header, computes the
// SigV4 signature via ExpectedSignature, and sets the Authorization header.
func (c *Client) applySigning(req *http.Request, payloadHash string) error {
	now := c.now()
	amzDate := now.UTC().Format("20060102T150405Z")
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	sig, err := ExpectedSignature(req, c.creds, payloadHash)
	if err != nil {
		return err
	}
	names := signedHeaderNames(req)
	sort.Strings(names)
	scope := amzDate[:8] + "/" + c.creds.Region + "/s3/aws4_request"
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.creds.AccessKeyID, scope, strings.Join(names, ";"), sig))
	return nil
}

// objectURL builds the path-style URL for one object key under the
// client's bucket, URI-encoding the bucket and key per AWS's canonical-URI
// rule and pinning both Path and RawPath so the bytes actually sent on the
// wire are exactly the bytes canonicalized for signing.
func (c *Client) objectURL(key string) (*url.URL, error) {
	base, err := url.Parse(c.creds.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3: parsing endpoint %q: %w", c.creds.Endpoint, err)
	}
	encPath := "/" + awsURIEncode(c.creds.Bucket, false) + "/" + awsURIEncode(key, false)
	decPath, err := url.PathUnescape(encPath)
	if err != nil {
		return nil, fmt.Errorf("s3: encoding key %q: %w", key, err)
	}
	u := *base
	u.Path = decPath
	u.RawPath = encPath
	u.RawQuery = ""
	return &u, nil
}

// bucketURL builds the path-style URL for the bucket itself (used by
// ListObjectsV2), with query set to the canonical-encoded params.
func (c *Client) bucketURL(query map[string]string) (*url.URL, error) {
	base, err := url.Parse(c.creds.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("s3: parsing endpoint %q: %w", c.creds.Endpoint, err)
	}
	encPath := "/" + awsURIEncode(c.creds.Bucket, false)
	decPath, err := url.PathUnescape(encPath)
	if err != nil {
		return nil, fmt.Errorf("s3: encoding bucket %q: %w", c.creds.Bucket, err)
	}
	u := *base
	u.Path = decPath
	u.RawPath = encPath
	u.RawQuery = canonicalQueryFromMap(query)
	return &u, nil
}
