package keyapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "dev-123", "awgpilot-test")
}

func TestGetRequestWrapped(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/client-api/v1/get-request" || r.URL.Query().Get("public_request_id") != "pid" {
			t.Errorf("unexpected request %s", r.URL)
		}
		if r.Header.Get("X-Device-Id") != "dev-123" {
			t.Errorf("missing device id header")
		}
		w.Write([]byte(`{"data":{"request":{"public_request_id":"pid","simplified_status":"paid","paid_until":"2026-12-01T00:00:00Z","number_of_available_keys":2,"key_limit":3}}}`))
	})
	req, err := c.GetRequest(context.Background(), "pid")
	if err != nil {
		t.Fatal(err)
	}
	if req.SimplifiedStatus != "paid" || req.KeyLimit != 3 || req.NumberOfAvailableKeys != 2 {
		t.Fatalf("unexpected %+v", req)
	}
}

func TestDownloadAWGKeyQueryAndShapes(t *testing.T) {
	// The live gateway answers text/plain with the config itself; JSON shapes are kept as fallbacks.
	for _, body := range []string{"[Interface]\nMTU = 1280\nPrivateKey = x\n", `{"raw_config":"[Interface]\nPrivateKey = x\n"}`, `{"data":{"raw_config":"[Interface]\nPrivateKey = x\n"}}`} {
		c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if r.URL.Path != "/client-api/v1/download-awg-key" || q.Get("iso_country_code") != "NL" || q.Get("force_update_device") != "" {
				t.Errorf("unexpected request %s", r.URL)
			}
			if r.Header.Get("X-Supported-Awg-Version") != "2.0" {
				t.Errorf("missing X-Supported-Awg-Version header")
			}
			w.Write([]byte(body))
		})
		cfg, err := c.DownloadAWGKey(context.Background(), "pid", DownloadOptions{ISOCountryCode: "NL"})
		if err != nil || cfg == "" {
			t.Fatalf("cfg=%q err=%v", cfg, err)
		}
	}
}

func TestAPIErrorSurfaced(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":"change_server_limit_exceeded","localized_message":"limit"}}`))
	})
	err := c.ChangeServer(context.Background(), "pid", "", "DE")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "change_server_limit_exceeded" || apiErr.Status != 429 {
		t.Fatalf("err = %v", err)
	}
}

func TestCountriesNotOffered(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	list, err := c.GetCountries(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("404 must mean 'no choice offered': list=%v err=%v", list, err)
	}
}

func TestPlainHTTPErrorIsRussian(t *testing.T) {
	c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	_, err := c.GetRequest(context.Background(), "pid")
	if err == nil || err.Error() != "сервер не нашёл ключ или адрес (HTTP 404)" {
		t.Fatalf("err = %v", err)
	}
}

func TestCountriesBothShapes(t *testing.T) {
	for _, body := range []string{`[{"country_label":"Netherlands","iso_country_code":"NL"}]`, `{"data":[{"country_label":"Netherlands","iso_country_code":"NL"}]}`} {
		c := newTestServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
		list, err := c.GetCountries(context.Background())
		if err != nil || len(list) != 1 || list[0].ISOCountryCode != "NL" {
			t.Fatalf("list=%v err=%v", list, err)
		}
	}
}

// Transport errors must not carry the request URL: get-request has the key's ID in it.
func TestTransportErrorHidesURL(t *testing.T) {
	c := New("https://127.0.0.1:1", "dev", "test")
	_, err := c.GetRequest(context.Background(), "secret-request-id")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if strings.Contains(err.Error(), "secret-request-id") || strings.Contains(err.Error(), "127.0.0.1:1/") {
		t.Fatalf("the error leaks the URL: %v", err)
	}
}
