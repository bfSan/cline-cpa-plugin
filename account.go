package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// fetchAccountSnapshot refreshes the account identity and subscription state
// from Cline. It never returns hard errors for individual endpoints: the OAuth
// credential is valid even when the account has no plan history, and a
// transient billing hiccup must not make the auth unusable.
func fetchAccountSnapshot(sa *storedAuth) {
	if sa == nil || strings.TrimSpace(sa.Auth.AccessToken) == "" {
		return
	}
	headers := clineHeaders(sa.Auth.AccessToken)

	if user, err := fetchClineUser(headers); err == nil {
		if user.ID != "" {
			sa.Account.ID = user.ID
		}
		if user.Email != "" {
			sa.Account.Email = user.Email
		}
		if user.DisplayName != "" {
			sa.Account.DisplayName = user.DisplayName
		}
	}

	if plan, status, err := fetchClinePlan(headers); err == nil && status != "" {
		sa.Account.Plan = plan
		sa.Account.PlanStatus = status
	}

}

func fetchClineUser(headers http.Header) (clineMeResponseData, error) {
	resp, err := clineGet(clineAPIBase+"/api/v1/users/me", headers)
	if err != nil {
		return clineMeResponseData{}, err
	}
	defer resp.Body.Close()
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return clineMeResponseData{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return clineMeResponseData{}, fmt.Errorf("users/me HTTP %d", resp.StatusCode)
	}
	var parsed clineMeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return clineMeResponseData{}, fmt.Errorf("decode users/me: %w", err)
	}
	return clineMeResponseData{
		ID:          parsed.Data.ID,
		Email:       parsed.Data.Email,
		DisplayName: parsed.Data.DisplayName,
	}, nil
}

type clineMeResponseData struct {
	ID          string
	Email       string
	DisplayName string
}

// fetchClinePlan reports the active subscription display name and a coarse
// status. Cline returns 404 "no plan history found for user" for accounts
// without a subscription; that is a normal state, not an error.
func fetchClinePlan(headers http.Header) (string, string, error) {
	resp, err := clineGet(clineAPIBase+"/api/v1/users/me/plan", headers)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", "none", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("users/me/plan HTTP %d", resp.StatusCode)
	}
	var parsed clinePlanResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("decode users/me/plan: %w", err)
	}
	if parsed.Data == nil || parsed.Data.Plan == nil {
		return "", "none", nil
	}
	name := firstNonEmpty(parsed.Data.Plan.DisplayName, parsed.Data.Plan.Name, parsed.Data.Plan.ID)
	if name == "" {
		name = "ClinePass"
	}
	if parsed.Data.CurrentPeriodEnd != "" {
		name += " (through " + parsed.Data.CurrentPeriodEnd + ")"
	}
	return name, "active", nil
}

func clineGet(url string, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	return sharedHTTPClient().Do(req)
}
