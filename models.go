package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type recommendedModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

type recommendedModels struct {
	Recommended []recommendedModel `json:"recommended"`
	Free        []recommendedModel `json:"free"`
	ClinePass   []recommendedModel `json:"clinePass"`
	ClineCloud  []recommendedModel `json:"clineCloud"`
}

var (
	modelCacheMu sync.RWMutex
	modelCache   = map[string]cachedModelCatalog{}
)

type cachedModelCatalog struct {
	Models      []pluginapi.ModelInfo
	Groups      map[string][]string
	FetchedAt   time.Time
	Source      string
	Entitlement string
}

const (
	staticModelGroup = "default"
	freeModelGroup   = "free"
	passModelGroup   = "clinepass"
	cloudModelGroup  = "clinecloud"
)

func fallbackModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		modelInfo("cline-pass/deepseek-v4.1-flash", "ClinePass DeepSeek V4.1 Flash", passModelGroup, "Fast ClinePass model with a 1M context window."),
		modelInfo("cline-pass/glm-5.3", "ClinePass GLM 5.3", passModelGroup, "Z-AI top open-weights coding model."),
		modelInfo("cline-pass/glm-5.2", "ClinePass GLM 5.2", passModelGroup, "Z-AI GLM-5.2 coding model."),
		modelInfo("cline-pass/deepseek-v4-pro", "ClinePass DeepSeek V4 Pro", passModelGroup, "Frontier reasoning and coding with 1M context."),
		modelInfo("cline-pass/deepseek-v4-flash", "ClinePass DeepSeek V4 Flash", passModelGroup, "Fast DeepSeek coding model."),
		modelInfo("cline-pass/kimi-k3", "ClinePass Kimi K3", passModelGroup, "Moonshot flagship open-weights model."),
		modelInfo("cline-pass/kimi-k2.7-code", "ClinePass Kimi K2.7 Code", passModelGroup, "Moonshot code-focused model."),
		modelInfo("cline-pass/kimi-k2.6", "ClinePass Kimi K2.6", passModelGroup, "Moonshot open-weights coding model."),
		modelInfo("cline-pass/qwen3.8-max", "ClinePass Qwen3.8 Max", passModelGroup, "Qwen SOTA coding model."),
		modelInfo("cline-free/kimi-k3", "Cline Free Kimi K3", freeModelGroup, "Free Moonshot flagship model."),
		modelInfo("cline-free/deepseek-v4.1-flash", "Cline Free DeepSeek V4.1 Flash", freeModelGroup, "Free Cline model with a 1M context window."),
		modelInfo("cline-free/muse-spark-1.3-contributor", "Cline Free Muse Spark 1.3 Contributor", freeModelGroup, "Meta multimodal reasoning model."),
		modelInfo("z-ai/glm-5.3-flash", "Cline Free GLM 5.3 Flash", freeModelGroup, "Latest multimodal GLM-5 model."),
		modelInfo("cline-free/solar-pro4", "Cline Free Solar Pro 4", freeModelGroup, "Document and coding model."),
		modelInfo("poolside/laguna-s-2.1:free", "Cline Free Laguna S 2.1", freeModelGroup, "Poolside coding agent model."),
		modelInfo("openai/gpt-6-astra", "Cline Recommended GPT-6 Astra", staticModelGroup, "Frontier OpenAI model through Cline."),
		modelInfo("moonshotai/kimi-k3", "Cline Recommended Kimi K3", staticModelGroup, "Moonshot flagship model."),
		modelInfo("anthropic/claude-opus-5", "Cline Recommended Claude Opus 5", staticModelGroup, "Anthropic frontier model."),
		modelInfo("x-ai/grok-4.5", "Cline Recommended Grok 4.5", staticModelGroup, "xAI frontier model."),
	}
}

// clientCompatibilityModels are IDs shipped or observed in Cline Desktop that
// have been absent from the recommended-models feed at times. Keep these as a
// union with live discovery so a feed omission cannot remove usable models.
func clientCompatibilityModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{
		modelInfo("cline-pass/glm-5.2", "ClinePass GLM 5.2", passModelGroup, "Z-AI GLM-5.2 coding model."),
		modelInfo("cline-pass/deepseek-v4-flash", "ClinePass DeepSeek V4 Flash", passModelGroup, "Fast DeepSeek coding model."),
		modelInfo("cline-pass/kimi-k2.7-code", "ClinePass Kimi K2.7 Code", passModelGroup, "Moonshot code-focused model."),
		modelInfo("cline-pass/kimi-k2.6", "ClinePass Kimi K2.6", passModelGroup, "Moonshot open-weights coding model."),
		modelInfo("cline-free/kimi-k3", "Cline Free Kimi K3", freeModelGroup, "Free Moonshot flagship model."),
	}
}

func modelInfo(id, name, group, description string) pluginapi.ModelInfo {
	return pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		OwnedBy:                    "cline",
		Type:                       "chat",
		DisplayName:                name,
		Name:                       name,
		Description:                description,
		ContextLength:              1048576,
		MaxCompletionTokens:        8192,
		SupportedGenerationMethods: []string{"chat"},
		SupportedParameters:        []string{"tools", "reasoning"},
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
	}
}

func handleModelRegister(raw []byte) ([]byte, error) {
	models, groups := effectiveModelCatalog()
	_ = groups
	return okEnvelope(pluginapi.ModelRegistrationResponse{
		Provider: providerName,
		Models:   models,
	})
}

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	models, _, _ := effectiveModelCatalogWithState()
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req pluginapi.AuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		models, _, _ := effectiveModelCatalogWithState()
		return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
	}
	// Discovery runs on the host's schedule, not the user's, so it is the path
	// most likely to meet an expired token: an hour after login the catalog
	// would silently drop out of the panel without this.
	models, _, _ := modelCatalogForAuth(freshStoredAuth(sa, req.Attributes))
	// The per-auth catalog is cached unfiltered; apply the overlay (including
	// config hidden_models) at read time so config changes take effect
	// without waiting out the cache TTL.
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: applyModelOverlay(models, loadedModelOverlay())})
}

func effectiveModelCatalog() ([]pluginapi.ModelInfo, map[string][]string) {
	models, groups, _ := effectiveModelCatalogWithState()
	return models, groups
}

func effectiveModelCatalogWithState() ([]pluginapi.ModelInfo, map[string][]string, string) {
	overlay := loadedModelOverlay()
	base, groups, source := baseModelCatalog()
	return applyModelOverlay(base, overlay), groups, source
}

func baseModelCatalog() ([]pluginapi.ModelInfo, map[string][]string, string) {
	return baseModelCatalogForce(false)
}

// baseModelCatalogForce serves the merged per account catalog, which is what CPA
// registers, and only falls back to the built in list when no account has a
// catalog yet, for example straight after a restart before any client pulled one.
func baseModelCatalogForce(force bool) ([]pluginapi.ModelInfo, map[string][]string, string) {
	if models, groups, source, ok := authModelCatalog(force); ok {
		return models, groups, source
	}
	return fallbackModels(), fallbackGroups(), "fallback"
}

// authModelCatalog merges the per account catalogs into the list CPA actually
// serves.
//
// This replaced a read of modelCache["global"], which nothing ever wrote:
// accountCacheKey only yields "global" for an auth with neither an id nor an
// email, so the merge never hit and the panel rendered fallbackModels() and
// labelled itself 内置回退 permanently. The consequence was visible to operators:
// a model Cline published after that built in list was cut could not be hidden
// from the UI, because the UI never showed it, even though CPA was serving it.
//
// force refreshes each account the way a client request would. It is off by
// default so simply opening the panel cannot fan out one upstream call per
// account; the panel's refresh button asks for it explicitly.
func authModelCatalog(force bool) ([]pluginapi.ModelInfo, map[string][]string, string, bool) {
	accounts, err := ownStoredAuths()
	if err != nil {
		return nil, nil, "", false
	}
	return mergeStoredAuthCatalogs(accounts, force)
}

// ownStoredAuths reads the plugin's own auth files through the host.
//
// Split out from the merge because it is the only part that needs a running
// CPA host, which leaves mergeStoredAuthCatalogs testable on its own.
func ownStoredAuths() ([]*storedAuth, error) {
	files, err := hostAuthListFiles()
	if err != nil {
		return nil, err
	}
	accounts := make([]*storedAuth, 0, len(files))
	for _, file := range files {
		if !isOwnAuthFile(file) {
			continue
		}
		raw, errGet := hostAuthGetByIndex(file.AuthIndex)
		if errGet != nil {
			continue
		}
		sa, errParse := parseStored(raw)
		if errParse != nil {
			continue
		}
		accounts = append(accounts, sa)
	}
	return accounts, nil
}

// mergeStoredAuthCatalogs folds the account catalogs into the list CPA serves.
//
// With force set it refreshes each account the way a client request would.
// Otherwise it only uses what is already cached, so that simply opening the
// panel cannot fan out one upstream call per account; the panel's refresh
// button asks for force explicitly.
func mergeStoredAuthCatalogs(accounts []*storedAuth, force bool) ([]pluginapi.ModelInfo, map[string][]string, string, bool) {
	merger := newCatalogMerger()
	pulled := 0
	for _, sa := range accounts {
		models, groups, source, ok := accountCatalogForMerge(sa, force)
		if !ok {
			continue
		}
		pulled++
		merger.add(models, groups, source)
	}
	return merger.result(pulled)
}

// catalogMerger folds per account catalogs into the single list CPA serves.
//
// Several accounts usually offer the same model, so both the models and the
// group memberships have to dedupe by ID; without that the panel shows the same
// ID once per account and a group lists a member twice.
type catalogMerger struct {
	seen      map[string]struct{}
	merged    []pluginapi.ModelInfo
	groups    map[string][]string
	groupSeen map[string]map[string]struct{}
	source    string
}

func newCatalogMerger() *catalogMerger {
	return &catalogMerger{
		seen:      make(map[string]struct{}),
		merged:    make([]pluginapi.ModelInfo, 0, 32),
		groups:    make(map[string][]string),
		groupSeen: make(map[string]map[string]struct{}),
	}
}

func (m *catalogMerger) add(models []pluginapi.ModelInfo, groups map[string][]string, source string) {
	// Any account backed by the real upstream catalog is enough to call the merged
	// list upstream-sourced. "fallback" means that account could not fetch, so it
	// must never outrank a real one.
	if m.source == "" || m.source == "fallback" {
		if source != "" && source != "fallback" {
			m.source = source
		} else if source == "fallback" && m.source == "" {
			m.source = source
		}
	}
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, dup := m.seen[id]; dup {
			continue
		}
		m.seen[id] = struct{}{}
		m.merged = append(m.merged, model)
	}
	for group, ids := range groups {
		members, has := m.groupSeen[group]
		if !has {
			members = make(map[string]struct{}, len(ids))
			m.groupSeen[group] = members
		}
		for _, id := range ids {
			if _, dup := members[id]; dup {
				continue
			}
			members[id] = struct{}{}
			m.groups[group] = append(m.groups[group], id)
		}
	}
}

// result reports ok when at least one account contributed a model. pulled is
// counted by the caller because an account can be present but contribute nothing.
func (m *catalogMerger) result(pulled int) ([]pluginapi.ModelInfo, map[string][]string, string, bool) {
	if pulled == 0 || len(m.merged) == 0 {
		return nil, nil, "", false
	}
	if m.source == "" {
		m.source = "fallback"
	}
	return m.merged, m.groups, m.source, true
}

// accountCatalogForMerge picks one account's catalog for the merge, without
// going to the network unless the caller asked for a refresh.
func accountCatalogForMerge(sa *storedAuth, force bool) ([]pluginapi.ModelInfo, map[string][]string, string, bool) {
	if !force {
		modelCacheMu.RLock()
		cached, ok := modelCache[accountCacheKey(sa)]
		modelCacheMu.RUnlock()
		if ok && time.Since(cached.FetchedAt) < modelCacheTTL {
			// Clone so a merge can never hand the caller an alias of the cache.
			return cloneModelInfos(cached.Models), cloneGroups(cached.Groups), cached.Source, true
		}
		return nil, nil, "", false
	}
	models, groups, source, _ := refreshModelCatalog(freshStoredAuth(sa, nil))
	return models, groups, source, len(models) > 0
}

func modelCatalogForAuth(sa *storedAuth) ([]pluginapi.ModelInfo, map[string][]string, string) {
	key := accountCacheKey(sa)
	modelCacheMu.RLock()
	cached, ok := modelCache[key]
	modelCacheMu.RUnlock()
	if ok && time.Since(cached.FetchedAt) < modelCacheTTL {
		return cloneModelInfos(cached.Models), cloneGroups(cached.Groups), cached.Source
	}
	models, groups, source, _ := refreshModelCatalog(sa)
	return models, groups, source
}

// refreshModelCatalog pulls the catalog and caches it.
//
// A failed pull keeps whatever was cached before rather than storing the built
// in fallback: an operator pressing refresh during a brief upstream hiccup would
// otherwise serve the wrong list for a full TTL, which is the failure mode this
// whole path is meant to remove. The error is still returned so an admin call can
// report it, and callers that only need a usable list may ignore it.
func refreshModelCatalog(sa *storedAuth) ([]pluginapi.ModelInfo, map[string][]string, string, error) {
	key := accountCacheKey(sa)
	models, groups, source, entitlement, err := fetchRecommendedModels(sa)
	if err != nil {
		modelCacheMu.RLock()
		previous, had := modelCache[key]
		modelCacheMu.RUnlock()
		if had && len(previous.Models) > 0 {
			log.Printf("cline: refresh failed for %s, keeping the previous catalog: %v", key, err)
			return cloneModelInfos(previous.Models), cloneGroups(previous.Groups), previous.Source, err
		}
		models, groups, source = fallbackModels(), fallbackGroups(), "fallback"
	}
	modelCacheMu.Lock()
	modelCache[key] = cachedModelCatalog{
		Models:      cloneModelInfos(models),
		Groups:      cloneGroups(groups),
		FetchedAt:   time.Now(),
		Source:      source,
		Entitlement: entitlement,
	}
	modelCacheMu.Unlock()
	return models, groups, source, err
}

func accountCacheKey(sa *storedAuth) string {
	if sa == nil {
		return "global"
	}
	if sa.Account.ID != "" {
		return sa.Account.ID
	}
	if sa.Account.Email != "" {
		return strings.ToLower(sa.Account.Email)
	}
	return "global"
}

func fetchRecommendedModels(sa *storedAuth) ([]pluginapi.ModelInfo, map[string][]string, string, string, error) {
	req, err := http.NewRequest(http.MethodGet, clineAPIBase+"/api/v1/ai/cline/recommended-models", nil)
	if err != nil {
		return nil, nil, "", "", err
	}
	req.Header = clineHeaders(sa.Auth.AccessToken)
	resp, err := sharedHTTPClient().Do(req)
	if err != nil {
		return nil, nil, "", "", err
	}
	body, err := readAllAndClose(resp.Body)
	if err != nil {
		return nil, nil, "", "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, "", "", fmt.Errorf("recommended models HTTP %d", resp.StatusCode)
	}
	var parsed recommendedModels
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, "", "", fmt.Errorf("decode recommended models: %w", err)
	}
	models := make([]pluginapi.ModelInfo, 0, len(parsed.Recommended)+len(parsed.Free)+len(parsed.ClinePass)+len(parsed.ClineCloud))
	groups := map[string][]string{}
	appendGroup := func(group string, entries []recommendedModel) {
		for _, entry := range entries {
			if entry.ID == "" {
				continue
			}
			models = append(models, modelInfo(entry.ID, entry.Name, group, entry.Description))
			groups[group] = append(groups[group], entry.ID)
		}
	}
	appendGroup(staticModelGroup, parsed.Recommended)
	appendGroup(freeModelGroup, parsed.Free)
	appendGroup(passModelGroup, parsed.ClinePass)
	appendGroup(cloudModelGroup, parsed.ClineCloud)
	if len(models) == 0 {
		return nil, nil, "", "", fmt.Errorf("recommended models response is empty")
	}
	models, groups = mergeModelCatalog(models, groups, clientCompatibilityModels(), clientCompatibilityGroups())
	entitlement := "unknown"
	if len(parsed.ClinePass) > 0 {
		entitlement = "listed"
	}
	return models, groups, "cline-recommended", entitlement, nil
}

func fallbackGroups() map[string][]string {
	return map[string][]string{
		staticModelGroup: {
			"openai/gpt-6-astra",
			"moonshotai/kimi-k3",
			"anthropic/claude-opus-5",
			"x-ai/grok-4.5",
		},
		freeModelGroup: {
			"cline-free/kimi-k3",
			"cline-free/deepseek-v4.1-flash",
			"cline-free/muse-spark-1.3-contributor",
			"z-ai/glm-5.3-flash",
			"cline-free/solar-pro4",
			"poolside/laguna-s-2.1:free",
		},
		passModelGroup: {
			"cline-pass/deepseek-v4.1-flash",
			"cline-pass/glm-5.3",
			"cline-pass/glm-5.2",
			"cline-pass/deepseek-v4-pro",
			"cline-pass/deepseek-v4-flash",
			"cline-pass/kimi-k3",
			"cline-pass/kimi-k2.7-code",
			"cline-pass/kimi-k2.6",
			"cline-pass/qwen3.8-max",
		},
	}
}

func clientCompatibilityGroups() map[string][]string {
	return map[string][]string{
		freeModelGroup: {
			"cline-free/kimi-k3",
		},
		passModelGroup: {
			"cline-pass/glm-5.2",
			"cline-pass/deepseek-v4-flash",
			"cline-pass/kimi-k2.7-code",
			"cline-pass/kimi-k2.6",
		},
	}
}

func mergeModelCatalog(primary []pluginapi.ModelInfo, primaryGroups map[string][]string, required []pluginapi.ModelInfo, requiredGroups map[string][]string) ([]pluginapi.ModelInfo, map[string][]string) {
	models := make([]pluginapi.ModelInfo, 0, len(primary)+len(required))
	groups := map[string][]string{}
	seenModels := map[string]struct{}{}
	modelGroups := map[string]string{}
	seenGroupModels := map[string]map[string]struct{}{}

	groupForModel := func(source map[string][]string, id string) string {
		for group, ids := range source {
			for _, candidate := range ids {
				if strings.TrimSpace(candidate) == id {
					return group
				}
			}
		}
		return ""
	}
	appendGroupModel := func(group, id string) {
		if group == "" {
			return
		}
		if seenGroupModels[group] == nil {
			seenGroupModels[group] = map[string]struct{}{}
		}
		if _, ok := seenGroupModels[group][id]; ok {
			return
		}
		seenGroupModels[group][id] = struct{}{}
		groups[group] = append(groups[group], id)
	}
	appendCatalog := func(source []pluginapi.ModelInfo, sourceGroups map[string][]string) {
		for _, model := range source {
			id := strings.TrimSpace(model.ID)
			if id == "" {
				continue
			}
			group := groupForModel(sourceGroups, id)
			if _, ok := seenModels[id]; ok {
				if modelGroups[id] == "" {
					appendGroupModel(group, id)
					modelGroups[id] = group
				}
				continue
			}
			model.ID = id
			seenModels[id] = struct{}{}
			modelGroups[id] = group
			models = append(models, model)
			appendGroupModel(group, id)
		}
	}
	appendCatalog(primary, primaryGroups)
	appendCatalog(required, requiredGroups)
	return models, groups
}

func cloneGroups(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func cloneModelInfos(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if models == nil {
		return nil
	}
	out := make([]pluginapi.ModelInfo, len(models))
	for i, model := range models {
		out[i] = model
		out[i].SupportedGenerationMethods = append([]string(nil), model.SupportedGenerationMethods...)
		out[i].SupportedParameters = append([]string(nil), model.SupportedParameters...)
		out[i].SupportedInputModalities = append([]string(nil), model.SupportedInputModalities...)
		out[i].SupportedOutputModalities = append([]string(nil), model.SupportedOutputModalities...)
	}
	return out
}

func modelOverlayMatchesAnyID(overlay modelOverlay, id string) bool {
	return newHiddenSet(overlay.Hide).hides(strings.TrimSpace(id))
}

func defaultModelInfo(id, name string) pluginapi.ModelInfo {
	return modelInfo(strings.TrimSpace(id), strings.TrimSpace(name), staticModelGroup, "")
}
