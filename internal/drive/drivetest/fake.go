// Package drivetest is an in-process fake of the small slice of the Google
// Drive v3 REST API that porter-backup uses: metadata-only create (folders),
// multipart upload (small media), RESUMABLE upload (initiate + chunked PUTs
// with Content-Range), list-by-query, alt=media download, and delete. It is
// honest about resumable-upload semantics — the google-api-go-client talks to
// it unmodified (it answers non-final chunks with the X-Http-Status-Code-
// Override: 308 convention the client's X-GUploader-No-308 handshake asks
// for) — and it can inject 403/429 failures to exercise backoff paths.
package drivetest

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

// FolderMimeType is Drive's folder mime type.
const FolderMimeType = "application/vnd.google-apps.folder"

// File is a stored fake-Drive file.
type File struct {
	ID       string
	Name     string
	Parents  []string
	MimeType string
	Content  []byte
}

type session struct {
	meta File
	buf  []byte
	done bool
}

// Server is the fake Drive server.
type Server struct {
	mu       sync.Mutex
	srv      *httptest.Server
	files    map[string]*File
	sessions map[string]*session
	nextID   int

	// failInits / failChunks: how many upcoming upload-initiate / chunk-PUT
	// requests fail with failCode before succeeding.
	failInits  int
	failChunks int
	failCode   int

	// Counters for test assertions.
	InitCount  int
	ChunkCount int
}

// New starts a fake Drive server.
func New() *Server {
	s := &Server{
		files:    map[string]*File{},
		sessions: map[string]*session{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/upload/drive/v3/files", s.handleUpload)
	mux.HandleFunc("/upload/porter-session/", s.handleChunk)
	mux.HandleFunc("/drive/v3/files", s.handleFilesCollection)
	mux.HandleFunc("/drive/v3/files/", s.handleFile)
	// google-api-go-client resolves paths relative to the endpoint root.
	mux.HandleFunc("/files", s.handleFilesCollection)
	mux.HandleFunc("/files/", s.handleFile)
	s.srv = httptest.NewServer(mux)
	return s
}

// URL is the server base URL (use as the API endpoint).
func (s *Server) URL() string { return s.srv.URL }

// Close shuts the server down.
func (s *Server) Close() { s.srv.Close() }

// FailNextUploadInits makes the next n upload-initiate requests (resumable
// initiate or multipart POST) fail with the HTTP code.
func (s *Server) FailNextUploadInits(n, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failInits, s.failCode = n, code
}

// FailNextChunks makes the next n resumable chunk PUTs fail with the code.
func (s *Server) FailNextChunks(n, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failChunks, s.failCode = n, code
}

// Files returns a snapshot of stored files.
func (s *Server) Files() []File {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]File, 0, len(s.files))
	for _, f := range s.files {
		out = append(out, *f)
	}
	return out
}

// FileByName finds a file by parent + name.
func (s *Server) FileByName(parentID, name string) (File, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		if f.Name == name && hasParent(f, parentID) {
			return *f, true
		}
	}
	return File{}, false
}

// AddFile injects a file directly (test arrangement, e.g. pre-existing old
// snapshots for retention tests). Returns the new id.
func (s *Server) AddFile(parentID, name string, content []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := &File{ID: s.newID(), Name: name, Parents: []string{parentID}, Content: content}
	s.files[f.ID] = f
	return f.ID
}

// Corrupt flips one bit near the end of a stored file's content (tamper
// simulation).
func (s *Server) Corrupt(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[id]
	if !ok || len(f.Content) == 0 {
		return false
	}
	f.Content[len(f.Content)-1] ^= 0x01
	return true
}

func hasParent(f *File, parent string) bool {
	for _, p := range f.Parents {
		if p == parent {
			return true
		}
	}
	return false
}

func (s *Server) newID() string {
	s.nextID++
	return fmt.Sprintf("fake-%04d", s.nextID)
}

// apiError writes a googleapi-shaped error body.
func apiError(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"errors":[{"reason":%q,"message":%q}]}}`,
		code, msg, reason, msg)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fileJSON(f *File) map[string]any {
	return map[string]any{
		"id":       f.ID,
		"name":     f.Name,
		"mimeType": f.MimeType,
		"parents":  f.Parents,
		"size":     strconv.Itoa(len(f.Content)), // Drive serializes size as string
	}
}

type fileMeta struct {
	Name     string   `json:"name"`
	MimeType string   `json:"mimeType"`
	Parents  []string `json:"parents"`
}

// handleUpload: POST /upload/drive/v3/files?uploadType=resumable|multipart.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	s.InitCount++
	if s.failInits > 0 {
		s.failInits--
		code := s.failCode
		s.mu.Unlock()
		apiError(w, code, "rateLimitExceeded", "injected failure")
		return
	}
	s.mu.Unlock()

	switch r.URL.Query().Get("uploadType") {
	case "resumable":
		var meta fileMeta
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			apiError(w, 400, "badRequest", "bad metadata: "+err.Error())
			return
		}
		s.mu.Lock()
		id := s.newID()
		s.sessions[id] = &session{meta: File{
			ID: id, Name: meta.Name, Parents: meta.Parents, MimeType: meta.MimeType,
		}}
		s.mu.Unlock()
		w.Header().Set("Location", s.srv.URL+"/upload/porter-session/"+id)
		w.WriteHeader(http.StatusOK)
	case "multipart":
		mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mt, "multipart/") {
			apiError(w, 400, "badRequest", "want multipart/related")
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		metaPart, err := mr.NextPart()
		if err != nil {
			apiError(w, 400, "badRequest", "missing metadata part")
			return
		}
		var meta fileMeta
		if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
			apiError(w, 400, "badRequest", "bad metadata: "+err.Error())
			return
		}
		mediaPart, err := mr.NextPart()
		if err != nil {
			apiError(w, 400, "badRequest", "missing media part")
			return
		}
		content, err := io.ReadAll(mediaPart)
		if err != nil {
			apiError(w, 400, "badRequest", "reading media: "+err.Error())
			return
		}
		s.mu.Lock()
		f := &File{ID: s.newID(), Name: meta.Name, Parents: meta.Parents, MimeType: meta.MimeType, Content: content}
		s.files[f.ID] = f
		s.mu.Unlock()
		writeJSON(w, fileJSON(f))
	default:
		apiError(w, 400, "badRequest", "unsupported uploadType")
	}
}

// handleChunk: PUT /upload/porter-session/{id} with Content-Range.
func (s *Server) handleChunk(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/upload/porter-session/")

	s.mu.Lock()
	s.ChunkCount++
	if s.failChunks > 0 {
		s.failChunks--
		code := s.failCode
		s.mu.Unlock()
		apiError(w, code, "rateLimitExceeded", "injected chunk failure")
		return
	}
	sess, ok := s.sessions[id]
	s.mu.Unlock()
	if !ok {
		apiError(w, 404, "notFound", "no such upload session")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		apiError(w, 400, "badRequest", "reading chunk: "+err.Error())
		return
	}
	start, total, err := parseContentRange(r.Header.Get("Content-Range"))
	if err != nil {
		apiError(w, 400, "badRequest", err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if int(start) != len(sess.buf) {
		apiError(w, 400, "badRequest", fmt.Sprintf(
			"chunk start %d does not continue session at offset %d", start, len(sess.buf)))
		return
	}
	sess.buf = append(sess.buf, body...)
	if total >= 0 && int64(len(sess.buf)) == total {
		// Final chunk: persist the file.
		f := sess.meta
		f.Content = sess.buf
		s.files[f.ID] = &f
		sess.done = true
		delete(s.sessions, id)
		writeJSON(w, fileJSON(&f))
		return
	}
	// Resume incomplete — the client sent X-GUploader-No-308, so signal via
	// the override header on a 200 (matching real GUploader behavior).
	if len(sess.buf) > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", len(sess.buf)-1))
	}
	w.Header().Set("X-Http-Status-Code-Override", "308")
	w.WriteHeader(http.StatusOK)
}

// parseContentRange parses "bytes start-end/total" with "*" allowed for
// total (unknown) and for start-end (no body). Returns total = -1 if unknown.
func parseContentRange(h string) (start, total int64, err error) {
	if h == "" {
		return 0, -1, fmt.Errorf("missing Content-Range")
	}
	rest, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return 0, -1, fmt.Errorf("bad Content-Range %q", h)
	}
	rangePart, totalPart, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, -1, fmt.Errorf("bad Content-Range %q", h)
	}
	total = -1
	if totalPart != "*" {
		if total, err = strconv.ParseInt(totalPart, 10, 64); err != nil {
			return 0, -1, fmt.Errorf("bad Content-Range total %q", h)
		}
	}
	if rangePart == "*" {
		return 0, total, nil
	}
	startStr, _, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, -1, fmt.Errorf("bad Content-Range range %q", h)
	}
	if start, err = strconv.ParseInt(startStr, 10, 64); err != nil {
		return 0, -1, fmt.Errorf("bad Content-Range start %q", h)
	}
	return start, total, nil
}

// handleFilesCollection: GET (list w/ query) and POST (metadata-only create).
func (s *Server) handleFilesCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query().Get("q")
		cond, err := parseQuery(q)
		if err != nil {
			apiError(w, 400, "badRequest", err.Error())
			return
		}
		s.mu.Lock()
		var out []map[string]any
		for _, f := range s.files {
			if cond.matches(f) {
				out = append(out, fileJSON(f))
			}
		}
		s.mu.Unlock()
		if out == nil {
			out = []map[string]any{}
		}
		writeJSON(w, map[string]any{"files": out})
	case http.MethodPost:
		var meta fileMeta
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			apiError(w, 400, "badRequest", "bad metadata: "+err.Error())
			return
		}
		s.mu.Lock()
		f := &File{ID: s.newID(), Name: meta.Name, Parents: meta.Parents, MimeType: meta.MimeType}
		s.files[f.ID] = f
		s.mu.Unlock()
		writeJSON(w, fileJSON(f))
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// handleFile: GET /drive/v3/files/{id} (alt=media → content) and DELETE.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path
	id = id[strings.LastIndex(id, "/")+1:]
	s.mu.Lock()
	f, ok := s.files[id]
	s.mu.Unlock()
	if !ok {
		apiError(w, 404, "notFound", "no such file: "+id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("alt") == "media" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(f.Content)
			return
		}
		writeJSON(w, fileJSON(f))
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.files, id)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

// query is the tiny subset of Drive's query language the porter client uses:
// AND-ed clauses of `name = 'X'`, `'P' in parents`, `mimeType = 'M'`,
// `trashed = false`.
type query struct {
	name, parent, mimeType string
}

func (q query) matches(f *File) bool {
	if q.name != "" && f.Name != q.name {
		return false
	}
	if q.parent != "" && !hasParent(f, q.parent) {
		return false
	}
	if q.mimeType != "" && f.MimeType != q.mimeType {
		return false
	}
	return true
}

func parseQuery(s string) (query, error) {
	var q query
	if strings.TrimSpace(s) == "" {
		return q, nil
	}
	for _, clause := range strings.Split(s, " and ") {
		clause = strings.TrimSpace(clause)
		switch {
		case strings.HasPrefix(clause, "name = '"):
			q.name = strings.TrimSuffix(strings.TrimPrefix(clause, "name = '"), "'")
		case strings.HasPrefix(clause, "mimeType = '"):
			q.mimeType = strings.TrimSuffix(strings.TrimPrefix(clause, "mimeType = '"), "'")
		case strings.HasSuffix(clause, "' in parents") && strings.HasPrefix(clause, "'"):
			q.parent = strings.TrimSuffix(strings.TrimPrefix(clause, "'"), "' in parents")
		case clause == "trashed = false":
			// fake never stores trashed files
		default:
			return q, fmt.Errorf("fake drive: unsupported query clause %q (in %q)", clause, s)
		}
	}
	return q, nil
}
