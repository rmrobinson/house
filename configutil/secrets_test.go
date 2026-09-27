package configutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestResolveSecrets(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "cast.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`bridge:
  id: "existing-id"
  listen_port: 17021
omada:
  oauth:
    client_id: not-a-secret
    client_secret: !secret omada_client_secret
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cast.secrets.yaml"), []byte(`omada_client_secret: "sk-real-value"
`), 0o644))

	resolved, err := ResolveSecrets(configPath)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, yaml.Unmarshal(resolved, &got))
	assert.Equal(t, "existing-id", got["bridge"].(map[string]any)["id"])
	oauth := got["omada"].(map[string]any)["oauth"].(map[string]any)
	assert.Equal(t, "sk-real-value", oauth["client_secret"])
	assert.Equal(t, "not-a-secret", oauth["client_id"])
}

func TestResolveSecrets_NoSecretTags_DoesNotRequireSecretsFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "plex.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`bridge:
  id: "x"
`), 0o644))
	// deliberately no plex.secrets.yaml written

	resolved, err := ResolveSecrets(configPath)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, yaml.Unmarshal(resolved, &got))
	assert.Equal(t, "x", got["bridge"].(map[string]any)["id"])
}

func TestResolveSecrets_MissingSecret_Errors(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "plex.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`plex:
  api_key: !secret plex_api_key
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plex.secrets.yaml"), []byte(`some_other_key: "value"
`), 0o644))

	_, err := ResolveSecrets(configPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "plex_api_key")
}

func TestPersistValue_ExistingKey(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "cast.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`bridge:
  id: ""
  listen_port: 17021
cast:
  devices: []
`), 0o644))

	require.NoError(t, PersistValue(configPath, "bridge.id", "new-id-123"))

	out, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, yaml.Unmarshal(out, &got))
	assert.Equal(t, "new-id-123", got["bridge"].(map[string]any)["id"])
	assert.Equal(t, 17021, got["bridge"].(map[string]any)["listen_port"])
	assert.Contains(t, string(out), "devices: []")
}

func TestPersistValue_CreatesMissingKey(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "housed.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`house:
  db: /data/house.db
`), 0o644))

	require.NoError(t, PersistValue(configPath, "bridge.id", "generated-id"))

	out, err := os.ReadFile(configPath)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, yaml.Unmarshal(out, &got))
	assert.Equal(t, "generated-id", got["bridge"].(map[string]any)["id"])
	assert.Equal(t, "/data/house.db", got["house"].(map[string]any)["db"])
}

func TestPersistValue_SecretRef_WritesSecretsFileAndReferenceOnly(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "webos.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`bridge:
  id: "x"
webos:
  devices: []
`), 0o644))

	type device struct {
		UUID      string `yaml:"uuid"`
		ClientKey any    `yaml:"client_key"`
	}
	devices := []device{
		{UUID: "u1", ClientKey: SecretRef{Name: "webos_client_key_u1", Value: "top-secret-key"}},
		{UUID: "u2", ClientKey: ""}, // not yet paired - stays a plain empty string
	}

	require.NoError(t, PersistValue(configPath, "webos.devices", devices))

	out, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "top-secret-key", "the secret value must never land in the tracked config")
	assert.Contains(t, string(out), "!secret webos_client_key_u1", "the tracked config must get a !secret reference in its place")

	secretsOut, err := os.ReadFile(secretsPath(configPath))
	require.NoError(t, err)
	var secrets map[string]string
	require.NoError(t, yaml.Unmarshal(secretsOut, &secrets))
	assert.Equal(t, "top-secret-key", secrets["webos_client_key_u1"])

	// Round-trip through ResolveSecrets, like a real bridge reboot would.
	resolved, err := ResolveSecrets(configPath)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, yaml.Unmarshal(resolved, &got))
	gotDevices := got["webos"].(map[string]any)["devices"].([]any)
	require.Len(t, gotDevices, 2)
	assert.Equal(t, "top-secret-key", gotDevices[0].(map[string]any)["client_key"])
	assert.Equal(t, "", gotDevices[1].(map[string]any)["client_key"])
}

func TestPersistValue_SecretRef_MergesWithExistingSecrets(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "webos.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`bridge:
  id: "x"
`), 0o644))
	require.NoError(t, os.WriteFile(secretsPath(configPath), []byte(`webos_client_key_other: "unrelated-key"
`), 0o644))

	require.NoError(t, PersistValue(configPath, "webos.devices", []map[string]any{
		{"uuid": "u1", "client_key": SecretRef{Name: "webos_client_key_u1", Value: "new-key"}},
	}))

	secretsOut, err := os.ReadFile(secretsPath(configPath))
	require.NoError(t, err)
	var secrets map[string]string
	require.NoError(t, yaml.Unmarshal(secretsOut, &secrets))
	assert.Equal(t, "unrelated-key", secrets["webos_client_key_other"], "an existing, unrelated secret must survive")
	assert.Equal(t, "new-key", secrets["webos_client_key_u1"])
}

func TestPersistValue_PlainValue_DoesNotCreateSecretsFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "cast.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`bridge:
  id: ""
`), 0o644))

	require.NoError(t, PersistValue(configPath, "bridge.id", "plain-id"))

	_, err := os.Stat(secretsPath(configPath))
	assert.True(t, os.IsNotExist(err), "a value with no SecretRef must never create a secrets file")
}

func TestPersistValue_NeverTouchesSecretsFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "cast.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(`bridge:
  id: ""
omada:
  oauth:
    client_secret: !secret omada_client_secret
`), 0o644))
	secretsPath := filepath.Join(dir, "cast.secrets.yaml")
	secretsBefore := []byte(`omada_client_secret: "sk-real-value"
`)
	require.NoError(t, os.WriteFile(secretsPath, secretsBefore, 0o644))

	require.NoError(t, PersistValue(configPath, "bridge.id", "new-id"))

	out, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.NotContains(t, string(out), "sk-real-value", "PersistValue must never write a resolved secret value back into the tracked config")
	assert.Contains(t, string(out), "!secret omada_client_secret", "PersistValue must leave the !secret tag itself untouched")

	secretsAfter, err := os.ReadFile(secretsPath)
	require.NoError(t, err)
	assert.Equal(t, secretsBefore, secretsAfter, "PersistValue must never modify the secrets file")
}
