package sink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Client is the HTTP client of the HTTP-based sinks. Sinks pass the call
// timeout through the context, so the client has none of its own. It does
// not follow redirects: a redirect would turn the POST into a GET without the
// body, and could send the auth header to another host. The 3xx answer is
// returned as it is.
var Client = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// HMACHex returns the hex HMAC-SHA256 of data.
func HMACHex(secret, data []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

// ValidateURL checks that raw is an absolute http or https URL.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q is not an absolute http or https URL", raw)
	}
	return nil
}

// Post sends body as JSON and returns the status code and the first 1 MiB of
// the answer.
func Post(ctx context.Context, rawURL string, body []byte, header http.Header) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "git-event-hub")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, data, err
}
