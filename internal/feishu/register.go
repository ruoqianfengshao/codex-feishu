package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultRegistrationDomain     = "accounts.feishu.cn"
	defaultRegistrationLarkDomain = "accounts.larksuite.com"
	registrationEndpoint          = "/oauth/v1/app/registration"
	registrationSDKName           = "codex-feishu-go"
	registrationDefaultAppName    = "Codex"
)

type RegistrationClient struct {
	HTTPClient *http.Client
	Domain     string
	LarkDomain string
	Source     string
	wait       func(context.Context, time.Duration) error
}

type RegistrationQRCode struct {
	URL      string
	ExpireIn time.Duration
}

type RegistrationStatus struct {
	Status   string
	Interval time.Duration
}

type RegistrationResult struct {
	ClientID     string
	ClientSecret string
	UserOpenID   string
	TenantBrand  string
}

type RegistrationOptions struct {
	OnQRCodeReady  func(RegistrationQRCode)
	OnStatusChange func(RegistrationStatus)
}

type registrationResponse struct {
	DeviceCode              string                 `json:"device_code"`
	VerificationURIComplete string                 `json:"verification_uri_complete"`
	ExpiresIn               int                    `json:"expires_in"`
	Interval                int                    `json:"interval"`
	ClientID                string                 `json:"client_id"`
	ClientSecret            string                 `json:"client_secret"`
	UserInfo                map[string]interface{} `json:"user_info"`
	Error                   string                 `json:"error"`
	ErrorDescription        string                 `json:"error_description"`
}

func (c RegistrationClient) Register(ctx context.Context, opts RegistrationOptions) (RegistrationResult, error) {
	beginBaseURL, err := registrationBaseURL(c.Domain, defaultRegistrationDomain)
	if err != nil {
		return RegistrationResult{}, err
	}
	larkBaseURL, err := registrationBaseURL(c.LarkDomain, defaultRegistrationLarkDomain)
	if err != nil {
		return RegistrationResult{}, err
	}
	begin, err := c.request(ctx, beginBaseURL, url.Values{
		"action":            {"begin"},
		"archetype":         {"PersonalAgent"},
		"auth_method":       {"client_secret"},
		"request_user_info": {"open_id"},
	})
	if err != nil {
		return RegistrationResult{}, err
	}
	if strings.TrimSpace(begin.DeviceCode) == "" || strings.TrimSpace(begin.VerificationURIComplete) == "" {
		return RegistrationResult{}, errors.New("feishu registration did not return a device code")
	}
	qrURL, err := registrationQRCodeURL(begin.VerificationURIComplete, c.Source)
	if err != nil {
		return RegistrationResult{}, err
	}
	expiresIn := time.Duration(positive(begin.ExpiresIn, 600)) * time.Second
	interval := time.Duration(positive(begin.Interval, 5)) * time.Second
	if opts.OnQRCodeReady != nil {
		opts.OnQRCodeReady(RegistrationQRCode{URL: qrURL, ExpireIn: expiresIn})
	}
	return c.poll(ctx, pollState{
		baseURL:        beginBaseURL,
		larkBaseURL:    larkBaseURL,
		deviceCode:     begin.DeviceCode,
		expiresIn:      expiresIn,
		interval:       interval,
		onStatusChange: opts.OnStatusChange,
	})
}

type pollState struct {
	baseURL        string
	larkBaseURL    string
	deviceCode     string
	expiresIn      time.Duration
	interval       time.Duration
	onStatusChange func(RegistrationStatus)
}

func (c RegistrationClient) poll(ctx context.Context, state pollState) (RegistrationResult, error) {
	ctx, cancel := context.WithTimeout(ctx, state.expiresIn)
	defer cancel()

	baseURL := state.baseURL
	interval := state.interval
	domainSwitched := false
	for {
		response, err := c.request(ctx, baseURL, url.Values{
			"action":      {"poll"},
			"device_code": {state.deviceCode},
		})
		if err != nil {
			return RegistrationResult{}, err
		}
		if tenantBrand(response.UserInfo) == "lark" && !domainSwitched {
			baseURL = state.larkBaseURL
			domainSwitched = true
			notifyRegistrationStatus(state.onStatusChange, "domain_switched", 0)
			continue
		}
		if strings.TrimSpace(response.ClientID) != "" && strings.TrimSpace(response.ClientSecret) != "" {
			return RegistrationResult{
				ClientID:     strings.TrimSpace(response.ClientID),
				ClientSecret: strings.TrimSpace(response.ClientSecret),
				UserOpenID:   openID(response.UserInfo),
				TenantBrand:  tenantBrand(response.UserInfo),
			}, nil
		}
		switch strings.TrimSpace(response.Error) {
		case "", "authorization_pending":
			notifyRegistrationStatus(state.onStatusChange, "polling", 0)
		case "slow_down":
			interval += 5 * time.Second
			notifyRegistrationStatus(state.onStatusChange, "slow_down", interval)
		case "access_denied", "expired_token":
			return RegistrationResult{}, registrationError(response)
		default:
			return RegistrationResult{}, registrationError(response)
		}
		if err := c.waitForPoll(ctx, interval); err != nil {
			return RegistrationResult{}, err
		}
	}
}

func (c RegistrationClient) waitForPoll(ctx context.Context, interval time.Duration) error {
	if c.wait != nil {
		return c.wait(ctx, interval)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c RegistrationClient) request(ctx context.Context, baseURL string, values url.Values) (registrationResponse, error) {
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+registrationEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return registrationResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return registrationResponse{}, err
	}
	defer resp.Body.Close()

	var payload registrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return registrationResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if payload.Error != "" {
			return payload, nil
		}
		return registrationResponse{}, fmt.Errorf("feishu registration http %d", resp.StatusCode)
	}
	return payload, nil
}

func registrationBaseURL(domain, fallback string) (string, error) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		domain = fallback
	}
	if strings.HasPrefix(domain, "http://") || strings.HasPrefix(domain, "https://") {
		parsed, err := url.Parse(domain)
		if err != nil {
			return "", err
		}
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return strings.TrimRight(parsed.String(), "/"), nil
	}
	return "https://" + strings.TrimRight(domain, "/"), nil
}

func registrationQRCodeURL(rawURL, source string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("from", "sdk")
	if strings.TrimSpace(source) == "" {
		query.Set("source", registrationSDKName)
	} else {
		query.Set("source", registrationSDKName+"/"+strings.TrimSpace(source))
	}
	query.Set("tp", "sdk")
	if strings.TrimSpace(query.Get("name")) == "" {
		query.Set("name", registrationDefaultAppName)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func notifyRegistrationStatus(fn func(RegistrationStatus), status string, interval time.Duration) {
	if fn != nil {
		fn(RegistrationStatus{Status: status, Interval: interval})
	}
}

func registrationError(response registrationResponse) error {
	code := strings.TrimSpace(response.Error)
	if code == "" {
		code = "unknown_error"
	}
	description := strings.TrimSpace(response.ErrorDescription)
	if description == "" {
		description = "unknown error"
	}
	return fmt.Errorf("feishu registration %s: %s", code, description)
}

func tenantBrand(info map[string]interface{}) string {
	value, _ := info["tenant_brand"].(string)
	return strings.TrimSpace(value)
}

func openID(info map[string]interface{}) string {
	value, _ := info["open_id"].(string)
	return strings.TrimSpace(value)
}

func positive(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}
