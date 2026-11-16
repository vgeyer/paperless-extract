// Package paperless is a minimal Paperless-ngx REST client.
package paperless

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

	"github.com/shopspring/decimal"
)

var (
	ErrNotFound       = errors.New("paperless: not found")
	ErrUnauthorized   = errors.New("paperless: unauthorized")
	ErrFieldNotFound  = errors.New("paperless: custom field not found")
	ErrFieldWrongType = errors.New("paperless: custom field is not monetary")
)

const (
	maxResponseBytes = 64 << 20
	maxErrBody       = 512
	maxPages         = 50
)

// StatusError is returned for every non-2xx response.
type StatusError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

// Error implements error.
func (e *StatusError) Error() string {
	// Body is deliberately not included: it may contain document data.
	return fmt.Sprintf("paperless: %s %s: status %d", e.Method, e.URL, e.StatusCode)
}

// HTTPStatus returns the status code; used by retry.IsTransient.
func (e *StatusError) HTTPStatus() int { return e.StatusCode }

// Unwrap maps 404 to ErrNotFound and 401/403 to ErrUnauthorized.
func (e *StatusError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	}
	return nil
}

// Document is the subset of a Paperless document that is used.
type Document struct {
	ID           int                   `json:"id"`
	Title        string                `json:"title"`
	Content      string                `json:"content"`
	CustomFields []CustomFieldInstance `json:"custom_fields"`
}

// CustomFieldInstance is a custom field value on a document.
type CustomFieldInstance struct {
	Field int             `json:"field"`
	Value json.RawMessage `json:"value"`
}

// IsEmpty: true if Value is nil/empty, JSON null, or the JSON string "".
func (c CustomFieldInstance) IsEmpty() bool {
	v := bytes.TrimSpace(c.Value)
	return len(v) == 0 || string(v) == "null" || string(v) == `""`
}

// CustomField describes a custom field definition.
type CustomField struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	DataType string `json:"data_type"`
}

// Client talks to the Paperless REST API.
type Client struct {
	base  *url.URL
	baseS string
	token string
	hc    *http.Client
}

// NewClient creates a client. nil httpClient gets a 30 s timeout.
func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	b := strings.TrimRight(baseURL, "/")
	u, _ := url.Parse(b)
	if u == nil {
		u = &url.URL{}
	}
	return &Client{base: u, baseS: b, token: token, hc: httpClient}
}

func (c *Client) do(ctx context.Context, method, fullURL string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return &StatusError{Method: method, URL: fullURL, StatusCode: resp.StatusCode, Body: string(b)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("paperless: decode %s %s: %w", method, fullURL, err)
	}
	return nil
}

// GetDocument fetches one document.
func (c *Client) GetDocument(ctx context.Context, id int) (*Document, error) {
	var d Document
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s/api/documents/%d/", c.baseS, id), nil, &d); err != nil {
		return nil, err
	}
	if d.CustomFields == nil {
		d.CustomFields = []CustomFieldInstance{}
	}
	return &d, nil
}

type fieldPage struct {
	Next    *string       `json:"next"`
	Results []CustomField `json:"results"`
}

// ListCustomFields lists all custom fields, following pagination.
func (c *Client) ListCustomFields(ctx context.Context) ([]CustomField, error) {
	var all []CustomField
	next := c.baseS + "/api/custom_fields/?page_size=100"
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("paperless: more than %d pages of custom fields", maxPages)
		}
		var p fieldPage
		if err := c.do(ctx, http.MethodGet, next, nil, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Results...)
		if p.Next == nil || *p.Next == "" {
			return all, nil
		}
		nu, err := url.Parse(*p.Next)
		if err != nil {
			return nil, fmt.Errorf("paperless: invalid next URL: %w", err)
		}
		// Keep configured scheme+host (reverse-proxy safe), take path+query from next.
		r := *c.base
		r.Path, r.RawPath, r.RawQuery = nu.Path, nu.RawPath, nu.RawQuery
		r.Fragment = ""
		next = r.String()
	}
}

// ResolveCustomField finds the field whose name matches exactly and checks its type.
func (c *Client) ResolveCustomField(ctx context.Context, name string) (CustomField, error) {
	fields, err := c.ListCustomFields(ctx)
	if err != nil {
		return CustomField{}, err
	}
	for _, f := range fields {
		if f.Name != name {
			continue
		}
		if f.DataType != "monetary" {
			return CustomField{}, fmt.Errorf("%w: field %q has type %q", ErrFieldWrongType, name, f.DataType)
		}
		return f, nil
	}
	return CustomField{}, fmt.Errorf("%w: %q", ErrFieldNotFound, name)
}

// SetCustomField sets one custom field by read-merge-write.
func (c *Client) SetCustomField(ctx context.Context, docID, fieldID int, value string) error {
	doc, err := c.GetDocument(ctx, docID)
	if err != nil {
		return err
	}
	v, err := json.Marshal(value)
	if err != nil {
		return err
	}
	fields := doc.CustomFields
	found := false
	for i := range fields {
		if fields[i].Field == fieldID {
			fields[i].Value = v
			found = true
			break
		}
	}
	if !found {
		fields = append(fields, CustomFieldInstance{Field: fieldID, Value: v})
	}
	body, err := json.Marshal(struct {
		CustomFields []CustomFieldInstance `json:"custom_fields"`
	}{fields})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("%s/api/documents/%d/", c.baseS, docID), body, nil)
}

// Ping checks reachability and token validity.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, c.baseS+"/api/custom_fields/?page_size=1", nil, nil)
}

// FormatMonetary returns currency + amount with exactly 2 decimals.
func FormatMonetary(currency string, amount decimal.Decimal) string {
	return currency + amount.StringFixed(2)
}
