package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServiceInstallNonInteractiveWritesConfigAndLaunchAgent(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	binary := os.Args[0]
	secret := "feishu-secret-value"
	var out bytes.Buffer

	err := runWithIO([]string{
		"service", "install",
		"--non-interactive",
		"--config", configPath,
		"--feishu-app-id", "cli_app_id",
		"--feishu-app-secret", secret,
		"--default-cwd", dir,
		"--codex-chats-root", filepath.Join(dir, "Codex"),
		"--codex-bin", binary,
		"--ctr-go-bin", binary,
		"--notify-new-run", "false",
		"--notify-system", "false",
	}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("service install failed: %v", err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("service install leaked secret:\n%s", out.String())
	}
	for _, want := range []string{"/start", "bot DM", "topic reply"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("service install output missing %q:\n%s", want, out.String())
		}
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`CTR_GO_ADAPTER="feishu"`,
		`CTR_GO_FEISHU_APP_ID="cli_app_id"`,
		`CTR_GO_FEISHU_APP_SECRET="` + secret + `"`,
		`CTR_GO_NOTIFY_NEW_RUN="false"`,
		`CTR_GO_NOTIFY_SYSTEM="false"`,
		`CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU="false"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "CTR_GO_CTR_GO_BIN") {
		t.Fatalf("internal service binary key leaked into config:\n%s", text)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat config failed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 0600", got)
	}
	plistPath := filepath.Join(home, "service", serviceLabel+".plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("ReadFile plist failed: %v", err)
	}
	plistText := string(plist)
	if strings.Contains(plistText, secret) {
		t.Fatalf("plist leaked secret:\n%s", plistText)
	}
	for _, want := range []string{
		"<key>CTR_GO_CONFIG</key>",
		configPath,
		"<string>" + binary + "</string>",
		"<string>daemon</string>",
		"<string>run</string>",
	} {
		if !strings.Contains(plistText, want) {
			t.Fatalf("plist missing %q:\n%s", want, plistText)
		}
	}
}

func TestServiceInstallNonInteractiveWritesFeishuConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	binary := os.Args[0]
	secret := "feishu-secret-value"
	var out bytes.Buffer

	err := runWithIO([]string{
		"service", "install",
		"--non-interactive",
		"--config", configPath,
		"--adapter", "feishu",
		"--feishu-app-id", "cli_app_id",
		"--feishu-app-secret", secret,
		"--feishu-allowed-open-ids", "ou_1,ou_2",
		"--feishu-allowed-chat-ids", "oc_chat",
		"--default-cwd", dir,
		"--codex-chats-root", filepath.Join(dir, "Codex"),
		"--codex-bin", binary,
		"--ctr-go-bin", binary,
		"--notify-new-run", "false",
		"--open-codex-desktop", "true",
	}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("service install failed: %v", err)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("service install leaked Feishu secret:\n%s", out.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`CTR_GO_ADAPTER="feishu"`,
		`CTR_GO_FEISHU_APP_ID="cli_app_id"`,
		`CTR_GO_FEISHU_APP_SECRET="` + secret + `"`,
		`CTR_GO_FEISHU_ALLOWED_OPEN_IDS="ou_1,ou_2"`,
		`CTR_GO_FEISHU_ALLOWED_CHAT_IDS="oc_chat"`,
		`CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU="true"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	plist, err := os.ReadFile(filepath.Join(home, "service", serviceLabel+".plist"))
	if err != nil {
		t.Fatalf("ReadFile plist failed: %v", err)
	}
	if strings.Contains(string(plist), secret) {
		t.Fatalf("plist leaked Feishu secret:\n%s", plist)
	}
}

func TestServiceInstallForcePreservesExistingUnknownConfigKeys(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	binary := os.Args[0]
	if err := os.WriteFile(configPath, []byte("CTR_GO_HOME=/custom/home\nCTR_GO_EXTRA_FLAG=yes\n"), 0o600); err != nil {
		t.Fatalf("WriteFile existing config failed: %v", err)
	}

	err := runWithIO([]string{
		"service", "install",
		"--force",
		"--non-interactive",
		"--config", configPath,
		"--feishu-app-id", "cli_app_id",
		"--feishu-app-secret", "secret",
		"--default-cwd", dir,
		"--codex-chats-root", filepath.Join(dir, "Codex"),
		"--codex-bin", binary,
		"--ctr-go-bin", binary,
		"--notify-new-run", "true",
	}, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("service install failed: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`CTR_GO_HOME="/custom/home"`,
		`CTR_GO_EXTRA_FLAG="yes"`,
		`CTR_GO_FEISHU_APP_ID="cli_app_id"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing preserved value %q:\n%s", want, text)
		}
	}
}

func TestServiceInstallCapturesRuntimeProxyEnvInConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:18080")
	t.Setenv("NODE_USE_ENV_PROXY", "1")
	binary := os.Args[0]

	err := runWithIO([]string{
		"service", "install",
		"--non-interactive",
		"--config", configPath,
		"--feishu-app-id", "cli_app_id",
		"--feishu-app-secret", "secret",
		"--default-cwd", dir,
		"--codex-chats-root", filepath.Join(dir, "Codex"),
		"--codex-bin", binary,
		"--ctr-go-bin", binary,
		"--notify-new-run", "true",
	}, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("service install failed: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`HTTPS_PROXY="http://127.0.0.1:18080"`,
		`NODE_USE_ENV_PROXY="1"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing runtime env %q:\n%s", want, text)
		}
	}
}

func TestServiceInstallInteractiveWizardRetriesInvalidValues(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	binary := os.Args[0]
	secret := "feishu-secret-value"
	input := strings.Join([]string{
		"",
		"cli_app_id",
		secret,
		"ou_1",
		"oc_chat",
		"",
		dir,
		filepath.Join(dir, "Codex"),
		binary,
		"maybe",
		"true",
		"true",
		"true",
		"",
	}, "\n")
	var out bytes.Buffer

	err := runWithIO([]string{
		"service", "install",
		"--config", configPath,
		"--ctr-go-bin", binary,
	}, strings.NewReader(input), &out)
	if err != nil {
		t.Fatalf("service install wizard failed: %v", err)
	}
	got := out.String()
	if strings.Contains(got, secret) {
		t.Fatalf("wizard leaked secret:\n%s", got)
	}
	for _, want := range []string{
		"codex-feishu service setup",
		"[1/10] Feishu app id",
		"[2/10] Feishu app secret",
		"value must be true or false",
		"Next steps",
		"/start",
		"bot DM",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("wizard output missing %q:\n%s", want, got)
		}
	}
}

func TestServiceInstallNonInteractiveReportsMissingFlags(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CTR_GO_HOME", filepath.Join(dir, "home"))
	t.Setenv("HOME", dir)

	err := runWithIO([]string{"service", "install", "--non-interactive", "--config", filepath.Join(dir, "config.env")}, strings.NewReader(""), io.Discard)
	if err == nil {
		t.Fatal("service install succeeded, want missing values error")
	}
	for _, want := range []string{"--feishu-app-id", "--feishu-app-secret"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestRenderLaunchAgentPlistContainsOnlyConfigEnvironment(t *testing.T) {
	plist, err := renderLaunchAgentPlist(launchAgentConfig{
		Label:      serviceLabel,
		BinaryPath: "/usr/local/bin/ctr-go",
		ConfigPath: "/Users/you/.codex-feishu/config.env",
		WorkingDir: "/Users/you/project",
		StdoutPath: "/Users/you/.codex-feishu/logs/daemon.out.log",
		StderrPath: "/Users/you/.codex-feishu/logs/daemon.err.log",
		KeepAlive:  true,
		RunAtLoad:  true,
	})
	if err != nil {
		t.Fatalf("renderLaunchAgentPlist failed: %v", err)
	}
	text := string(plist)
	for _, want := range []string{"CTR_GO_CONFIG", "daemon", "run", "KeepAlive", "RunAtLoad"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plist missing %q:\n%s", want, text)
		}
	}
}

func TestServiceLifecycleUsesLaunchctlRunner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CTR_GO_HOME", filepath.Join(dir, "home"))
	t.Setenv("HOME", dir)
	paths, err := defaultServicePaths(filepath.Join(dir, "config.env"))
	if err != nil {
		t.Fatalf("defaultServicePaths failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ServicePlistPath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(paths.ServicePlistPath, []byte("plist"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	fake := &fakeServiceRunner{}
	oldRunner, oldGOOS, oldUID := serviceRunner, serviceGOOS, serviceUID
	serviceRunner = fake
	serviceGOOS = "darwin"
	serviceUID = func() int { return 501 }
	defer func() {
		serviceRunner = oldRunner
		serviceGOOS = oldGOOS
		serviceUID = oldUID
	}()

	var out bytes.Buffer
	if err := runServiceStart(&out); err != nil {
		t.Fatalf("service start failed: %v", err)
	}
	if err := runServiceRestart(&out); err != nil {
		t.Fatalf("service restart failed: %v", err)
	}
	if err := runServiceStatus(&out); err != nil {
		t.Fatalf("service status failed: %v", err)
	}
	joined := strings.Join(fake.calls, "\n")
	for _, want := range []string{
		"launchctl bootstrap gui/501 " + paths.ServicePlistPath,
		"launchctl kickstart -k gui/501/" + serviceLabel,
		"launchctl bootout gui/501/" + serviceLabel,
		"launchctl print gui/501/" + serviceLabel,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing launchctl call %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(out.String(), "Restarting can interrupt") {
		t.Fatalf("restart warning missing:\n%s", out.String())
	}
}

func TestServiceStartAcceptsKickstartFailureWhenServiceLoaded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CTR_GO_HOME", filepath.Join(dir, "home"))
	t.Setenv("HOME", dir)
	paths, err := defaultServicePaths(filepath.Join(dir, "config.env"))
	if err != nil {
		t.Fatalf("defaultServicePaths failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ServicePlistPath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(paths.ServicePlistPath, []byte("plist"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	fake := &fakeServiceRunner{fail: map[string]error{"kickstart": errors.New("transient kickstart failure")}}
	oldRunner, oldGOOS, oldUID := serviceRunner, serviceGOOS, serviceUID
	serviceRunner = fake
	serviceGOOS = "darwin"
	serviceUID = func() int { return 501 }
	defer func() {
		serviceRunner = oldRunner
		serviceGOOS = oldGOOS
		serviceUID = oldUID
	}()

	var out bytes.Buffer
	if err := runServiceStart(&out); err != nil {
		t.Fatalf("service start failed: %v", err)
	}
	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, "launchctl print gui/501/"+serviceLabel) {
		t.Fatalf("start did not re-check loaded service after kickstart failure:\n%s", joined)
	}
}

type fakeServiceRunner struct {
	calls  []string
	fail   map[string]error
	loaded bool
}

func (f *fakeServiceRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if len(args) > 0 && f.fail != nil {
		if err := f.fail[args[0]]; err != nil {
			return nil, err
		}
	}
	if len(args) > 0 {
		switch args[0] {
		case "bootstrap":
			f.loaded = true
		case "bootout":
			f.loaded = false
		case "print":
			if f.loaded {
				return []byte("ok"), nil
			}
			return nil, errors.New("not loaded")
		}
	}
	if name == "fail" {
		return nil, errors.New("failed")
	}
	return []byte("ok"), nil
}
