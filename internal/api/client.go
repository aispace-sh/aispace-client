// Package api is a small client for the aispace bot API (/v1).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Per-phase timeouts for one request. There is deliberately no deadline on the
// exchange as a whole: a large file over a slow link is slow, not broken.
//
// A single cap covering the body meant transfer time was really a bandwidth
// floor. At the previous 10 minutes, a 100 MB upload — the documented maximum
// on Pro — had to sustain roughly 1.4 Mbit/s or fail after transferring most of
// itself. These bound the phases before any bytes flow instead, so an
// unreachable or silent server is still given up on quickly.
const (
	// DialTimeout bounds establishing the TCP connection.
	DialTimeout = 30 * time.Second
	// TLSTimeout bounds the TLS handshake.
	TLSTimeout = 10 * time.Second
	// HeaderTimeout bounds the wait for response headers, which is what catches
	// a server that accepts the connection and then goes quiet.
	HeaderTimeout = 60 * time.Second
	// TransferInactivityTimeout cancels an upload or download when no body bytes
	// move for this long. It resets whenever progress is made.
	TransferInactivityTimeout = 2 * time.Minute
)

// NewHTTPClient returns the HTTP client used for aispace requests: quick to
// give up before bytes start moving, patient once they are.
func NewHTTPClient() *http.Client {
	return newHTTPClient(DialTimeout, TLSTimeout, HeaderTimeout)
}

func newHTTPClient(dial, tlsHandshake, header time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dial, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   tlsHandshake,
			ResponseHeaderTimeout: header,
			ExpectContinueTimeout: time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   2,
		},
	}
}

// MaxRetryAfter caps how long the client waits before retrying a 429.
const MaxRetryAfter = 30 * time.Second

// DefaultRetryAfter is used when a 429 carries no usable Retry-After header.
const DefaultRetryAfter = time.Second

// Client talks to one aispace server with one bot key.
type Client struct {
	BaseURL   string
	Key       string
	UserAgent string
	HTTP      *http.Client
	// InactivityTimeout bounds time without byte progress during transfers. Zero disables it.
	InactivityTimeout time.Duration
	// Sleep overrides retry waiting for tests. Nil uses a context-aware timer.
	Sleep func(time.Duration)
}

// New returns a Client with sane defaults. baseURL must include the scheme.
func New(baseURL, key, userAgent string) *Client {
	return &Client{
		BaseURL:           strings.TrimRight(baseURL, "/"),
		Key:               key,
		UserAgent:         userAgent,
		HTTP:              NewHTTPClient(),
		InactivityTimeout: TransferInactivityTimeout,
	}
}

// UploadOptions describe a raw-body upload.
type UploadOptions struct {
	Name        string
	ContentType string
	// Encryption names the client-side encryption format. Empty means plaintext.
	Encryption string
	// Size is the exact number of bytes in Body; sent as Content-Length.
	Size int64
	// ExpiresIn seconds; zero means "server default".
	ExpiresIn int64
	// SHA256 hex; empty means "do not send".
	SHA256 string
	// Visibility overrides the account default when set to "private" or "account".
	Visibility string
	// IdempotencyKey identifies one logical upload. Empty preserves the legacy CLI behavior.
	IdempotencyKey string
}

// Upload streams body to POST /v1/files.
func (c *Client) Upload(ctx context.Context, body io.Reader, opts UploadOptions) (Result[File], error) {
	transferCtx, watch := startTransferWatch(ctx, c.InactivityTimeout)
	defer watch.stop()
	if watch != nil {
		body = &progressReader{r: body, watch: watch}
	}
	req, err := c.newRequest(transferCtx, http.MethodPost, "/v1/files", body)
	if err != nil {
		return Result[File]{}, err
	}
	req.ContentLength = opts.Size
	if opts.Size == 0 {
		// Force an explicit "Content-Length: 0" instead of a chunked/absent header.
		req.Body = http.NoBody
	}
	ct := opts.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	req.Header.Set("Content-Type", ct)
	if opts.Name != "" {
		req.Header.Set("X-File-Name", opts.Name)
	}
	if opts.ExpiresIn > 0 {
		req.Header.Set("X-Expires-In", strconv.FormatInt(opts.ExpiresIn, 10))
	}
	if opts.SHA256 != "" {
		req.Header.Set("X-SHA256", opts.SHA256)
	}
	if opts.Encryption != "" {
		req.Header.Set("X-Aispace-Encryption", opts.Encryption)
	}
	if opts.Visibility != "" {
		req.Header.Set("X-File-Visibility", opts.Visibility)
	}
	setIdempotencyKey(req, opts.IdempotencyKey)
	res, err := doRequest[File](c, req, http.StatusCreated, watch)
	if errors.Is(context.Cause(transferCtx), ErrTransferStalled) {
		return Result[File]{}, stalledError()
	}
	return res, err
}

// Download opens an authenticated stream from GET /v1/files/:id/content.
// The caller must close the response body.
func (c *Client) Download(ctx context.Context, id string) (*http.Response, error) {
	return c.DownloadWithIdempotency(ctx, id, "")
}

// DownloadWithIdempotency opens an authenticated stream and identifies retries
// of the same logical download to the service.
func (c *Client) DownloadWithIdempotency(ctx context.Context, id, idempotencyKey string) (*http.Response, error) {
	return c.DownloadRange(ctx, id, idempotencyKey, "")
}

// DownloadRange is DownloadWithIdempotency starting at a byte offset. A
// non-empty byteRange must be an HTTP Range value such as "bytes=42-".
//
// A server that honours the range answers 206 and the caller appends. A server
// that ignores it answers 200 with the whole file, which is not an error but
// means the caller must start the file again rather than append. Both are
// returned so the caller can tell them apart from resp.StatusCode; 416 arrives
// as an *Error carrying that status, which means the offset is past the end.
func (c *Client) DownloadRange(ctx context.Context, id, idempotencyKey, byteRange string) (*http.Response, error) {
	transferCtx, watch := startTransferWatch(ctx, c.InactivityTimeout)
	req, err := c.newRequest(transferCtx, http.MethodGet, "/v1/files/"+url.PathEscape(id)+"/content", nil)
	if err != nil {
		watch.stop()
		return nil, err
	}
	setIdempotencyKey(req, idempotencyKey)
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	for attempt := 0; ; attempt++ {
		resp, err := httpc.Do(req)
		if err != nil {
			watch.stop()
			if errors.Is(context.Cause(transferCtx), ErrTransferStalled) {
				return nil, stalledError()
			}
			return nil, networkError(err)
		}
		if resp.StatusCode == http.StatusOK ||
			(byteRange != "" && resp.StatusCode == http.StatusPartialContent) {
			if watch != nil {
				watch.touch()
				resp.Body = &watchedBody{ReadCloser: resp.Body, ctx: transferCtx, watch: watch}
			}
			return resp, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			watch.stop()
			if errors.Is(context.Cause(transferCtx), ErrTransferStalled) {
				return nil, stalledError()
			}
			return nil, &Error{Code: "network", Message: "reading response: " + readErr.Error(), cause: readErr}
		}
		// Authenticated downloads consume monthly allowance. A gateway error may
		// arrive after the server recorded that download, so replaying it could
		// charge the account twice. A rate-limit rejection happens before the
		// download handler and remains safe to retry.
		if attempt == 0 && retryableRateLimit(resp, body) {
			if err := waitForRetry(req.Context(), retryDelay(resp.Header.Get("Retry-After")), c.Sleep); err != nil {
				watch.stop()
				return nil, err
			}
			req = req.Clone(req.Context())
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			watch.stop()
			return nil, unexpectedStatus(resp.StatusCode, http.StatusOK)
		}
		watch.stop()
		return nil, errorFromResponse(resp, body)
	}
}

const maxListFilesLimit = 100

// ListFiles fetches one page of GET /v1/files.
func (c *Client) ListFiles(ctx context.Context, cursor string, limit int) (Result[FileList], error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/files"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return Result[FileList]{}, err
	}
	return do[FileList](c, req, http.StatusOK)
}

// ListPage fetches at most limit files starting at cursor, following
// next_cursor only as far as it needs to. It returns the raw entries, the
// decoded values, and the cursor to resume from, which is empty once the
// listing is exhausted.
//
// Each request asks for the smaller of the number still wanted and the API's
// per-request maximum, so the cursor stays aligned with what was consumed.
func (c *Client) ListPage(ctx context.Context, cursor string, limit int) ([]json.RawMessage, []File, string, error) {
	if limit <= 0 {
		return nil, nil, "", &Error{Code: "bad_request", Message: "limit must be positive"}
	}
	var raws []json.RawMessage
	var files []File
	seen := map[string]struct{}{}
	for len(raws) < limit {
		requestLimit := min(limit-len(raws), maxListFilesLimit)
		res, err := c.ListFiles(ctx, cursor, requestLimit)
		if err != nil {
			return nil, nil, "", err
		}
		page := res.Value.Files
		if len(page) > requestLimit {
			return nil, nil, "", &Error{Code: "bad_response", Message: fmt.Sprintf("server returned %d files after a request for at most %d", len(page), requestLimit)}
		}
		for _, raw := range page {
			var f File
			if err := json.Unmarshal(raw, &f); err != nil {
				return nil, nil, "", &Error{Code: "bad_response", Message: "cannot decode file entry: " + err.Error(), cause: err}
			}
			raws = append(raws, raw)
			files = append(files, f)
		}
		next := res.Value.NextCursor
		if next == nil || *next == "" {
			return raws, files, "", nil
		}
		if _, ok := seen[*next]; ok {
			return nil, nil, "", &Error{Code: "bad_response", Message: "pagination loop: repeated cursor"}
		}
		seen[*next] = struct{}{}
		cursor = *next
		if len(page) == 0 {
			// A server handing out cursors without files would spin forever.
			return raws, files, cursor, nil
		}
	}
	return raws, files, cursor, nil
}

// WalkFiles follows next_cursor until exhausted and visits each decoded page.
func (c *Client) WalkFiles(ctx context.Context, visit func([]json.RawMessage, []File) error) error {
	return c.WalkFilesFrom(ctx, "", visit)
}

// WalkFilesFrom is WalkFiles starting at an existing cursor.
func (c *Client) WalkFilesFrom(ctx context.Context, cursor string, visit func([]json.RawMessage, []File) error) error {
	seen := map[string]struct{}{}
	for {
		res, err := c.ListFiles(ctx, cursor, 0)
		if err != nil {
			return err
		}
		files := make([]File, len(res.Value.Files))
		for i, r := range res.Value.Files {
			if err := json.Unmarshal(r, &files[i]); err != nil {
				return &Error{Code: "bad_response", Message: "cannot decode file entry: " + err.Error(), cause: err}
			}
		}
		if err := visit(res.Value.Files, files); err != nil {
			return err
		}
		next := res.Value.NextCursor
		if next == nil || *next == "" {
			return nil
		}
		if _, ok := seen[*next]; ok {
			return &Error{Code: "bad_response", Message: "pagination loop: repeated cursor"}
		}
		seen[*next] = struct{}{}
		cursor = *next
	}
}

// ListAllFiles follows next_cursor until exhausted and returns every file's
// raw JSON plus the decoded values.
func (c *Client) ListAllFiles(ctx context.Context) ([]json.RawMessage, []File, error) {
	var raws []json.RawMessage
	var files []File
	err := c.WalkFiles(ctx, func(pageRaws []json.RawMessage, pageFiles []File) error {
		raws = append(raws, pageRaws...)
		files = append(files, pageFiles...)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return raws, files, nil
}

// GetFile fetches GET /v1/files/:id.
func (c *Client) GetFile(ctx context.Context, id string) (Result[File], error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/files/"+url.PathEscape(id), nil)
	if err != nil {
		return Result[File]{}, err
	}
	return do[File](c, req, http.StatusOK)
}

// DeleteFile calls DELETE /v1/files/:id.
func (c *Client) DeleteFile(ctx context.Context, id string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/files/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	_, err = do[struct{}](c, req, http.StatusNoContent)
	return err
}

// LinkOptions describe a share link creation.
type LinkOptions struct {
	// ExpiresIn seconds; zero means "server default".
	ExpiresIn int64
	// MaxDownloads; zero means unlimited / not sent.
	MaxDownloads int64
}

// CreateLink calls POST /v1/files/:id/links.
func (c *Client) CreateLink(ctx context.Context, fileID string, opts LinkOptions) (Result[ShareLink], error) {
	payload := map[string]int64{}
	if opts.ExpiresIn > 0 {
		payload["expires_in"] = opts.ExpiresIn
	}
	if opts.MaxDownloads > 0 {
		payload["max_downloads"] = opts.MaxDownloads
	}
	buf, _ := json.Marshal(payload)
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/files/"+url.PathEscape(fileID)+"/links", bytes.NewReader(buf))
	if err != nil {
		return Result[ShareLink]{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	return do[ShareLink](c, req, http.StatusCreated)
}

// ListLinks calls GET /v1/files/:id/links.
func (c *Client) ListLinks(ctx context.Context, fileID string) (Result[LinkList], error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/files/"+url.PathEscape(fileID)+"/links", nil)
	if err != nil {
		return Result[LinkList]{}, err
	}
	return do[LinkList](c, req, http.StatusOK)
}

// RevokeLink calls DELETE /v1/links/:id.
func (c *Client) RevokeLink(ctx context.Context, id string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, "/v1/links/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	_, err = do[struct{}](c, req, http.StatusNoContent)
	return err
}

// Quota calls GET /v1/quota.
func (c *Client) Quota(ctx context.Context) (Result[Quota], error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/quota", nil)
	if err != nil {
		return Result[Quota]{}, err
	}
	return do[Quota](c, req, http.StatusOK)
}

// Whoami calls GET /v1/whoami.
func (c *Client) Whoami(ctx context.Context) (Result[Whoami], error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/whoami", nil)
	if err != nil {
		return Result[Whoami]{}, err
	}
	return do[Whoami](c, req, http.StatusOK)
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if c.BaseURL == "" {
		return nil, &Error{Code: "config", Message: "no base URL configured"}
	}
	if c.Key == "" {
		return nil, &Error{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: "no API key: run `aispace login --key ask_...` or set AISPACE_KEY"}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, &Error{Code: "bad_request", Message: err.Error(), cause: err}
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	return req, nil
}

// do executes the request, retrying GET requests once on temporary rate limits.
func do[T any](c *Client, req *http.Request, wantStatus int) (Result[T], error) {
	return doRequest[T](c, req, wantStatus, nil)
}

func doRequest[T any](c *Client, req *http.Request, wantStatus int, watch *transferWatch) (Result[T], error) {
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, body, err := send(httpc, req, watch)
	if err != nil {
		return Result[T]{}, err
	}
	if req.Method == http.MethodGet && retryableOnce(resp, body) {
		wait := retryDelay(resp.Header.Get("Retry-After"))
		if err := waitForRetry(req.Context(), wait, c.Sleep); err != nil {
			return Result[T]{}, err
		}
		retry := req.Clone(req.Context())
		resp, body, err = send(httpc, retry, watch)
		if err != nil {
			return Result[T]{}, err
		}
	}
	if resp.StatusCode != wantStatus {
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return Result[T]{}, unexpectedStatus(resp.StatusCode, wantStatus)
		}
		return Result[T]{}, errorFromResponse(resp, body)
	}
	var out Result[T]
	if wantStatus == http.StatusNoContent {
		return out, nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return Result[T]{}, &Error{Status: resp.StatusCode, Code: "bad_response", Message: "server returned an empty response body"}
	}
	out.Raw = json.RawMessage(bytes.TrimSpace(body))
	if err := json.Unmarshal(body, &out.Value); err != nil {
		return Result[T]{}, &Error{Status: resp.StatusCode, Code: "bad_response", Message: fmt.Sprintf("cannot decode response: %v", err), cause: err}
	}
	return out, nil
}

func unexpectedStatus(got, want int) *Error {
	return &Error{
		Status:  got,
		Code:    "bad_response",
		Message: fmt.Sprintf("server returned HTTP %d, expected HTTP %d", got, want),
	}
}

func waitForRetry(ctx context.Context, delay time.Duration, sleep func(time.Duration)) error {
	if sleep == nil {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	done := make(chan struct{}, 1)
	go func() {
		sleep(delay)
		done <- struct{}{}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func send(httpc *http.Client, req *http.Request, watch *transferWatch) (*http.Response, []byte, error) {
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, nil, networkError(err)
	}
	if watch != nil {
		watch.touch()
		resp.Body = &watchedBody{ReadCloser: resp.Body, ctx: req.Context(), watch: watch}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, &Error{Code: "network", Message: "reading response: " + err.Error(), cause: err}
	}
	return resp, body, nil
}

// retryableOnce reports whether a failed idempotent request is worth exactly
// one more attempt.
//
// A rate limit is retried only when the server called it one: a monthly cap
// also arrives as 429, but its Retry-After points at the start of the next UTC
// month, so waiting is pointless.
//
// 502, 503 and 504 are the availability answers a gateway gives while it is
// between healthy backends, and a GET that failed that way has not changed
// anything, so repeating it is safe. 500 is deliberately excluded: it usually
// means the request itself is the problem, and retrying only doubles the load
// while returning the same error.
func retryableOnce(resp *http.Response, body []byte) bool {
	if retryableRateLimit(resp, body) {
		return true
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryableRateLimit(resp *http.Response, body []byte) bool {
	return resp.StatusCode == http.StatusTooManyRequests && errorFromResponse(resp, body).Code == "rate_limited"
}

func networkError(err error) *Error {
	var uerr *url.Error
	msg := err.Error()
	if errors.As(err, &uerr) {
		msg = fmt.Sprintf("%s %s: %v", uerr.Op, redact(uerr.URL), uerr.Err)
	}
	return &Error{Code: "network", Message: msg, cause: err}
}

// redact strips any query string from a URL before it lands in an error message.
func redact(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

func retryDelay(header string) time.Duration {
	secs := parseRetryAfter(header)
	if secs < 0 {
		return DefaultRetryAfter
	}
	d := time.Duration(secs) * time.Second
	if d > MaxRetryAfter {
		d = MaxRetryAfter
	}
	return d
}

// parseRetryAfter returns seconds (>= 0) or -1 when the header is absent/unparseable.
// It accepts delta-seconds and HTTP-date forms.
func parseRetryAfter(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return -1
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 0 {
			return 0
		}
		return n
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		return int(d.Round(time.Second) / time.Second)
	}
	return -1
}
