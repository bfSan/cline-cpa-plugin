package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxModelIDBytes = 256

type modelOverlay struct {
	Hide  []string `json:"hide"`
	Order []string `json:"order"`
	Add   []string `json:"add"`
}

type modelOverlayState struct {
	Overlay  modelOverlay `json:"overlay"`
	Revision int64        `json:"revision"`
}

var (
	modelOverlayMu           sync.RWMutex
	currentModelOverlayState = modelOverlayState{}
	hiddenModelsFromConfig   []string
)

func configureModels(hidden []string) {
	normalized, err := normalizeModelIDList(hidden, "hidden_models")
	if err != nil {
		normalized = nil
	}
	modelOverlayMu.Lock()
	hiddenModelsFromConfig = normalized
	currentModelOverlayState.Overlay.Hide = append([]string(nil), normalized...)
	currentModelOverlayState.Revision++
	modelOverlayMu.Unlock()
}

func loadedConfiguredHiddenModels() []string {
	modelOverlayMu.RLock()
	defer modelOverlayMu.RUnlock()
	return append([]string(nil), hiddenModelsFromConfig...)
}

func setModelOverlayForTest(o modelOverlay) func() {
	modelOverlayMu.Lock()
	previous := currentModelOverlayState
	currentModelOverlayState.Overlay = o
	currentModelOverlayState.Revision++
	modelOverlayMu.Unlock()
	return func() {
		modelOverlayMu.Lock()
		currentModelOverlayState = previous
		modelOverlayMu.Unlock()
	}
}

func loadedModelOverlay() modelOverlay {
	modelOverlayMu.RLock()
	defer modelOverlayMu.RUnlock()
	overlay := currentModelOverlayState.Overlay
	return modelOverlay{
		Hide:  append([]string(nil), overlay.Hide...),
		Order: append([]string(nil), overlay.Order...),
		Add:   append([]string(nil), overlay.Add...),
	}
}

func loadedModelOverlayState() modelOverlayState {
	modelOverlayMu.RLock()
	defer modelOverlayMu.RUnlock()
	return modelOverlayState{
		Overlay: modelOverlay{
			Hide:  append([]string(nil), currentModelOverlayState.Overlay.Hide...),
			Order: append([]string(nil), currentModelOverlayState.Overlay.Order...),
			Add:   append([]string(nil), currentModelOverlayState.Overlay.Add...),
		},
		Revision: currentModelOverlayState.Revision,
	}
}

type modelConfigError struct {
	Field string `json:"field"`
	Msg   string `json:"message"`
}

func (e *modelConfigError) Error() string {
	if e.Field == "" {
		return e.Msg
	}
	return e.Field + ": " + e.Msg
}

func normalizeModelIDList(in []string, field string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, &modelConfigError{Field: field, Msg: "entries must not be empty"}
		}
		if strings.ContainsAny(id, "\r\n") {
			return nil, &modelConfigError{Field: field, Msg: "entries must be single-line strings"}
		}
		if len(id) > maxModelIDBytes {
			return nil, &modelConfigError{Field: field, Msg: "entry exceeds maximum ID length"}
		}
		if _, exists := seen[id]; exists {
			return nil, &modelConfigError{Field: field, Msg: "entries must not be duplicated"}
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func storeModelOverlay(next modelOverlay) (modelOverlayState, error) {
	hide, err := normalizeModelIDList(next.Hide, "hide")
	if err != nil {
		return modelOverlayState{}, err
	}
	order, err := normalizeModelIDList(next.Order, "order")
	if err != nil {
		return modelOverlayState{}, err
	}
	add, err := normalizeModelIDList(next.Add, "add")
	if err != nil {
		return modelOverlayState{}, err
	}
	modelOverlayMu.Lock()
	defer modelOverlayMu.Unlock()
	currentModelOverlayState.Overlay = modelOverlay{Hide: hide, Order: order, Add: add}
	currentModelOverlayState.Revision++
	return loadedModelOverlayStateLocked(), nil
}

func loadedModelOverlayStateLocked() modelOverlayState {
	return modelOverlayState{
		Overlay: modelOverlay{
			Hide:  append([]string(nil), currentModelOverlayState.Overlay.Hide...),
			Order: append([]string(nil), currentModelOverlayState.Overlay.Order...),
			Add:   append([]string(nil), currentModelOverlayState.Overlay.Add...),
		},
		Revision: currentModelOverlayState.Revision,
	}
}

func applyModelOverlay(base []pluginapi.ModelInfo, overlay modelOverlay) []pluginapi.ModelInfo {
	hidden := make(map[string]struct{}, len(overlay.Hide))
	for _, id := range overlay.Hide {
		hidden[strings.TrimSpace(id)] = struct{}{}
	}
	pinned := make(map[string]int, len(overlay.Order))
	for index, id := range overlay.Order {
		pinned[strings.TrimSpace(id)] = index
	}

	pinnedOut := make([]pluginapi.ModelInfo, 0, len(overlay.Order))
	kept := make([]pluginapi.ModelInfo, 0, len(base)+len(overlay.Add))
	for _, model := range base {
		id := strings.TrimSpace(model.ID)
		if _, gone := hidden[id]; gone {
			continue
		}
		if _, isPinned := pinned[id]; isPinned {
			pinnedOut = append(pinnedOut, model)
			continue
		}
		kept = append(kept, model)
	}
	sort.SliceStable(pinnedOut, func(i, j int) bool {
		return pinned[strings.TrimSpace(pinnedOut[i].ID)] < pinned[strings.TrimSpace(pinnedOut[j].ID)]
	})
	out := make([]pluginapi.ModelInfo, 0, len(pinnedOut)+len(kept)+len(overlay.Add))
	out = append(out, pinnedOut...)
	out = append(out, kept...)
	for _, id := range overlay.Add {
		id = strings.TrimSpace(id)
		if _, gone := hidden[id]; gone {
			continue
		}
		out = append(out, defaultModelInfo(id, id))
	}
	return out
}

func applyModelOverlayForAdmin(base []pluginapi.ModelInfo, overlay modelOverlay) []pluginapi.ModelInfo {
	visible := applyModelOverlay(base, overlay)
	hidden := make(map[string]struct{}, len(overlay.Hide))
	for _, id := range overlay.Hide {
		hidden[strings.TrimSpace(id)] = struct{}{}
	}
	if len(hidden) == 0 {
		return visible
	}
	seen := make(map[string]struct{}, len(visible)+len(hidden))
	out := make([]pluginapi.ModelInfo, 0, len(visible)+len(hidden))
	for _, model := range visible {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}
	for _, model := range base {
		id := strings.TrimSpace(model.ID)
		if _, isHidden := hidden[id]; !isHidden {
			continue
		}
		if _, already := seen[id]; already {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}
	return out
}

func marshalOverlayState(state modelOverlayState) json.RawMessage {
	raw, _ := json.Marshal(state)
	return raw
}
