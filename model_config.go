package main

import (
	"encoding/json"
	"log"
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
		// A duplicate or over long entry should not cost the operator the whole
		// deny list, so fall back to the raw entries and let the per entry checks
		// below decide what survives.
		log.Printf("cline: hidden_models rejected: %v; validating per entry", err)
		normalized = trimStrings(hidden)
	}
	usable, rejected := sanitizeHideList(normalized)
	if len(rejected) > 0 {
		log.Printf("cline: hidden_models entries ignored, wildcard is only valid as a trailing *: %v", rejected)
	}
	if len(normalized) > 0 && len(usable) == 0 {
		// Saying nothing here would mean every cline model quietly reappears.
		log.Printf("cline: hidden_models had %d entries and all were rejected; nothing will be hidden", len(normalized))
	}
	modelOverlayMu.Lock()
	hiddenModelsFromConfig = usable
	currentModelOverlayState.Overlay.Hide = append([]string(nil), usable...)
	currentModelOverlayState.Revision++
	modelOverlayMu.Unlock()
}

// trimStrings normalizes whitespace without applying the ID rules, used only on
// the fallback path above.
func trimStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		if id := strings.TrimSpace(raw); id != "" {
			out = append(out, id)
		}
	}
	return out
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

// sanitizeHideList splits a deny list into usable entries and rejected ones.
//
// "*" is meaningful in exactly one place, at the end, where it turns an entry
// into a family prefix that also covers models published later. Anywhere else it
// reads like a substring match, which on a deny list would silently hide much
// more than it says, so those entries are refused instead of guessed at.
func sanitizeHideList(in []string) (ok []string, rejected []string) {
	ok = make([]string, 0, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		switch {
		case id == "":
			rejected = append(rejected, raw)
		case id == "*":
			// A bare star hides the entire catalog; always a mistake here.
			rejected = append(rejected, raw)
		case strings.Contains(strings.TrimSuffix(id, "*"), "*"):
			rejected = append(rejected, raw)
		default:
			ok = append(ok, id)
		}
	}
	return ok, rejected
}

func storeModelOverlay(next modelOverlay) (modelOverlayState, error) {
	hide, err := normalizeModelIDList(next.Hide, "hide")
	if err != nil {
		return modelOverlayState{}, err
	}
	hide, bad := sanitizeHideList(hide)
	if len(bad) > 0 {
		return modelOverlayState{}, &modelConfigError{Field: "hide",
			Msg: "wildcards are only allowed as a single trailing *, rejected: " + strings.Join(bad, ", ")}
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

// hiddenSet compiles the deny list.
//
// An entry matches exactly, or as a family prefix when it ends in "*":
// "cline-pass/*" keeps hiding models that upstream publishes tomorrow, which an
// enumeration cannot do. That is the whole point of the feature, so "*" is only
// special in that one trailing position. Leading and interior stars are rejected
// at config time rather than silently treated as substrings, because a deny list
// that quietly matches "kimi" across every provider hides far more than it reads.
type hiddenSet struct {
	exact  map[string]struct{}
	prefix []string
}

func newHiddenSet(ids []string) hiddenSet {
	set := hiddenSet{exact: make(map[string]struct{}, len(ids))}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if strings.HasSuffix(id, "*") {
			set.prefix = append(set.prefix, strings.TrimSuffix(id, "*"))
			continue
		}
		set.exact[id] = struct{}{}
	}
	return set
}

func (h hiddenSet) hides(id string) bool {
	if _, gone := h.exact[id]; gone {
		return true
	}
	for _, prefix := range h.prefix {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}

func (h hiddenSet) empty() bool { return len(h.exact) == 0 && len(h.prefix) == 0 }

func applyModelOverlay(base []pluginapi.ModelInfo, overlay modelOverlay) []pluginapi.ModelInfo {
	hidden := newHiddenSet(overlay.Hide)
	pinned := make(map[string]int, len(overlay.Order))
	for index, id := range overlay.Order {
		pinned[strings.TrimSpace(id)] = index
	}

	pinnedOut := make([]pluginapi.ModelInfo, 0, len(overlay.Order))
	kept := make([]pluginapi.ModelInfo, 0, len(base)+len(overlay.Add))
	for _, model := range base {
		id := strings.TrimSpace(model.ID)
		if hidden.hides(id) {
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
		if hidden.hides(id) {
			continue
		}
		out = append(out, defaultModelInfo(id, id))
	}
	return out
}

func applyModelOverlayForAdmin(base []pluginapi.ModelInfo, overlay modelOverlay) []pluginapi.ModelInfo {
	visible := applyModelOverlay(base, overlay)
	hidden := newHiddenSet(overlay.Hide)
	if hidden.empty() {
		return visible
	}
	seen := make(map[string]struct{}, len(visible)+len(overlay.Hide))
	out := make([]pluginapi.ModelInfo, 0, len(visible)+len(overlay.Hide))
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
		if !hidden.hides(id) {
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
