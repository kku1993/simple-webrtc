package turnstile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifySuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.Form.Get("secret") != "sek" {
			t.Errorf("secret = %q", r.Form.Get("secret"))
		}
		if r.Form.Get("response") != "tok" {
			t.Errorf("response = %q", r.Form.Get("response"))
		}
		if r.Form.Get("remoteip") != "1.2.3.4" {
			t.Errorf("remoteip = %q", r.Form.Get("remoteip"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer srv.Close()
	c := New("sek")
	c.SetEndpoint(srv.URL)
	ok, err := c.Verify(context.Background(), "tok", "1.2.3.4")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Errorf("expected success")
	}
}

func TestVerifyFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error-codes": []string{"invalid-input-response"}})
	}))
	defer srv.Close()
	c := New("sek")
	c.SetEndpoint(srv.URL)
	ok, err := c.Verify(context.Background(), "tok", "")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Errorf("expected failure")
	}
}

func TestVerifyNoSecret(t *testing.T) {
	c := New("")
	if _, err := c.Verify(context.Background(), "tok", ""); err == nil {
		t.Errorf("expected error for missing secret")
	}
}
