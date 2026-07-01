package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/mideco-tech/codex-tg/internal/config"
)

const serviceLabel = "tech.mideco.codex-tg"

var (
	serviceRunner     serviceCommandRunner = execServiceRunner{}
	serviceUID                             = os.Getuid
	serviceExecutable                      = os.Executable
	serviceGOOS                            = runtime.GOOS
)

type serviceCommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execServiceRunner struct{}

func (execServiceRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func runService(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		printServiceUsage(out)
		return nil
	}
	switch args[0] {
	case "install":
		return runServiceInstall(args[1:], in, out)
	case "start":
		return runServiceStart(out)
	case "stop":
		return runServiceStop(out)
	case "restart":
		return runServiceRestart(out)
	case "status":
		return runServiceStatus(out)
	case "enable-login":
		return runServiceEnableLogin(out)
	case "disable-login":
		return runServiceDisableLogin(out)
	case "uninstall":
		return runServiceUninstall(args[1:], out)
	case "help", "--help", "-h":
		printServiceUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown service command: %s", strings.Join(args, " "))
	}
}

type serviceInstallOptions struct {
	Force            bool
	Start            bool
	StartAtLogin     bool
	NonInteractive   bool
	ConfigPath       string
	Adapter          string
	FeishuAppID      string
	FeishuAppSecret  string
	FeishuOpenIDs    string
	FeishuChatIDs    string
	DefaultCWD       string
	CodexChatsRoot   string
	CodexBin         string
	NotifyNewRun     string
	NotifySystem     string
	OpenCodexDesktop string
	CTRGoBinaryPath  string
}

func parseServiceInstallOptions(args []string) (serviceInstallOptions, error) {
	opts := serviceInstallOptions{
		ConfigPath:   config.ConfigFilePath(),
		NotifyNewRun: "",
	}
	if exe, err := serviceExecutable(); err == nil {
		opts.CTRGoBinaryPath = exe
	}
	fs := flag.NewFlagSet("ctr-go service install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.Force, "force", false, "overwrite existing config/service files")
	fs.BoolVar(&opts.Start, "start", false, "start service after install")
	fs.BoolVar(&opts.StartAtLogin, "start-at-login", false, "enable user LaunchAgent at login")
	fs.BoolVar(&opts.NonInteractive, "non-interactive", false, "fail instead of prompting for missing values")
	fs.StringVar(&opts.ConfigPath, "config", opts.ConfigPath, "config.env path")
	fs.StringVar(&opts.Adapter, "adapter", "", "adapter: feishu")
	fs.StringVar(&opts.FeishuAppID, "feishu-app-id", "", "Feishu app id")
	fs.StringVar(&opts.FeishuAppSecret, "feishu-app-secret", "", "Feishu app secret")
	fs.StringVar(&opts.FeishuOpenIDs, "feishu-allowed-open-ids", "", "allowed Feishu open ids")
	fs.StringVar(&opts.FeishuChatIDs, "feishu-allowed-chat-ids", "", "allowed Feishu chat ids")
	fs.StringVar(&opts.DefaultCWD, "default-cwd", "", "default Codex working directory")
	fs.StringVar(&opts.CodexChatsRoot, "codex-chats-root", "", "Codex UI Chats root")
	fs.StringVar(&opts.CodexBin, "codex-bin", "", "Codex binary path")
	fs.StringVar(&opts.NotifyNewRun, "notify-new-run", "", "notify on New run")
	fs.StringVar(&opts.NotifySystem, "notify-system", "", "send macOS system notifications for completion, failure, and approval")
	fs.StringVar(&opts.OpenCodexDesktop, "open-codex-desktop", "", "open Codex Desktop to the target thread after Feishu input")
	fs.StringVar(&opts.CTRGoBinaryPath, "ctr-go-bin", opts.CTRGoBinaryPath, "ctr-go binary path for LaunchAgent")
	if err := fs.Parse(args); err != nil {
		return opts, fmt.Errorf("usage: ctr-go service install [flags]")
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("usage: ctr-go service install [flags]")
	}
	return opts, nil
}

func runServiceInstall(args []string, in io.Reader, out io.Writer) error {
	opts, err := parseServiceInstallOptions(args)
	if err != nil {
		return err
	}
	opts.ConfigPath = filepath.Clean(opts.ConfigPath)
	existing, err := LoadEnvFileIfExists(opts.ConfigPath)
	if err != nil {
		return err
	}
	if len(existing) > 0 && !opts.Force {
		return fmt.Errorf("%s already exists; rerun with --force to overwrite it", opts.ConfigPath)
	}

	values, err := collectServiceInstallValues(opts, existing, in, out)
	if err != nil {
		return err
	}
	configValues := serviceConfigValues(values)
	if err := writeConfigEnv(opts.ConfigPath, configValues, opts.Force); err != nil {
		return err
	}

	paths, err := defaultServicePaths(opts.ConfigPath)
	if err != nil {
		return err
	}
	plist, err := renderLaunchAgentPlist(launchAgentConfig{
		Label:      serviceLabel,
		BinaryPath: values["CTR_GO_CTR_GO_BIN"],
		ConfigPath: opts.ConfigPath,
		WorkingDir: values["CTR_GO_DEFAULT_CWD"],
		StdoutPath: filepath.Join(paths.LogDir, "daemon.out.log"),
		StderrPath: filepath.Join(paths.LogDir, "daemon.err.log"),
		KeepAlive:  true,
		RunAtLoad:  true,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.ServiceDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.LogDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(paths.ServicePlistPath, plist, 0o644); err != nil {
		return err
	}
	if opts.StartAtLogin {
		if err := copyFile(paths.ServicePlistPath, paths.LoginPlistPath, 0o644); err != nil {
			return err
		}
	}

	_, _ = fmt.Fprintf(out, "\nWrote config: %s\n", opts.ConfigPath)
	_, _ = fmt.Fprintf(out, "Wrote service: %s\n", paths.ServicePlistPath)
	if opts.StartAtLogin {
		_, _ = fmt.Fprintf(out, "Enabled login item: %s\n", paths.LoginPlistPath)
	}
	_, _ = fmt.Fprintln(out, "\nSetup summary")
	adapter := strings.TrimSpace(values["CTR_GO_ADAPTER"])
	if adapter == "" {
		adapter = "feishu"
	}
	_, _ = fmt.Fprintf(out, "  Adapter: %s\n", adapter)
	_, _ = fmt.Fprintln(out, "  Feishu app credentials: configured")
	if strings.TrimSpace(values["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"]) != "" {
		_, _ = fmt.Fprintf(out, "  Allowed Feishu open ids: %s\n", values["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"])
	} else {
		_, _ = fmt.Fprintln(out, "  Allowed Feishu open ids: any")
	}
	if strings.TrimSpace(values["CTR_GO_FEISHU_ALLOWED_CHAT_IDS"]) != "" {
		_, _ = fmt.Fprintf(out, "  Allowed Feishu chats: %s\n", values["CTR_GO_FEISHU_ALLOWED_CHAT_IDS"])
	} else {
		_, _ = fmt.Fprintln(out, "  Allowed Feishu chats: any")
	}
	_, _ = fmt.Fprintf(out, "  Default cwd: %s\n", values["CTR_GO_DEFAULT_CWD"])
	_, _ = fmt.Fprintf(out, "  Codex Chats root: %s\n", values["CTR_GO_CODEX_CHATS_ROOT"])
	_, _ = fmt.Fprintf(out, "  Codex binary: %s\n", values["CTR_GO_CODEX_BIN"])
	_, _ = fmt.Fprintf(out, "  New run notifications: %s\n", values["CTR_GO_NOTIFY_NEW_RUN"])
	_, _ = fmt.Fprintf(out, "  macOS system notifications: %s\n", values["CTR_GO_NOTIFY_SYSTEM"])
	_, _ = fmt.Fprintf(out, "  Open Codex Desktop on Feishu input: %s\n", values["CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU"])
	_, _ = fmt.Fprintln(out, "\nNext steps")
	_, _ = fmt.Fprintln(out, "  ctr-go service status")
	_, _ = fmt.Fprintln(out, "  ctr-go doctor")
	if !opts.Start {
		_, _ = fmt.Fprintln(out, "  ctr-go service start")
	}
	printFeishuWorkspaceNextSteps(out, "  ")

	if opts.Start {
		_ = serviceStop()
		return runServiceStart(out)
	}
	return nil
}

func serviceConfigValues(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		if key == "CTR_GO_CTR_GO_BIN" {
			continue
		}
		if strings.TrimSpace(value) == "" && optionalServiceConfigKey(key) {
			continue
		}
		out[key] = value
	}
	return out
}

func optionalServiceConfigKey(key string) bool {
	switch key {
	case "CTR_GO_FEISHU_ALLOWED_OPEN_IDS", "CTR_GO_FEISHU_ALLOWED_CHAT_IDS":
		return true
	default:
		return false
	}
}

func normalizeServiceAdapter(value string) string {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "", "auto", "feishu", "lark":
		return "feishu"
	default:
		return ""
	}
}

func collectServiceInstallValues(opts serviceInstallOptions, existing map[string]string, in io.Reader, out io.Writer) (map[string]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	codexBin := "codex"
	if found, err := exec.LookPath("codex"); err == nil && strings.TrimSpace(found) != "" {
		codexBin = found
	}
	values := make(map[string]string, len(existing)+8)
	for key, value := range existing {
		values[key] = value
	}
	values["CTR_GO_ADAPTER"] = normalizeServiceAdapter(firstNonEmpty(opts.Adapter, existing["CTR_GO_ADAPTER"], "feishu"))
	values["CTR_GO_FEISHU_APP_ID"] = strings.TrimSpace(firstNonEmpty(opts.FeishuAppID, existing["CTR_GO_FEISHU_APP_ID"]))
	values["CTR_GO_FEISHU_APP_SECRET"] = strings.TrimSpace(firstNonEmpty(opts.FeishuAppSecret, existing["CTR_GO_FEISHU_APP_SECRET"]))
	values["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"] = strings.TrimSpace(firstNonEmpty(opts.FeishuOpenIDs, existing["CTR_GO_FEISHU_ALLOWED_OPEN_IDS"]))
	values["CTR_GO_FEISHU_ALLOWED_CHAT_IDS"] = strings.TrimSpace(firstNonEmpty(opts.FeishuChatIDs, existing["CTR_GO_FEISHU_ALLOWED_CHAT_IDS"]))
	values["CTR_GO_DEFAULT_CWD"] = strings.TrimSpace(firstNonEmpty(opts.DefaultCWD, existing["CTR_GO_DEFAULT_CWD"], cwd))
	values["CTR_GO_CODEX_CHATS_ROOT"] = strings.TrimSpace(firstNonEmpty(opts.CodexChatsRoot, existing["CTR_GO_CODEX_CHATS_ROOT"], config.DefaultCodexChatsRoot()))
	values["CTR_GO_CODEX_BIN"] = strings.TrimSpace(firstNonEmpty(opts.CodexBin, existing["CTR_GO_CODEX_BIN"], codexBin))
	values["CTR_GO_NOTIFY_NEW_RUN"] = strings.TrimSpace(firstNonEmpty(opts.NotifyNewRun, existing["CTR_GO_NOTIFY_NEW_RUN"], "true"))
	values["CTR_GO_NOTIFY_SYSTEM"] = strings.TrimSpace(firstNonEmpty(opts.NotifySystem, existing["CTR_GO_NOTIFY_SYSTEM"], "false"))
	values["CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU"] = strings.TrimSpace(firstNonEmpty(opts.OpenCodexDesktop, existing["CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU"], "false"))
	values["CTR_GO_CTR_GO_BIN"] = strings.TrimSpace(firstNonEmpty(opts.CTRGoBinaryPath, existing["CTR_GO_CTR_GO_BIN"]))
	for _, key := range config.RuntimeEnvPassthroughKeys() {
		if strings.TrimSpace(values[key]) != "" {
			continue
		}
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			values[key] = value
		}
	}
	if opts.NonInteractive {
		return validateServiceValues(values)
	}
	return runServiceWizard(values, in, out)
}

func runServiceWizard(values map[string]string, in io.Reader, out io.Writer) (map[string]string, error) {
	reader := bufio.NewReader(in)
	_, _ = fmt.Fprintln(out, "codex-tg service setup")
	_, _ = fmt.Fprintln(out, "This wizard creates a private config.env and a user-level macOS service.")
	_, _ = fmt.Fprintln(out, "Secrets are written to the config file only; they are not printed in the summary.")
	values["CTR_GO_ADAPTER"] = "feishu"
	fields := []wizardField{
		{
			Key:      "CTR_GO_FEISHU_APP_ID",
			Label:    "Feishu app id",
			Help:     "Enterprise self-built app id, usually cli_...",
			Required: true,
			Validate: validateNonEmpty,
		},
		{
			Key:      "CTR_GO_FEISHU_APP_SECRET",
			Label:    "Feishu app secret",
			Help:     "Stored only in the private config.env.",
			Required: true,
			Secret:   true,
			Validate: validateNonEmpty,
		},
		{
			Key:   "CTR_GO_FEISHU_ALLOWED_OPEN_IDS",
			Label: "Allowed Feishu open id(s)",
			Help:  "Optional comma-separated Feishu user open_id allowlist.",
		},
		{
			Key:   "CTR_GO_FEISHU_ALLOWED_CHAT_IDS",
			Label: "Allowed Feishu chat id(s)",
			Help:  "Optional comma-separated Feishu chat_id allowlist.",
		},
		wizardField{
			Key:      "CTR_GO_DEFAULT_CWD",
			Label:    "Default Codex work directory",
			Help:     "Used for no-cwd threads and fallback routing.",
			Required: true,
			Validate: validateDirectory,
		},
		wizardField{
			Key:      "CTR_GO_CODEX_CHATS_ROOT",
			Label:    "Codex UI Chats root",
			Help:     "New /new folders are created here, usually ~/Documents/Codex.",
			Required: true,
			Validate: validateNonEmpty,
		},
		wizardField{
			Key:      "CTR_GO_CODEX_BIN",
			Label:    "Codex binary",
			Help:     "Absolute path is best. The detected default is used when available.",
			Required: true,
			Validate: validateExecutableRef,
		},
		wizardField{
			Key:      "CTR_GO_NOTIFY_NEW_RUN",
			Label:    "Notify on New run",
			Help:     "Use true/false. Final and Plan prompts still notify by design.",
			Required: true,
			Validate: validateBoolText,
		},
		wizardField{
			Key:      "CTR_GO_NOTIFY_SYSTEM",
			Label:    "macOS system notifications (removed)",
			Help:     "Kept for config compatibility. Local system notifications are no longer sent.",
			Required: true,
			Validate: validateBoolText,
		},
		wizardField{
			Key:      "CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU",
			Label:    "Open Codex Desktop on Feishu input",
			Help:     "Use true/false. Opens codex://threads/<id> locally after Feishu input.",
			Required: true,
			Validate: validateBoolText,
		},
	}
	numberWizardSteps(fields, 1, len(fields))
	for _, field := range fields {
		current := values[field.Key]
		value, err := promptWizardField(reader, in, out, field, current)
		if err != nil {
			return nil, err
		}
		values[field.Key] = value
	}
	if strings.TrimSpace(values["CTR_GO_CTR_GO_BIN"]) == "" {
		if exe, err := serviceExecutable(); err == nil {
			values["CTR_GO_CTR_GO_BIN"] = exe
		}
	}
	return validateServiceValues(values)
}

func numberWizardSteps(fields []wizardField, start, total int) {
	for i := range fields {
		fields[i].Step = fmt.Sprintf("%d/%d", start+i, total)
	}
}

type wizardField struct {
	Key      string
	Step     string
	Label    string
	Help     string
	Required bool
	Secret   bool
	Validate func(string) error
}

func promptWizardField(reader *bufio.Reader, in io.Reader, out io.Writer, field wizardField, fallback string) (string, error) {
	for {
		_, _ = fmt.Fprintf(out, "\n[%s] %s\n", field.Step, field.Label)
		if field.Help != "" {
			_, _ = fmt.Fprintf(out, "  %s\n", field.Help)
		}
		value, err := readWizardValue(reader, in, out, field, fallback)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(value) == "" && field.Required {
			_, _ = fmt.Fprintln(out, "  Value is required. Please try again.")
			continue
		}
		if field.Validate != nil {
			if err := field.Validate(value); err != nil {
				_, _ = fmt.Fprintf(out, "  %v\n", err)
				continue
			}
		}
		return strings.TrimSpace(value), nil
	}
}

func readWizardValue(reader *bufio.Reader, in io.Reader, out io.Writer, field wizardField, fallback string) (string, error) {
	if fallback == "" || field.Secret {
		_, _ = fmt.Fprintf(out, "  %s: ", field.Label)
	} else {
		_, _ = fmt.Fprintf(out, "  %s [%s]: ", field.Label, fallback)
	}
	if field.Secret {
		if file, ok := in.(*os.File); ok && file == os.Stdin && term.IsTerminal(int(file.Fd())) {
			data, err := term.ReadPassword(int(file.Fd()))
			_, _ = fmt.Fprintln(out)
			if err != nil {
				return "", err
			}
			value := strings.TrimSpace(string(data))
			if value == "" {
				return fallback, nil
			}
			return value, nil
		}
	}
	line, err := reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return fallback, nil
	}
	return value, nil
}

func validateServiceValues(values map[string]string) (map[string]string, error) {
	required := map[string]string{
		"CTR_GO_DEFAULT_CWD":                  "--default-cwd",
		"CTR_GO_CODEX_CHATS_ROOT":             "--codex-chats-root",
		"CTR_GO_CODEX_BIN":                    "--codex-bin",
		"CTR_GO_NOTIFY_NEW_RUN":               "--notify-new-run",
		"CTR_GO_NOTIFY_SYSTEM":                "--notify-system",
		"CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU": "--open-codex-desktop",
		"CTR_GO_CTR_GO_BIN":                   "--ctr-go-bin",
		"CTR_GO_FEISHU_APP_ID":                "--feishu-app-id",
		"CTR_GO_FEISHU_APP_SECRET":            "--feishu-app-secret",
	}
	adapter := normalizeServiceAdapter(values["CTR_GO_ADAPTER"])
	if adapter != "feishu" {
		return nil, errors.New("adapter must be feishu")
	}
	var missing []string
	for key, flagName := range required {
		if strings.TrimSpace(values[key]) == "" {
			missing = append(missing, flagName)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required values: %s", strings.Join(missing, ", "))
	}
	checks := []struct {
		key string
		fn  func(string) error
	}{
		{"CTR_GO_ADAPTER", validateAdapterText},
		{"CTR_GO_FEISHU_APP_ID", validateNonEmpty},
		{"CTR_GO_FEISHU_APP_SECRET", validateNonEmpty},
		{"CTR_GO_DEFAULT_CWD", validateDirectory},
		{"CTR_GO_CODEX_CHATS_ROOT", validateNonEmpty},
		{"CTR_GO_CODEX_BIN", validateExecutableRef},
		{"CTR_GO_NOTIFY_NEW_RUN", validateBoolText},
		{"CTR_GO_NOTIFY_SYSTEM", validateBoolText},
		{"CTR_GO_OPEN_CODEX_DESKTOP_ON_FEISHU", validateBoolText},
	}
	for _, check := range checks {
		if err := check.fn(values[check.key]); err != nil {
			return nil, fmt.Errorf("%s: %w", check.key, err)
		}
	}
	return values, nil
}

func runServiceStart(out io.Writer) error {
	if serviceGOOS != "darwin" {
		return errors.New("ctr-go service is macOS-only in v0.4.0")
	}
	paths, err := defaultServicePaths(config.ConfigFilePath())
	if err != nil {
		return err
	}
	plist := paths.ServicePlistPath
	if _, err := os.Stat(paths.LoginPlistPath); err == nil {
		plist = paths.LoginPlistPath
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	target := launchctlTarget()
	if _, err := serviceRunner.Run(ctx, "launchctl", "bootstrap", target, plist); err != nil {
		if !serviceIsLoaded(ctx, target) {
			return fmt.Errorf("launchctl bootstrap failed: %w", err)
		}
	}
	if _, err := serviceRunner.Run(ctx, "launchctl", "kickstart", "-k", target+"/"+serviceLabel); err != nil {
		if !serviceIsLoaded(ctx, target) {
			return fmt.Errorf("launchctl kickstart failed: %w", err)
		}
	}
	if !waitServiceLoaded(ctx, target, true) {
		return errors.New("launchctl start did not load codex-tg service")
	}
	_, _ = fmt.Fprintln(out, "codex-tg service started.")
	return nil
}

func serviceIsLoaded(ctx context.Context, target string) bool {
	_, err := serviceRunner.Run(ctx, "launchctl", "print", target+"/"+serviceLabel)
	return err == nil
}

func waitServiceLoaded(ctx context.Context, target string, want bool) bool {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if serviceIsLoaded(ctx, target) == want {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func runServiceStop(out io.Writer) error {
	if err := serviceStop(); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "codex-tg service stopped.")
	return nil
}

func serviceStop() error {
	if serviceGOOS != "darwin" {
		return errors.New("ctr-go service is macOS-only in v0.4.0")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	target := launchctlTarget()
	_, _ = serviceRunner.Run(ctx, "launchctl", "bootout", target+"/"+serviceLabel)
	if !waitServiceLoaded(ctx, target, false) {
		return errors.New("launchctl stop did not unload codex-tg service")
	}
	return nil
}

func runServiceRestart(out io.Writer) error {
	_, _ = fmt.Fprintln(out, "Restarting can interrupt an active run.")
	if err := runServiceStop(out); err != nil {
		return err
	}
	return runServiceStart(out)
}

func runServiceStatus(out io.Writer) error {
	if serviceGOOS != "darwin" {
		return errors.New("ctr-go service is macOS-only in v0.4.0")
	}
	paths, err := defaultServicePaths(config.ConfigFilePath())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, printErr := serviceRunner.Run(ctx, "launchctl", "print", launchctlTarget()+"/"+serviceLabel)
	loaded := printErr == nil
	_, _ = fmt.Fprintln(out, "codex-tg service status")
	_, _ = fmt.Fprintf(out, "  Config: %s\n", config.ConfigFilePath())
	_, _ = fmt.Fprintf(out, "  Service plist: %s\n", paths.ServicePlistPath)
	_, _ = fmt.Fprintf(out, "  Start with system: %t\n", fileExists(paths.LoginPlistPath))
	_, _ = fmt.Fprintf(out, "  Loaded: %t\n", loaded)
	return nil
}

func runServiceEnableLogin(out io.Writer) error {
	paths, err := defaultServicePaths(config.ConfigFilePath())
	if err != nil {
		return err
	}
	if !fileExists(paths.ServicePlistPath) {
		return fmt.Errorf("%s does not exist; run ctr-go service install first", paths.ServicePlistPath)
	}
	if err := copyFile(paths.ServicePlistPath, paths.LoginPlistPath, 0o644); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Enabled start with system: %s\n", paths.LoginPlistPath)
	return nil
}

func runServiceDisableLogin(out io.Writer) error {
	paths, err := defaultServicePaths(config.ConfigFilePath())
	if err != nil {
		return err
	}
	if err := os.Remove(paths.LoginPlistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = fmt.Fprintln(out, "Disabled start with system.")
	return nil
}

func runServiceUninstall(args []string, out io.Writer) error {
	keepConfig := false
	fs := flag.NewFlagSet("ctr-go service uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&keepConfig, "keep-config", false, "keep config.env")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("usage: ctr-go service uninstall [--keep-config]")
	}
	if err := serviceStop(); err != nil && serviceGOOS == "darwin" {
		return err
	}
	paths, err := defaultServicePaths(config.ConfigFilePath())
	if err != nil {
		return err
	}
	for _, path := range []string{paths.LoginPlistPath, paths.ServicePlistPath} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if !keepConfig {
		if err := os.Remove(config.ConfigFilePath()); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_, _ = fmt.Fprintln(out, "codex-tg service uninstalled.")
	if keepConfig {
		_, _ = fmt.Fprintln(out, "Config kept.")
	}
	return nil
}

type servicePaths struct {
	Home             string
	ServiceDir       string
	LogDir           string
	ServicePlistPath string
	LoginPlistPath   string
}

func defaultServicePaths(configPath string) (servicePaths, error) {
	home := config.DefaultPaths().Home
	return servicePaths{
		Home:             home,
		ServiceDir:       filepath.Join(home, "service"),
		LogDir:           filepath.Join(home, "logs"),
		ServicePlistPath: filepath.Join(home, "service", serviceLabel+".plist"),
		LoginPlistPath:   filepath.Join(userHomeDir(), "Library", "LaunchAgents", serviceLabel+".plist"),
	}, nil
}

type launchAgentConfig struct {
	Label      string
	BinaryPath string
	ConfigPath string
	WorkingDir string
	StdoutPath string
	StderrPath string
	KeepAlive  bool
	RunAtLoad  bool
}

func renderLaunchAgentPlist(cfg launchAgentConfig) ([]byte, error) {
	if strings.TrimSpace(cfg.Label) == "" || strings.TrimSpace(cfg.BinaryPath) == "" || strings.TrimSpace(cfg.ConfigPath) == "" {
		return nil, errors.New("launch agent label, binary path, and config path are required")
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	plistString(&b, "Label", cfg.Label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range []string{cfg.BinaryPath, "daemon", "run"} {
		b.WriteString("    <string>" + xmlEscape(arg) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
	plistString(&b, "CTR_GO_CONFIG", cfg.ConfigPath)
	b.WriteString("  </dict>\n")
	if strings.TrimSpace(cfg.WorkingDir) != "" {
		plistString(&b, "WorkingDirectory", cfg.WorkingDir)
	}
	plistString(&b, "StandardOutPath", cfg.StdoutPath)
	plistString(&b, "StandardErrorPath", cfg.StderrPath)
	plistBool(&b, "RunAtLoad", cfg.RunAtLoad)
	plistBool(&b, "KeepAlive", cfg.KeepAlive)
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String()), nil
}

func plistString(b *strings.Builder, key, value string) {
	b.WriteString("  <key>" + xmlEscape(key) + "</key>\n")
	b.WriteString("  <string>" + xmlEscape(value) + "</string>\n")
}

func plistBool(b *strings.Builder, key string, value bool) {
	b.WriteString("  <key>" + xmlEscape(key) + "</key>\n")
	if value {
		b.WriteString("  <true/>\n")
	} else {
		b.WriteString("  <false/>\n")
	}
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}

func launchctlTarget() string {
	return fmt.Sprintf("gui/%d", serviceUID())
}

func LoadEnvFileIfExists(path string) (map[string]string, error) {
	values, err := config.LoadEnvFile(path)
	if err != nil {
		return nil, err
	}
	if values == nil {
		return map[string]string{}, nil
	}
	return values, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "."
	}
	return home
}

func validateNonEmpty(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("value is required")
	}
	return nil
}

func validateAdapterText(value string) error {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "feishu":
		return nil
	default:
		return errors.New("adapter must be feishu")
	}
	return nil
}

func validateDirectory(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("directory is required")
	}
	info, err := os.Stat(value)
	if err != nil {
		return fmt.Errorf("directory must exist: %w", err)
	}
	if !info.IsDir() {
		return errors.New("path must be a directory")
	}
	return nil
}

func validateExecutableRef(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("binary path is required")
	}
	if strings.ContainsRune(value, os.PathSeparator) {
		info, err := os.Stat(value)
		if err != nil {
			return fmt.Errorf("binary path must exist: %w", err)
		}
		if info.IsDir() {
			return errors.New("binary path must not be a directory")
		}
		return nil
	}
	if _, err := exec.LookPath(value); err != nil {
		return fmt.Errorf("binary %q was not found in PATH", value)
	}
	return nil
}

func validateBoolText(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "false", "on", "off", "yes", "no", "1", "0":
		return nil
	default:
		return errors.New("value must be true or false")
	}
}

func printServiceUsage(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Usage:")
	_, _ = fmt.Fprintln(out, "  ctr-go service install [--force] [--start] [--start-at-login] [flags]")
	_, _ = fmt.Fprintln(out, "  ctr-go service start")
	_, _ = fmt.Fprintln(out, "  ctr-go service stop")
	_, _ = fmt.Fprintln(out, "  ctr-go service restart")
	_, _ = fmt.Fprintln(out, "  ctr-go service status")
	_, _ = fmt.Fprintln(out, "  ctr-go service enable-login")
	_, _ = fmt.Fprintln(out, "  ctr-go service disable-login")
	_, _ = fmt.Fprintln(out, "  ctr-go service uninstall [--keep-config]")
}
