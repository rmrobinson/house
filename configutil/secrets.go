// Package configutil provides shared config-loading helpers used by this
// repo's bridges and services: resolving "!secret <name>" YAML tags against
// a sibling secrets file - the same convention esphome/*/secrets.yaml
// already uses elsewhere in house-config, replicated here so a bridge's
// tracked config can hold its real structure/devices/etc. in git, with
// only actual secret values (API keys, OAuth secrets, pairing state)
// pulled from a gitignored file - and persisting a generated ID back to a
// config file without risking a resolved secret value being written back
// into it.
package configutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

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
func PersistValue(configPath, keyPath, value string) error {
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

	if err := setMapValue(root.Content[0], strings.Split(keyPath, "."), value); err != nil {
		return fmt.Errorf("setting %s in %s: %w", keyPath, configPath, err)
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("re-marshaling %s: %w", configPath, err)
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", configPath, err)
	}
	return nil
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
// mapping nodes as needed, and sets the final segment's scalar value.
func setMapValue(n *yaml.Node, segs []string, value string) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping node, got kind %d", n.Kind)
	}
	key := segs[0]
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value != key {
			continue
		}
		if len(segs) == 1 {
			n.Content[i+1].Kind = yaml.ScalarNode
			n.Content[i+1].Tag = "!!str"
			n.Content[i+1].Value = value
			n.Content[i+1].Content = nil
			return nil
		}
		return setMapValue(n.Content[i+1], segs[1:], value)
	}

	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	var valNode *yaml.Node
	if len(segs) == 1 {
		valNode = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	} else {
		valNode = &yaml.Node{Kind: yaml.MappingNode}
		if err := setMapValue(valNode, segs[1:], value); err != nil {
			return err
		}
	}
	n.Content = append(n.Content, keyNode, valNode)
	return nil
}
