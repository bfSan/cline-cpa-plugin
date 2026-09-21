package main

import (
	"encoding/json"
	"fmt"
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
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
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
	modelCacheMu.RLock()
	defer modelCacheMu.RUnlock()
	if cached, ok := modelCache["global"]; ok && time.Since(cached.FetchedAt) < modelCacheTTL {
		return cloneModelInfos(cached.Models), cloneGroups(cached.Groups), cached.Source
	}
	return fallbackModels(), fallbackGroups(), "fallback"
}

func modelCatalogForAuth(sa *storedAuth) ([]pluginapi.ModelInfo, map[string][]string, string) {
	key := accountCacheKey(sa)
	modelCacheMu.RLock()
	cached, ok := modelCache[key]
	modelCacheMu.RUnlock()
	if ok && time.Since(cached.FetchedAt) < modelCacheTTL {
		return cloneModelInfos(cached.Models), cloneGroups(cached.Groups), cached.Source
	}
	models, groups, source, entitlement, err := fetchRecommendedModels(sa)
	if err != nil {
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
	return models, groups, source
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
	id = strings.TrimSpace(id)
	for _, hidden := range overlay.Hide {
		if strings.TrimSpace(hidden) == id {
			return true
		}
	}
	return false
}

func defaultModelInfo(id, name string) pluginapi.ModelInfo {
	return modelInfo(strings.TrimSpace(id), strings.TrimSpace(name), staticModelGroup, "")
}
