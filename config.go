package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"

	"gopkg.in/yaml.v3"
)

func configure(raw []byte) error {
	if len(raw) == 0 {
		configureModels(nil)
		return nil
	}
	// The host may send config_yaml either as a JSON string or as base64 bytes.
	var req struct {
		ConfigYAML json.RawMessage `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return errors.New("invalid plugin configuration")
	}
	configYAML, ok := decodeConfigYAML(req.ConfigYAML)
	if !ok || len(bytes.TrimSpace(configYAML)) == 0 {
		configureModels(nil)
		return nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(configYAML, &root); err != nil {
		return errors.New("invalid config_yaml")
	}
	configureModels(yamlStringList(&root, "hidden_models"))
	return nil
}

func decodeConfigYAML(raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		// Some host builds base64-encode the YAML payload.
		if decoded, errDecode := base64.StdEncoding.DecodeString(text); errDecode == nil && len(decoded) > 0 {
			return decoded, true
		}
		return []byte(text), true
	}
	var blob []byte
	if err := json.Unmarshal(raw, &blob); err == nil {
		return blob, true
	}
	return nil, false
}

func yamlStringList(root *yaml.Node, key string) []string {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	node := root.Content[0]
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value != key {
			continue
		}
		value := node.Content[i+1]
		if value.Kind != yaml.SequenceNode {
			return nil
		}
		out := make([]string, 0, len(value.Content))
		for _, item := range value.Content {
			if item.Kind == yaml.ScalarNode {
				out = append(out, item.Value)
			}
		}
		return out
	}
	return nil
}
