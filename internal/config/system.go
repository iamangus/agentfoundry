package config

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type TemporalConf struct {
	HostPort  string `yaml:"host_port"`
	Namespace string `yaml:"namespace"`
	APIKey    string `yaml:"api_key"`
}

type LLMConf struct {
	BaseURL          string            `yaml:"base_url"`
	APIKey           string            `yaml:"api_key"`
	DefaultModel     string            `yaml:"default_model"`
	Headers          map[string]string `yaml:"headers"`
	SchemaValidation bool              `yaml:"schema_validation"`
}

type SystemConfig struct {
	Listen         string       `yaml:"listen"`
	InternalAPIKey string       `yaml:"internal_api_key"`
	Temporal       TemporalConf `yaml:"temporal"`
	LLM            LLMConf      `yaml:"llm"`
}

func DefaultSystem() *SystemConfig {
	return &SystemConfig{
		Listen: ":3000",
		Temporal: TemporalConf{
			HostPort:  "localhost:7233",
			Namespace: "default",
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

	if cfg.Temporal.HostPort == "" {
		cfg.Temporal.HostPort = os.Getenv("TEMPORAL_HOST_PORT")
		if cfg.Temporal.HostPort == "" {
			cfg.Temporal.HostPort = "localhost:7233"
		}
	}
	if cfg.Temporal.Namespace == "" {
		cfg.Temporal.Namespace = "default"
	}
	if cfg.Temporal.APIKey == "" {
		cfg.Temporal.APIKey = os.Getenv("TEMPORAL_API_KEY")
	}
	cfg.Temporal.APIKey = expandEnvVar(cfg.Temporal.APIKey)

	cfg.LLM.BaseURL = expandEnvVar(cfg.LLM.BaseURL)
	cfg.LLM.DefaultModel = expandEnvVar(cfg.LLM.DefaultModel)
	cfg.LLM.APIKey = expandEnvVar(cfg.LLM.APIKey)

	if cfg.LLM.BaseURL == "" {
		cfg.LLM.BaseURL = os.Getenv("LLM_BASE_URL")
	}
	if cfg.LLM.DefaultModel == "" {
		cfg.LLM.DefaultModel = os.Getenv("LLM_DEFAULT_MODEL")
	}
	if cfg.LLM.APIKey == "" {
		cfg.LLM.APIKey = os.Getenv("LLM_API_KEY")
	}
	if cfg.LLM.APIKey == "" {
		cfg.LLM.APIKey = os.Getenv("OPENROUTER_API_KEY")
	}

	for k, v := range cfg.LLM.Headers {
		cfg.LLM.Headers[k] = expandEnvVar(v)
	}

	return cfg, nil
}

func expandEnvVar(v string) string {
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		envVar := v[2 : len(v)-1]
		return os.Getenv(envVar)
	}
	return v
}
