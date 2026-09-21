package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostAuthFileEntry struct {
	Name      string `json:"name"`
	AuthIndex string `json:"auth_index"`
	Provider  string `json:"provider"`
	Type      string `json:"type"`
	Path      string `json:"path"`
}

// effectiveAuthName returns the physical auth file name. The host can report a
// registered runtime name ("cline.json") while Path points at the real
// per-account file ("cline-usr-....json"); rename/delete must use the latter.
func effectiveAuthName(file hostAuthFileEntry) string {
	if base := strings.TrimSpace(filepath.Base(strings.TrimSpace(file.Path))); base != "" && base != "." {
		return base
	}
	return strings.TrimSpace(file.Name)
}

type rpcHostAuthListResponse struct {
	Files []hostAuthFileEntry `json:"files"`
}

type rpcHostAuthGetResponse struct {
	JSON json.RawMessage `json:"json"`
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
	return decodeHostAuthGetResponse(raw)
}

// decodeHostAuthGetResponse unwraps one host.auth.get reply.
//
// The response's "json" field is a JSON object, not a base64 string, so it must
// be decoded into json.RawMessage. Decoding it into []byte fails with a type
// error that used to be reported as a misleading "bad envelope".
func decodeHostAuthGetResponse(raw []byte) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return nil, fmt.Errorf("host.auth.get failed")
	}
	var resp rpcHostAuthGetResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		return nil, fmt.Errorf("host.auth.get decode result: %w", err)
	}
	if len(resp.JSON) == 0 {
		return nil, fmt.Errorf("host.auth.get returned empty credential JSON")
	}
	return cloneJSON(resp.JSON), nil
}

// cloneJSON returns a standalone copy so callers never hold an alias into the
// RPC response buffer.
func cloneJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	return append([]byte(nil), raw...)
}

// authFileDocument is the physical credential file as CPA stores it. Rename
// rewrites the note in place; delete asks the host to drop the whole record.
type authFileDocument struct {
	Name string `json:"name"`
	JSON []byte `json:"json"`
}

// noteFromAuthFile returns the host-level note attached to a stored auth file.
func noteFromAuthFile(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var doc struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return strings.TrimSpace(doc.Note)
}

// setAuthFileNote rewrites one auth file with note replaced, preserving every
// other field. The host derives panel labels from this file, so a partial
// write would silently drop credential data.
func setAuthFileNote(name string, raw []byte, note string) error {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode stored auth: %w", err)
	}
	if note == "" {
		delete(doc, "note")
	} else {
		doc["note"] = note
	}
	updated, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode stored auth: %w", err)
	}
	return hostAuthSaveJSON(name, updated)
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
