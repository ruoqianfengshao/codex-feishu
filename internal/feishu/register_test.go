package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistrationClientRegisterPollsUntilSuccess(t *testing.T) {
	var actions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != registrationEndpoint {
			t.Fatalf("path = %q, want %q", r.URL.Path, registrationEndpoint)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm failed: %v", err)
		}
		action := r.Form.Get("action")
		actions = append(actions, action)
		switch action {
		case "begin":
			writeJSON(t, w, map[string]any{
				"device_code":               "device-1",
				"verification_uri_complete": serverURL(r) + "/scan?code=1",
				"expires_in":                60,
				"interval":                  1,
			})
		case "poll":
			if r.Form.Get("device_code") != "device-1" {
				t.Fatalf("device_code = %q, want device-1", r.Form.Get("device_code"))
			}
			writeJSON(t, w, map[string]any{
				"client_id":     "cli_test",
				"client_secret": "secret",
				"user_info": map[string]any{
					"open_id":      "ou_1",
					"tenant_brand": "feishu",
				},
			})
		default:
			t.Fatalf("unexpected action %q", action)
		}
	}))
	defer server.Close()

	var qr RegistrationQRCode
	result, err := (RegistrationClient{
		HTTPClient: server.Client(),
		Domain:     server.URL,
	}).Register(context.Background(), RegistrationOptions{
		OnQRCodeReady: func(info RegistrationQRCode) {
			qr = info
		},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if result.ClientID != "cli_test" || result.ClientSecret != "secret" || result.UserOpenID != "ou_1" {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(qr.URL, "from=sdk") || !strings.Contains(qr.URL, "source=codex-tg-go") || !strings.Contains(qr.URL, "tp=sdk") || !strings.Contains(qr.URL, "name=Codex") {
		t.Fatalf("qr url missing sdk params: %s", qr.URL)
	}
	if got := strings.Join(actions, ","); got != "begin,poll" {
		t.Fatalf("actions = %s, want begin,poll", got)
	}
}

func TestRegistrationClientHandlesSlowDownAndLarkDomainSwitch(t *testing.T) {
	var feishuPolls int
	var larkPolled bool
	var larkURL string
	lark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		larkPolled = true
		writeJSON(t, w, map[string]any{
			"client_id":     "cli_lark",
			"client_secret": "secret",
			"user_info": map[string]any{
				"open_id":      "ou_lark",
				"tenant_brand": "lark",
			},
		})
	}))
	defer lark.Close()
	larkURL = lark.URL
	feishu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm failed: %v", err)
		}
		switch r.Form.Get("action") {
		case "begin":
			writeJSON(t, w, map[string]any{
				"device_code":               "device-1",
				"verification_uri_complete": serverURL(r) + "/scan?code=1",
				"expires_in":                60,
				"interval":                  0,
			})
		case "poll":
			feishuPolls++
			if feishuPolls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(t, w, map[string]any{"error": "slow_down", "error_description": "wait"})
				return
			}
			writeJSON(t, w, map[string]any{
				"error": "authorization_pending",
				"user_info": map[string]any{
					"tenant_brand": "lark",
				},
			})
		default:
			t.Fatalf("unexpected action %q", r.Form.Get("action"))
		}
	}))
	defer feishu.Close()

	var statuses []string
	result, err := (RegistrationClient{
		HTTPClient: feishu.Client(),
		Domain:     feishu.URL,
		LarkDomain: larkURL,
		wait: func(context.Context, time.Duration) error {
			return nil
		},
	}).Register(context.Background(), RegistrationOptions{
		OnStatusChange: func(status RegistrationStatus) {
			statuses = append(statuses, status.Status)
		},
	})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	if !larkPolled {
		t.Fatal("lark domain was not polled")
	}
	if result.ClientID != "cli_lark" || result.TenantBrand != "lark" {
		t.Fatalf("result = %#v", result)
	}
	if got := strings.Join(statuses, ","); got != "slow_down,domain_switched" {
		t.Fatalf("statuses = %s, want slow_down,domain_switched", got)
	}
}

func TestRegistrationClientAccessDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm failed: %v", err)
		}
		switch r.Form.Get("action") {
		case "begin":
			writeJSON(t, w, map[string]any{
				"device_code":               "device-1",
				"verification_uri_complete": serverURL(r) + "/scan?code=1",
				"expires_in":                60,
				"interval":                  1,
			})
		case "poll":
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(t, w, map[string]any{"error": "access_denied", "error_description": "denied"})
		}
	}))
	defer server.Close()

	_, err := (RegistrationClient{
		HTTPClient: server.Client(),
		Domain:     server.URL,
	}).Register(context.Background(), RegistrationOptions{})
	if err == nil {
		t.Fatal("Register succeeded, want access_denied error")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("error = %v, want access_denied", err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("Encode failed: %v", err)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestRegistrationQRCodeURL(t *testing.T) {
	got, err := registrationQRCodeURL("https://example.com/device?code=abc", "test-source")
	if err != nil {
		t.Fatalf("registrationQRCodeURL failed: %v", err)
	}
	for _, want := range []string{"from=sdk", "source=codex-tg-go%2Ftest-source", "tp=sdk", "name=Codex"} {
		if !strings.Contains(got, want) {
			t.Fatalf("url missing %q: %s", want, got)
		}
	}
}

func TestRegistrationQRCodeURLPreservesExistingName(t *testing.T) {
	got, err := registrationQRCodeURL("https://example.com/device?code=abc&name=Custom", "")
	if err != nil {
		t.Fatalf("registrationQRCodeURL failed: %v", err)
	}
	if !strings.Contains(got, "name=Custom") || strings.Contains(got, "name=Codex") {
		t.Fatalf("url name preset = %s, want Custom only", got)
	}
}

func TestRegistrationBaseURLAcceptsDomainsAndURLs(t *testing.T) {
	tests := map[string]string{
		"":                    "https://accounts.feishu.cn",
		"accounts.example.cn": "https://accounts.example.cn",
		"https://local.test/": "https://local.test",
	}
	for input, want := range tests {
		got, err := registrationBaseURL(input, defaultRegistrationDomain)
		if err != nil {
			t.Fatalf("registrationBaseURL(%q) failed: %v", input, err)
		}
		if got != want {
			t.Fatalf("registrationBaseURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRegistrationPollRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"error": "authorization_pending"})
	}))
	defer server.Close()

	_, err := (RegistrationClient{
		HTTPClient: server.Client(),
	}).poll(ctx, pollState{
		baseURL:    server.URL,
		deviceCode: "device-1",
		expiresIn:  time.Minute,
		interval:   time.Second,
	})
	if err == nil {
		t.Fatal("poll succeeded, want context error")
	}
}
