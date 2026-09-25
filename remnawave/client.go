package remnawave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"xray-checker/paneltelemetry"
)

const maxAPIResponseBytes = 8 * 1024 * 1024

type API interface {
	GetHosts(context.Context) ([]Host, error)
	GetInternalSquads(context.Context) ([]InternalSquad, error)
	GetExternalSquads(context.Context) ([]ExternalSquad, error)
	UpdateExternalHeaders(context.Context, string, map[string]string) error
}

type HTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewHTTPClient(baseURL, token string, timeout time.Duration) (*HTTPClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	token = strings.TrimSpace(token)
	if baseURL == "" {
		return nil, fmt.Errorf("Remnawave API URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Remnawave API URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("Remnawave API URL must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("Remnawave API URL must not contain credentials, query or fragment")
	}
	if token == "" {
		return nil, fmt.Errorf("Remnawave API token is required")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &HTTPClient{
		baseURL: baseURL,
		token:   token,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("Remnawave API redirects are disabled")
			},
		},
	}, nil
}

func (c *HTTPClient) apiPath(path string) string {
	if strings.HasSuffix(strings.ToLower(c.baseURL), "/api") {
		return c.baseURL + path
	}
	return c.baseURL + "/api" + path
}

func (c *HTTPClient) GetHosts(ctx context.Context) ([]Host, error) {
	var envelope struct {
		Response []Host `json:"response"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/hosts", nil, &envelope); err != nil {
		return nil, err
	}
	return envelope.Response, nil
}

func (c *HTTPClient) GetInternalSquads(ctx context.Context) ([]InternalSquad, error) {
	var envelope struct {
		Response struct {
			InternalSquads []InternalSquad `json:"internalSquads"`
		} `json:"response"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/internal-squads", nil, &envelope); err != nil {
		return nil, err
	}
	return envelope.Response.InternalSquads, nil
}

func (c *HTTPClient) GetExternalSquads(ctx context.Context) ([]ExternalSquad, error) {
	var envelope struct {
		Response struct {
			ExternalSquads []ExternalSquad `json:"externalSquads"`
		} `json:"response"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/external-squads", nil, &envelope); err != nil {
		return nil, err
	}
	return envelope.Response.ExternalSquads, nil
}

func (c *HTTPClient) UpdateExternalHeaders(ctx context.Context, uuid string, headers map[string]string) error {
	body := struct {
		UUID               string            `json:"uuid"`
		ResponseHeadersAdd map[string]string `json:"responseHeadersAdd"`
	}{
		UUID:               uuid,
		ResponseHeadersAdd: headers,
	}
	return c.doJSON(ctx, http.MethodPatch, "/external-squads", body, nil)
}

func (c *HTTPClient) doJSON(ctx context.Context, method, path string, body, destination any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode Remnawave API request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiPath(path), reader)
	if err != nil {
		return fmt.Errorf("create Remnawave API request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	response, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("Remnawave API %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxAPIResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("read Remnawave API response: %w", err)
	}
	if len(data) > maxAPIResponseBytes {
		return fmt.Errorf("Remnawave API response exceeds %d bytes", maxAPIResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := compactAPIError(data)
		if c.token != "" {
			message = strings.ReplaceAll(message, c.token, "[redacted]")
		}
		return &APIError{Method: method, Path: path, StatusCode: response.StatusCode, Message: message}
	}
	if destination == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return fmt.Errorf("decode Remnawave API %s response: %w", path, err)
	}
	return nil
}

// APIError is a response outside 2xx. It keeps the status code so a caller can
// tell a missing permission from a broken panel; the text is what the error
// always said.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Remnawave API %s %s returned HTTP %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// GetNodes reads the panel's node list for telemetry. A refused token is
// reported as paneltelemetry.ErrForbidden, because the fix — granting the
// nodes:list scope — is the same whatever the panel's message says.
func (c *HTTPClient) GetNodes(ctx context.Context) ([]paneltelemetry.Node, error) {
	var envelope struct {
		Response []paneltelemetry.Node `json:"response"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/nodes", nil, &envelope); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("%w (HTTP %d)", paneltelemetry.ErrForbidden, apiErr.StatusCode)
		}
		return nil, err
	}
	return envelope.Response, nil
}

func compactAPIError(data []byte) string {
	text := strings.Join(strings.Fields(string(data)), " ")
	if text == "" {
		return "empty response"
	}
	const max = 300
	if len(text) > max {
		return text[:max] + "..."
	}
	return text
}
