package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type storedAuth struct {
	Auth    storedTokens  `json:"auth"`
	Account storedAccount `json:"account"`
}

type storedTokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	TokenType    string `json:"tokenType,omitempty"`
}

type storedAccount struct {
	ID          string `json:"id"`
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Plan        string `json:"plan,omitempty"`
	PlanStatus  string `json:"planStatus,omitempty"`
	Balance     int64  `json:"balance,omitempty"`
	Currency    string `json:"currency,omitempty"`
	CreditsAt   string `json:"creditsAt,omitempty"`
}

type clineMeResponse struct {
	Success bool `json:"success"`
	Data    struct {
		ID          string `json:"id"`
		Email       string `json:"email"`
		DisplayName string `json:"displayName"`
	} `json:"data"`
}

type clinePlanResponse struct {
	Success bool `json:"success"`
	Data    *struct {
		Plan *struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			Interval    string `json:"interval"`
			Type        string `json:"type"`
		} `json:"plan"`
		SubscriptionID   string `json:"subscriptionId"`
		CurrentPeriodEnd string `json:"currentPeriodEnd"`
	} `json:"data"`
}

type clineBalanceResponse struct {
	Success bool `json:"success"`
	Data    struct {
		UserID  string `json:"userId"`
		Balance int64  `json:"balance"`
	} `json:"data"`
}

type loginState struct {
	DeviceCode   string
	UserCode     string
	Verification string
	Interval     time.Duration
	Expires      time.Time
	Provider     string
	StartedAt    time.Time
}

type clineAuthResponse struct {
	Success bool          `json:"success"`
	Data    clineAuthData `json:"data"`
}

type clineAuthData struct {
	AccessToken  string        `json:"accessToken"`
	RefreshToken string        `json:"refreshToken"`
	TokenType    string        `json:"tokenType"`
	ExpiresAt    string        `json:"expiresAt"`
	UserInfo     clineUserInfo `json:"userInfo"`
}

type clineUserInfo struct {
	Subject  string `json:"subject"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	ClineUID string `json:"clineUserId"`
}

type workOSDeviceResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	Error                   string `json:"error"`
	ErrorDescription        string `json:"error_description"`
}

type workOSTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func handleParseAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.RawJSON)
	if err != nil {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	if strings.TrimSpace(sa.Auth.AccessToken) == "" {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	auth := authDataFromStored(req.FileName, sa)
	return okEnvelope(pluginapi.AuthParseResponse{Handled: true, Auth: auth})
}

func handleStartLogin(raw []byte) ([]byte, error) {
	if len(raw) > 0 {
		var req pluginapi.AuthLoginStartRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	resp, err := startLogin()
	if err != nil {
		return nil, err
	}
	return okEnvelope(resp)
}

// startLogin opens a WorkOS device flow and registers the pending state so the
// panel can poll it by state token.
func startLogin() (pluginapi.AuthLoginStartResponse, error) {
	form := "client_id=" + workOSClientID
	httpReq, err := http.NewRequest(http.MethodPost, workOSAPIBase+"/user_management/authorize/device", strings.NewReader(form))
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	var device workOSDeviceResponse
	if err := json.Unmarshal(body, &device); err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("decode WorkOS device response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || device.DeviceCode == "" {
		msg := device.ErrorDescription
		if msg == "" {
			msg = string(body)
		}
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("WorkOS device authorization failed: %s", msg)
	}
	resultURL := buildDeviceLoginURL(device)
	loginURL := device.VerificationURI
	if loginURL == "" {
		loginURL = device.VerificationURIComplete
	}
	state := randomHex(24)
	expiresIn := time.Duration(device.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 5 * time.Minute
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	loginStates.Store(state, &loginState{
		DeviceCode:   device.DeviceCode,
		UserCode:     device.UserCode,
		Verification: loginURL,
		Interval:     interval,
		Expires:      time.Now().Add(expiresIn),
		Provider:     providerName,
		StartedAt:    time.Now(),
	})
	return pluginapi.AuthLoginStartResponse{
		Provider:  providerName,
		URL:       resultURL,
		State:     state,
		ExpiresAt: time.Now().Add(expiresIn),
	}, nil
}

// buildDeviceLoginURL returns the browser URL for a WorkOS device grant.
//
// WorkOS already returns verification_uri_complete with the user_code query
// parameter embedded. Appending it again produced
// "...?user_code=ABC?user_code=ABC", which browsers treat as a broken URL.
func buildDeviceLoginURL(device workOSDeviceResponse) string {
	if complete := strings.TrimSpace(device.VerificationURIComplete); complete != "" {
		return complete
	}
	base := strings.TrimSpace(device.VerificationURI)
	if base == "" || strings.TrimSpace(device.UserCode) == "" {
		return base
	}
	if strings.Contains(base, "user_code=") {
		return base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "user_code=" + url.QueryEscape(strings.TrimSpace(device.UserCode))
}

func handlePollLogin(raw []byte) ([]byte, error) {
	var req pluginapi.AuthLoginPollRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return okEnvelope(pollLogin(req.State))
}

func pollLogin(stateKey string) pluginapi.AuthLoginPollResponse {
	value, ok := loginStates.Load(stateKey)
	if !ok {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "login state not found or expired; restart login",
		}
	}
	state := value.(*loginState)
	if time.Now().After(state.Expires) {
		loginStates.Delete(stateKey)
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "login expired; restart login",
		}
	}
	token, pending, err := pollWorkOSToken(state)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: err.Error(),
		}
	}
	if pending {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusPending,
			Message: "waiting for browser authentication confirmation",
		}
	}
	auth, err := registerWorkOSTokens(token)
	if err != nil {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: err.Error(),
		}
	}
	loginStates.Delete(stateKey)
	return pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   auth,
	}
}

func pollWorkOSToken(state *loginState) (workOSTokenResponse, bool, error) {
	form := "grant_type=urn:ietf:params:oauth:grant-type:device_code" +
		"&device_code=" + state.DeviceCode +
		"&client_id=" + workOSClientID
	httpReq, err := http.NewRequest(http.MethodPost, workOSAPIBase+"/user_management/authenticate", strings.NewReader(form))
	if err != nil {
		return workOSTokenResponse{}, false, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := sharedHTTPClient().Do(httpReq)
	if err != nil {
		return workOSTokenResponse{}, false, err
	}
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return workOSTokenResponse{}, false, err
	}
	var token workOSTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return workOSTokenResponse{}, false, fmt.Errorf("decode WorkOS token response: %w", err)
	}
	if token.Error == "authorization_pending" || token.Error == "slow_down" {
		return token, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := token.ErrorDescription
		if msg == "" {
			msg = token.Error
		}
		if msg == "" {
			msg = string(body)
		}
		return token, false, fmt.Errorf("WorkOS token exchange failed: %s", msg)
	}
	if token.AccessToken == "" || token.RefreshToken == "" {
		return token, false, fmt.Errorf("WorkOS token response is incomplete")
	}
	return token, false, nil
}

func registerWorkOSTokens(token workOSTokenResponse) (pluginapi.AuthData, error) {
	payload := map[string]string{
		"accessToken":  token.AccessToken,
		"refreshToken": token.RefreshToken,
	}
	resp, err := postJSON(clineAPIBase+"/api/v1/auth/register", payload, nil)
	if err != nil {
		return pluginapi.AuthData{}, err
	}
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return pluginapi.AuthData{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.AuthData{}, fmt.Errorf("Cline token registration failed: HTTP %d: %s", resp.StatusCode, truncate(string(body), 240))
	}
	var parsed clineAuthResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return pluginapi.AuthData{}, fmt.Errorf("decode Cline token response: %w", err)
	}
	if !parsed.Success || parsed.Data.AccessToken == "" {
		return pluginapi.AuthData{}, fmt.Errorf("Cline token registration returned no access token")
	}
	sa := storedAuth{
		Auth: storedTokens{
			AccessToken:  parsed.Data.AccessToken,
			RefreshToken: parsed.Data.RefreshToken,
			ExpiresAt:    parseExpiryMillis(parsed.Data.ExpiresAt),
			TokenType:    parsed.Data.TokenType,
		},
		Account: storedAccount{
			ID:          parsed.Data.UserInfo.ClineUID,
			Email:       parsed.Data.UserInfo.Email,
			DisplayName: parsed.Data.UserInfo.Name,
		},
	}
	return authDataFromStored(authFileName, &sa), nil
}

func handleRefreshAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, err
	}
	payload := map[string]string{
		"refreshToken": sa.Auth.RefreshToken,
		"grantType":    "refresh_token",
	}
	resp, err := postJSON(clineAPIBase+"/api/v1/auth/refresh", payload, nil)
	if err != nil {
		return nil, err
	}
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Cline token refresh failed: HTTP %d: %s", resp.StatusCode, truncate(string(body), 240))
	}
	var parsed clineAuthResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode Cline refresh response: %w", err)
	}
	if !parsed.Success || parsed.Data.AccessToken == "" {
		return nil, fmt.Errorf("Cline refresh returned no access token")
	}
	sa.Auth.AccessToken = parsed.Data.AccessToken
	if parsed.Data.RefreshToken != "" {
		sa.Auth.RefreshToken = parsed.Data.RefreshToken
	}
	sa.Auth.ExpiresAt = parseExpiryMillis(parsed.Data.ExpiresAt)
	if parsed.Data.UserInfo.ClineUID != "" {
		sa.Account.ID = parsed.Data.UserInfo.ClineUID
	}
	if parsed.Data.UserInfo.Email != "" {
		sa.Account.Email = parsed.Data.UserInfo.Email
	}
	if parsed.Data.UserInfo.Name != "" {
		sa.Account.DisplayName = parsed.Data.UserInfo.Name
	}
	fetchAccountSnapshot(sa)
	auth := authDataFromStored(req.AuthID, sa)
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth:             auth,
		NextRefreshAfter: time.UnixMilli(sa.Auth.ExpiresAt).Add(-5 * time.Minute),
	})
}

func parseStored(raw []byte) (*storedAuth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var direct storedAuth
	if err := json.Unmarshal(raw, &direct); err == nil && direct.Auth.AccessToken != "" {
		return &direct, nil
	}
	var flat struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Email        string `json:"email"`
		DisplayName  string `json:"displayName"`
		ID           string `json:"id"`
	}
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	if flat.AccessToken == "" {
		return nil, fmt.Errorf("storage_parse_error: access token is missing")
	}
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:  flat.AccessToken,
			RefreshToken: flat.RefreshToken,
			ExpiresAt:    flat.ExpiresAt,
		},
		Account: storedAccount{
			ID:          flat.ID,
			Email:       flat.Email,
			DisplayName: flat.DisplayName,
		},
	}, nil
}

func authDataFromStored(id string, sa *storedAuth) pluginapi.AuthData {
	raw, _ := json.Marshal(sa)
	if strings.TrimSpace(id) == "" {
		id = authFileName
	}
	label := strings.TrimSpace(sa.Account.DisplayName)
	if label == "" {
		label = strings.TrimSpace(sa.Account.Email)
	}
	if label == "" {
		label = providerName
	}
	metadata := map[string]any{}
	if sa.Account.Email != "" {
		metadata["email"] = sa.Account.Email
	}
	if sa.Account.DisplayName != "" {
		metadata["display_name"] = sa.Account.DisplayName
	}
	if sa.Account.Plan != "" {
		metadata["plan"] = sa.Account.Plan
	}
	if sa.Account.PlanStatus != "" {
		metadata["plan_status"] = sa.Account.PlanStatus
	}
	if sa.Account.Balance > 0 {
		metadata["credits"] = sa.Account.Balance
	}
	return pluginapi.AuthData{
		Provider:    providerName,
		ID:          id,
		FileName:    authFileName,
		Label:       label,
		StorageJSON: raw,
		Metadata:    metadata,
		Attributes: map[string]string{
			"auth_kind": "oauth",
		},
		NextRefreshAfter: time.UnixMilli(sa.Auth.ExpiresAt).Add(-5 * time.Minute),
	}
}

func parseExpiryMillis(value string) int64 {
	if value == "" {
		return time.Now().Add(55 * time.Minute).UnixMilli()
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Now().Add(55 * time.Minute).UnixMilli()
	}
	return parsed.UnixMilli()
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}
