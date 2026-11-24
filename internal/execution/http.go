package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

func Endpoint(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid provider endpoint")
	}
	return u, nil
}

func IsLoopback(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func PostJSON(ctx context.Context, client *http.Client, endpoint, auth string, payload, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return requestJSON(ctx, client, http.MethodPost, endpoint, auth, bytes.NewReader(data), result)
}

func GetJSON(ctx context.Context, client *http.Client, endpoint, auth string, result any) error {
	return requestJSON(ctx, client, http.MethodGet, endpoint, auth, nil, result)
}

type HTTPStatusError int

func (e HTTPStatusError) Error() string {
	return fmt.Sprintf("upstream HTTP %d", int(e))
}

func requestJSON(ctx context.Context, client *http.Client, method, endpoint, auth string, body io.Reader, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("invalid upstream request")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		var requestErr *url.Error
		if errors.As(err, &requestErr) {
			err = requestErr.Err
		}
		return fmt.Errorf("upstream transport failure: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return HTTPStatusError(res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(result); err != nil {
		return fmt.Errorf("invalid upstream response: %w", err)
	}
	return nil
}

// PostJSONBounded uses a strict envelope and keeps untrusted response text and
// transport details out of persisted diagnostics.
func PostJSONBounded(ctx context.Context, client *http.Client, endpoint, auth string, payload, result any) error {
	const limit = 1 << 20
	data, err := json.Marshal(payload)
	if err != nil || len(data) > limit {
		return fmt.Errorf("invalid or oversized adapter request")
	}
	return adapterJSON(ctx, client, http.MethodPost, endpoint, auth, bytes.NewReader(data), result)
}

func GetAdapterJSON(ctx context.Context, client *http.Client, endpoint, auth string, result any) error {
	return adapterJSON(ctx, client, http.MethodGet, endpoint, auth, nil, result)
}

func adapterJSON(ctx context.Context, client *http.Client, method, endpoint, auth string, body io.Reader, result any) error {
	const limit = 1 << 20
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("invalid adapter request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := client.Do(req)
	if err != nil {
		return AdapterTransportError{timeout: os.IsTimeout(err)}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return HTTPStatusError(res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || len(data) > limit {
		return fmt.Errorf("unreadable or oversized adapter response")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return fmt.Errorf("invalid adapter response")
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return fmt.Errorf("expected one adapter response object")
	}
	return nil
}

type AdapterTransportError struct{ timeout bool }

func (e AdapterTransportError) Error() string {
	if e.timeout {
		return "adapter request timed out"
	}
	return "adapter transport failed"
}
func (e AdapterTransportError) Timeout() bool   { return e.timeout }
func (e AdapterTransportError) Temporary() bool { return false }
