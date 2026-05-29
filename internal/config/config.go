// Package config loads steven-reviewer's config.yml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Repo is a single ingestion target.
type Repo struct {
	Name   string `yaml:"name"`           // "owner/name"
	Tag    string `yaml:"tag"`            // "work" | "personal"
	Author string `yaml:"author,omitempty"` // override DefaultAuthor
}

// Config is the parsed shape of config.yml.
type Config struct {
	TokenPath     string `yaml:"token_path"`
	DBPath        string `yaml:"db_path"`
	MyAgentPath   string `yaml:"my_agent_path"`
	DefaultAuthor string `yaml:"default_author"`
	Repos         []Repo `yaml:"repos"`
	Hermes        Hermes `yaml:"hermes"`
}

// Hermes configures the local Hermes API server used for lesson proposal.
// URL/key may be sourced from env (HERMES_API_URL, HERMES_API_KEY) — config
// values win if both set.
type Hermes struct {
	URL    string `yaml:"url"`     // e.g. http://localhost:8642/v1
	Key    string `yaml:"key"`     // bearer token (API_SERVER_KEY)
	KeyEnv string `yaml:"key_env"` // alternate: read key from named env var
	Model  string `yaml:"model"`   // always "hermes-agent" unless changed
}

// Load reads and parses the config file at path. Tilde-prefixed paths are
// expanded against $HOME.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.TokenPath = expand(c.TokenPath)
	c.DBPath = expand(c.DBPath)
	c.MyAgentPath = expand(c.MyAgentPath)
	if c.DefaultAuthor == "" {
		c.DefaultAuthor = "Emyrk"
	}
	if c.DBPath == "" {
		c.DBPath = "./ingest.db"
	}
	for i := range c.Repos {
		if c.Repos[i].Author == "" {
			c.Repos[i].Author = c.DefaultAuthor
		}
	}
	// Hermes defaults + env fallback.
	if c.Hermes.URL == "" {
		c.Hermes.URL = os.Getenv("HERMES_API_URL")
	}
	if c.Hermes.URL == "" {
		c.Hermes.URL = "http://localhost:8642/v1"
	}
	if c.Hermes.Model == "" {
		c.Hermes.Model = "hermes-agent"
	}
	if c.Hermes.Key == "" && c.Hermes.KeyEnv != "" {
		c.Hermes.Key = os.Getenv(c.Hermes.KeyEnv)
	}
	if c.Hermes.Key == "" {
		c.Hermes.Key = os.Getenv("HERMES_API_KEY")
	}
	return &c, nil
}

// Token reads the PAT from c.TokenPath, stripping whitespace.
func (c *Config) Token() (string, error) {
	b, err := os.ReadFile(c.TokenPath)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("token file %s is empty", c.TokenPath)
	}
	return t, nil
}

func expand(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
