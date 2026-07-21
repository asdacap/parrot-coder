package app

import (
	"sort"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/provider"
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
	Decoder       provider.ModelListDecoder
	// SupportsProviderPreferences marks providers that read a top-level
	// "provider" object from the request body to steer routing, such as the
	// OpenRouter aggregation API. Only these providers receive configured
	// provider preferences; other providers never see the field.
	SupportsProviderPreferences bool
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
		Decoder:       provider.DecodeOpenRouterModels,
		// OpenRouter routes across many backing providers and accepts a
		// "provider" object to influence that routing.
		SupportsProviderPreferences: true,
	},
	// opencode-go is the OpenCode Go low-cost subscription for open coding
	// models. It speaks the chat-completions protocol and serves a curated,
	// frequently changing catalog from /v1/models, so the preset carries no
	// model metadata: the fetched catalog is authoritative. Model IDs carry
	// no vendor prefix (e.g. "glm-5.2", "kimi-k3"), so model selection splits
	// "opencode-go/<model-id>" cleanly on the first slash. The /zen/go/v1/usage
	// endpoint reports subscription usage for the Go plan.
	//
	// The Models map supplies metadata (context window, max tokens, reasoning
	// variants) that the endpoint's model list does not expose. Each entry
	// describes a model the endpoint serves; unknown IDs are silently dropped
	// from the final catalog. Metadata is sourced from OpenRouter, which is
	// the upstream routing layer for OpenCode Go.
	"opencode-go": {
		Protocol:      "chat-completions",
		BaseURL:       "https://opencode.ai/zen/go/v1",
		APIKeyEnv:     "OPENCODE_GO_API_KEY",
		HeaderTimeout: 10 * time.Second,
		Models: map[string]config.Model{
			"minimax-m3": {
				Name: "MiniMax M3", Context: 1048576, MaxTokens: 512000, Tools: true,
			},
			"minimax-m2.7": {
				Name: "MiniMax M2.7", Context: 204800, MaxTokens: 131072, Tools: true,
			},
			"minimax-m2.5": {
				Name: "MiniMax M2.5", Context: 204800, MaxTokens: 196608, Tools: true,
			},
			"kimi-k3": {
				Name: "Kimi K3", Context: 1048576, Tools: true, Reasoning: true,
				Variants: map[string]config.Variant{
					"max":  {ReasoningEffort: "max"},
					"high": {ReasoningEffort: "high"},
					"low":  {ReasoningEffort: "low"},
				},
			},
			"kimi-k2.7-code": {
				Name: "Kimi K2.7 Code", Context: 262144, MaxTokens: 262144, Tools: true,
			},
			"kimi-k2.6": {
				Name: "Kimi K2.6", Context: 262144, MaxTokens: 262144, Tools: true,
			},
			"kimi-k2.5": {
				Name: "Kimi K2.5", Context: 262144, MaxTokens: 262144, Tools: true,
			},
			"glm-5.2": {
				Name: "GLM 5.2", Context: 1048576, MaxTokens: 131072, Tools: true, Reasoning: true,
				Variants: map[string]config.Variant{
					"xhigh": {ReasoningEffort: "xhigh"},
					"high":  {ReasoningEffort: "high"},
				},
			},
			"glm-5.1": {
				Name: "GLM 5.1", Context: 202752, MaxTokens: 128000, Tools: true,
			},
			"glm-5": {
				Name: "GLM 5", Context: 204800, MaxTokens: 131072, Tools: true,
			},
			"deepseek-v4-pro": {
				Name: "DeepSeek V4 Pro", Context: 1048576, MaxTokens: 384000, Tools: true, Reasoning: true,
				Variants: map[string]config.Variant{
					"xhigh": {ReasoningEffort: "xhigh"},
					"high":  {ReasoningEffort: "high"},
				},
			},
			"deepseek-v4-flash": {
				Name: "DeepSeek V4 Flash", Context: 1048576, Tools: true, Reasoning: true,
				Variants: map[string]config.Variant{
					"xhigh": {ReasoningEffort: "xhigh"},
					"high":  {ReasoningEffort: "high"},
				},
			},
			"qwen3.7-max": {
				Name: "Qwen3.7 Max", Context: 1000000, MaxTokens: 65536, Tools: true,
			},
			"qwen3.7-plus": {
				Name: "Qwen3.7 Plus", Context: 1000000, MaxTokens: 65536, Tools: true,
			},
			"qwen3.6-plus": {
				Name: "Qwen3.6 Plus", Context: 1000000, MaxTokens: 65536, Tools: true,
			},
			"qwen3.5-plus": {
				Name: "Qwen3.5 Plus", Context: 1000000, MaxTokens: 65536, Tools: true,
			},
			"mimo-v2.5-pro": {
				Name: "MiMo V2.5 Pro", Context: 1048576, MaxTokens: 131072, Tools: true,
			},
			"mimo-v2.5": {
				Name: "MiMo V2.5", Context: 1048576, MaxTokens: 131072, Tools: true,
			},
			"mimo-v2-pro": {
				Name: "MiMo V2 Pro", Tools: true,
			},
			"mimo-v2-omni": {
				Name: "MiMo V2 Omni", Tools: true,
			},
			"hy3-preview": {
				Name: "Hy3 Preview", Context: 262144, Tools: true, Reasoning: true,
				Variants: map[string]config.Variant{
					"high": {ReasoningEffort: "high"},
					"low":  {ReasoningEffort: "low"},
					"none": {ReasoningEffort: "none"},
				},
			},
			"grok-4.5": {
				Name: "Grok 4.5", Context: 500000, Tools: true, Reasoning: true,
				Variants: map[string]config.Variant{
					"high":   {ReasoningEffort: "high"},
					"medium": {ReasoningEffort: "medium"},
					"low":    {ReasoningEffort: "low"},
				},
			},
		},
	},
	// kimi-code is the Kimi For Coding subscription. Its endpoint serves the
	// plan rather than an account balance, so it reports no usage, and it
	// serves a single plan-named model rather than the platform model IDs.
	"kimi-code": {
		Protocol:  "chat-completions",
		BaseURL:   "https://api.kimi.com/coding/v1",
		APIKeyEnv: "KIMI_API_KEY",
		Decoder:   provider.DecodeKimiModels,
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

// presetDecoder returns the model list decoder a preset selects, or nil to
// select the default standard decoder.
func presetDecoder(id string) provider.ModelListDecoder {
	return providerPresets[id].Decoder
}

// presetSupportsProviderPreferences reports whether a preset's provider reads
// the top-level "provider" request-body object. Only such providers receive
// configured provider preferences, so the field is never sent to a provider
// that does not understand it.
func presetSupportsProviderPreferences(id string) bool {
	return providerPresets[id].SupportsProviderPreferences
}
