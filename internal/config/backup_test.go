package config

import (
	"encoding/json"
	"gopkg.in/yaml.v3"
	"testing"
)

func TestBackupDefinitionRoundTrip(t *testing.T) {
	def := Definition{Name: "test", Kind: KindAgent, SystemPrompt: "help", ProviderID: "primary", BackupProviderID: "backup", BackupModel: "backup-model"}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"yaml", "json"} {
		var data []byte
		var err error
		var result Definition
		if format == "yaml" {
			data, err = yaml.Marshal(def)
			if err == nil {
				err = yaml.Unmarshal(data, &result)
			}
		} else {
			data, err = json.Marshal(def)
			if err == nil {
				err = json.Unmarshal(data, &result)
			}
		}
		if err != nil || result.BackupProviderID != "backup" || result.BackupModel != "backup-model" {
			t.Fatalf("%s: %+v, %v", format, result, err)
		}
	}
	def.ProviderID = ""
	if def.Validate() == nil {
		t.Fatal("accepted backup without primary")
	}
	def.BackupProviderID = ""
	if def.Validate() == nil {
		t.Fatal("accepted backup model without provider")
	}
}
