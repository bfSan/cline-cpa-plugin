package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if len(models) != 5 {
		t.Fatalf("models = %d, want 5", len(models))
	}
	if len(groups[passModelGroup]) != 2 {
		t.Fatalf("clinepass group = %v, want 2 ids", groups[passModelGroup])
	}
	if len(groups[freeModelGroup]) != 1 || groups[freeModelGroup][0] != "cline-free/kimi-k3" {
		t.Fatalf("free group = %v", groups[freeModelGroup])
	}
	if len(groups[cloudModelGroup]) != 1 {
		t.Fatalf("cloud group = %v", groups[cloudModelGroup])
	}
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

func TestAccountSnapshotReadsActivePlanAndBalance(t *testing.T) {
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/users/me":
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"usr-1","email":"user@example.com","displayName":"User"}}`))
		case "/api/v1/users/me/plan":
			_, _ = w.Write([]byte(`{"success":true,"data":{"plan":{"displayName":"Cline Pass (Annual)"},"currentPeriodEnd":"2030-01-01"}}`))
		case "/api/v1/users/usr-1/balance":
			_, _ = w.Write([]byte(`{"success":true,"data":{"userId":"usr-1","balance":499637}}`))
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
	if sa.Account.Balance != 499637 {
		t.Fatalf("balance = %d, want 499637", sa.Account.Balance)
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
