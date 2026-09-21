package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostAuthFileEntry struct {
	Name      string `json:"name"`
	AuthIndex string `json:"auth_index"`
}

type rpcHostAuthListResponse struct {
	Files []hostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	JSON []byte `json:"json"`
}

func hostAuthListFiles() ([]hostAuthFileEntry, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, nil)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.list failed")
	}
	var resp rpcHostAuthListResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

func hostAuthGetByIndex(authIndex string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"auth_index": authIndex})
	raw, err := hostCall(pluginabi.MethodHostAuthGet, body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get failed")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, err
	}
	return resp.JSON, nil
}

// authFileNameFor gives each Cline account its own file so several accounts can
// coexist under one provider.
func authFileNameFor(sa *storedAuth) string {
	if sa == nil {
		return authFileName
	}
	id := strings.TrimSpace(sa.Account.ID)
	if id == "" {
		id = strings.TrimSpace(sa.Account.Email)
	}
	if id == "" {
		return authFileName
	}
	return providerName + "-" + sanitizeFileToken(id) + ".json"
}

func sanitizeFileToken(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	trimmed := strings.Trim(string(out), "-")
	if trimmed == "" {
		return "account"
	}
	if len(trimmed) > 32 {
		trimmed = trimmed[:32]
	}
	return trimmed
}

// buildAuthFileJSON renders the physical credential file: nested storage under
// auth/account plus the host-level metadata CPA renders in the auth list.
func buildAuthFileJSON(sa *storedAuth) ([]byte, error) {
	if sa == nil {
		return nil, fmt.Errorf("nil stored auth")
	}
	storage, err := json.Marshal(sa)
	if err != nil {
		return nil, err
	}
	var nested map[string]any
	if err := json.Unmarshal(storage, &nested); err != nil {
		return nil, err
	}
	out := map[string]any{
		"type":     providerName,
		"provider": providerName,
		"disabled": false,
		"auth":     nested["auth"],
		"account":  nested["account"],
	}
	return json.Marshal(out)
}

// persistAuthData writes an auth produced by the OAuth flow to the host auth
// directory so CPA loads it without a restart.
func persistAuthData(auth pluginapi.AuthData) error {
	sa, err := parseStored(auth.StorageJSON)
	if err != nil {
		return err
	}
	fileJSON, err := buildAuthFileJSON(sa)
	if err != nil {
		return err
	}
	return hostAuthSaveJSON(authFileNameFor(sa), fileJSON)
}

func hostAuthSaveJSON(name string, raw []byte) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty auth file name")
	}
	body, _ := json.Marshal(pluginapi.HostAuthSaveRequest{Name: name, JSON: raw})
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, body)
	if err != nil {
		return fmt.Errorf("host.auth.save: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		msg := "host.auth.save failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
