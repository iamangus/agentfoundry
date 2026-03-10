package config

import (
	"os"
	"strings"

	"github.com/angoo/agentfile/internal/mcpclient"
	"gopkg.in/yaml.v3"
)

// SystemConfig is the top-level configuration for the agentfile daemon.
type SystemConfig struct {
	Listen         string                   `yaml:"listen"`
	DefinitionsDir string                   `yaml:"definitions_dir"`
	OpenRouter     OpenRouterConf           `yaml:"openrouter"`
	MCPServers     []mcpclient.ServerConfig `yaml:"mcp_servers"`
}

type OpenRouterConf struct {
	APIKey       string `yaml:"api_key"`
	DefaultModel string `yaml:"default_model"`
}

func DefaultSystem() *SystemConfig {
	return &SystemConfig{
		Listen:         ":3000",
		DefinitionsDir: "./definitions",
		OpenRouter: OpenRouterConf{
			APIKey:       os.Getenv("OPENROUTER_API_KEY"),
			DefaultModel: "openai/gpt-4o",
		},
	}
}

func LoadSystem(path string) (*SystemConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := DefaultSystem()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// Expand environment variables in api_key
	if strings.HasPrefix(cfg.OpenRouter.APIKey, "${") && strings.HasSuffix(cfg.OpenRouter.APIKey, "}") {
		envVar := cfg.OpenRouter.APIKey[2 : len(cfg.OpenRouter.APIKey)-1]
		cfg.OpenRouter.APIKey = os.Getenv(envVar)
	}

	// Also check env var directly if still empty
	if cfg.OpenRouter.APIKey == "" {
		cfg.OpenRouter.APIKey = os.Getenv("OPENROUTER_API_KEY")
	}

	return cfg, nil
}
