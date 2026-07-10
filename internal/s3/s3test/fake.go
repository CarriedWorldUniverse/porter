// Package s3test is an in-process fake of the small slice of the S3 REST
// API internal/s3.Client uses: path-style PUT (honoring If-None-Match: "*"
// with a 412), GET, DELETE, and ListObjectsV2 (with prefix filtering and
// pagination — the page size is deliberately small so tests exercise
// continuation tokens without multi-MB fixtures).
//
// Every request is independently re-verified: the fake recomputes the
// SigV4 signature it SHOULD have received (via s3.ExpectedSignature, using
// the same test credentials handed to the client under test) and rejects
// any mismatch with a 403 — so every round trip a test makes through the
// real client is also a signing test.
package s3test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/CarriedWorldUniverse/porter/internal/s3"
)

// sha256Hex is a local copy of the same tiny helper internal/s3 uses
// (unexported there): the fake independently recomputes the payload hash
// off the wire rather than trusting the client's header.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DefaultPageSize is the fake's default ListObjectsV2 page size — small on
// purpose to force pagination in tests without huge fixtures.
const DefaultPageSize = 2

// Server is the fake S3-compatible server.
type Server struct {
	mu       sync.Mutex
	srv      *httptest.Server
	bucket   string
	creds    s3.Credentials
	objects  map[string][]byte
	pageSize int
}

// New starts a fake S3 server for one bucket, verifying every request
// against creds (the same Credentials value the Client under test uses).
func New(bucket string, creds s3.Credentials) *Server {
	s := &Server{
		bucket:   bucket,
		creds:    creds,
		objects:  map[string][]byte{},
		pageSize: DefaultPageSize,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.srv = httptest.NewServer(mux)
	return s
}

// URL is the server base URL (use as Credentials.Endpoint).
func (s *Server) URL() string { return s.srv.URL }

// Close shuts the server down.
func (s *Server) Close() { s.srv.Close() }

// SetPageSize changes the ListObjectsV2 page size (test arrangement).
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// Objects returns a snapshot of every stored key -> content.
func (s *Server) Objects() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]byte, len(s.objects))
	for k, v := range s.objects {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if !s.verifySignature(w, r, body) {
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] != s.bucket {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}

	if len(parts) < 2 || parts[1] == "" {
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			s.handleList(w, r)
			return
		}
		http.Error(w, "unsupported bucket-level request", http.StatusBadRequest)
		return
	}
	key := parts[1]

	switch r.Method {
	case http.MethodPut:
		s.handlePut(w, key, body, r)
	case http.MethodGet:
		s.handleGet(w, key)
	case http.MethodDelete:
		s.handleDelete(w, key)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// verifySignature recomputes the expected SigV4 signature for the received
// request (using the test creds) and compares it against the Signature
// component of the Authorization header actually presented. On any
// mismatch or malformed header it writes a 403 and returns false.
func (s *Server) verifySignature(w http.ResponseWriter, r *http.Request, body []byte) bool {
	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		http.Error(w, "missing x-amz-content-sha256", http.StatusBadRequest)
		return false
	}
	if payloadHash != sha256Hex(body) {
		http.Error(w, "x-amz-content-sha256 does not match body", http.StatusBadRequest)
		return false
	}

	expected, err := s3.ExpectedSignature(r, s.creds, payloadHash)
	if err != nil {
		http.Error(w, "computing expected signature: "+err.Error(), http.StatusForbidden)
		return false
	}
	got := extractSignature(r.Header.Get("Authorization"))
	if got == "" || got != expected {
		http.Error(w, "signature mismatch", http.StatusForbidden)
		return false
	}
	return true
}

// extractSignature pulls the Signature=... value out of a SigV4
// Authorization header.
func extractSignature(auth string) string {
	const marker = "Signature="
	i := strings.Index(auth, marker)
	if i < 0 {
		return ""
	}
	return auth[i+len(marker):]
}

func (s *Server) handlePut(w http.ResponseWriter, key string, body []byte, r *http.Request) {
	ifNoneMatch := r.Header.Get("If-None-Match") == "*"

	s.mu.Lock()
	_, exists := s.objects[key]
	if ifNoneMatch && exists {
		s.mu.Unlock()
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	}
	stored := make([]byte, len(body))
	copy(stored, body)
	s.objects[key] = stored
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleGet(w http.ResponseWriter, key string) {
	s.mu.Lock()
	data, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (s *Server) handleDelete(w http.ResponseWriter, key string) {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

type listBucketResult struct {
	XMLName               xml.Name    `xml:"ListBucketResult"`
	Contents              []listEntry `xml:"Contents"`
	IsTruncated           bool        `xml:"IsTruncated"`
	NextContinuationToken string      `xml:"NextContinuationToken,omitempty"`
}

type listEntry struct {
	Key string `xml:"Key"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	token := r.URL.Query().Get("continuation-token")

	s.mu.Lock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	pageSize := s.pageSize
	s.mu.Unlock()

	start := 0
	if token != "" {
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 || n > len(keys) {
			http.Error(w, "bad continuation token", http.StatusBadRequest)
			return
		}
		start = n
	}
	end := start + pageSize
	truncated := end < len(keys)
	if end > len(keys) {
		end = len(keys)
	}

	result := listBucketResult{IsTruncated: truncated}
	for _, k := range keys[start:end] {
		result.Contents = append(result.Contents, listEntry{Key: k})
	}
	if truncated {
		result.NextContinuationToken = strconv.Itoa(end)
	}

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	enc := xml.NewEncoder(w)
	enc.Encode(result)
}
