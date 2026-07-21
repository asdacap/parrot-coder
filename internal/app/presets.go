package app

import (
	"sort"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/config"
)

// providerPreset supplies built-in defaults for a well-known provider ID so a
// user only has to supply a credential. Every field a user sets in parrot.yaml
// overrides the preset; the preset only fills what was left empty.
type providerPreset struct {
	Protocol      string
	BaseURL       string
	APIKeyEnv     string
	HeaderTimeout time.Duration
	Models        map[string]config.Model
}

// providerPresets are built-in, not configuration, so a project-scope file
// cannot redirect a preset provider's base URL. Model metadata is a starting
// point that a user may override per model in parrot.yaml.
var providerPresets = map[string]providerPreset{
	"openai": {HeaderTimeout: 10 * time.Second},
	// openrouter is the OpenRouter aggregation API. It speaks the
	// chat-completions protocol and serves a large, frequently changing catalog
	// from /api/v1/models, so the preset carries no model metadata: the fetched
	// catalog is authoritative. Model IDs include a vendor prefix (such as
	// "openai/gpt-4o"); model selection splits on the first slash, so the
	// model portion keeps the vendor prefix, as in
	// "openrouter/openai/gpt-4o".
	"openrouter": {
		Protocol:      "chat-completions",
		BaseURL:       "https://openrouter.ai/api/v1",
		APIKeyEnv:     "OPENROUTER_API_KEY",
		HeaderTimeout: 10 * time.Second,
	},
	// opencode-go is the OpenCode Go low-cost subscription for open coding
	// models. It speaks the chat-completions protocol and serves a curated,
	// frequently changing catalog from /v1/models, so the preset carries no
	// model metadata: the fetched catalog is authoritative. Model IDs carry
	// no vendor prefix (e.g. "glm-5.2", "kimi-k3"), so model selection splits
	// "opencode-go/<model-id>" cleanly on the first slash. The endpoint has
	// no balance route, so parrot usage reports nothing for it.
	"opencode-go": {
		Protocol:      "chat-completions",
		BaseURL:       "https://opencode.ai/zen/go/v1",
		APIKeyEnv:     "OPENCODE_GO_API_KEY",
		HeaderTimeout: 10 * time.Second,
	},
	// kimi-code is the Kimi For Coding subscription. Its endpoint serves the
	// plan rather than an account balance, so it reports no usage, and it
	// serves a single plan-named model rather than the platform model IDs.
	"kimi-code": {
		Protocol:  "chat-completions",
		BaseURL:   "https://api.kimi.com/coding/v1",
		APIKeyEnv: "KIMI_API_KEY",
		Models: map[string]config.Model{
			"kimi-for-coding": {Name: "Kimi For Coding", Context: 262144, Tools: true, Reasoning: true},
		},
	},
	// kimi-api is the Moonshot platform API, billed against a prepaid balance.
	"kimi-api": {
		Protocol:  "chat-completions",
		BaseURL:   "https://api.moonshot.ai/v1",
		APIKeyEnv: "MOONSHOT_API_KEY",
		Models: map[string]config.Model{
			"kimi-k2-thinking":      {Name: "Kimi K2 Thinking", Context: 262144, MaxTokens: 32768, Tools: true, Reasoning: true},
			"kimi-k2-turbo-preview": {Name: "Kimi K2 Turbo", Context: 262144, MaxTokens: 32768, Tools: true},
			"kimi-k2-0905-preview":  {Name: "Kimi K2 0905", Context: 262144, MaxTokens: 32768, Tools: true},
		},
	},
}

// PresetProviderIDs lists the provider IDs that carry built-in defaults, so a
// caller can offer them before any configuration or credential exists.
func PresetProviderIDs() []string {
	ids := make([]string, 0, len(providerPresets))
	for id := range providerPresets {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// applyProviderPreset fills the fields a user left empty with built-in defaults.
func applyProviderPreset(id string, item config.Provider) config.Provider {
	preset, ok := providerPresets[id]
	if !ok {
		return item
	}
	if item.Protocol == "" {
		item.Protocol = preset.Protocol
	}
	if item.BaseURL == "" {
		item.BaseURL = preset.BaseURL
	}
	if item.APIKeyEnv == "" {
		item.APIKeyEnv = preset.APIKeyEnv
	}
	if item.HeaderTimeoutMS == nil && preset.HeaderTimeout > 0 {
		milliseconds := int(preset.HeaderTimeout / time.Millisecond)
		item.HeaderTimeoutMS = &milliseconds
	}
	// Models are deliberately untouched. A preset describes models rather than
	// declaring them, so its metadata is supplied separately through
	// presetModelDefaults and never adds entries to a fetched catalog.
	return item
}

// presetModelDefaults returns the metadata a preset knows for models its
// endpoint may serve — context windows and reasoning variants a model list
// cannot express. Callers treat these as descriptions, not as a catalog.
func presetModelDefaults(id string) map[string]config.Model {
	return providerPresets[id].Models
}

// presetOnlyProviderIDs are preset providers with a usable base URL that the
// configuration does not mention at all. They are built from the preset alone
// so that storing a credential is enough to use them.
func presetOnlyProviderIDs(configured map[string]config.Provider) []string {
	ids := make([]string, 0, len(providerPresets))
	for id, preset := range providerPresets {
		if _, exists := configured[id]; exists || preset.BaseURL == "" {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
