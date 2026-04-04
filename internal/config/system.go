package config

import (
	"os"
	"strings"

	"github.com/angoo/agentfoundry/internal/mcpclient"
	"gopkg.in/yaml.v3"
)

type TemporalConf struct {
	HostPort  string `yaml:"host_port"`
	Namespace string `yaml:"namespace"`
	APIKey    string `yaml:"api_key"`
}

type SystemConfig struct {
	Listen         string                   `yaml:"listen"`
	DefinitionsDir string                   `yaml:"definitions_dir"`
	Temporal       TemporalConf             `yaml:"temporal"`
	MCPServers     []mcpclient.ServerConfig `yaml:"mcp_servers"`
}

func DefaultSystem() *SystemConfig {
	return &SystemConfig{
		Listen:         ":3000",
		DefinitionsDir: "./definitions",
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

	for i := range cfg.MCPServers {
		for k, v := range cfg.MCPServers[i].Headers {
			cfg.MCPServers[i].Headers[k] = expandEnvVar(v)
		}
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
