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
	LLM            LLMConf                  `yaml:"llm"`
	MCPServers     []mcpclient.ServerConfig `yaml:"mcp_servers"`
	SummaryAgent   string                   `yaml:"summary_agent,omitempty"`

	// OpenRouter is the legacy config key. If present and LLM is not configured,
	// it is used as a shorthand that sets BaseURL to OpenRouter and adds the
	// OpenRouter-specific headers automatically.
	OpenRouter *OpenRouterConf `yaml:"openrouter,omitempty"`
}

// LLMConf configures the OpenAI-compatible LLM provider.
type LLMConf struct {
	// BaseURL is the API base URL. The client appends "/chat/completions".
	// Defaults to "https://openrouter.ai/api/v1" for backward compatibility.
	BaseURL      string            `yaml:"base_url"`
	APIKey       string            `yaml:"api_key"`
	DefaultModel string            `yaml:"default_model"`
	Headers      map[string]string `yaml:"headers"`
}

// OpenRouterConf is the legacy configuration format.
type OpenRouterConf struct {
	APIKey       string `yaml:"api_key"`
	DefaultModel string `yaml:"default_model"`
}

func DefaultSystem() *SystemConfig {
	return &SystemConfig{
		Listen:         ":3000",
		DefinitionsDir: "./definitions",
		LLM: LLMConf{
			BaseURL:      "https://openrouter.ai/api/v1",
			APIKey:       os.Getenv("OPENROUTER_API_KEY"),
			DefaultModel: "openai/gpt-4o",
			Headers: map[string]string{
				"HTTP-Referer": "https://github.com/angoo/agentfile",
				"X-Title":      "agentfile",
			},
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

	// Backward compatibility: if the legacy "openrouter" key is set and the
	// new "llm" section was not explicitly provided, migrate the values.
	if cfg.OpenRouter != nil {
		if cfg.LLM.APIKey == "" || cfg.LLM.APIKey == os.Getenv("OPENROUTER_API_KEY") {
			cfg.LLM.BaseURL = "https://openrouter.ai/api/v1"
			cfg.LLM.APIKey = cfg.OpenRouter.APIKey
			if cfg.OpenRouter.DefaultModel != "" {
				cfg.LLM.DefaultModel = cfg.OpenRouter.DefaultModel
			}
			// Ensure OpenRouter-specific headers are set.
			if cfg.LLM.Headers == nil {
				cfg.LLM.Headers = make(map[string]string)
			}
			if _, ok := cfg.LLM.Headers["HTTP-Referer"]; !ok {
				cfg.LLM.Headers["HTTP-Referer"] = "https://github.com/angoo/agentfile"
			}
			if _, ok := cfg.LLM.Headers["X-Title"]; !ok {
				cfg.LLM.Headers["X-Title"] = "agentfile"
			}
		}
		cfg.OpenRouter = nil // clear after migration
	}

	// Expand environment variables in api_key (supports ${ENV_VAR} syntax).
	cfg.LLM.APIKey = expandEnvVar(cfg.LLM.APIKey)

	// Also check env var directly if still empty.
	if cfg.LLM.APIKey == "" {
		cfg.LLM.APIKey = os.Getenv("OPENROUTER_API_KEY")
	}

	// Expand environment variables in LLM header values.
	for k, v := range cfg.LLM.Headers {
		cfg.LLM.Headers[k] = expandEnvVar(v)
	}

	// Expand environment variables in MCP server header values.
	for i := range cfg.MCPServers {
		for k, v := range cfg.MCPServers[i].Headers {
			cfg.MCPServers[i].Headers[k] = expandEnvVar(v)
		}
	}

	// Apply env var fallback for summary agent.
	if cfg.SummaryAgent == "" {
		cfg.SummaryAgent = os.Getenv("TOOL_SUMMARY_AGENT")
	}

	return cfg, nil
}

// expandEnvVar expands a value of the form "${VAR}" to the environment variable's value.
// If the value is not in that form, it is returned unchanged.
func expandEnvVar(v string) string {
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		envVar := v[2 : len(v)-1]
		return os.Getenv(envVar)
	}
	return v
}
