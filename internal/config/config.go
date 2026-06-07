// Package config loads and validates the gateway's YAML configuration.
//
// The configuration file (config/router.yaml) is the single source of truth for
// WHICH providers exist and WHAT their routing characteristics are. Keeping this
// in YAML (instead of hard-coded in Go) means you can add/remove providers or
// re-tune cost/latency without recompiling.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Provider describes a single downstream AI provider the gateway can route to.
//
// IMPORTANT — the two scoring fields use a deliberate convention:
//
//	CostScore : a normalised COST-EFFICIENCY score in [0.0, 1.0].
//	            HIGHER means CHEAPER. (e.g. 0.9 is cheaper than 0.5)
//	LatencyMs : typical response time in milliseconds.
//	            LOWER means FASTER.
//
// This "higher score = better" convention (also used for cost) is what lets the
// router treat every signal uniformly: in every mode, a bigger number wins.
type Provider struct {
	ID        string  `yaml:"id"`
	URL       string  `yaml:"url"`
	CostScore float64 `yaml:"cost_score"`
	LatencyMs int     `yaml:"latency_ms"`
}

// Config is the top-level shape of router.yaml.
type Config struct {
	Providers []Provider `yaml:"providers"`
}

// Load reads, parses, and validates the YAML config at the given path.
// It returns a clear, wrapped error if anything is wrong so startup fails loudly.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q: %w", path, err)
	}
	return &cfg, nil
}

// validate enforces the minimum invariants the rest of the gateway relies on:
// at least one provider, unique IDs, non-empty URLs, and in-range scores.
func (c *Config) validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers defined")
	}

	seen := make(map[string]bool)
	for i, p := range c.Providers {
		if p.ID == "" {
			return fmt.Errorf("provider #%d: missing id", i)
		}
		if seen[p.ID] {
			return fmt.Errorf("duplicate provider id %q", p.ID)
		}
		seen[p.ID] = true

		if p.URL == "" {
			return fmt.Errorf("provider %q: missing url", p.ID)
		}
		if p.CostScore < 0 || p.CostScore > 1 {
			return fmt.Errorf("provider %q: cost_score %.2f out of range [0,1]", p.ID, p.CostScore)
		}
		if p.LatencyMs < 0 {
			return fmt.Errorf("provider %q: latency_ms must be >= 0", p.ID)
		}
	}
	return nil
}
