package skills

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	MaxPackageBytes = 25 << 20
	MaxFileBytes    = 10 << 20
)

type Binding struct {
	Tool string `json:"tool"`
	Path string `json:"path"`
}

type Manifest struct {
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Size     int64     `json:"size"`
	Bindings []Binding `json:"bindings"`
}

type Snapshot struct {
	Revision       string     `json:"revision,omitempty"`
	ParentRevision string     `json:"parent_revision,omitempty"`
	DeviceID       string     `json:"device_id,omitempty"`
	CreatedAt      string     `json:"created_at,omitempty"`
	Skills         []Manifest `json:"skills"`
}

type Package struct {
	Manifest Manifest
	Archive  []byte
}

type State struct {
	Revision      string            `json:"revision,omitempty"`
	LastBackupAt  string            `json:"last_backup_at,omitempty"`
	LastAttemptAt string            `json:"last_attempt_at,omitempty"`
	BindingHashes map[string]string `json:"binding_hashes,omitempty"`
}

func BindingKey(binding Binding) string {
	return binding.Tool + ":" + binding.Path
}

func SortSnapshot(snapshot *Snapshot) {
	for i := range snapshot.Skills {
		sort.Slice(snapshot.Skills[i].Bindings, func(a, b int) bool {
			return BindingKey(snapshot.Skills[i].Bindings[a]) < BindingKey(snapshot.Skills[i].Bindings[b])
		})
	}
	sort.Slice(snapshot.Skills, func(i, j int) bool {
		if snapshot.Skills[i].Name == snapshot.Skills[j].Name {
			return snapshot.Skills[i].Hash < snapshot.Skills[j].Hash
		}
		return snapshot.Skills[i].Name < snapshot.Skills[j].Name
	})
}

func SnapshotFromPackages(packages []Package, deviceID string) Snapshot {
	snapshot := Snapshot{DeviceID: deviceID, Skills: make([]Manifest, 0, len(packages))}
	for _, pkg := range packages {
		snapshot.Skills = append(snapshot.Skills, pkg.Manifest)
	}
	SortSnapshot(&snapshot)
	return snapshot
}

func StateFromSnapshot(snapshot Snapshot) State {
	state := State{Revision: snapshot.Revision, BindingHashes: map[string]string{}}
	for _, skill := range snapshot.Skills {
		for _, binding := range skill.Bindings {
			state.BindingHashes[BindingKey(binding)] = skill.Hash
		}
	}
	return state
}

func LoadState(path string) State {
	raw, err := os.ReadFile(path)
	if err != nil {
		return State{BindingHashes: map[string]string{}}
	}
	var state State
	if json.Unmarshal(raw, &state) != nil {
		return State{BindingHashes: map[string]string{}}
	}
	if state.BindingHashes == nil {
		state.BindingHashes = map[string]string{}
	}
	return state
}

func SaveState(path string, state State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ArchiveHash(data []byte) (string, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, zr); err != nil {
		zr.Close()
		return "", err
	}
	if err := zr.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ValidateHash(value string) error {
	if len(value) != sha256.Size*2 {
		return fmt.Errorf("invalid SHA-256 hash length")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("invalid SHA-256 hash: %w", err)
	}
	return nil
}

func ParseTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return t
}
