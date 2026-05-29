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
