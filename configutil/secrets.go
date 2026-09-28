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
	"sync"

	"gopkg.in/yaml.v3"
)

// configLocks serializes PersistValue calls per resolved config path, so two
// concurrent callers (e.g. a bridge's discovery loop persisting a learned
// device config while an inbound SetBridgeConfig RPC persists a name edit)
// can't each read the file before the other writes it and clobber one
// another's change - PersistValue only patches the one key path it was
// given, so a lost update here means the *other* caller's write silently
// vanishes, not just a torn file.
var configLocks sync.Map // map[string]*sync.RWMutex

func configLock(configPath string) *sync.RWMutex {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	mu, _ := configLocks.LoadOrStore(abs, &sync.RWMutex{})
	return mu.(*sync.RWMutex)
}

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
	mu := configLock(configPath)
	mu.RLock()
	defer mu.RUnlock()

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
//
// Safe to call concurrently, including from multiple different keyPaths
// against the same configPath (e.g. one goroutine persisting a device's
// learned config while an inbound RPC persists a name edit): calls against
// the same configPath are serialized against each other and against
// ResolveSecrets reads of it, and each file (configPath, and its secrets
// sibling if touched) is written atomically via a temp file + rename, so a
// crash mid-call can't leave either file torn.
//
// To persist more than one key path in the same call - so a concurrent
// PersistValue/PersistValues call against a different key path can't land
// between them - use PersistValues instead.
func PersistValue(configPath, keyPath string, value any) error {
	return PersistValues(configPath, KeyValue{KeyPath: keyPath, Value: value})
}

// KeyValue pairs a dot-separated key path (see PersistValue) with the value
// to persist there, for PersistValues.
type KeyValue struct {
	KeyPath string
	Value   any
}

// PersistValues is PersistValue for multiple key paths, written together
// under one read-modify-write of configPath (and, if any pair's value
// contains a SecretRef, one merge into the secrets file) instead of one
// per pair. Use this instead of separate PersistValue calls whenever a
// caller has more than one key path to persist at once (e.g.
// SetBridgeConfig persisting bridge.name and bridge.description together):
// each separate PersistValue call reads and writes the whole file, so back
// to back calls leave a window where a concurrent call against yet another
// key path can land in between and have its own change overwritten by the
// second call's write, since that write is based on a read taken before the
// concurrent change landed.
//
// If replacing a key path drops a "!secret <name>" tag that was there
// before (e.g. removing a paired device from webos.devices along with its
// client_key), name's entry is removed from the secrets file too, but only
// if the document no longer references name anywhere else - so the secrets
// file doesn't accumulate entries nothing points to anymore, without ever
// risking removal of a secret still in use.
func PersistValues(configPath string, kvs ...KeyValue) error {
	if len(kvs) == 0 {
		return nil
	}

	mu := configLock(configPath)
	mu.Lock()
	defer mu.Unlock()

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

	// staleSecretNames collects every "!secret <name>" tag reachable under
	// each key path's *old* value, before it's overwritten below - e.g. a
	// webos device's client_key secret, if that device is dropped from
	// webos.devices by this call. Collected up front, since setMapValue
	// below overwrites these nodes in place.
	staleSecretNames := map[string]bool{}
	for _, kv := range kvs {
		if old := getMapValue(root.Content[0], strings.Split(kv.KeyPath, ".")); old != nil {
			walkScalars(old, func(n *yaml.Node) {
				if n.Tag == "!secret" {
					staleSecretNames[n.Value] = true
				}
			})
		}
	}

	secrets := map[string]string{}
	for _, kv := range kvs {
		valueNode, err := marshalNode(kv.Value)
		if err != nil {
			return fmt.Errorf("marshaling value for %s: %w", kv.KeyPath, err)
		}
		if err := setMapValue(root.Content[0], strings.Split(kv.KeyPath, "."), valueNode); err != nil {
			return fmt.Errorf("setting %s in %s: %w", kv.KeyPath, configPath, err)
		}
		collectSecretRefs(reflect.ValueOf(kv.Value), secrets)
	}

	// A stale name only gets pruned if the document no longer references it
	// anywhere at all (not just at the key path that used to hold it) -
	// this only runs when staleSecretNames is non-empty, so a call that
	// never touched a "!secret" tag leaves the secrets file untouched, same
	// as before.
	var toRemove []string
	if len(staleSecretNames) > 0 {
		stillReferenced := map[string]bool{}
		walkScalars(&root, func(n *yaml.Node) {
			if n.Tag == "!secret" {
				stillReferenced[n.Value] = true
			}
		})
		for name := range staleSecretNames {
			if !stillReferenced[name] {
				toRemove = append(toRemove, name)
			}
		}
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("re-marshaling %s: %w", configPath, err)
	}

	if len(secrets) > 0 || len(toRemove) > 0 {
		path := secretsPath(configPath)
		existing, err := loadSecretsOrEmpty(path)
		if err != nil {
			return fmt.Errorf("loading %s: %w", path, err)
		}
		for name, v := range secrets {
			existing[name] = v
		}
		for _, name := range toRemove {
			delete(existing, name)
		}
		if err := saveSecrets(path, existing); err != nil {
			return err
		}
	}

	return atomicWriteFile(configPath, out, 0o644)
}

// getMapValue returns the node currently at segs within mapping node n, or
// nil if any segment along the way doesn't exist (e.g. keyPath names a key
// PersistValue/PersistValues is about to create for the first time - there
// is no old value to inspect).
func getMapValue(n *yaml.Node, segs []string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	key := segs[0]
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value != key {
			continue
		}
		if len(segs) == 1 {
			return n.Content[i+1]
		}
		return getMapValue(n.Content[i+1], segs[1:])
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
	return atomicWriteFile(path, data, 0o600)
}

// atomicWriteFile writes data to a temp file next to path and renames it
// into place, so a crash/power-loss/OOM-kill mid-write can never leave path
// truncated or half-written - os.WriteFile alone truncates path in place,
// and PersistValue/saveSecrets both run at arbitrary points during a
// bridge's runtime (not just at startup), so a torn write here would corrupt
// a file the next process start can't parse.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("setting permissions on %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, path, err)
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
