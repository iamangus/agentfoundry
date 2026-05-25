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

type SystemConfig struct {
	Listen         string       `yaml:"listen"`
	InternalAPIKey string       `yaml:"internal_api_key"`
	Temporal       TemporalConf `yaml:"temporal"`
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

	return cfg, nil
}

func expandEnvVar(v string) string {
	if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		envVar := v[2 : len(v)-1]
		return os.Getenv(envVar)
	}
	return v
}
