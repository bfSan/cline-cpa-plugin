package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestHiddenFamilyPrefix covers the reason the deny list stopped being an
// enumeration: a provider publishes new models, and an exact ID list leaks every
// one of them until someone notices and appends it by hand.
func joinIDs(models []pluginapi.ModelInfo) string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ID)
	}
	return strings.Join(out, ",")
}

func TestHiddenFamilyPrefix(t *testing.T) {
	overlay := modelOverlay{Hide: []string{"cline-pass/*", "cline-cloud/*", "openai/gpt-6-astra"}}
	base := []pluginapi.ModelInfo{
		{ID: "cline-free/kimi-k3"},
		{ID: "cline-pass/mimo-v2.5"},
		{ID: "cline-pass/mimo-v2.6-flash"}, // published after the list was written
		{ID: "cline-pass/mimo-v2.6-pro"},   // same
		{ID: "cline-cloud/kimi-k3"},
		{ID: "openai/gpt-6-astra"},
		{ID: "z-ai/glm-5.3-flash"},
	}
	got := joinIDs(applyModelOverlay(base, overlay))
	want := "cline-free/kimi-k3,z-ai/glm-5.3-flash"
	if got != want {
		t.Fatalf("family prefix leaked\n got %s\nwant %s", got, want)
	}
}

func TestSanitizeHideListRejectsAmbiguousWildcards(t *testing.T) {
	ok, rejected := sanitizeHideList([]string{
		"cline-pass/*",       // family prefix, valid
		"*kimi*",             // substring: far wider than it reads on a deny list
		"*",                  // would hide everything
		"openai/gpt-6-astra", // exact, valid
	})
	if strings.Join(ok, ",") != "cline-pass/*,openai/gpt-6-astra" {
		t.Fatalf("unexpected survivors: %v", ok)
	}
	if len(rejected) != 2 {
		t.Fatalf("want 2 rejected, got %v", rejected)
	}
}

// TestConfigureModelsKeepsUsableEntries guards the old behaviour where one bad
// entry dropped the whole list, which quietly unhid every model.
func TestConfigureModelsKeepsUsableEntries(t *testing.T) {
	t.Cleanup(func() { configureModels(nil) })
	configureModels([]string{"cline-pass/*", "*kimi*"})
	kept := loadedConfiguredHiddenModels()
	if len(kept) != 1 || kept[0] != "cline-pass/*" {
		t.Fatalf("want the valid entry kept and the bad one dropped, got %v", kept)
	}
}

// TestOverlayHiddenStillMatchesExact keeps the plain path honest after the
// matcher was introduced.
func TestOverlayHiddenStillMatchesExact(t *testing.T) {
	overlay := modelOverlay{Hide: []string{"cline-pass/mimo-v2.5"}}
	if !modelOverlayMatchesAnyID(overlay, "cline-pass/mimo-v2.5") {
		t.Fatal("exact id must still match")
	}
	if modelOverlayMatchesAnyID(overlay, "cline-pass/mimo-v2.6-pro") {
		t.Fatal("an exact entry must not match a different id")
	}
	if !modelOverlayMatchesAnyID(modelOverlay{Hide: []string{"cline-pass/*"}}, "cline-pass/mimo-v2.6-pro") {
		t.Fatal("prefix entry must match a later model")
	}
}

// TestCatalogMergerDedupesSharedModels is the panel bug in miniature: two
// accounts both offer cline-free/kimi-k3, and the merged catalog CPA shows must
// list it once, not once per account.
func TestCatalogMergerDedupesSharedModels(t *testing.T) {
	m := newCatalogMerger()
	m.add([]pluginapi.ModelInfo{{ID: "cline-free/kimi-k3"}, {ID: "cline-free/solar-pro4"}},
		map[string][]string{"free": {"cline-free/kimi-k3"}}, "cline-recommended")
	m.add([]pluginapi.ModelInfo{{ID: "cline-free/kimi-k3"}, {ID: "cline-free/deepseek-v4.1-flash"}},
		map[string][]string{"free": {"cline-free/kimi-k3", "cline-free/deepseek-v4.1-flash"}}, "fallback")

	models, groups, source, ok := m.result(2)
	if !ok {
		t.Fatal("merged catalog must be usable")
	}
	if got := joinIDs(models); got != "cline-free/kimi-k3,cline-free/solar-pro4,cline-free/deepseek-v4.1-flash" {
		t.Fatalf("unexpected merge: %s", got)
	}
	if got := strings.Join(groups["free"], ","); got != "cline-free/kimi-k3,cline-free/deepseek-v4.1-flash" {
		t.Fatalf("group members must dedupe, got %s", got)
	}
	// One upstream-backed account is enough, and a later fallback must not
	// outrank it.
	if source != "cline-recommended" {
		t.Fatalf("want cline-recommended, got %s", source)
	}
}

// TestCatalogMergerNeedsAContributor keeps the fallback honest: accounts that
// exist but have no cached catalog yet must not produce an empty merged list,
// because an empty list would blank out every cline model in CPA.
func TestCatalogMergerNeedsAContributor(t *testing.T) {
	m := newCatalogMerger()
	if _, _, _, ok := m.result(0); ok {
		t.Fatal("no accounts must not report a catalog")
	}
	m.add(nil, nil, "cache")
	if _, _, _, ok := m.result(1); ok {
		t.Fatal("an account that contributed no models must not report a catalog")
	}
}

// TestMergeStoredAuthCatalogsUsesRealAccountCaches is the shipped bug: the panel
// read modelCache["global"], which nothing ever wrote, so it rendered the built
// in fallback forever and a model published after that list was cut could not be
// hidden from the UI even though CPA was serving it.
func TestMergeStoredAuthCatalogsUsesRealAccountCaches(t *testing.T) {
	modelCacheMu.Lock()
	previous := modelCache
	modelCache = map[string]cachedModelCatalog{
		"acct-cn": {
			Models:    []pluginapi.ModelInfo{{ID: "cline-free/kimi-k3"}, {ID: "cline-pass/mimo-v2.6-flash"}},
			Groups:    map[string][]string{"free": {"cline-free/kimi-k3"}},
			FetchedAt: time.Now(),
			Source:    "cline-recommended",
		},
		"acct-intl": {
			// The same model from a second account, plus one only this account sees.
			Models:    []pluginapi.ModelInfo{{ID: "cline-free/kimi-k3"}, {ID: "cline-free/solar-pro4"}},
			Groups:    map[string][]string{"free": {"cline-free/kimi-k3", "cline-free/solar-pro4"}},
			FetchedAt: time.Now(),
			Source:    "cline-recommended",
		},
	}
	modelCacheMu.Unlock()
	t.Cleanup(func() {
		modelCacheMu.Lock()
		modelCache = previous
		modelCacheMu.Unlock()
	})

	accounts := []*storedAuth{{Account: storedAccount{ID: "acct-cn"}}, {Account: storedAccount{ID: "acct-intl"}}}
	models, groups, source, ok := mergeStoredAuthCatalogs(accounts, false)
	if !ok {
		t.Fatal("account catalogs must be usable without the host")
	}
	if got := joinIDs(models); got != "cline-free/kimi-k3,cline-pass/mimo-v2.6-flash,cline-free/solar-pro4" {
		t.Fatalf("unexpected merged catalog: %s", got)
	}
	if got := strings.Join(groups["free"], ","); got != "cline-free/kimi-k3,cline-free/solar-pro4" {
		t.Fatalf("unexpected free group: %s", got)
	}
	if source != "cline-recommended" {
		t.Fatalf("want cline-recommended, got %s", source)
	}

	// The leaked model is now visible to the overlay, so a family prefix hides it.
	hidden := applyModelOverlay(models, modelOverlay{Hide: []string{"cline-pass/*"}})
	if got := joinIDs(hidden); strings.Contains(got, "mimo") {
		t.Fatalf("prefix must hide a model newer than the config: %s", got)
	}
}

// TestMergeStoredAuthCatalogsWithoutCacheKeepsFallback guards the other side: an
// account with no cached catalog yet must not produce an empty list, which would
// blank out every cline model in CPA.
func TestMergeStoredAuthCatalogsWithoutCacheKeepsFallback(t *testing.T) {
	modelCacheMu.Lock()
	previous := modelCache
	modelCache = map[string]cachedModelCatalog{}
	modelCacheMu.Unlock()
	t.Cleanup(func() {
		modelCacheMu.Lock()
		modelCache = previous
		modelCacheMu.Unlock()
	})

	if _, _, _, ok := mergeStoredAuthCatalogs([]*storedAuth{{Account: storedAccount{ID: "uncached"}}}, false); ok {
		t.Fatal("an uncached account must fall back, not serve an empty catalog")
	}
}

// withModelCache isolates the process-wide catalog cache for one test.
func withModelCache(t *testing.T, seed map[string]cachedModelCatalog) {
	t.Helper()
	modelCacheMu.Lock()
	previous := modelCache
	modelCache = seed
	modelCacheMu.Unlock()
	t.Cleanup(func() {
		modelCacheMu.Lock()
		modelCache = previous
		modelCacheMu.Unlock()
	})
}

// TestForceRefreshBypassesCacheTTL keeps the panel's refresh button honest. It
// used to route through the TTL check, so pressing 刷新 within five minutes
// re-served the cache and a model published upstream stayed invisible.
func TestForceRefreshBypassesCacheTTL(t *testing.T) {
	calls := 0
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"recommended":[{"id":"cline-pass/mimo-v2.6-flash","name":"Mimo"}],"free":[],"recommendedByProvider":[],"byok":[]}`))
	}))
	sa := testStoredAuth()
	withModelCache(t, map[string]cachedModelCatalog{
		accountCacheKey(sa): {
			Models:    []pluginapi.ModelInfo{{ID: "cline-free/stale"}},
			FetchedAt: time.Now(), // well inside the TTL
			Source:    "cline-recommended",
		},
	})

	if _, _, _, ok := mergeStoredAuthCatalogs([]*storedAuth{sa}, false); !ok {
		t.Fatal("the cached catalog must be usable without a refresh")
	}
	if calls != 0 {
		t.Fatalf("a non-forcing merge must not hit upstream, got %d calls", calls)
	}

	models, _, source, ok := mergeStoredAuthCatalogs([]*storedAuth{sa}, true)
	if !ok {
		t.Fatal("forced merge must produce a catalog")
	}
	if calls != 1 {
		t.Fatalf("want one forced upstream pull, got %d", calls)
	}
	// The forced pull replaces the cached entry. clientCompatibilityModels() is
	// still merged in by fetchRecommendedModels, so the assertions here are about
	// the stale model leaving and the new one arriving, not an exact list.
	joined := joinIDs(models)
	if !strings.Contains(joined, "cline-pass/mimo-v2.6-flash") {
		t.Fatalf("the newly published model must appear after a forced refresh: %s", joined)
	}
	if strings.Contains(joined, "cline-free/stale") {
		t.Fatalf("the stale cached model must be gone after a forced refresh: %s", joined)
	}
	if source != "cline-recommended" {
		t.Fatalf("a forced pull must report the upstream source, got %s", source)
	}
}

// TestFailedRefreshKeepsPreviousCatalog stops a hiccup from being destructive.
// The old path cached the built in fallback on any error, so one bad refresh
// made CPA serve the wrong list for a full TTL.
func TestFailedRefreshKeepsPreviousCatalog(t *testing.T) {
	withUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	sa := testStoredAuth()
	withModelCache(t, map[string]cachedModelCatalog{
		accountCacheKey(sa): {
			Models:    []pluginapi.ModelInfo{{ID: "cline-free/kimi-k3"}},
			Groups:    map[string][]string{"free": {"cline-free/kimi-k3"}},
			FetchedAt: time.Now().Add(-time.Hour), // expired, so a refresh is attempted
			Source:    "cline-recommended",
		},
	})

	models, _, source, ok := mergeStoredAuthCatalogs([]*storedAuth{sa}, true)
	if !ok {
		t.Fatal("a failed refresh must still serve the previous catalog")
	}
	if got := joinIDs(models); got != "cline-free/kimi-k3" {
		t.Fatalf("want the previous catalog kept, got %s", got)
	}
	if source != "cline-recommended" {
		t.Fatalf("must keep the real source, not %s", source)
	}
}
