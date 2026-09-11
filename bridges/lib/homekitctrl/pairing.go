// Package homekitctrl is a HomeKit Accessory Protocol (HAP) controller client: it pairs with and
// talks to HAP accessories directly, the role Apple Home normally plays. It has no knowledge of
// house's device/trait/command protos - callers translate between the two.
package homekitctrl

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ControllerIdentity is this controller's own long-term Ed25519 keypair and pairing ID. Generated
// once and reused for every accessory this controller pairs with.
type ControllerIdentity struct {
	// PairingID identifies this controller to accessories. HAP accessories expect this in
	// UUID-string form.
	PairingID  string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
}

// AccessoryRecord is what's needed to reconnect to an already-paired accessory: its identity and
// long-term public key (to authenticate it during pair-verify), plus a cached network address.
type AccessoryRecord struct {
	// Name is a caller-chosen alias (e.g. "ecobee") used to look up this record - not the
	// accessory's own advertised name.
	Name string

	// PairingID is the accessory's own HAP pairing ID, as reported at pair-setup time.
	PairingID string
	PublicKey ed25519.PublicKey

	// LastKnownIP/Port are a cache only. HAP accessories can change address (DHCP); callers
	// should re-resolve via mDNS and only fall back to these if discovery itself fails.
	LastKnownIP   string
	LastKnownPort int
}

// Store persists a controller's identity and its accessory pairings across restarts. Pairing is a
// one-time interactive operation (it requires a PIN read off the accessory), so without
// persistence every restart would require re-pairing.
type Store interface {
	ControllerIdentity() (*ControllerIdentity, error)
	SaveControllerIdentity(identity *ControllerIdentity) error

	Accessory(name string) (*AccessoryRecord, error)
	Accessories() ([]*AccessoryRecord, error)
	SaveAccessory(record *AccessoryRecord) error
}

// storeData is the on-disk JSON shape. ed25519 keys marshal as base64 strings via []byte's
// default JSON encoding.
type storeData struct {
	Controller  *ControllerIdentity `json:"controller,omitempty"`
	Accessories []*AccessoryRecord  `json:"accessories,omitempty"`
}

// fileStore is a JSON-file-backed Store. A handful of accessories at most is expected, so a flat
// file is simpler than a real database and is re-read/rewritten wholesale on every access.
type fileStore struct {
	mu   sync.Mutex
	path string
}

// NewFileStore returns a Store backed by a JSON file at path. The file and its parent directory
// are created on first write if they don't exist.
func NewFileStore(path string) Store {
	return &fileStore{path: path}
}

func (s *fileStore) load() (*storeData, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return &storeData{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pairing store: %w", err)
	}

	var data storeData
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("parse pairing store: %w", err)
	}
	return &data, nil
}

func (s *fileStore) save(data *storeData) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pairing store: %w", err)
	}

	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create pairing store directory: %w", err)
		}
	}

	// Contains the controller's private key - not world/group readable.
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		return fmt.Errorf("write pairing store: %w", err)
	}
	return nil
}

func (s *fileStore) ControllerIdentity() (*ControllerIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return nil, err
	}
	return data.Controller, nil
}

func (s *fileStore) SaveControllerIdentity(identity *ControllerIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return err
	}
	data.Controller = identity
	return s.save(data)
}

func (s *fileStore) Accessories() ([]*AccessoryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return nil, err
	}
	return data.Accessories, nil
}

func (s *fileStore) Accessory(name string) (*AccessoryRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return nil, err
	}
	for _, a := range data.Accessories {
		if a.Name == name {
			return a, nil
		}
	}
	return nil, fmt.Errorf("accessory %q not found in pairing store", name)
}

func (s *fileStore) SaveAccessory(record *AccessoryRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := s.load()
	if err != nil {
		return err
	}

	for i, a := range data.Accessories {
		if a.Name == record.Name {
			data.Accessories[i] = record
			return s.save(data)
		}
	}
	data.Accessories = append(data.Accessories, record)
	return s.save(data)
}
