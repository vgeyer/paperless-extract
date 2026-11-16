package paperless_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/vgeyer/paperless-extract/internal/paperless"
)

const token = "tok-0123456789"

type recorded struct {
	Method, Path, RawQuery string
	Header                 http.Header
	Body                   []byte
}

type fakePaperless struct {
	t       *testing.T
	mu      sync.Mutex
	reqs    []recorded
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) (*fakePaperless, *httptest.Server) {
	f := &fakePaperless{t: t, handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), b})
		f.mu.Unlock()
		f.handler(w, r, b)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakePaperless) requests() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.reqs...)
}

func writeJSON(w http.ResponseWriter, status int, v string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, v)
}

func checkHeaders(t *testing.T, r recorded) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Token "+token {
		t.Errorf("%s %s Authorization = %q", r.Method, r.Path, got)
	}
	if got := r.Header.Get("Accept"); got != "application/json" {
		t.Errorf("%s %s Accept = %q", r.Method, r.Path, got)
	}
}

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("want invalid JSON: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

const docJSON = `{"id":123,"title":"Rechnung 4711","content":"Gesamt 1.234,56 EUR","owner":2,"tags":[1,2],
 "custom_fields":[{"field":3,"value":"abc"},{"field":7,"value":null},{"field":9,"value":{"nested":[1,2]}}]}`

func TestGetDocument(t *testing.T) {
	f, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/documents/123/" {
			writeJSON(w, 404, `{"detail":"Not found."}`)
			return
		}
		writeJSON(w, 200, docJSON)
	})
	c := paperless.NewClient(srv.URL+"/", token, nil) // trailing slash trimmed
	doc, err := c.GetDocument(context.Background(), 123)
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if doc.ID != 123 || doc.Title != "Rechnung 4711" || doc.Content != "Gesamt 1.234,56 EUR" {
		t.Errorf("doc = %+v", doc)
	}
	if len(doc.CustomFields) != 3 || doc.CustomFields[0].Field != 3 || doc.CustomFields[1].Field != 7 {
		t.Fatalf("custom fields = %+v", doc.CustomFields)
	}
	if string(doc.CustomFields[0].Value) != `"abc"` {
		t.Errorf("value 0 = %s", doc.CustomFields[0].Value)
	}
	jsonEqual(t, doc.CustomFields[2].Value, `{"nested":[1,2]}`)
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	checkHeaders(t, reqs[0])
}

func TestGetDocumentNullCustomFields(t *testing.T) {
	_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 200, `{"id":5,"title":"t","content":"c","custom_fields":null}`)
	})
	doc, err := paperless.NewClient(srv.URL, token, nil).GetDocument(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if doc.CustomFields == nil || len(doc.CustomFields) != 0 {
		t.Errorf("CustomFields = %#v, want empty non-nil slice", doc.CustomFields)
	}
}

func TestGetDocumentErrors(t *testing.T) {
	cases := []struct {
		status   int
		sentinel error
	}{
		{404, paperless.ErrNotFound},
		{401, paperless.ErrUnauthorized},
		{403, paperless.ErrUnauthorized},
		{500, nil},
		{429, nil},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			body := `{"detail":"` + strings.Repeat("x", 2000) + `"}`
			_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				writeJSON(w, c.status, body)
			})
			_, err := paperless.NewClient(srv.URL, token, nil).GetDocument(context.Background(), 1)
			if err == nil {
				t.Fatal("expected error")
			}
			var se *paperless.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("not a *StatusError: %T %v", err, err)
			}
			if se.StatusCode != c.status || se.HTTPStatus() != c.status {
				t.Errorf("status = %d/%d", se.StatusCode, se.HTTPStatus())
			}
			if se.Method != http.MethodGet || !strings.Contains(se.URL, "/api/documents/1/") {
				t.Errorf("Method=%q URL=%q", se.Method, se.URL)
			}
			if len(se.Body) > 512 || len(se.Body) == 0 {
				t.Errorf("Body length = %d, want 1..512", len(se.Body))
			}
			if strings.Contains(se.Error(), token) || strings.Contains(se.URL, token) {
				t.Errorf("error leaks token")
			}
			for _, s := range []error{paperless.ErrNotFound, paperless.ErrUnauthorized} {
				if want := c.sentinel != nil && errors.Is(s, c.sentinel); errors.Is(err, s) != want {
					t.Errorf("errors.Is(%v) = %v, want %v", s, !want, want)
				}
			}
		})
	}
}

func TestGetDocumentTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, err := paperless.NewClient(url, token, nil).GetDocument(context.Background(), 1)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestListCustomFieldsPagination(t *testing.T) {
	f, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != "/api/custom_fields/" {
			writeJSON(w, 404, `{}`)
			return
		}
		switch r.URL.Query().Get("page") {
		case "":
			// next points at a different (internal) host: must be ignored.
			writeJSON(w, 200, `{"count":3,"next":"http://paperless.internal:8000/api/custom_fields/?page=2&page_size=100","previous":null,
				"results":[{"id":1,"name":"Notes","data_type":"string"},{"id":7,"name":"Total amount","data_type":"monetary"}]}`)
		case "2":
			writeJSON(w, 200, `{"count":3,"next":null,"previous":"x",
				"results":[{"id":9,"name":"Other","data_type":"monetary"}]}`)
		default:
			writeJSON(w, 404, `{}`)
		}
	})
	fields, err := paperless.NewClient(srv.URL, token, nil).ListCustomFields(context.Background())
	if err != nil {
		t.Fatalf("ListCustomFields: %v", err)
	}
	want := []paperless.CustomField{
		{ID: 1, Name: "Notes", DataType: "string"},
		{ID: 7, Name: "Total amount", DataType: "monetary"},
		{ID: 9, Name: "Other", DataType: "monetary"},
	}
	if !reflect.DeepEqual(fields, want) {
		t.Errorf("fields = %+v", fields)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[0].RawQuery != "page_size=100" {
		t.Errorf("first query = %q", reqs[0].RawQuery)
	}
	if !strings.Contains(reqs[1].RawQuery, "page=2") {
		t.Errorf("second query = %q", reqs[1].RawQuery)
	}
	for _, r := range reqs {
		checkHeaders(t, r)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
	}
}

func TestListCustomFieldsSubPath(t *testing.T) {
	_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case r.URL.Path == "/paperless/api/custom_fields/" && r.URL.Query().Get("page") == "":
			writeJSON(w, 200, `{"count":2,"next":"http://other/paperless/api/custom_fields/?page=2&page_size=100","results":[{"id":1,"name":"A","data_type":"monetary"}]}`)
		case r.URL.Path == "/paperless/api/custom_fields/" && r.URL.Query().Get("page") == "2":
			writeJSON(w, 200, `{"count":2,"next":null,"results":[{"id":2,"name":"B","data_type":"monetary"}]}`)
		default:
			writeJSON(w, 404, `{}`)
		}
	})
	fields, err := paperless.NewClient(srv.URL+"/paperless", token, nil).ListCustomFields(context.Background())
	if err != nil {
		t.Fatalf("ListCustomFields: %v", err)
	}
	if len(fields) != 2 {
		t.Errorf("fields = %+v", fields)
	}
}

func TestListCustomFieldsTooManyPages(t *testing.T) {
	f, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 200, `{"count":1,"next":"http://x/api/custom_fields/?page=2&page_size=100","results":[]}`)
	})
	_, err := paperless.NewClient(srv.URL, token, nil).ListCustomFields(context.Background())
	if err == nil {
		t.Fatal("expected error for endless pagination")
	}
	if n := len(f.requests()); n > 51 {
		t.Errorf("made %d requests, want ≤ 51", n)
	}
}

func fieldsServer(t *testing.T, results string) *httptest.Server {
	_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 200, `{"count":1,"next":null,"results":`+results+`}`)
	})
	return srv
}

func TestResolveCustomField(t *testing.T) {
	srv := fieldsServer(t, `[{"id":1,"name":"total amount","data_type":"monetary"},
		{"id":2,"name":"Total amount","data_type":"monetary"},
		{"id":3,"name":"Total amount","data_type":"string"},
		{"id":4,"name":"Notes","data_type":"string"}]`)
	c := paperless.NewClient(srv.URL, token, nil)
	f, err := c.ResolveCustomField(context.Background(), "Total amount")
	if err != nil || f.ID != 2 || f.DataType != "monetary" {
		t.Errorf("Resolve = %+v, %v", f, err)
	}
	_, err = c.ResolveCustomField(context.Background(), "Missing")
	if !errors.Is(err, paperless.ErrFieldNotFound) {
		t.Errorf("missing: %v", err)
	}
	_, err = c.ResolveCustomField(context.Background(), "Notes")
	if !errors.Is(err, paperless.ErrFieldWrongType) {
		t.Errorf("wrong type: %v", err)
	} else if !strings.Contains(err.Error(), "string") {
		t.Errorf("wrong-type error should include actual type: %v", err)
	}
}

func TestResolveCustomFieldFirstMatchWrongType(t *testing.T) {
	srv := fieldsServer(t, `[{"id":3,"name":"Total amount","data_type":"string"},{"id":2,"name":"Total amount","data_type":"monetary"}]`)
	_, err := paperless.NewClient(srv.URL, token, nil).ResolveCustomField(context.Background(), "Total amount")
	if !errors.Is(err, paperless.ErrFieldWrongType) {
		t.Errorf("first match wins: want ErrFieldWrongType, got %v", err)
	}
}

func TestResolveCustomFieldPropagatesStatus(t *testing.T) {
	_, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 401, `{"detail":"Invalid token."}`)
	})
	_, err := paperless.NewClient(srv.URL, token, nil).ResolveCustomField(context.Background(), "Total amount")
	if !errors.Is(err, paperless.ErrUnauthorized) {
		t.Errorf("want ErrUnauthorized, got %v", err)
	}
}

func setFieldServer(t *testing.T, docBody string, patchStatus int) (*fakePaperless, *httptest.Server) {
	return newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.URL.Path != "/api/documents/123/" {
			writeJSON(w, 404, `{}`)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, docBody)
		case http.MethodPatch:
			writeJSON(w, patchStatus, `{"ignored":true}`)
		default:
			writeJSON(w, 405, `{}`)
		}
	})
}

func lastPatch(t *testing.T, f *fakePaperless) recorded {
	t.Helper()
	reqs := f.requests()
	if len(reqs) != 2 || reqs[0].Method != http.MethodGet || reqs[1].Method != http.MethodPatch {
		var ms []string
		for _, r := range reqs {
			ms = append(ms, r.Method+" "+r.Path)
		}
		t.Fatalf("requests = %v, want GET then PATCH", ms)
	}
	p := reqs[1]
	checkHeaders(t, p)
	if ct := p.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("PATCH Content-Type = %q", ct)
	}
	if p.Path != "/api/documents/123/" {
		t.Errorf("PATCH path = %q", p.Path)
	}
	return p
}

func TestSetCustomFieldReplaceInPlace(t *testing.T) {
	f, srv := setFieldServer(t, docJSON, 200)
	err := paperless.NewClient(srv.URL, token, nil).SetCustomField(context.Background(), 123, 7, "EUR1234.56")
	if err != nil {
		t.Fatalf("SetCustomField: %v", err)
	}
	p := lastPatch(t, f)
	jsonEqual(t, p.Body, `{"custom_fields":[{"field":3,"value":"abc"},{"field":7,"value":"EUR1234.56"},{"field":9,"value":{"nested":[1,2]}}]}`)
}

func TestSetCustomFieldAppend(t *testing.T) {
	f, srv := setFieldServer(t, docJSON, 200)
	err := paperless.NewClient(srv.URL, token, nil).SetCustomField(context.Background(), 123, 11, "CHF5.00")
	if err != nil {
		t.Fatalf("SetCustomField: %v", err)
	}
	p := lastPatch(t, f)
	jsonEqual(t, p.Body, `{"custom_fields":[{"field":3,"value":"abc"},{"field":7,"value":null},{"field":9,"value":{"nested":[1,2]}},{"field":11,"value":"CHF5.00"}]}`)
}

func TestSetCustomFieldNullFields(t *testing.T) {
	f, srv := setFieldServer(t, `{"id":123,"title":"t","content":"c","custom_fields":null}`, 204)
	err := paperless.NewClient(srv.URL, token, nil).SetCustomField(context.Background(), 123, 7, "EUR1.00")
	if err != nil {
		t.Fatalf("SetCustomField: %v", err)
	}
	p := lastPatch(t, f)
	jsonEqual(t, p.Body, `{"custom_fields":[{"field":7,"value":"EUR1.00"}]}`)
}

func TestSetCustomFieldPatchOnlyCustomFields(t *testing.T) {
	f, srv := setFieldServer(t, docJSON, 200)
	if err := paperless.NewClient(srv.URL, token, nil).SetCustomField(context.Background(), 123, 7, "EUR1.00"); err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(lastPatch(t, f).Body, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Errorf("PATCH body has keys %v, want only custom_fields", m)
	}
}

func TestSetCustomFieldErrors(t *testing.T) {
	t.Run("get 404", func(t *testing.T) {
		f, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { writeJSON(w, 404, `{}`) })
		err := paperless.NewClient(srv.URL, token, nil).SetCustomField(context.Background(), 123, 7, "EUR1.00")
		if !errors.Is(err, paperless.ErrNotFound) {
			t.Errorf("err = %v", err)
		}
		for _, r := range f.requests() {
			if r.Method == http.MethodPatch {
				t.Errorf("PATCH sent although GET failed")
			}
		}
	})
	t.Run("patch 400", func(t *testing.T) {
		_, srv := setFieldServer(t, docJSON, 400)
		err := paperless.NewClient(srv.URL, token, nil).SetCustomField(context.Background(), 123, 7, "EUR1.00")
		var se *paperless.StatusError
		if !errors.As(err, &se) || se.StatusCode != 400 || se.Method != http.MethodPatch {
			t.Errorf("err = %#v", err)
		}
	})
}

func TestPing(t *testing.T) {
	f, srv := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		writeJSON(w, 200, `{"count":0,"next":null,"results":[]}`)
	})
	if err := paperless.NewClient(srv.URL, token, nil).Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	r := f.requests()[0]
	checkHeaders(t, r)
	if r.Method != http.MethodGet || r.Path != "/api/custom_fields/" || r.RawQuery != "page_size=1" {
		t.Errorf("Ping request = %s %s?%s", r.Method, r.Path, r.RawQuery)
	}

	_, srv2 := newFake(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { writeJSON(w, 403, `{}`) })
	if err := paperless.NewClient(srv2.URL, token, nil).Ping(context.Background()); !errors.Is(err, paperless.ErrUnauthorized) {
		t.Errorf("Ping 403: %v", err)
	}
}

func TestStatusErrorUnwrap(t *testing.T) {
	cases := map[int]error{404: paperless.ErrNotFound, 401: paperless.ErrUnauthorized, 403: paperless.ErrUnauthorized, 500: nil, 400: nil}
	for code, want := range cases {
		se := &paperless.StatusError{Method: "GET", URL: "http://x/api/documents/1/", StatusCode: code, Body: "b"}
		if got := se.Unwrap(); (got == nil) != (want == nil) || (want != nil && !errors.Is(got, want)) {
			t.Errorf("Unwrap(%d) = %v, want %v", code, got, want)
		}
		if se.HTTPStatus() != code {
			t.Errorf("HTTPStatus = %d", se.HTTPStatus())
		}
		if !strings.Contains(se.Error(), fmt.Sprint(code)) {
			t.Errorf("Error() %q lacks status", se.Error())
		}
	}
}

func TestIsEmpty(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"", true},
		{"null", true},
		{`""`, true},
		{`"EUR1.00"`, false},
		{`0`, false},
		{`" "`, false},
		{`false`, false},
	}
	for _, c := range cases {
		var raw json.RawMessage
		if c.v != "" {
			raw = json.RawMessage(c.v)
		}
		if got := (paperless.CustomFieldInstance{Field: 1, Value: raw}).IsEmpty(); got != c.want {
			t.Errorf("IsEmpty(%q) = %v, want %v", c.v, got, c.want)
		}
	}
	if !(paperless.CustomFieldInstance{Value: json.RawMessage{}}).IsEmpty() {
		t.Error("empty RawMessage not empty")
	}
}

func TestFormatMonetary(t *testing.T) {
	cases := []struct {
		cur, amt, want string
	}{
		{"EUR", "1234.5", "EUR1234.50"},
		{"CHF", "0.005", "CHF0.01"},
		{"EUR", "1234", "EUR1234.00"},
		{"EUR", "1234.56", "EUR1234.56"},
		{"USD", "0.994", "USD0.99"},
		{"PLN", "999999.995", "PLN1000000.00"},
		{"EUR", "1000000", "EUR1000000.00"},
	}
	for _, c := range cases {
		if got := paperless.FormatMonetary(c.cur, decimal.RequireFromString(c.amt)); got != c.want {
			t.Errorf("FormatMonetary(%s,%s) = %q, want %q", c.cur, c.amt, got, c.want)
		}
	}
}
