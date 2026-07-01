package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mideco-tech/codex-tg/internal/config"
)

func TestDaemonLogOutputCanBeDisabled(t *testing.T) {
	if got := daemonLogOutput(config.Config{LogEnabled: false}); got != io.Discard {
		t.Fatalf("daemonLogOutput(false) = %#v, want io.Discard", got)
	}
	if got := daemonLogOutput(config.Config{LogEnabled: true}); got == io.Discard {
		t.Fatal("daemonLogOutput(true) = io.Discard, want stdout logger")
	}
}

func TestDiagnosticLoggerHonorsFlags(t *testing.T) {
	logger := log.New(io.Discard, "", 0)

	if got := diagnosticLogger(config.Config{LogEnabled: true, DiagnosticLogs: true}, logger); got != logger {
		t.Fatal("diagnosticLogger(enabled, enabled) did not return logger")
	}
	if got := diagnosticLogger(config.Config{LogEnabled: false, DiagnosticLogs: true}, logger); got != nil {
		t.Fatal("diagnosticLogger(log disabled) returned logger, want nil")
	}
	if got := diagnosticLogger(config.Config{LogEnabled: true, DiagnosticLogs: false}, logger); got != nil {
		t.Fatal("diagnosticLogger(diagnostics disabled) returned logger, want nil")
	}
}

func TestSelectedAdapterPrefersExplicitValueThenAutoCredentials(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{
			name: "explicit feishu",
			cfg: config.Config{
				Adapter: "feishu",
			},
			want: "feishu",
		},
		{
			name: "auto feishu",
			cfg: config.Config{
				Adapter:         "auto",
				FeishuAppID:     "cli_app",
				FeishuAppSecret: "secret",
			},
			want: "feishu",
		},
		{
			name: "auto is feishu",
			cfg:  config.Config{Adapter: "auto"},
			want: "feishu",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectedAdapter(tt.cfg); got != tt.want {
				t.Fatalf("selectedAdapter() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunInitWritesPrivateConfigAndRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	t.Setenv("CTR_GO_CONFIG", configPath)
	secret := "feishu-secret-value"
	input := strings.Join([]string{
		"cli_app_id",
		secret,
		"ou_1,ou_2",
		"oc_chat",
		filepath.Join(dir, "project"),
		filepath.Join(dir, "chats"),
		"codex",
		"false",
		"false",
		"",
	}, "\n")
	var out bytes.Buffer

	if err := runWithIO([]string{"init"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("run init failed: %v", err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatal("init output leaked Feishu app secret")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`CTR_GO_ADAPTER="feishu"`,
		`CTR_GO_FEISHU_APP_ID="cli_app_id"`,
		`CTR_GO_FEISHU_APP_SECRET="` + secret + `"`,
		`CTR_GO_FEISHU_ALLOWED_OPEN_IDS="ou_1,ou_2"`,
		`CTR_GO_FEISHU_ALLOWED_CHAT_IDS="oc_chat"`,
		`CTR_GO_CODEX_BIN="codex"`,
		`CTR_GO_NOTIFY_NEW_RUN="false"`,
		`CTR_GO_NOTIFY_SYSTEM="false"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config file missing %q:\n%s", want, text)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatalf("Stat failed: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("config mode = %o, want 0600", got)
		}
	}
	if err := runWithIO([]string{"init"}, strings.NewReader(input), io.Discard); err == nil {
		t.Fatal("second init succeeded, want overwrite refusal")
	}
}

func TestRunInitWritesFeishuConfigWithoutLeakingSecret(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	t.Setenv("CTR_GO_CONFIG", configPath)
	secret := "feishu-secret-value"
	input := strings.Join([]string{
		"cli_app_id",
		secret,
		"ou_1,ou_2",
		"oc_chat",
		filepath.Join(dir, "project"),
		filepath.Join(dir, "chats"),
		"codex",
		"true",
		"true",
		"",
	}, "\n")
	var out bytes.Buffer

	if err := runWithIO([]string{"init"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("run init failed: %v", err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatal("init output leaked Feishu app secret")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`CTR_GO_ADAPTER="feishu"`,
		`CTR_GO_FEISHU_APP_ID="cli_app_id"`,
		`CTR_GO_FEISHU_APP_SECRET="` + secret + `"`,
		`CTR_GO_FEISHU_ALLOWED_OPEN_IDS="ou_1,ou_2"`,
		`CTR_GO_FEISHU_ALLOWED_CHAT_IDS="oc_chat"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config file missing %q:\n%s", want, text)
		}
	}
}

func TestRunInitForceOverwritesConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	t.Setenv("CTR_GO_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("old=true\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	input := strings.Join([]string{
		"cli_app_id",
		"secret",
		"",
		"",
		filepath.Join(dir, "project"),
		filepath.Join(dir, "chats"),
		"codex",
		"true",
		"true",
		"",
	}, "\n")

	if err := runWithIO([]string{"init", "--force"}, strings.NewReader(input), io.Discard); err != nil {
		t.Fatalf("run init --force failed: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if strings.Contains(string(data), "old=true") {
		t.Fatalf("config was not overwritten:\n%s", data)
	}
}

func TestRunFeishuSetupWritesConfigFromScanRegistration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:18080")
	binary := os.Args[0]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/v1/app/registration" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm failed: %v", err)
		}
		switch r.Form.Get("action") {
		case "begin":
			_, _ = w.Write([]byte(`{"device_code":"device-1","verification_uri_complete":"` + serverURL(r) + `/scan?code=1","expires_in":60,"interval":1}`))
		case "poll":
			_, _ = w.Write([]byte(`{"client_id":"cli_app","client_secret":"feishu-secret-value","user_info":{"open_id":"ou_creator","tenant_brand":"feishu"}}`))
		default:
			t.Fatalf("unexpected action %q", r.Form.Get("action"))
		}
	}))
	defer server.Close()
	var out bytes.Buffer

	err := runFeishuSetupWithOptions(feishuSetupOptions{
		Force:          false,
		NoQR:           true,
		ConfigPath:     configPath,
		Domain:         server.URL,
		DefaultCWD:     dir,
		CodexChatsRoot: filepath.Join(dir, "Codex"),
		CodexBin:       binary,
		NotifyNewRun:   "false",
		NotifySystem:   "false",
		RequestTimeout: time.Minute,
	}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("feishu setup failed: %v", err)
	}
	output := out.String()
	if strings.Contains(output, "feishu-secret-value") {
		t.Fatalf("setup output leaked secret:\n%s", output)
	}
	if !strings.Contains(output, "Setup link:") || strings.Contains(output, "█") {
		t.Fatalf("setup output did not honor no-qr:\n%s", output)
	}
	for _, want := range []string{"/start", "bot DM", "topic reply"} {
		if !strings.Contains(output, want) {
			t.Fatalf("setup output missing %q:\n%s", want, output)
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`CTR_GO_ADAPTER="feishu"`,
		`CTR_GO_FEISHU_APP_ID="cli_app"`,
		`CTR_GO_FEISHU_APP_SECRET="feishu-secret-value"`,
		`CTR_GO_FEISHU_ALLOWED_OPEN_IDS="ou_creator"`,
		`CTR_GO_NOTIFY_NEW_RUN="false"`,
		`CTR_GO_NOTIFY_SYSTEM="false"`,
		`CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU="true"`,
		`HTTPS_PROXY="http://127.0.0.1:18080"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
}

func TestRunFeishuSetupRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	if err := os.WriteFile(configPath, []byte("CTR_GO_ADAPTER=feishu\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	err := runFeishuSetupWithOptions(feishuSetupOptions{
		ConfigPath:     configPath,
		DefaultCWD:     dir,
		CodexChatsRoot: filepath.Join(dir, "Codex"),
		CodexBin:       os.Args[0],
		RequestTimeout: time.Millisecond,
	}, strings.NewReader(""), io.Discard)
	if err == nil {
		t.Fatal("feishu setup succeeded, want overwrite refusal")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Fatalf("error = %v, want --force hint", err)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestStatusAndDoctorDoNotLeakConfigFileSecret(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	secret := "feishu-secret-value"
	if err := os.WriteFile(configPath, []byte(strings.Join([]string{
		`CTR_GO_HOME="` + home + `"`,
		`CTR_GO_FEISHU_APP_ID="cli_app_id"`,
		`CTR_GO_FEISHU_APP_SECRET="` + secret + `"`,
		`CTR_GO_DEFAULT_CWD="` + dir + `"`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)

	for _, command := range [][]string{{"status"}, {"doctor"}} {
		var out bytes.Buffer
		if err := runWithIO(command, strings.NewReader(""), &out); err != nil {
			t.Fatalf("%v failed: %v", command, err)
		}
		if strings.Contains(out.String(), secret) {
			t.Fatalf("%v leaked secret:\n%s", command, out.String())
		}
	}
}

func TestRunHealthChecksReportsActionableCodexBinaryFailure(t *testing.T) {
	cfg := config.Config{
		CodexBin:       filepath.Join(t.TempDir(), "missing-codex"),
		DefaultCWD:     t.TempDir(),
		CodexChatsRoot: t.TempDir(),
		RequestTimeout: time.Second,
	}
	report := runHealthChecks(context.Background(), cfg, nil)
	if report.OK {
		t.Fatal("health report OK = true, want false")
	}
	check := findHealthCheck(report.Checks, "codex.binary")
	if check == nil {
		t.Fatalf("codex.binary check missing: %#v", report.Checks)
	}
	if check.Status != healthFail {
		t.Fatalf("codex.binary status = %q, want fail", check.Status)
	}
	if !strings.Contains(check.Remediation, "CTR_GO_CODEX_BIN") {
		t.Fatalf("codex.binary remediation = %q, want CTR_GO_CODEX_BIN guidance", check.Remediation)
	}
}

func TestRunHealthChecksAcceptsFakeCodexAppServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake app-server shell script is Unix-only")
	}
	root := t.TempDir()
	script := writeDoctorFakeCodex(t, root, `#!/bin/sh
set -eu
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '{"jsonrpc":"2.0","id":1,"result":{}}\n' ;;
    *'"method":"initialized"'*) ;;
    *'"method":"thread/list"'*) printf '{"jsonrpc":"2.0","id":2,"result":{"threads":[]}}\n' ;;
    *'"method":"model/list"'*) printf '{"jsonrpc":"2.0","id":3,"result":{"models":[{"id":"gpt-test","isDefault":true}]}}\n' ;;
  esac
done
`)
	cfg := config.Config{
		CodexBin:        script,
		AppServerListen: "stdio://",
		DefaultCWD:      root,
		CodexChatsRoot:  root,
		RequestTimeout:  5 * time.Second,
	}
	report := runHealthChecks(context.Background(), cfg, map[string]string{"appserver.live.last_error": ""})
	appServer := findHealthCheck(report.Checks, "codex.app_server")
	if appServer == nil || appServer.Status != healthPass {
		t.Fatalf("codex.app_server = %#v, want pass", appServer)
	}
	threadList := findHealthCheck(report.Checks, "codex.thread_list")
	if threadList == nil || threadList.Status != healthPass {
		t.Fatalf("codex.thread_list = %#v, want pass", threadList)
	}
	modelList := findHealthCheck(report.Checks, "codex.model_list")
	if modelList == nil || modelList.Status != healthPass {
		t.Fatalf("codex.model_list = %#v, want pass", modelList)
	}
}

func findHealthCheck(checks []healthCheck, id string) *healthCheck {
	for i := range checks {
		if checks[i].ID == id {
			return &checks[i]
		}
	}
	return nil
}

func writeDoctorFakeCodex(t *testing.T, root, body string) string {
	t.Helper()
	path := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(fake-codex) failed: %v", err)
	}
	return path
}
