package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

//go:embed panel.html
var panelHTML []byte

type managementRequestWire struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

var (
	managementBasePathCache = "/v0/management"
	resourceBasePathCache   = "/v0/resource/plugins/" + providerName
	managementBasePathMu    sync.RWMutex
	managementResourceMu    sync.RWMutex
)

func loadedManagementBasePath() string {
	managementBasePathMu.RLock()
	defer managementBasePathMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(path string) {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return
	}
	managementBasePathMu.Lock()
	managementBasePathCache = path
	managementBasePathMu.Unlock()
}

func loadedResourceBasePath() string {
	managementResourceMu.RLock()
	defer managementResourceMu.RUnlock()
	return resourceBasePathCache
}

func setResourceBasePath(path string) {
	path = strings.TrimRight(strings.TrimSpace(path), "/")
	if path == "" {
		return
	}
	managementResourceMu.Lock()
	resourceBasePathCache = path
	managementResourceMu.Unlock()
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/models", Description: "List the effective Cline model catalog with groups and source."},
			{Method: http.MethodPut, Path: base + "/models", Description: "Replace the model overlay (hide/order/add)."},
			{Method: http.MethodPost, Path: base + "/models/action", Description: "Apply one model action: hide, restore, move, add."},
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List Cline accounts and subscription status."},
			{Method: http.MethodPost, Path: base + "/oauth/start", Description: "Start a Cline WorkOS device login."},
			{Method: http.MethodPost, Path: base + "/oauth/poll", Description: "Poll a Cline WorkOS device login."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "Cline", Description: "Cline/ClinePass account and model dashboard."},
		},
	}
}

type managementJSON struct {
	StatusCode int
	Body       any
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")
	resPrefix := loadedResourceBasePath()
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		if sub != "" && sub != "/" && sub != "/panel" && sub != "/panel.html" {
			return okEnvelope(mgmtHTMLResponse(http.StatusNotFound, []byte("<h1>404</h1>")))
		}
		return okEnvelope(mgmtHTMLResponse(http.StatusOK, servePanel()))
	}
	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/models":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, modelCatalogResponse()))
	case req.Method == http.MethodPut && path == base+"/models":
		var overlay modelOverlay
		if err := json.Unmarshal(req.Body, &overlay); err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "invalid overlay"}))
		}
		state, err := storeModelOverlay(overlay)
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()}))
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, modelCatalogResponseWithState(state)))
	case req.Method == http.MethodPost && path == base+"/models/action":
		return okEnvelope(handleModelAction(req.Body))
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, accountSummary()))
	case req.Method == http.MethodPost && path == base+"/oauth/start":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, managementOAuthStart(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/oauth/poll":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, managementOAuthPoll(req.ManagementRequest)))
	default:
		return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
	}
}

// managementOAuthStart opens a WorkOS device login for the panel. The host's
// generic auth URL route does not return the state reliably for panel polling,
// so the panel drives start/poll through these plugin-owned routes.
func managementOAuthStart(req pluginapi.ManagementRequest) map[string]any {
	resp, err := startLogin()
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}
	}
	return map[string]any{
		"status":     "pending",
		"url":        resp.URL,
		"state":      resp.State,
		"expires_at": resp.ExpiresAt,
	}
}

// managementOAuthPoll drives one poll and persists the credential through
// host.auth.save when the browser grant lands. Returns "ok" on success so the
// panel does not have to guess the host auth-provider status vocabulary.
func managementOAuthPoll(req pluginapi.ManagementRequest) map[string]any {
	state := strings.TrimSpace(req.Query.Get("state"))
	if state == "" && len(req.Body) > 0 {
		var body struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(req.Body, &body); err == nil {
			state = strings.TrimSpace(body.State)
		}
	}
	if state == "" {
		return map[string]any{"status": "error", "error": "state is required"}
	}
	resp := pollLogin(state)
	switch resp.Status {
	case pluginapi.AuthLoginStatusPending:
		return map[string]any{"status": "pending", "message": resp.Message}
	case pluginapi.AuthLoginStatusSuccess:
		if err := persistAuthData(resp.Auth); err != nil {
			return map[string]any{"status": "error", "error": err.Error()}
		}
		return map[string]any{
			"status": "ok",
			"file":   resp.Auth.FileName,
			"label":  resp.Auth.Label,
		}
	default:
		message := resp.Message
		if message == "" {
			message = "login failed"
		}
		return map[string]any{"status": "error", "error": message}
	}
}

type modelCatalogResponseWire struct {
	Provider string                `json:"provider"`
	Source   string                `json:"source"`
	Groups   map[string][]string   `json:"groups"`
	Models   []pluginapi.ModelInfo `json:"models"`
	Overlay  modelOverlayState     `json:"overlay"`
}

func modelCatalogResponse() modelCatalogResponseWire {
	state := loadedModelOverlayState()
	return modelCatalogResponseWithState(state)
}

func modelCatalogResponseWithState(state modelOverlayState) modelCatalogResponseWire {
	base, groups, source := baseModelCatalog()
	models := applyModelOverlayForAdmin(base, state.Overlay)
	return modelCatalogResponseWire{
		Provider: providerName,
		Source:   source,
		Groups:   groups,
		Models:   models,
		Overlay:  state,
	}
}

type modelActionRequest struct {
	Action string `json:"action"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Before string `json:"before,omitempty"`
}

func handleModelAction(raw []byte) pluginapi.ManagementResponse {
	var req modelActionRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "invalid action request"})
	}
	state := loadedModelOverlayState()
	overlay := state.Overlay
	id := strings.TrimSpace(req.ID)
	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "hide":
		if id == "" {
			return mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "id is required"})
		}
		overlay.Hide = appendUnique(overlay.Hide, id)
		overlay.Order = removeString(overlay.Order, id)
	case "restore":
		overlay.Hide = removeString(overlay.Hide, id)
	case "add":
		if id == "" {
			return mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "id is required"})
		}
		if findModelByID(baseModelCatalogOnly(), id) == nil {
			overlay.Add = appendUnique(overlay.Add, id)
		}
		overlay.Hide = removeString(overlay.Hide, id)
	case "move":
		overlay.Order = moveString(overlay.Order, id, req.Before)
	default:
		return mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": "unknown action"})
	}
	next, err := storeModelOverlay(overlay)
	if err != nil {
		return mgmtJSONResponse(http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
	return mgmtJSONResponse(http.StatusOK, modelCatalogResponseWithState(next))
}

func baseModelCatalogOnly() []pluginapi.ModelInfo {
	models, _, _ := baseModelCatalog()
	return models
}

func findModelByID(models []pluginapi.ModelInfo, id string) *pluginapi.ModelInfo {
	for i := range models {
		if strings.TrimSpace(models[i].ID) == id {
			return &models[i]
		}
	}
	return nil
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

func removeString(items []string, value string) []string {
	out := items[:0]
	for _, item := range items {
		if item != value {
			out = append(out, item)
		}
	}
	return out
}

func moveString(items []string, value, before string) []string {
	if value == "" {
		return items
	}
	items = removeString(items, value)
	if before == "" {
		return append(items, value)
	}
	for i, item := range items {
		if item == before {
			out := append([]string{}, items[:i]...)
			out = append(out, value)
			out = append(out, items[i:]...)
			return out
		}
	}
	return append(items, value)
}

type accountSummaryWire struct {
	Accounts []accountSummaryEntry `json:"accounts"`
	Error    string                `json:"error,omitempty"`
}

type accountSummaryEntry struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Email       string `json:"email,omitempty"`
	Plan        string `json:"plan,omitempty"`
	PlanStatus  string `json:"plan_status,omitempty"`
	Credit      string `json:"credits,omitempty"`
	LastChecked string `json:"last_checked,omitempty"`
}

func accountSummary() accountSummaryWire {
	files, err := hostAuthListFiles()
	if err != nil {
		return accountSummaryWire{Error: "host auth list unavailable"}
	}
	result := accountSummaryWire{}
	for _, file := range files {
		if !strings.EqualFold(strings.TrimSpace(file.Name), authFileName) &&
			!strings.HasPrefix(strings.ToLower(strings.TrimSpace(file.Name)), providerName+"-") {
			continue
		}
		raw, err := hostAuthGetByIndex(file.AuthIndex)
		if err != nil {
			continue
		}
		sa, err := parseStored(raw)
		if err != nil {
			continue
		}
		result.Accounts = append(result.Accounts, accountSummaryEntry{
			ID:          file.AuthIndex,
			Label:       firstNonEmpty(sa.Account.DisplayName, sa.Account.Email, providerName),
			Email:       sa.Account.Email,
			Plan:        sa.Account.Plan,
			PlanStatus:  sa.Account.PlanStatus,
			LastChecked: time.Now().UTC().Format(time.RFC3339),
		})
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func mgmtJSONResponse(status int, value any) pluginapi.ManagementResponse {
	raw, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       raw,
	}
}

func mgmtHTMLResponse(status int, value []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       value,
	}
}

func mgmtRawResponse(status int, value []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       value,
	}
}

// servePanel injects the host-provided management base path so the embedded
// panel can call the plugin routes without hardcoding /v0/management.
func servePanel() []byte {
	base, _ := json.Marshal(loadedManagementBasePath())
	return bytes.ReplaceAll(panelHTML, []byte("__CLINE_MANAGEMENT_BASE_PATH_JSON__"), base)
}
