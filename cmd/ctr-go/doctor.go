package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	"github.com/mideco-tech/codex-tg/internal/appserver"
	"github.com/mideco-tech/codex-tg/internal/config"
)

const (
	healthPass = "pass"
	healthWarn = "warn"
	healthFail = "fail"
	healthSkip = "skip"
)

type healthCheck struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Message     string         `json:"message"`
	Remediation string         `json:"remediation,omitempty"`
	Details     map[string]any `json:"details,omitempty"`
}

type healthReport struct {
	OK          bool          `json:"ok"`
	Summary     healthSummary `json:"summary"`
	Checks      []healthCheck `json:"checks"`
	NextActions []string      `json:"next_actions,omitempty"`
}

type healthSummary struct {
	Pass int `json:"pass"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

func runHealthChecks(ctx context.Context, cfg config.Config, daemonState map[string]string) healthReport {
	runner := healthRunner{cfg: cfg, daemonState: daemonState}
	return runner.run(ctx)
}

type healthRunner struct {
	cfg         config.Config
	daemonState map[string]string
	checks      []healthCheck
}

func (r *healthRunner) run(ctx context.Context) healthReport {
	r.checkConfig()
	codexUsable := r.checkCodexBinary()
	if codexUsable {
		r.checkCodexAppServer(ctx)
	} else {
		r.add(healthCheck{
			ID:          "codex.app_server",
			Name:        "Codex app-server",
			Status:      healthSkip,
			Message:     "Skipped because the Codex binary is not usable.",
			Remediation: "Install Codex CLI and set CTR_GO_CODEX_BIN to the absolute binary path.",
		})
	}
	r.checkFeishuCredentials(ctx)
	r.checkPersistedDaemonState()
	return r.report()
}

func (r *healthRunner) checkConfig() {
	if strings.TrimSpace(r.cfg.FeishuAppID) == "" || strings.TrimSpace(r.cfg.FeishuAppSecret) == "" {
		r.add(healthCheck{
			ID:          "config.feishu_credentials",
			Name:        "Feishu credentials",
			Status:      healthFail,
			Message:     "Feishu app id or app secret is missing.",
			Remediation: "Run ctr-go feishu setup, or set CTR_GO_FEISHU_APP_ID and CTR_GO_FEISHU_APP_SECRET.",
		})
	} else {
		r.add(healthCheck{
			ID:      "config.feishu_credentials",
			Name:    "Feishu credentials",
			Status:  healthPass,
			Message: "Feishu app id and secret are configured.",
			Details: map[string]any{"app_id": safeConfigValue(r.cfg.FeishuAppID)},
		})
	}

	r.checkDirectory("config.default_cwd", "Default cwd", r.cfg.DefaultCWD, true, "Set CTR_GO_DEFAULT_CWD to an existing directory.")
	r.checkDirectory("config.codex_chats_root", "Codex Chats root", r.cfg.CodexChatsRoot, false, "Create the directory or set CTR_GO_CODEX_CHATS_ROOT to a writable location.")
}

func (r *healthRunner) checkDirectory(id, name, path string, required bool, remediation string) {
	path = strings.TrimSpace(path)
	if path == "" {
		status := healthWarn
		if required {
			status = healthFail
		}
		r.add(healthCheck{ID: id, Name: name, Status: status, Message: name + " is empty.", Remediation: remediation})
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		status := healthWarn
		if required {
			status = healthFail
		}
		r.add(healthCheck{
			ID:          id,
			Name:        name,
			Status:      status,
			Message:     fmt.Sprintf("%s is not accessible: %v", name, err),
			Remediation: remediation,
			Details:     map[string]any{"path": path},
		})
		return
	}
	if !info.IsDir() {
		r.add(healthCheck{
			ID:          id,
			Name:        name,
			Status:      healthFail,
			Message:     name + " exists but is not a directory.",
			Remediation: remediation,
			Details:     map[string]any{"path": path},
		})
		return
	}
	r.add(healthCheck{ID: id, Name: name, Status: healthPass, Message: name + " is accessible.", Details: map[string]any{"path": path}})
}

func (r *healthRunner) checkCodexBinary() bool {
	bin := strings.TrimSpace(r.cfg.CodexBin)
	if bin == "" {
		r.add(healthCheck{
			ID:          "codex.binary",
			Name:        "Codex binary",
			Status:      healthFail,
			Message:     "CTR_GO_CODEX_BIN is empty.",
			Remediation: "Install Codex CLI and set CTR_GO_CODEX_BIN to the absolute binary path.",
		})
		return false
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		r.add(healthCheck{
			ID:          "codex.binary",
			Name:        "Codex binary",
			Status:      healthFail,
			Message:     fmt.Sprintf("Could not find Codex binary %q: %v", bin, err),
			Remediation: "Install Codex CLI, or set CTR_GO_CODEX_BIN to an absolute path visible to the service.",
			Details:     map[string]any{"configured": bin},
		})
		return false
	}
	r.add(healthCheck{
		ID:      "codex.binary",
		Name:    "Codex binary",
		Status:  healthPass,
		Message: "Codex binary is resolvable.",
		Details: map[string]any{"configured": bin, "resolved": resolved},
	})
	return true
}

func (r *healthRunner) checkCodexAppServer(ctx context.Context) {
	timeout := r.cfg.RequestTimeout
	if timeout <= 0 || timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := appserver.NewClient(r.cfg.CodexBin, r.cfg.AppServerListen, r.cfg.DefaultCWD, timeout)
	started := time.Now()
	err := client.Start(checkCtx)
	if err != nil {
		r.add(healthCheck{
			ID:          "codex.app_server",
			Name:        "Codex app-server",
			Status:      healthFail,
			Message:     fmt.Sprintf("Could not start and initialize Codex app-server: %v", err),
			Remediation: "Run `codex app-server` manually in the same user account. If it fails, update/login Codex CLI and check proxy settings.",
			Details: map[string]any{
				"listen":      r.cfg.AppServerListen,
				"duration_ms": time.Since(started).Milliseconds(),
				"stderr_tail": client.StderrTail(),
			},
		})
		_ = client.Close()
		return
	}
	defer client.Close()
	r.add(healthCheck{
		ID:      "codex.app_server",
		Name:    "Codex app-server",
		Status:  healthPass,
		Message: "Codex app-server started and initialized.",
		Details: map[string]any{"listen": r.cfg.AppServerListen, "duration_ms": time.Since(started).Milliseconds()},
	})

	started = time.Now()
	if _, err := client.ThreadList(checkCtx, 1, ""); err != nil {
		r.add(healthCheck{
			ID:          "codex.thread_list",
			Name:        "Codex thread/list",
			Status:      healthFail,
			Message:     fmt.Sprintf("Codex app-server did not accept thread/list: %v", err),
			Remediation: "Update Codex CLI. If Codex is not logged in, login locally and rerun ctr-go doctor.",
			Details:     map[string]any{"duration_ms": time.Since(started).Milliseconds(), "stderr_tail": client.StderrTail()},
		})
		return
	}
	r.add(healthCheck{
		ID:      "codex.thread_list",
		Name:    "Codex thread/list",
		Status:  healthPass,
		Message: "Codex app-server accepted thread/list.",
		Details: map[string]any{"duration_ms": time.Since(started).Milliseconds()},
	})

	started = time.Now()
	models, err := client.ModelList(checkCtx, false)
	if err != nil {
		r.add(healthCheck{
			ID:          "codex.model_list",
			Name:        "Codex model/list",
			Status:      healthWarn,
			Message:     fmt.Sprintf("Codex model/list failed: %v", err),
			Remediation: "Open Codex locally, confirm the account is logged in, and choose a default model with /setting if needed.",
			Details:     map[string]any{"duration_ms": time.Since(started).Milliseconds(), "stderr_tail": client.StderrTail()},
		})
		return
	}
	r.add(healthCheck{
		ID:      "codex.model_list",
		Name:    "Codex model/list",
		Status:  healthPass,
		Message: fmt.Sprintf("Codex returned %d visible model(s).", len(models)),
		Details: map[string]any{"duration_ms": time.Since(started).Milliseconds(), "count": len(models)},
	})
}

func (r *healthRunner) checkFeishuCredentials(ctx context.Context) {
	if strings.TrimSpace(r.cfg.FeishuAppID) == "" || strings.TrimSpace(r.cfg.FeishuAppSecret) == "" {
		r.add(healthCheck{
			ID:          "feishu.tenant_access_token",
			Name:        "Feishu tenant token",
			Status:      healthSkip,
			Message:     "Skipped because Feishu credentials are missing.",
			Remediation: "Run ctr-go feishu setup.",
		})
		return
	}
	timeout := r.cfg.RequestTimeout
	if timeout <= 0 || timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	client := lark.NewClient(r.cfg.FeishuAppID, r.cfg.FeishuAppSecret)
	resp, err := client.GetTenantAccessTokenBySelfBuiltApp(checkCtx, &larkcore.SelfBuiltTenantAccessTokenReq{
		AppID:     r.cfg.FeishuAppID,
		AppSecret: r.cfg.FeishuAppSecret,
	})
	if err != nil {
		r.add(healthCheck{
			ID:          "feishu.tenant_access_token",
			Name:        "Feishu tenant token",
			Status:      healthFail,
			Message:     fmt.Sprintf("Could not request Feishu tenant token: %v", err),
			Remediation: "Check network/proxy settings and verify the Feishu app id/secret.",
			Details:     map[string]any{"duration_ms": time.Since(started).Milliseconds()},
		})
		return
	}
	if resp == nil || !resp.Success() || strings.TrimSpace(resp.TenantAccessToken) == "" {
		code := 0
		message := ""
		if resp != nil {
			code = resp.Code
			message = resp.Msg
		}
		r.add(healthCheck{
			ID:          "feishu.tenant_access_token",
			Name:        "Feishu tenant token",
			Status:      healthFail,
			Message:     "Feishu rejected the configured app credentials.",
			Remediation: "Re-run ctr-go feishu setup or update CTR_GO_FEISHU_APP_ID/CTR_GO_FEISHU_APP_SECRET.",
			Details:     map[string]any{"code": code, "message": message, "duration_ms": time.Since(started).Milliseconds()},
		})
		return
	}
	r.add(healthCheck{
		ID:      "feishu.tenant_access_token",
		Name:    "Feishu tenant token",
		Status:  healthPass,
		Message: "Feishu tenant token request succeeded.",
		Details: map[string]any{"duration_ms": time.Since(started).Milliseconds(), "expires_in": resp.Expire},
	})
}

func (r *healthRunner) checkPersistedDaemonState() {
	if len(r.daemonState) == 0 {
		r.add(healthCheck{
			ID:          "daemon.persisted_state",
			Name:        "Daemon persisted state",
			Status:      healthWarn,
			Message:     "No daemon state is recorded yet. The service may not have started.",
			Remediation: "Run ctr-go service start, then rerun ctr-go doctor.",
		})
		return
	}
	errorKeys := []string{
		"appserver.live.last_error",
		"appserver.poll.last_error",
		"daemon.last_error",
	}
	failures := map[string]string{}
	for _, key := range errorKeys {
		if value := strings.TrimSpace(r.daemonState[key]); value != "" {
			failures[key] = value
		}
	}
	if len(failures) > 0 {
		r.add(healthCheck{
			ID:          "daemon.persisted_state",
			Name:        "Daemon persisted state",
			Status:      healthWarn,
			Message:     "Persisted daemon state contains recent errors.",
			Remediation: "Inspect ctr-go status and service logs after rerunning the failing action.",
			Details:     map[string]any{"errors": failures},
		})
		return
	}
	r.add(healthCheck{
		ID:      "daemon.persisted_state",
		Name:    "Daemon persisted state",
		Status:  healthPass,
		Message: "No recent persisted daemon errors were found.",
	})
}

func (r *healthRunner) add(check healthCheck) {
	check.Status = normalizeHealthStatus(check.Status)
	r.checks = append(r.checks, check)
}

func (r *healthRunner) report() healthReport {
	report := healthReport{OK: true, Checks: append([]healthCheck(nil), r.checks...)}
	for _, check := range report.Checks {
		switch check.Status {
		case healthPass:
			report.Summary.Pass++
		case healthWarn:
			report.Summary.Warn++
		case healthFail:
			report.Summary.Fail++
			report.OK = false
			if check.Remediation != "" {
				report.NextActions = append(report.NextActions, check.Remediation)
			}
		case healthSkip:
			report.Summary.Skip++
		}
	}
	if runtime.GOOS == "darwin" && report.Summary.Fail > 0 {
		report.NextActions = appendUnique(report.NextActions, "If this only fails as a service, confirm LaunchAgent has the same Codex binary path, proxy variables, and login state as your terminal.")
	}
	return report
}

func normalizeHealthStatus(status string) string {
	switch strings.TrimSpace(strings.ToLower(status)) {
	case healthPass, healthWarn, healthFail, healthSkip:
		return strings.TrimSpace(strings.ToLower(status))
	default:
		return healthWarn
	}
}

func safeConfigValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
