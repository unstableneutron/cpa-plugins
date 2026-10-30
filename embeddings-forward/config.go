package main

import (
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	apiDecisions = "decisions"
	apiSystemOne = "systemone"

	defaultMaxConcurrency = 4
)

// modelMapping maps a client-visible alias to the provider's upstream model name.
type modelMapping struct {
	Name  string `yaml:"name"`
	Alias string `yaml:"alias"`
	// StringInstructions JSON-encodes non-string question instructions for
	// models that accept only string instructions.
	StringInstructions bool `yaml:"string_instructions"`
}

// provider is one upstream endpoint with its own keys and model mappings.
type provider struct {
	Name    string         `yaml:"name"`
	BaseURL string         `yaml:"base_url"`
	Path    string         `yaml:"path"`
	APIKeys []string       `yaml:"api_keys"`
	Models  []modelMapping `yaml:"models"`
	// Strategy is "failover" (default: start at the first key) or "round_robin".
	Strategy string `yaml:"strategy"`
	// CooldownSeconds moves a key that returned 429 or 402 to the end of the
	// key order for this long. Retry-After extends it. 0 disables.
	CooldownSeconds int `yaml:"cooldown_seconds"`
	// API selects the decisions wire format: "decisions" (default) or "systemone".
	API string `yaml:"api"`
	// MaxQuestions splits decision requests into batches of at most this many questions. 0 is unlimited.
	MaxQuestions int `yaml:"max_questions"`
	// MaxConcurrency bounds concurrent batches of one request on this provider.
	MaxConcurrency int `yaml:"max_concurrency"`

	url string
}

type module struct {
	Enabled   bool       `yaml:"enabled"`
	Providers []provider `yaml:"providers"`
}

type pluginConfig struct {
	Embeddings module `yaml:"embeddings"`
	Rerank     module `yaml:"rerank"`
	Decisions  module `yaml:"decisions"`
	// SystemOne is the upstream plugin's separate System One module. Its
	// providers join the decisions pool and default to the System One API.
	SystemOne module `yaml:"systemone"`
}

func parseConfig(raw []byte) (pluginConfig, error) {
	var cfg pluginConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return pluginConfig{}, fmt.Errorf("parse config: %w", err)
	}
	if cfg.SystemOne.Enabled {
		for _, p := range cfg.SystemOne.Providers {
			if p.API == "" {
				p.API = apiSystemOne
			}
			cfg.Decisions.Providers = append(cfg.Decisions.Providers, p)
		}
		cfg.Decisions.Enabled = true
	}
	cfg.SystemOne = module{}
	for _, m := range []struct {
		name string
		mod  *module
		path func(*provider) string
	}{
		{"embeddings", &cfg.Embeddings, func(*provider) string { return "/embeddings" }},
		{"rerank", &cfg.Rerank, func(*provider) string { return "/rerank" }},
		{"decisions", &cfg.Decisions, func(p *provider) string {
			if p.API == apiSystemOne {
				return "/v1/systemone"
			}
			return "/alpha/decisions"
		}},
	} {
		if !m.mod.Enabled {
			continue
		}
		if len(m.mod.Providers) == 0 {
			return pluginConfig{}, fmt.Errorf("%s: enabled without providers", m.name)
		}
		seen := map[string]bool{}
		for i := range m.mod.Providers {
			p := &m.mod.Providers[i]
			if err := p.normalize(m.name == "decisions", m.path(p)); err != nil {
				return pluginConfig{}, fmt.Errorf("%s provider %d: %w", m.name, i, err)
			}
			if seen[p.Name] {
				return pluginConfig{}, fmt.Errorf("%s: duplicate provider name %q", m.name, p.Name)
			}
			seen[p.Name] = true
		}
	}
	return cfg, nil
}

func (p *provider) normalize(decisions bool, defaultPath string) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(p.APIKeys) == 0 {
		return fmt.Errorf("provider %q has no api_keys", p.Name)
	}
	for _, key := range p.APIKeys {
		if strings.TrimSpace(key) == "" || strings.ContainsAny(key, "\r\n") {
			return fmt.Errorf("provider %q has an empty or invalid api key", p.Name)
		}
	}
	switch strings.ToLower(strings.TrimSpace(p.Strategy)) {
	case "", "failover":
		p.Strategy = "failover"
	case "round_robin":
		p.Strategy = "round_robin"
	default:
		return fmt.Errorf("provider %q: unknown strategy %q", p.Name, p.Strategy)
	}
	if decisions {
		switch p.API {
		case "":
			p.API = apiDecisions
		case apiDecisions, apiSystemOne:
		default:
			return fmt.Errorf("provider %q: api must be decisions or systemone", p.Name)
		}
	} else if p.API != "" || p.MaxQuestions != 0 || p.MaxConcurrency != 0 {
		return fmt.Errorf("provider %q: api, max_questions and max_concurrency apply only to decisions", p.Name)
	}
	if p.CooldownSeconds < 0 || p.MaxQuestions < 0 || p.MaxConcurrency < 0 {
		return fmt.Errorf("provider %q: numeric limits must not be negative", p.Name)
	}
	if p.MaxConcurrency == 0 {
		p.MaxConcurrency = defaultMaxConcurrency
	}
	for _, m := range p.Models {
		if strings.TrimSpace(m.Name) == "" {
			return fmt.Errorf("provider %q has a model without a name", p.Name)
		}
	}
	path := strings.TrimSpace(p.Path)
	if path == "" {
		path = defaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(p.BaseURL), "/") + path)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(parsed.Path, "/..") {
		return fmt.Errorf("provider %q: base_url and path must form an HTTP(S) URL without credentials, query or fragment", p.Name)
	}
	p.url = parsed.String()
	return nil
}

// match is a provider that serves the requested model, with its upstream mapping.
type match struct {
	provider *provider
	mapping  modelMapping
}

// resolve returns providers serving the client model in config order. A
// provider without models is a catch-all and forwards the model unchanged.
func (m *module) resolve(clientModel string) []match {
	var out []match
	for i := range m.Providers {
		p := &m.Providers[i]
		if len(p.Models) == 0 {
			out = append(out, match{provider: p, mapping: modelMapping{Name: clientModel}})
			continue
		}
		for _, mm := range p.Models {
			alias := mm.Alias
			if alias == "" {
				alias = mm.Name
			}
			if alias == clientModel || mm.Name == clientModel {
				out = append(out, match{provider: p, mapping: mm})
				break
			}
		}
	}
	return out
}
