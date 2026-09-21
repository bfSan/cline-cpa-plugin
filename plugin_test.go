package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func decodeEnvelope(t *testing.T, raw []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope %s: %v", string(raw), err)
	}
	return env
}

func decodeResult[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var zero T
	env := decodeEnvelope(t, raw)
	if !env.OK {
		t.Fatalf("call failed: %+v", env.Error)
	}
	if len(env.Result) == 0 {
		return zero
	}
	var out T
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("decode result %s: %v", string(env.Result), err)
	}
	return out
}

func callMethod[T any](t *testing.T, method string, request []byte) T {
	t.Helper()
	raw, err := handleMethod(method, request)
	if err != nil {
		t.Fatalf("handleMethod(%s) error = %v", method, err)
	}
	return decodeResult[T](t, raw)
}

// withUpstream points clineAPIBase at a local server for the duration of a test.
func withUpstream(t *testing.T, handler http.Handler) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previous := clineAPIBase
	clineAPIBase = server.URL
	t.Cleanup(func() { clineAPIBase = previous })
	// The credential layer keeps process-wide state, so tests that refresh one
	// account would otherwise leak a newer credential into the next test and
	// quietly skip the refresh being asserted.
	t.Cleanup(clearCredentialState)
}

func testStoredAuth() *storedAuth {
	return &storedAuth{
		Auth: storedTokens{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		},
		Account: storedAccount{ID: "usr-1", Email: "user@example.com"},
	}
}

func TestRecommendedModelsParseFourGroups(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/ai/cline/recommended-models" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"recommended":[{"id":"openai/gpt-6-astra","name":"GPT-6 Astra"}],
			"free":[{"id":"cline-free/kimi-k3","name":"Kimi K3"}],
			"clinePass":[{"id":"cline-pass/glm-5.3","name":"GLM 5.3"},{"id":"cline-pass/kimi-k3","name":"Kimi K3"}],
			"clineCloud":[{"id":"cline-cloud/glm-5.3","name":"Cloud GLM"}]
		}`))
	}))
	defer server.Close()
	previous := clineAPIBase
	clineAPIBase = server.URL
	defer func() { clineAPIBase = previous }()

	models, groups, _, _, err := fetchRecommendedModels(testStoredAuth())
	if err != nil {
		t.Fatalf("fetchRecommendedModels error = %v", err)
	}
	if len(models) != 9 {
		t.Fatalf("models = %d, want 5 feed + 4 compatibility-only models", len(models))
	}
	if len(groups[passModelGroup]) != 6 {
		t.Fatalf("clinepass group = %v, want 2 feed + 4 compatibility ids", groups[passModelGroup])
	}
	if len(groups[freeModelGroup]) != 1 || groups[freeModelGroup][0] != "cline-free/kimi-k3" {
		t.Fatalf("free group = %v", groups[freeModelGroup])
	}
	if len(groups[cloudModelGroup]) != 1 {
		t.Fatalf("cloud group = %v", groups[cloudModelGroup])
	}
	for _, id := range []string{"cline-free/kimi-k3", "cline-pass/glm-5.2", "cline-pass/kimi-k2.7-code", "cline-pass/kimi-k2.6", "cline-pass/deepseek-v4-flash"} {
		found := false
		for _, model := range models {
			if model.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("client-compatible model %q missing from discovered catalog", id)
		}
	}
}

func TestMergeModelCatalogDeduplicatesRequiredModels(t *testing.T) {
	primary := []pluginapi.ModelInfo{
		{ID: "openai/gpt-6-astra"},
		{ID: "cline-free/kimi-k3"},
	}
	primaryGroups := map[string][]string{
		staticModelGroup: {"openai/gpt-6-astra"},
		freeModelGroup:   {"cline-free/kimi-k3"},
	}
	models, groups := mergeModelCatalog(primary, primaryGroups, clientCompatibilityModels(), clientCompatibilityGroups())
	seen := map[string]int{}
	for _, model := range models {
		seen[model.ID]++
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("model %q appeared %d times after merge", id, count)
		}
	}
	for _, id := range groups[freeModelGroup] {
		if id == "cline-free/kimi-k3" {
			return
		}
	}
	t.Fatalf("free group = %v, want cline-free/kimi-k3", groups[freeModelGroup])
}

func TestFallbackModelsAreUsedWhenDiscoveryFails(t *testing.T) {
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	models, groups, source := modelCatalogForAuth(testStoredAuth())
	if source != "fallback" {
		t.Fatalf("source = %q, want fallback", source)
	}
	if len(models) == 0 {
		t.Fatal("fallback catalog is empty")
	}
	if len(groups[passModelGroup]) == 0 {
		t.Fatal("fallback groups missing clinepass")
	}
}

func TestModelOverlayHideRestoresAndOrder(t *testing.T) {
	restore := setModelOverlayForTest(modelOverlay{})
	defer restore()

	base := []pluginapi.ModelInfo{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}
	state, err := storeModelOverlay(modelOverlay{Hide: []string{"b"}, Order: []string{"c", "a"}})
	if err != nil {
		t.Fatalf("storeModelOverlay error = %v", err)
	}
	got := applyModelOverlay(base, state.Overlay)
	if len(got) != 2 || got[0].ID != "c" || got[1].ID != "a" {
		t.Fatalf("overlay result = %+v, want ordered [c a]", got)
	}

	next, err := storeModelOverlay(modelOverlay{})
	if err != nil {
		t.Fatalf("restore overlay error = %v", err)
	}
	got = applyModelOverlay(base, next.Overlay)
	if len(got) != 3 {
		t.Fatalf("after restore got %d models, want 3", len(got))
	}
}

func TestModelOverlayRejectsInvalidIDs(t *testing.T) {
	if _, err := storeModelOverlay(modelOverlay{Hide: []string{"a", "a"}}); err == nil {
		t.Fatal("duplicate ids accepted")
	}
	if _, err := storeModelOverlay(modelOverlay{Hide: []string{" "}}); err == nil {
		t.Fatal("blank id accepted")
	}
}

func TestModelRegisterAppliesOverlay(t *testing.T) {
	restore := setModelOverlayForTest(modelOverlay{Hide: []string{"cline-free/solar-pro4"}})
	defer restore()
	resp := callMethod[pluginapi.ModelRegistrationResponse](t, pluginabi.MethodModelRegister, nil)
	for _, model := range resp.Models {
		if model.ID == "cline-free/solar-pro4" {
			t.Fatal("hidden model reached the host registry")
		}
	}
	if len(resp.Models) == 0 {
		t.Fatal("registered model list is empty")
	}
}

func TestConfigHiddenModelsSeedOverlay(t *testing.T) {
	restore := setModelOverlayForTest(modelOverlay{})
	defer restore()
	if err := configure([]byte(`{"config_yaml":"hidden_models:\n  - cline-cloud/glm-5.3\n"}`)); err != nil {
		t.Fatalf("configure error = %v", err)
	}
	hidden := loadedModelOverlay().Hide
	if len(hidden) != 1 || hidden[0] != "cline-cloud/glm-5.3" {
		t.Fatalf("hidden = %v, want cline-cloud/glm-5.3", hidden)
	}
}

func TestEnsureWorkOSPrefixIsIdempotent(t *testing.T) {
	if got := ensureWorkOSPrefix("abc"); got != "workos:abc" {
		t.Fatalf("ensureWorkOSPrefix = %q", got)
	}
	if got := ensureWorkOSPrefix("workos:abc"); got != "workos:abc" {
		t.Fatalf("prefix duplicated: %q", got)
	}
}

func TestRefreshAuthPostsCamelCaseBody(t *testing.T) {
	var body []byte
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			body = buf
			_, _ = w.Write([]byte(`{"success":true,"data":{
				"accessToken":"new-access","refreshToken":"new-refresh",
				"expiresAt":"2030-01-01T00:00:00Z","tokenType":"Bearer",
				"userInfo":{"clineUserId":"usr-1","email":"user@example.com","name":"User"}}}`))
		case "/api/v1/users/me":
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"usr-1","email":"user@example.com","displayName":"User"}}`))
		case "/api/v1/users/me/plan":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no plan history found for user"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	req := pluginapi.AuthRefreshRequest{AuthID: "cline-1", StorageJSON: mustJSON(t, testStoredAuth())}
	resp := callMethod[pluginapi.AuthRefreshResponse](t, pluginabi.MethodAuthRefresh, mustJSON(t, req))
	var sa struct {
		Auth    storedTokens  `json:"auth"`
		Account storedAccount `json:"account"`
	}
	if err := json.Unmarshal(resp.Auth.StorageJSON, &sa); err != nil {
		t.Fatalf("decode storage: %v", err)
	}
	if sa.Auth.AccessToken != "new-access" {
		t.Fatalf("access token = %q, want new-access", sa.Auth.AccessToken)
	}
	if !strings.Contains(string(body), `"grantType":"refresh_token"`) {
		t.Fatalf("refresh body = %s, want camelCase grantType", string(body))
	}
	if sa.Account.PlanStatus != "none" {
		t.Fatalf("plan status = %q, want none for 404", sa.Account.PlanStatus)
	}
}

func TestAccountSnapshotReadsActivePlan(t *testing.T) {
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/users/me":
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"usr-1","email":"user@example.com","displayName":"User"}}`))
		case "/api/v1/users/me/plan":
			_, _ = w.Write([]byte(`{"success":true,"data":{"plan":{"displayName":"Cline Pass (Annual)"},"currentPeriodEnd":"2030-01-01"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	sa := testStoredAuth()
	fetchAccountSnapshot(sa)
	if !strings.Contains(sa.Account.Plan, "Cline Pass") {
		t.Fatalf("plan = %q, want Cline Pass", sa.Account.Plan)
	}
	if sa.Account.PlanStatus != "active" {
		t.Fatalf("status = %q, want active", sa.Account.PlanStatus)
	}
}

func TestParseAuthReadsNestedAndFlatStorage(t *testing.T) {
	nested := callMethod[pluginapi.AuthParseResponse](t, pluginabi.MethodAuthParse, mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cline-user-example-com.json",
		RawJSON:  mustJSON(t, testStoredAuth()),
	}))
	if !nested.Handled {
		t.Fatal("nested storage not handled")
	}
	if nested.Auth.Label != "user@example.com" {
		t.Fatalf("label = %q", nested.Auth.Label)
	}

	flat := callMethod[pluginapi.AuthParseResponse](t, pluginabi.MethodAuthParse, mustJSON(t, pluginapi.AuthParseRequest{
		FileName: "cline.json",
		RawJSON:  []byte(`{"type":"cline","accessToken":"a","refreshToken":"r","expiresAt":1}`),
	}))
	if !flat.Handled {
		t.Fatal("flat storage not handled")
	}
}

func TestEntitlementErrorIsReportedClearly(t *testing.T) {
	err := clineUpstreamError(http.StatusForbidden, []byte(`{"error":{"code":"ENTITLEMENT_ERROR","message":"Error 403: the user is not subscribed to required model plan"}}`))
	if !strings.Contains(err.Error(), "ClinePass subscription is not active") {
		t.Fatalf("entitlement error = %q", err.Error())
	}
	statusErr, ok := err.(*upstreamStatusError)
	if !ok || statusErr.status != http.StatusForbidden {
		t.Fatalf("status = %+v, want 403", err)
	}
}

func TestExecutorNonEntitlementErrorKeepsStatus(t *testing.T) {
	err := clineUpstreamError(http.StatusTooManyRequests, []byte(`rate limited`))
	if strings.Contains(err.Error(), "subscript") {
		t.Fatalf("429 misclassified as entitlement: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "upstream 429") {
		t.Fatalf("error = %q", err.Error())
	}
}

// Cline answers 500 `empty response content` when the request leaves no room
// for a reply (reasoning models whose max_tokens is consumed by the reasoning
// phase). CPA cools credentials on 5xx, so one bad request must not be able to
// take the whole account out of rotation: report it as a 400 client fault.
func TestEmptyContentErrorIsReportedAsClientFault(t *testing.T) {
	err := clineUpstreamError(http.StatusInternalServerError, []byte(`{"error":"empty response content","success":false}`))
	statusErr, ok := err.(*upstreamStatusError)
	if !ok {
		t.Fatalf("error type = %T, want *upstreamStatusError", err)
	}
	if statusErr.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (5xx would cool the credential)", statusErr.status)
	}
	if !strings.Contains(statusErr.message, "max_tokens") {
		t.Fatalf("message should hint at the cause, got %q", statusErr.message)
	}
}

// A genuine upstream outage must still surface as 5xx so CPA can rotate.
func TestRealServerErrorKeeps5xxStatus(t *testing.T) {
	err := clineUpstreamError(http.StatusBadGateway, []byte(`bad gateway`))
	statusErr, ok := err.(*upstreamStatusError)
	if !ok || statusErr.status != http.StatusBadGateway {
		t.Fatalf("status = %+v, want 502", err)
	}
}

// The host only honours the top-level "error" field of host.stream.emit, and
// only text containing "unexpected eof" is classified as a connection
// lifecycle event that skips credential cooldown. Both properties are asserted
// here because a regression silently cools the whole Cline auth.
func TestStreamErrorFrameUsesTopLevelErrorAndEofMarker(t *testing.T) {
	raw, err := streamErrorFrame("stream-1", emptyStreamError().Error())
	if err != nil {
		t.Fatalf("streamErrorFrame error = %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if _, nested := frame["payload"]; nested {
		t.Fatalf("frame wraps the error in payload: %s", raw)
	}
	message, _ := frame["error"].(string)
	if message == "" {
		t.Fatalf("frame has no top-level error: %s", raw)
	}
	if !strings.Contains(strings.ToLower(message), "unexpected eof") {
		t.Fatalf("error text lacks the lifecycle marker: %q", message)
	}
}

func TestEmptyStreamErrorTextIsLifecycle(t *testing.T) {
	for _, message := range []string{emptyStreamError().Error(), upstreamReadError(http.ErrHandlerTimeout).Error()} {
		if !strings.Contains(strings.ToLower(message), "unexpected eof") {
			t.Fatalf("message %q lacks lifecycle marker", message)
		}
	}
}

func TestExecuteStreamForwardsEntitlementStatus(t *testing.T) {
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"ENTITLEMENT_ERROR","message":"not subscribed"}}`))
	}))
	raw, err := handleMethod(pluginabi.MethodExecutorExecute, mustJSON(t, pluginapi.ExecutorRequest{
		Model:        "cline-pass/glm-5.3",
		StorageJSON:  mustJSON(t, testStoredAuth()),
		Payload:      []byte(`{"model":"cline-pass/glm-5.3","messages":[]}`),
		AuthProvider: providerName,
	}))
	if err != nil {
		t.Fatalf("handleMethod error = %v", err)
	}
	env := decodeEnvelope(t, raw)
	if env.OK {
		t.Fatal("expected an error envelope")
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "ClinePass subscription is not active") {
		t.Fatalf("error = %+v, want entitlement message", env.Error)
	}
	if env.Error.HTTPStatus != http.StatusForbidden {
		t.Fatalf("http status = %d, want 403", env.Error.HTTPStatus)
	}
}

func TestExecuteUnwrapsClineNonStreamingDataEnvelope(t *testing.T) {
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"choices":[{"message":{"content":"OK"}}]}}`))
	}))
	raw, err := handleMethod(pluginabi.MethodExecutorExecute, mustJSON(t, pluginapi.ExecutorRequest{
		Model:        "cline-free/kimi-k3",
		StorageJSON:  mustJSON(t, testStoredAuth()),
		Payload:      []byte(`{"model":"cline-free/kimi-k3","messages":[]}`),
		AuthProvider: providerName,
	}))
	if err != nil {
		t.Fatalf("handleMethod error = %v", err)
	}
	resp := decodeResult[pluginapi.ExecutorResponse](t, raw)
	var parsed struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(resp.Payload, &parsed); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if len(parsed.Choices) != 1 {
		t.Fatalf("payload has %d top-level choices, want 1: %s", len(parsed.Choices), resp.Payload)
	}
}

func TestNormalizeUpstreamResponseKeepsPlainPayload(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"OK"}}]}`)
	if got := normalizeUpstreamResponse(body); string(got) != string(body) {
		t.Fatalf("plain payload changed: %s", got)
	}
}

// clearCredentialState drops the process-wide credential cache so a test never
// inherits another test's rotated token.
func clearCredentialState() {
	credentialMu.Lock()
	credentialCache = map[string]*storedAuth{}
	credentialMu.Unlock()
	credentialCallMu.Lock()
	credentialCalls = map[string]*refreshCall{}
	credentialCallMu.Unlock()
}

func resetCredentialState(t *testing.T) {
	t.Helper()
	clearCredentialState()
}

func storedAuthExpiringIn(d time.Duration) *storedAuth {
	sa := testStoredAuth()
	sa.Auth.ExpiresAt = time.Now().Add(d).UnixMilli()
	return sa
}

// heldCredential is what a manually held in-flight call resolves with. Its
// tokens differ from anything the fake upstream issues, so a caller that
// bypassed the single flight could not pass by accident.
func heldCredential() *storedAuth {
	sa := storedAuthExpiringIn(48 * time.Hour)
	sa.Auth.AccessToken = "held-access"
	sa.Auth.RefreshToken = "held-refresh"
	return sa
}

func TestNeedsRefreshHonoursLead(t *testing.T) {
	now := time.Now()
	if needsRefresh(storedAuthExpiringIn(5*time.Minute), now) != true {
		t.Fatal("token inside the lead should need refresh")
	}
	if needsRefresh(storedAuthExpiringIn(time.Hour), now) != false {
		t.Fatal("token outside the lead should not need refresh")
	}
	unknown := testStoredAuth()
	unknown.Auth.ExpiresAt = 0
	if needsRefresh(unknown, now) != true {
		t.Fatal("unknown expiry must be treated as stale")
	}
}

// The whole point of the credential layer: an expired token is replaced before
// the request goes out, so the plugin never needs the host to schedule it.
func TestExecutorRefreshesExpiringCredentialBeforeRequest(t *testing.T) {
	resetCredentialState(t)
	var refreshCalls int
	var sawAuthorization string
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls++
			_, _ = w.Write([]byte(`{"success":true,"data":{
				"accessToken":"fresh-access","refreshToken":"fresh-refresh",
				"expiresAt":"2030-01-01T00:00:00Z","tokenType":"Bearer"}}`))
		case "/api/v1/chat/completions":
			sawAuthorization = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	raw, err := handleMethod(pluginabi.MethodExecutorExecute, mustJSON(t, pluginapi.ExecutorRequest{
		Model:       "cline-free/kimi-k3",
		StorageJSON: mustJSON(t, storedAuthExpiringIn(2*time.Minute)),
		Payload:     []byte(`{"model":"cline-free/kimi-k3","messages":[]}`),
	}))
	if err != nil {
		t.Fatalf("handleMethod error = %v", err)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("execute failed: %+v", env.Error)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if !strings.Contains(sawAuthorization, "fresh-access") {
		t.Fatalf("upstream saw %q, want the refreshed token", sawAuthorization)
	}
}

// A token revoked server side still serves the user: the 401 triggers one
// refresh and one replay of the request.
func TestExecutorRetriesOnceAfterUpstream401(t *testing.T) {
	resetCredentialState(t)
	var refreshCalls, chatCalls int
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls++
			_, _ = w.Write([]byte(`{"success":true,"data":{
				"accessToken":"fresh-access","refreshToken":"fresh-refresh",
				"expiresAt":"2030-01-01T00:00:00Z","tokenType":"Bearer"}}`))
		case "/api/v1/chat/completions":
			chatCalls++
			if chatCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"Unauthorized: re-authenticate your Cline account."}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	raw, err := handleMethod(pluginabi.MethodExecutorExecute, mustJSON(t, pluginapi.ExecutorRequest{
		Model:       "cline-free/kimi-k3",
		StorageJSON: mustJSON(t, storedAuthExpiringIn(time.Hour)),
		Payload:     []byte(`{"model":"cline-free/kimi-k3","messages":[]}`),
	}))
	if err != nil {
		t.Fatalf("handleMethod error = %v", err)
	}
	if env := decodeEnvelope(t, raw); !env.OK {
		t.Fatalf("execute failed after retry: %+v", env.Error)
	}
	if refreshCalls != 1 || chatCalls != 2 {
		t.Fatalf("refresh=%d chat=%d, want 1 refresh and 2 chat calls", refreshCalls, chatCalls)
	}
}

func TestConcurrentRefreshesJoinInFlightGrant(t *testing.T) {
	resetCredentialState(t)
	var refreshCalls atomic.Int32
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"data":{
				"accessToken":"fresh-access","refreshToken":"fresh-refresh",
				"expiresAt":"2030-01-01T00:00:00Z","tokenType":"Bearer"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	// Hold a refresh open for the account the way an in-flight grant would, so
	// every caller below has to wait on it instead of starting its own.
	// Add before publishing the entry, exactly as refreshCredential does, so a
	// joiner can never wait on an empty WaitGroup and slip through.
	inFlight := &refreshCall{}
	inFlight.done.Add(1)
	credentialCallMu.Lock()
	credentialCalls["id:usr-1"] = inFlight
	credentialCallMu.Unlock()

	var wg sync.WaitGroup
	results := make([]*storedAuth, 4)
	for i := range results {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			sa, err := refreshCredential("id:usr-1", storedAuthExpiringIn(time.Minute))
			if err != nil {
				t.Errorf("refresh: %v", err)
				return
			}
			results[slot] = sa
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	inFlight.sa = heldCredential()
	inFlight.done.Done()
	wg.Wait()

	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh grants = %d, want callers to join the in-flight one", got)
	}
	for i, sa := range results {
		if sa == nil || sa.Auth.AccessToken != "held-access" {
			t.Fatalf("caller %d got %+v, want the held credential", i, sa)
		}
	}

	// A finished leader must unregister itself. If it stayed in the map, every
	// later refresh would join a completed call and the credential would never
	// rotate again.
	credentialCallMu.Lock()
	credentialCalls = map[string]*refreshCall{}
	credentialCallMu.Unlock()
	if _, err := refreshCredential("id:usr-1", storedAuthExpiringIn(time.Minute)); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh grants = %d, want one after the held call finished", got)
	}
	credentialCallMu.Lock()
	left := len(credentialCalls)
	credentialCallMu.Unlock()
	if left != 0 {
		t.Fatalf("in-flight registrations left = %d, want 0", left)
	}
}

// The host can ask for a refresh against a snapshot the plugin already
// superseded. Spending that refresh token would break the account, so the cache
// answer is returned without an upstream grant.
func TestHostRefreshReusesRotatedCredential(t *testing.T) {
	resetCredentialState(t)
	// The credential the plugin already rotated: newer expiry, different pair.
	rotated := heldCredential()
	rotated.Auth.AccessToken = "fresh-access"
	rotated.Auth.RefreshToken = "fresh-refresh"
	cacheCredential(rotated)

	var refreshCalls int
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			refreshCalls++
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"refresh token already used"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	stale := storedAuthExpiringIn(-time.Hour)
	resp := callMethod[pluginapi.AuthRefreshResponse](t, pluginabi.MethodAuthRefresh, mustJSON(t, pluginapi.AuthRefreshRequest{
		AuthID:      "cline-1",
		StorageJSON: mustJSON(t, stale),
	}))
	if refreshCalls != 0 {
		t.Fatalf("refresh grants = %d, want 0 when a newer credential is cached", refreshCalls)
	}
	var stored storedAuth
	if err := json.Unmarshal(resp.Auth.StorageJSON, &stored); err != nil {
		t.Fatalf("decode storage: %v", err)
	}
	if stored.Auth.RefreshToken != "fresh-refresh" {
		t.Fatalf("refresh token = %q, want the cached rotated value", stored.Auth.RefreshToken)
	}
}

// A dead refresh token needs a login, not a retry, so it must surface as 401;
// a transient grant failure must not look like an authentication problem.
func TestCredentialFailureStatuses(t *testing.T) {
	terminal := credentialFailure(&credentialError{terminal: true, status: 401, message: "re-login required"})
	statusError, ok := terminal.(*upstreamStatusError)
	if !ok || statusError.status != http.StatusUnauthorized {
		t.Fatalf("terminal refresh error = %+v, want 401", terminal)
	}
	transient := credentialFailure(&credentialError{status: 502, message: "bad gateway"})
	statusError, ok = transient.(*upstreamStatusError)
	if !ok || statusError.status != http.StatusServiceUnavailable {
		t.Fatalf("transient refresh error = %+v, want 503", transient)
	}
}

// Persisting must not rewrite the host-owned fields: an account the management
// UI disabled would otherwise come back to life on the next refresh.
func TestCredentialPersistKeepsHostOwnedFields(t *testing.T) {
	dir := t.TempDir()
	name := authFileNameFor(testStoredAuth())
	path := filepath.Join(dir, name)
	disk := `{"type":"cline","provider":"cline","disabled":true,"note":"积分未知","auth":{},"account":{}}`
	if err := os.WriteFile(path, []byte(disk), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cred := newCredential(storedAuthExpiringIn(time.Hour), map[string]string{authPathAttribute: path})
	raw, err := cred.fileJSON()
	if err != nil {
		t.Fatalf("fileJSON: %v", err)
	}
	var doc map[string]any
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode doc: %v", err)
	}
	if doc["disabled"] != true {
		t.Fatalf("disabled = %v, want preserved true", doc["disabled"])
	}
	if doc["note"] != "积分未知" {
		t.Fatalf("note = %v, want preserved", doc["note"])
	}
	auth, _ := doc["auth"].(map[string]any)
	if auth["accessToken"] != "access-token" {
		t.Fatalf("auth fields not written: %v", doc["auth"])
	}
	if cred.fileName != name {
		t.Fatalf("file name = %q, want %q", cred.fileName, name)
	}
}

// The host only schedules refreshes for providers that declare a cadence, so the
// parsed auth must carry one plus a concrete next-refresh time.
func TestParsedAuthDeclaresRefreshCadence(t *testing.T) {
	sa := storedAuthExpiringIn(time.Hour)
	auth := authDataFromStored("cline-1", sa)
	if got := auth.Attributes[authRefreshIntervalAttribute]; got == "" {
		t.Fatalf("attributes = %v, want a refresh cadence", auth.Attributes)
	}
	want := time.UnixMilli(sa.Auth.ExpiresAt).Add(-credentialRefreshLead)
	if !auth.NextRefreshAfter.Equal(want) {
		t.Fatalf("next refresh = %s, want %s", auth.NextRefreshAfter, want)
	}
}

func TestManagementRegisterDeclaresRoutesAndPanel(t *testing.T) {
	resp := callMethod[managementRegistrationResponse](t, pluginabi.MethodManagementRegister, mustJSON(t, pluginapi.ManagementRegistrationRequest{
		BasePath:         "/v0/management",
		ResourceBasePath: "/v0/resource/plugins/cline",
	}))
	if len(resp.Routes) == 0 {
		t.Fatal("no management routes registered")
	}
	if len(resp.Resources) != 1 || resp.Resources[0].Path != "/panel" {
		t.Fatalf("resources = %+v, want panel resource", resp.Resources)
	}
}

func TestManagementServesPanelHTML(t *testing.T) {
	callMethod[managementRegistrationResponse](t, pluginabi.MethodManagementRegister, nil)
	raw, err := handleMethod(pluginabi.MethodManagementHandle, mustJSON(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedResourceBasePath() + "/panel",
	}))
	if err != nil {
		t.Fatalf("handleMethod error = %v", err)
	}
	resp := decodeResult[pluginapi.ManagementResponse](t, raw)
	if !strings.Contains(string(resp.Body), "Cline") {
		t.Fatal("panel html missing Cline marker")
	}
	if strings.Contains(string(resp.Body), "积分") || strings.Contains(string(resp.Body), "credits") {
		t.Fatal("ClinePass uses quota semantics and must not expose credit fields")
	}
}

func TestManagementModelsRoutes(t *testing.T) {
	restore := setModelOverlayForTest(modelOverlay{})
	defer restore()
	callMethod[managementRegistrationResponse](t, pluginabi.MethodManagementRegister, nil)
	base := loadedManagementBasePath() + "/plugins/" + providerName

	catalog := callMethod[pluginapi.ManagementResponse](t, pluginabi.MethodManagementHandle, mustJSON(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   base + "/models",
	}))
	var parsed modelCatalogResponseWire
	if err := json.Unmarshal(catalog.Body, &parsed); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if parsed.Provider != providerName || len(parsed.Models) == 0 {
		t.Fatalf("catalog = %+v", parsed)
	}

	acted := callMethod[pluginapi.ManagementResponse](t, pluginabi.MethodManagementHandle, mustJSON(t, pluginapi.ManagementRequest{
		Method: http.MethodPost,
		Path:   base + "/models/action",
		Body:   []byte(`{"action":"hide","id":"cline-cloud/glm-5.3"}`),
	}))
	var after modelCatalogResponseWire
	if err := json.Unmarshal(acted.Body, &after); err != nil {
		t.Fatalf("decode action response: %v", err)
	}
	if len(after.Overlay.Overlay.Hide) != 1 {
		t.Fatalf("hide not persisted: %+v", after.Overlay)
	}
}

func TestManagementUnknownRouteReturns404(t *testing.T) {
	callMethod[managementRegistrationResponse](t, pluginabi.MethodManagementRegister, nil)
	resp := callMethod[pluginapi.ManagementResponse](t, pluginabi.MethodManagementHandle, mustJSON(t, pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   loadedManagementBasePath() + "/plugins/" + providerName + "/nope",
	}))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestOwnAuthFileUsesRuntimeProviderInsteadOfRegisteredName(t *testing.T) {
	entry := hostAuthFileEntry{
		Name:      "cline.json",
		AuthIndex: "demo-index",
		Provider:  providerName,
		Type:      providerName,
	}
	if !isOwnAuthFile(entry) {
		t.Fatal("runtime Cline credential was rejected because its registered name is cline.json")
	}
	if isOwnAuthFile(hostAuthFileEntry{Name: "cline.json", Provider: "qoder"}) {
		t.Fatal("another provider's credential was accepted")
	}
	if !isOwnAuthFile(hostAuthFileEntry{Name: "cline-account.json"}) {
		t.Fatal("disk-only callback path should keep accepting cline-*.json names")
	}
}

func TestEffectiveAuthNamePrefersPhysicalPath(t *testing.T) {
	entry := hostAuthFileEntry{
		Name: "cline.json",
		Path: "/opt/cpa/auths/cline-usr-123.json",
	}
	if got := effectiveAuthName(entry); got != "cline-usr-123.json" {
		t.Fatalf("effectiveAuthName = %q, want physical file name", got)
	}
	if got := effectiveAuthName(hostAuthFileEntry{Name: "cline.json"}); got != "cline.json" {
		t.Fatalf("effectiveAuthName fallback = %q", got)
	}
}

func TestHostAuthGetDecodesJSONObjectPayload(t *testing.T) {
	result := json.RawMessage(`{"auth_index":"demo-index","name":"cline-demo.json","path":"/tmp/cline-demo.json","json":{"provider":"cline","auth":{"accessToken":"token"}}}`)
	raw, err := json.Marshal(envelope{OK: true, Result: result})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	got, err := decodeHostAuthGetResponse(raw)
	if err != nil {
		t.Fatalf("decodeHostAuthGetResponse: %v", err)
	}
	if string(got) != `{"provider":"cline","auth":{"accessToken":"token"}}` {
		t.Fatalf("json payload = %s", got)
	}
}

func TestManagementOAuthPollRequiresState(t *testing.T) {
	resp := managementOAuthPoll(pluginapi.ManagementRequest{})
	if resp["status"] != "error" {
		t.Fatalf("status = %v, want error", resp["status"])
	}
}

func TestPollLoginUnknownStateReportsError(t *testing.T) {
	resp := pollLogin("missing-state")
	if resp.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("status = %v, want error", resp.Status)
	}
}

func TestBuildDeviceLoginURLDoesNotDuplicateUserCode(t *testing.T) {
	complete := "https://authkit.cline.bot/device?user_code=ABCD-EFGH"
	if got := buildDeviceLoginURL(workOSDeviceResponse{
		UserCode:                "ABCD-EFGH",
		VerificationURI:         "https://authkit.cline.bot/device",
		VerificationURIComplete: complete,
	}); got != complete {
		t.Fatalf("complete URL = %q, want %q", got, complete)
	}
	if got := buildDeviceLoginURL(workOSDeviceResponse{
		UserCode:        "ABCD-EFGH",
		VerificationURI: "https://authkit.cline.bot/device",
	}); got != "https://authkit.cline.bot/device?user_code=ABCD-EFGH" {
		t.Fatalf("fallback URL = %q", got)
	}
}

func TestAuthFileNameForDerivesPerAccountFile(t *testing.T) {
	if got := authFileNameFor(&storedAuth{Account: storedAccount{ID: "usr-01M2ZWV83PXA5R27G739NAEKCW"}}); !strings.HasPrefix(got, "cline-usr-") || !strings.HasSuffix(got, ".json") {
		t.Fatalf("file name = %q", got)
	}
	if got := authFileNameFor(&storedAuth{}); got != authFileName {
		t.Fatalf("file name = %q, want %q", got, authFileName)
	}
}

func TestBuildAuthFileJSONKeepsHostMetadata(t *testing.T) {
	raw, err := buildAuthFileJSON(testStoredAuth())
	if err != nil {
		t.Fatalf("buildAuthFileJSON error = %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed["type"] != providerName {
		t.Fatalf("type = %v, want cline", parsed["type"])
	}
	if _, ok := parsed["auth"]; !ok {
		t.Fatalf("auth block missing: %s", raw)
	}
	if parsed["label"] != "user@example.com" {
		t.Fatalf("label = %v, want user@example.com", parsed["label"])
	}
}

func TestAuthFileNicknameRewriteUpdatesLabelAndNestedAccount(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"type":     providerName,
		"provider": providerName,
		"label":    "old@example.com",
		"note":     "old note",
		"auth":     map[string]any{"accessToken": "token"},
		"account": map[string]any{
			"id":          "usr-1",
			"email":       "old@example.com",
			"displayName": "Old User",
		},
	})
	updated, err := rewriteAuthFileNickname(raw, "新昵称")
	if err != nil {
		t.Fatalf("rewriteAuthFileNickname error = %v", err)
	}
	var parsed struct {
		Label   string         `json:"label"`
		Note    string         `json:"note"`
		Account map[string]any `json:"account"`
	}
	if err := json.Unmarshal(updated, &parsed); err != nil {
		t.Fatalf("decode rewritten auth: %v", err)
	}
	if parsed.Label != "新昵称" {
		t.Fatalf("label = %q, want 新昵称", parsed.Label)
	}
	if got := stringValue(parsed.Account["nickname"]); got != "新昵称" {
		t.Fatalf("account.nickname = %q, want 新昵称", got)
	}
	if parsed.Note != "" {
		t.Fatalf("legacy note = %q, want removed", parsed.Note)
	}
}

func TestBuildAuthFileJSONPrefersNicknameForLabel(t *testing.T) {
	sa := testStoredAuth()
	sa.Account.DisplayName = "Old User"
	sa.Account.Nickname = "新昵称"
	raw, err := buildAuthFileJSON(sa)
	if err != nil {
		t.Fatalf("buildAuthFileJSON error = %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed["label"] != "新昵称" {
		t.Fatalf("label = %v, want 新昵称", parsed["label"])
	}
	account, _ := parsed["account"].(map[string]any)
	if got := stringValue(account["nickname"]); got != "新昵称" {
		t.Fatalf("account.nickname = %q, want 新昵称", got)
	}
}

func TestParseStoredMigratesLegacyNoteToNickname(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"type":     providerName,
		"provider": providerName,
		"label":    "Old User",
		"note":     "legacy rename",
		"auth":     map[string]any{"accessToken": "token"},
		"account": map[string]any{
			"id":          "usr-1",
			"email":       "old@example.com",
			"displayName": "Old User",
		},
	})
	sa, err := parseStored(raw)
	if err != nil {
		t.Fatalf("parseStored error = %v", err)
	}
	if sa.Account.Nickname != "legacy rename" {
		t.Fatalf("nickname = %q, want legacy rename", sa.Account.Nickname)
	}
}
