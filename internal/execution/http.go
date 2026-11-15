package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
		return fmt.Errorf("upstream transport failure")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("upstream HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(result); err != nil {
		return fmt.Errorf("invalid upstream response")
	}
	return nil
}
