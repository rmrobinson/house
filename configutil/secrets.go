// Package configutil provides shared config-loading helpers used by this
// repo's bridges and services: resolving "!secret <name>" YAML tags against
// a sibling secrets file - the same convention esphome/*/secrets.yaml
// already uses elsewhere in house-config, replicated here so a bridge's
// tracked config can hold its real structure/devices/etc. in git, with
// only actual secret values (API keys, OAuth secrets, pairing state)
// pulled from a gitignored file - and persisting a generated ID, or a
// value learned at runtime (see SecretRef), back to a config file without
// risking a resolved secret value being written back into it.
package configutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// SecretRef marks a value, passed to PersistValue, that must never be
// written into the tracked config file - only a "!secret Name" reference
// is, matching how ResolveSecrets resolves one back out on read. Value is
// written into configPath's sibling secrets file under Name instead. Use
// this for anything a bridge learns at runtime (e.g. a device pairing key
// obtained during first-time pairing) rather than a value a human is
// expected to have configured up front.
//
// A SecretRef can appear anywhere within the value passed to PersistValue -
// directly, or nested inside a struct/slice/map field - PersistValue finds
// every one via reflection before writing.
type SecretRef struct {
	Name  string
	Value string
}

// MarshalYAML makes a SecretRef marshal as a "!secret <Name>" tag wherever
// it appears in a value passed to PersistValue.
func (r SecretRef) MarshalYAML() (interface{}, error) {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!secret", Value: r.Name}, nil
}

// collectSecretRefs finds every SecretRef reachable within v - directly, or
// nested inside a struct/slice/array/map/pointer/interface - and adds its
// Name/Value into out.
func collectSecretRefs(v reflect.Value, out map[string]string) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Ptr:
		if v.IsNil() {
			return
		}
		collectSecretRefs(v.Elem(), out)
	case reflect.Struct:
		if sr, ok := v.Interface().(SecretRef); ok {
			out[sr.Name] = sr.Value
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue // unexported field, not reachable via Interface()
			}
			collectSecretRefs(v.Field(i), out)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			collectSecretRefs(v.Index(i), out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			collectSecretRefs(v.MapIndex(k), out)
		}
	}
}

// FindConfigFile searches paths, in order, for name+"."+configType and
// returns the first match - the same search viper's
// SetConfigName/AddConfigPath/ReadInConfig performs, but returning the
// resolved path so callers can also pass it to ResolveSecrets/PersistValue.
func FindConfigFile(name, configType string, paths []string) (string, error) {
	filename := name + "." + configType
	for _, dir := range paths {
		candidate := filepath.Join(dir, filename)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found in %v", filename, paths)
}

// ResolveSecrets reads configPath's raw YAML and resolves any scalar node
// tagged "!secret <name>" against name's value in a sibling secrets file:
// for configPath ".../foo.yaml", that's ".../foo.secrets.yaml". Returns
// the fully-resolved YAML, ready to hand to viper.ReadConfig.
//
// The sibling secrets file is only read (and need only exist) if
// configPath actually contains a "!secret" tag - most services have no
// secrets at all.
func ResolveSecrets(configPath string) ([]byte, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", configPath, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", configPath, err)
	}

	var secrets map[string]string
	var loadErr error
	walkScalars(&root, func(n *yaml.Node) {
		if n.Tag != "!secret" || loadErr != nil {
			return
		}
		if secrets == nil {
			path := secretsPath(configPath)
			secrets, loadErr = loadSecrets(path)
			if loadErr != nil {
				return
			}
		}
		value, ok := secrets[n.Value]
		if !ok {
			loadErr = fmt.Errorf("secret %q referenced in %s not found in %s", n.Value, configPath, secretsPath(configPath))
			return
		}
		n.Value = value
		n.Tag = "!!str"
	})
	if loadErr != nil {
		return nil, loadErr
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return nil, fmt.Errorf("re-marshaling %s: %w", configPath, err)
	}
	return out, nil
}

// PersistValue writes value at the dot-separated keyPath (e.g. "bridge.id")
// into configPath's own YAML node tree, creating any missing intermediate
// mapping keys and leaving every other key, comment, and formatting
// untouched. It re-reads and re-parses configPath itself rather than
// operating on anything ResolveSecrets already resolved, so a secret
// value can never end up written back into a tracked config file this
// way.
//
// value may be a plain scalar (string, bool, int, ...) or a more complex
// value (a struct, a slice of structs, ...) - it's marshaled via
// yaml.Marshal and the result is what's written at keyPath, replacing
// whatever was there before (scalar, sequence, or mapping) wholesale. If
// value contains a SecretRef anywhere - directly, or nested inside a
// struct/slice/map field - each one's Value is written into configPath's
// sibling secrets file under its Name instead, merged with whatever else
// is already there, and configPath gets a "!secret <Name>" reference in
// its place. The secrets file is only touched (and created, if missing)
// when value actually contains a SecretRef.
func PersistValue(configPath, keyPath string, value any) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", configPath, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return fmt.Errorf("parsing %s: %w", configPath, err)
	}
	if len(root.Content) < 1 {
		root.Kind = yaml.DocumentNode
		root.Content = []*yaml.Node{{Kind: yaml.MappingNode}}
	}

	valueNode, err := marshalNode(value)
	if err != nil {
		return fmt.Errorf("marshaling value for %s: %w", keyPath, err)
	}

	if err := setMapValue(root.Content[0], strings.Split(keyPath, "."), valueNode); err != nil {
		return fmt.Errorf("setting %s in %s: %w", keyPath, configPath, err)
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("re-marshaling %s: %w", configPath, err)
	}

	secrets := map[string]string{}
	collectSecretRefs(reflect.ValueOf(value), secrets)
	if len(secrets) > 0 {
		path := secretsPath(configPath)
		existing, err := loadSecretsOrEmpty(path)
		if err != nil {
			return fmt.Errorf("loading %s: %w", path, err)
		}
		for name, v := range secrets {
			existing[name] = v
		}
		if err := saveSecrets(path, existing); err != nil {
			return err
		}
	}

	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", configPath, err)
	}
	return nil
}

// marshalNode round-trips value through yaml.Marshal into a *yaml.Node -
// the general way to turn an arbitrary Go value (not just a plain scalar)
// into something setMapValue can splice into an existing YAML document.
func marshalNode(value any) (*yaml.Node, error) {
	data, err := yaml.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshaling value: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("re-parsing marshaled value: %w", err)
	}
	if len(doc.Content) < 1 {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}, nil
	}
	return doc.Content[0], nil
}

func secretsPath(configPath string) string {
	ext := filepath.Ext(configPath)
	return strings.TrimSuffix(configPath, ext) + ".secrets.yaml"
}

func loadSecrets(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var m map[string]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return m, nil
}

// loadSecretsOrEmpty is loadSecrets, but a missing file (the common case -
// most bridges have no secrets file at all until PersistValue's first
// SecretRef creates one) returns an empty map instead of an error.
func loadSecretsOrEmpty(path string) (map[string]string, error) {
	m, err := loadSecrets(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	return m, err
}

// saveSecrets writes secrets to path as flat "key: value" pairs - the same
// shape loadSecrets/ResolveSecrets read back.
func saveSecrets(path string, secrets map[string]string) error {
	data, err := yaml.Marshal(secrets)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// walkScalars calls fn on every scalar node in the tree rooted at n
// (depth-first).
func walkScalars(n *yaml.Node, fn func(*yaml.Node)) {
	if n.Kind == yaml.ScalarNode {
		fn(n)
	}
	for _, c := range n.Content {
		walkScalars(c, fn)
	}
}

// setMapValue walks a mapping node down segs, creating intermediate
// mapping nodes as needed, and sets the final segment's value to a copy of
// valueNode wholesale - valueNode may be any YAML node kind (scalar,
// sequence, or mapping), not just a plain scalar, so this also replaces a
// list/mapping outright rather than merging into it.
func setMapValue(n *yaml.Node, segs []string, valueNode *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping node, got kind %d", n.Kind)
	}
	key := segs[0]
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value != key {
			continue
		}
		if len(segs) == 1 {
			*n.Content[i+1] = *valueNode
			return nil
		}
		return setMapValue(n.Content[i+1], segs[1:], valueNode)
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	var valNode *yaml.Node
	if len(segs) == 1 {
		valNode = valueNode
	} else {
		valNode = &yaml.Node{Kind: yaml.MappingNode}
		if err := setMapValue(valNode, segs[1:], valueNode); err != nil {
			return err
		}
	}
	n.Content = append(n.Content, keyNode, valNode)
	return nil
}
