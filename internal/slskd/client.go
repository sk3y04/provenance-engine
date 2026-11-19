package slskd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes bounds a single response body. A transfers list with
// includeRemoved can be large, but an unbounded read would let a hostile or
// broken slskd exhaust memory.
const maxResponseBytes = 16 << 20

// Default per-operation timeouts, from the upstream contract: health and
// transfer reads are cheap, a directory listing is not, and a browse can take
// most of a minute on a slow peer.
const (
	DefaultHealthTimeout    = 5 * time.Second
	DefaultTransferTimeout  = 5 * time.Second
	DefaultDirectoryTimeout = 15 * time.Second
	DefaultBrowseTimeout    = 60 * time.Second
	DefaultEnqueueTimeout   = 60 * time.Second
	DefaultSearchTimeout    = 30 * time.Second
	DefaultMutationTimeout  = 15 * time.Second
)

// Config configures a Client. BaseURL and APIKey are required.
type Config struct {
	// BaseURL is the slskd origin, e.g. "http://127.0.0.1:5030". It must carry a
	// scheme; a bare host:port is rejected rather than guessed at.
	BaseURL string
	// URLBase is an optional path prefix such as "/slskd" for a reverse proxy.
	URLBase string
	// APIKey is sent as X-API-Key. slskd rejects Authorization: Bearer.
	APIKey string
	// WebhookSecret is only used for redaction here; the receiver lives in
	// webhook.go.
	WebhookSecret string
	// InsecureSkipVerify disables TLS verification. Strict by default.
	InsecureSkipVerify bool

	HealthTimeout    time.Duration
	TransferTimeout  time.Duration
	DirectoryTimeout time.Duration
	BrowseTimeout    time.Duration
	EnqueueTimeout   time.Duration
	SearchTimeout    time.Duration
	MutationTimeout  time.Duration
}

// Client is a typed slskd API client. It is safe for concurrent use.
type Client struct {
	origin *url.URL
	prefix string
	apiKey string
	secret string
	http   *http.Client
	cfg    Config
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		return nil, errors.New("slskd base URL is required")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("slskd API key is required")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("slskd base URL %q is not a URL: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("slskd base URL %q must start with http:// or https://", base)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("slskd base URL %q has no host", base)
	}
	prefix := ""
	if p := strings.TrimSpace(cfg.URLBase); p != "" {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		prefix = "/" + strings.Trim(p, "/")
		if prefix == "/" {
			prefix = ""
		}
	}
	origin := &url.URL{Scheme: u.Scheme, Host: u.Host}
	if u.Path != "" && u.Path != "/" {
		// Allow a base URL that already carries a prefix, but never double it.
		basePath := "/" + strings.Trim(u.Path, "/")
		if prefix == "" {
			prefix = basePath
		} else if !strings.HasPrefix(prefix, basePath) {
			prefix = basePath + prefix
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	return &Client{
		origin: origin,
		prefix: prefix,
		apiKey: cfg.APIKey,
		secret: cfg.WebhookSecret,
		cfg:    cfg,
		http: &http.Client{
			Transport: transport,
			// A per-call context deadline bounds each operation; no global
			// client timeout, which would also cut long browses.
		},
	}, nil
}

// Redact removes every configured secret from s.
func (c *Client) Redact(s string) string {
	return Redact([]string{c.apiKey, c.secret}, s)
}

// Redact replaces each non-empty secret in s with a fixed marker. Callers use it
// for logs, errors and diagnostics; a secret must never reach any of them.
func Redact(secrets []string, s string) string {
	for _, secret := range secrets {
		if len(secret) < 4 {
			continue
		}
		s = strings.ReplaceAll(s, secret, "****")
	}
	return s
}

// ErrNoClient is returned by operations that need a configured client.
var ErrNoClient = errors.New("no slskd client is configured")

// APIError is a non-2xx response.
type APIError struct {
	Op         string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("slskd %s: HTTP %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("slskd %s: HTTP %d: %s", e.Op, e.StatusCode, e.Message)
}

// IsAuthError reports whether err is an authentication or authorisation
// rejection.
func IsAuthError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden
	}
	return false
}

// IsTimeout reports whether err is a deadline or timeout, which the health probe
// classifies as unreachable rather than misconfigured.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusRequestTimeout {
		return true
	}
	return false
}

// IsStatus reports whether err is an APIError with the given status code.
func IsStatus(err error, code int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

func (c *Client) timeout(d time.Duration, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// do performs one request. path is joined after the configured prefix; segments
// are escaped by the caller where needed.
func (c *Client) do(ctx context.Context, op string, timeout time.Duration, method, path string, query url.Values, body, out any) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := c.origin.String() + c.prefix + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("slskd %s: encode request: %w", op, err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, fmt.Errorf("slskd %s: build request: %w", op, err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, &TransportError{Op: op, Err: err, msg: c.redactError(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, &TransportError{Op: op, Err: err, msg: c.redactError(err)}
	}
	if int64(len(data)) > maxResponseBytes {
		return 0, fmt.Errorf("slskd %s: response too large (over %d bytes)", op, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(data))
		msg = strings.Trim(msg, `"`)
		if len(msg) > 512 {
			msg = msg[:512] + "..."
		}
		return resp.StatusCode, &APIError{Op: op, StatusCode: resp.StatusCode, Message: c.Redact(msg)}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.StatusCode, fmt.Errorf("slskd %s: decode response: %w", op, err)
	}
	return resp.StatusCode, nil
}

// redactError renders a transport error without its URL: the URL carries no
// secret, but it does carry the operator's internal host names.
func (c *Client) redactError(err error) string {
	msg := err.Error()
	if u, ok := err.(*url.Error); ok {
		msg = u.Op + ": " + u.Err.Error()
	}
	return c.Redact(msg)
}

func (c *Client) get(ctx context.Context, op string, timeout time.Duration, path string, query url.Values, out any) error {
	_, err := c.do(ctx, op, timeout, http.MethodGet, path, query, nil, out)
	return err
}

// TransportError is a connection-level failure with a redacted message. The
// original error stays reachable through Unwrap so errors.Is and net.Error
// checks keep working.
type TransportError struct {
	Op  string
	Err error
	msg string
}

func (e *TransportError) Error() string { return "slskd " + e.Op + ": " + e.msg }

func (e *TransportError) Unwrap() error { return e.Err }

// ---------------------------------------------------------------------------
// Health, server, options
// ---------------------------------------------------------------------------

// Ping checks reachability with the cheapest endpoint available.
func (c *Client) Ping(ctx context.Context) error {
	var raw string
	return c.get(ctx, "ping", c.timeout(c.cfg.HealthTimeout, DefaultHealthTimeout), "/api/v0/application/version", nil, &raw)
}

// GetVersion returns the raw version string, e.g. "0.26.0.0".
func (c *Client) GetVersion(ctx context.Context) (string, error) {
	var raw string
	if err := c.get(ctx, "version", c.timeout(c.cfg.HealthTimeout, DefaultHealthTimeout), "/api/v0/application/version", nil, &raw); err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// GetApplication returns the application document, which includes the version
// block and the current server state.
func (c *Client) GetApplication(ctx context.Context) (*Application, error) {
	var app Application
	if err := c.get(ctx, "application", c.timeout(c.cfg.HealthTimeout, DefaultHealthTimeout), "/api/v0/application", nil, &app); err != nil {
		return nil, err
	}
	return &app, nil
}

// GetServer returns the current Soulseek client state.
func (c *Client) GetServer(ctx context.Context) (*ServerState, error) {
	var st ServerState
	if err := c.get(ctx, "server", c.timeout(c.cfg.HealthTimeout, DefaultHealthTimeout), "/api/v0/server", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// ConnectServer asks slskd to (re)connect to the Soulseek server. slskd may
// answer 204 or with the resulting state; both are accepted.
func (c *Client) ConnectServer(ctx context.Context) (*ServerState, error) {
	var st ServerState
	if _, err := c.do(ctx, "connect server", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodPut, "/api/v0/server", nil, nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// DisconnectServer asks slskd to disconnect from Soulseek.
func (c *Client) DisconnectServer(ctx context.Context) error {
	_, err := c.do(ctx, "disconnect server", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodDelete, "/api/v0/server", nil, nil, nil)
	return err
}

// GetOptions reads the running configuration. On 0.26.2 a readwrite key gets
// 403 here (#1905); callers must treat that as a diagnostic warning, never as a
// connection failure.
func (c *Client) GetOptions(ctx context.Context) (*Options, error) {
	var opts Options
	if err := c.get(ctx, "options", c.timeout(c.cfg.HealthTimeout, DefaultHealthTimeout), "/api/v0/options", nil, &opts); err != nil {
		return nil, err
	}
	return &opts, nil
}

// RaiseEvent asks slskd to raise a sample event, which is the webhook self-test.
func (c *Client) RaiseEvent(ctx context.Context, eventType string) (*Event, error) {
	ev := Event{}
	path := "/api/v0/events/" + url.PathEscape(eventType)
	if _, err := c.do(ctx, "raise event", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodPost, path, nil, nil, &ev); err != nil {
		return nil, err
	}
	return &ev, nil
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// CreateSearch starts a search. slskd allows exactly one concurrent search per
// instance and answers 429 otherwise; the coordinator in search.go serialises
// this call.
func (c *Client) CreateSearch(ctx context.Context, req SearchRequest) (*Search, error) {
	var s Search
	if _, err := c.do(ctx, "create search", c.timeout(c.cfg.SearchTimeout, DefaultSearchTimeout), http.MethodPost, "/api/v0/searches", nil, req, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSearch returns one search, optionally with its responses.
func (c *Client) GetSearch(ctx context.Context, id string, includeResponses bool) (*Search, error) {
	q := url.Values{}
	if includeResponses {
		q.Set("includeResponses", "true")
	}
	var s Search
	if err := c.get(ctx, "get search", c.timeout(c.cfg.SearchTimeout, DefaultSearchTimeout), "/api/v0/searches/"+url.PathEscape(id), q, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// StopSearch stops a running search (PUT /api/v0/searches/{id}).
func (c *Client) StopSearch(ctx context.Context, id string) error {
	_, err := c.do(ctx, "stop search", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodPut, "/api/v0/searches/"+url.PathEscape(id), nil, nil, nil)
	return err
}

// DeleteSearch removes the search and its responses. Callers wait out the
// ~5 s settle grace first; deleting immediately after a poll can leave slskd's
// search.db in a concurrency error that silently returns empty results (#1819).
func (c *Client) DeleteSearch(ctx context.Context, id string) error {
	_, err := c.do(ctx, "delete search", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodDelete, "/api/v0/searches/"+url.PathEscape(id), nil, nil, nil)
	return err
}

// ListSearches returns all in-memory searches without responses.
func (c *Client) ListSearches(ctx context.Context) ([]Search, error) {
	var out []Search
	if err := c.get(ctx, "list searches", c.timeout(c.cfg.SearchTimeout, DefaultSearchTimeout), "/api/v0/searches", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Downloads
// ---------------------------------------------------------------------------

// EnqueueBatch enqueues a batch of downloads. A 207 is a partial success: the
// returned response carries the failures and err is nil. A 200 means every file
// was refused and is returned as an APIError.
func (c *Client) EnqueueBatch(ctx context.Context, req EnqueueBatchRequest) (*EnqueueBatchResponse, error) {
	var resp EnqueueBatchResponse
	status, err := c.do(ctx, "enqueue batch", c.timeout(c.cfg.EnqueueTimeout, DefaultEnqueueTimeout), http.MethodPost, "/api/v0/transfers/downloads/batches", nil, req, &resp)
	if err != nil {
		return nil, err
	}
	// 200 means slskd accepted the request but refused every file; 207 is a
	// partial success and carries both a batch and failures. Only 200 is an
	// error, and it must still report which files failed.
	if status == http.StatusOK && resp.Batch == nil && len(resp.Failures) > 0 {
		return &resp, &APIError{Op: "enqueue batch", StatusCode: status, Message: firstFailure(resp.Failures)}
	}
	return &resp, nil
}

func firstFailure(failures []EnqueueBatchFailure) string {
	if len(failures) == 0 {
		return ""
	}
	f := failures[0]
	if f.Filename == "" {
		return f.Message
	}
	return f.Filename + ": " + f.Message
}

// ListDownloads returns every download transfer. includeRemoved keeps records
// that the user removed from the slskd UI, which a reconciler needs to see.
func (c *Client) ListDownloads(ctx context.Context, includeRemoved bool) ([]Transfer, error) {
	q := url.Values{}
	if includeRemoved {
		q.Set("includeRemoved", "true")
	}
	var out []Transfer
	if err := c.get(ctx, "list downloads", c.timeout(c.cfg.TransferTimeout, DefaultTransferTimeout), "/api/v0/transfers/downloads", q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetDownload returns one download transfer.
func (c *Client) GetDownload(ctx context.Context, username, id string) (*Transfer, error) {
	var t Transfer
	path := "/api/v0/transfers/downloads/" + url.PathEscape(username) + "/" + url.PathEscape(id)
	if err := c.get(ctx, "get download", c.timeout(c.cfg.TransferTimeout, DefaultTransferTimeout), path, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// CancelDownload cancels one download; remove additionally drops the record.
func (c *Client) CancelDownload(ctx context.Context, username, id string, remove bool) error {
	q := url.Values{}
	q.Set("remove", strconv.FormatBool(remove))
	path := "/api/v0/transfers/downloads/" + url.PathEscape(username) + "/" + url.PathEscape(id)
	_, err := c.do(ctx, "cancel download", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodDelete, path, q, nil, nil)
	return err
}

// QueuePosition asks the peer for this transfer's place in its queue. It is
// rate-limited per transfer by the caller.
func (c *Client) QueuePosition(ctx context.Context, username, id string) (*QueuePosition, error) {
	var t Transfer
	path := "/api/v0/transfers/downloads/" + url.PathEscape(username) + "/" + url.PathEscape(id) + "/position"
	if err := c.get(ctx, "queue position", c.timeout(c.cfg.TransferTimeout, DefaultTransferTimeout), path, nil, &t); err != nil {
		return nil, err
	}
	p := &QueuePosition{State: t.State, Username: username, Filename: t.Filename}
	if t.PlaceInQueue != nil {
		p.Position = *t.PlaceInQueue
		p.Known = true
	}
	return p, nil
}

// ClearCompletedDownloads removes every completed download record.
func (c *Client) ClearCompletedDownloads(ctx context.Context) error {
	_, err := c.do(ctx, "clear completed downloads", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodDelete, "/api/v0/transfers/downloads/all/completed", nil, nil, nil)
	return err
}

// ---------------------------------------------------------------------------
// Uploads
// ---------------------------------------------------------------------------

// ListUploads returns every upload transfer.
func (c *Client) ListUploads(ctx context.Context) ([]Transfer, error) {
	var out []Transfer
	if err := c.get(ctx, "list uploads", c.timeout(c.cfg.TransferTimeout, DefaultTransferTimeout), "/api/v0/transfers/uploads", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CancelUpload cancels one upload; remove additionally drops the record.
func (c *Client) CancelUpload(ctx context.Context, username, id string, remove bool) error {
	q := url.Values{}
	q.Set("remove", strconv.FormatBool(remove))
	path := "/api/v0/transfers/uploads/" + url.PathEscape(username) + "/" + url.PathEscape(id)
	_, err := c.do(ctx, "cancel upload", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodDelete, path, q, nil, nil)
	return err
}

// ClearCompletedUploads removes every completed upload record.
func (c *Client) ClearCompletedUploads(ctx context.Context) error {
	_, err := c.do(ctx, "clear completed uploads", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodDelete, "/api/v0/transfers/uploads/all/completed", nil, nil, nil)
	return err
}

// ---------------------------------------------------------------------------
// Shares and files
// ---------------------------------------------------------------------------

// ListShares returns configured shares. The map key is "local" for local
// shares, not the alias, so callers must iterate values rather than index by
// alias.
func (c *Client) ListShares(ctx context.Context) (map[string][]Share, error) {
	out := map[string][]Share{}
	if err := c.get(ctx, "list shares", c.timeout(c.cfg.DirectoryTimeout, DefaultDirectoryTimeout), "/api/v0/shares", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ShareContents returns every shared directory and file. On a large library
// this can exceed 100k files; callers paginate in-process rather than shipping
// the whole tree to a UI.
func (c *Client) ShareContents(ctx context.Context) ([]Directory, error) {
	var out []Directory
	if err := c.get(ctx, "share contents", c.timeout(c.cfg.BrowseTimeout, DefaultBrowseTimeout), "/api/v0/shares/contents", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ShareContentsByID returns the contents of one share.
func (c *Client) ShareContentsByID(ctx context.Context, id string) ([]Directory, error) {
	var out []Directory
	if err := c.get(ctx, "share contents by id", c.timeout(c.cfg.BrowseTimeout, DefaultBrowseTimeout), "/api/v0/shares/"+url.PathEscape(id)+"/contents", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RescanShares starts a share scan (PUT /api/v0/shares). A 409 means one is
// already running.
func (c *Client) RescanShares(ctx context.Context) error {
	_, err := c.do(ctx, "rescan shares", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodPut, "/api/v0/shares", nil, nil, nil)
	return err
}

// CancelShareScan cancels a running share scan (DELETE /api/v0/shares).
func (c *Client) CancelShareScan(ctx context.Context) error {
	_, err := c.do(ctx, "cancel share scan", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodDelete, "/api/v0/shares", nil, nil, nil)
	return err
}

// ListDownloadFiles lists slskd's downloads directory. slskd has no byte-stream
// download API; the shared volume mount is the only way to read completed files,
// so this listing is for diagnostics, not for ingestion.
func (c *Client) ListDownloadFiles(ctx context.Context, recursive bool) (*FilesystemDirectory, error) {
	q := url.Values{}
	q.Set("recursive", strconv.FormatBool(recursive))
	var out FilesystemDirectory
	if err := c.get(ctx, "list download files", c.timeout(c.cfg.DirectoryTimeout, DefaultDirectoryTimeout), "/api/v0/files/downloads/directories", q, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteDownloadedFile deletes a file under slskd's downloads directory. It
// requires remote_file_management: true in slskd and is strictly opt-in here:
// the ingestor never calls it before a verified import.
func (c *Client) DeleteDownloadedFile(ctx context.Context, relPath string) error {
	enc := base64.StdEncoding.EncodeToString([]byte(relPath))
	_, err := c.do(ctx, "delete downloaded file", c.timeout(c.cfg.MutationTimeout, DefaultMutationTimeout), http.MethodDelete, "/api/v0/files/downloads/files/"+url.PathEscape(enc), nil, nil, nil)
	return err
}

// ---------------------------------------------------------------------------
// Users (remote peer inspection)
// ---------------------------------------------------------------------------

// UserStatus returns a peer's presence and privilege.
func (c *Client) UserStatus(ctx context.Context, username string) (*UserStatus, error) {
	var st UserStatus
	if err := c.get(ctx, "user status", c.timeout(c.cfg.HealthTimeout, DefaultHealthTimeout), "/api/v0/users/"+url.PathEscape(username)+"/status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// UserInfo returns a peer's share summary.
func (c *Client) UserInfo(ctx context.Context, username string) (*UserInfo, error) {
	var info UserInfo
	if err := c.get(ctx, "user info", c.timeout(c.cfg.BrowseTimeout, DefaultBrowseTimeout), "/api/v0/users/"+url.PathEscape(username)+"/info", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// BrowseUser requests a peer's file list. BrowseTimeout must be generous: a
// slow peer can take most of a minute.
func (c *Client) BrowseUser(ctx context.Context, username string) ([]Directory, error) {
	var out []Directory
	if err := c.get(ctx, "browse user", c.timeout(c.cfg.BrowseTimeout, DefaultBrowseTimeout), "/api/v0/users/"+url.PathEscape(username)+"/browse", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// BrowseStatus returns the browse progress for a peer. slskd answers 404 when no
// browse is in progress (#910), which callers treat as idle, not as an error.
func (c *Client) BrowseStatus(ctx context.Context, username string) (bool, error) {
	var raw struct {
		IsComplete      bool    `json:"isComplete"`
		PercentComplete float64 `json:"percentComplete"`
	}
	err := c.get(ctx, "browse status", c.timeout(c.cfg.TransferTimeout, DefaultTransferTimeout), "/api/v0/users/"+url.PathEscape(username)+"/browse/status", nil, &raw)
	if IsStatus(err, http.StatusNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return raw.IsComplete, nil
}

// BrowseDirectory requests one directory from a peer.
func (c *Client) BrowseDirectory(ctx context.Context, username, dir string) ([]Directory, error) {
	var out []Directory
	body := map[string]string{"directory": dir}
	path := "/api/v0/users/" + url.PathEscape(username) + "/directory"
	if _, err := c.do(ctx, "browse directory", c.timeout(c.cfg.DirectoryTimeout, DefaultDirectoryTimeout), http.MethodPost, path, nil, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}
