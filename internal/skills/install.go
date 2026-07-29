package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type InstallOptions struct {
	Home      string
	State     State
	StatePath string
	Download  func(hash string) ([]byte, error)
}

type InstallResult struct {
	Installed int
	Unchanged int
	Backups   []string
}

func Install(snapshot Snapshot, options InstallOptions) (InstallResult, error) {
	if options.Home == "" || options.Download == nil {
		return InstallResult{}, fmt.Errorf("home and download function are required")
	}
	rootByTool := map[string]string{}
	for _, root := range DefaultRoots(options.Home) {
		rootByTool[root.Tool] = root.Path
	}
	storeRoot := filepath.Join(options.Home, ".yiduo", "skills", "store")
	blobRoot := filepath.Join(options.Home, ".yiduo", "skills", "blobs")
	if err := os.MkdirAll(storeRoot, 0o700); err != nil {
		return InstallResult{}, err
	}
	if err := os.MkdirAll(blobRoot, 0o700); err != nil {
		return InstallResult{}, err
	}

	for _, skill := range snapshot.Skills {
		if err := ValidateHash(skill.Hash); err != nil {
			return InstallResult{}, fmt.Errorf("invalid %s package: %w", skill.Name, err)
		}
		archivePath := filepath.Join(blobRoot, skill.Hash+".tar.gz")
		archive, err := readVerifiedArchive(archivePath, skill.Hash)
		if err != nil {
			archive, err = options.Download(skill.Hash)
			if err != nil {
				return InstallResult{}, fmt.Errorf("download %s: %w", skill.Name, err)
			}
			if err := writeAtomic(archivePath, archive, 0o600); err != nil {
				return InstallResult{}, err
			}
		}
		storePath := filepath.Join(storeRoot, skill.Hash)
		if _, err := os.Stat(filepath.Join(storePath, "SKILL.md")); err != nil {
			if err := extractAtomic(archive, storePath); err != nil {
				return InstallResult{}, fmt.Errorf("extract %s: %w", skill.Name, err)
			}
		}
	}

	type operation struct {
		binding Binding
		target  string
		hash    string
	}
	operations := make([]operation, 0)
	result := InstallResult{}
	for _, skill := range snapshot.Skills {
		for _, binding := range skill.Bindings {
			root, ok := rootByTool[binding.Tool]
			if !ok {
				return result, fmt.Errorf("unsupported skill tool %q", binding.Tool)
			}
			if err := validateBindingPath(binding.Path); err != nil {
				return result, err
			}
			target := filepath.Join(root, binding.Path)
			currentHash, exists := currentBindingHash(target)
			if exists && currentHash == skill.Hash {
				result.Unchanged++
				continue
			}
			if exists {
				managedHash := options.State.BindingHashes[BindingKey(binding)]
				if managedHash == "" || managedHash != currentHash {
					return result, fmt.Errorf("skill conflict at %s; local content is unmanaged or modified", target)
				}
			}
			operations = append(operations, operation{binding: binding, target: target, hash: skill.Hash})
		}
	}

	backupRoot := filepath.Join(options.Home, ".yiduo", "skills", "backups", time.Now().UTC().Format("20060102T150405Z"))
	for _, operation := range operations {
		if _, err := os.Lstat(operation.target); err == nil {
			backup := filepath.Join(backupRoot, operation.binding.Tool, operation.binding.Path)
			if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
				return result, err
			}
			if err := os.Rename(operation.target, backup); err != nil {
				return result, fmt.Errorf("backup %s: %w", operation.target, err)
			}
			result.Backups = append(result.Backups, backup)
		}
		if err := os.MkdirAll(filepath.Dir(operation.target), 0o700); err != nil {
			return result, err
		}
		source := filepath.Join(storeRoot, operation.hash)
		if err := linkOrCopy(source, operation.target); err != nil {
			return result, fmt.Errorf("install %s: %w", operation.target, err)
		}
		result.Installed++
	}

	state := StateFromSnapshot(snapshot)
	state.LastBackupAt = options.State.LastBackupAt
	state.LastAttemptAt = options.State.LastAttemptAt
	if options.StatePath != "" {
		if err := SaveState(options.StatePath, state); err != nil {
			return result, err
		}
	}
	sort.Strings(result.Backups)
	return result, nil
}

func currentBindingHash(path string) (string, bool) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", false
	}
	archive, _, err := archiveSkill(real, filepath.Base(path))
	if err != nil {
		return "modified", true
	}
	hash, err := ArchiveHash(archive)
	if err != nil {
		return "modified", true
	}
	return hash, true
}

func readVerifiedArchive(path string, hash string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := validateArchiveHash(raw, hash); err != nil {
		return nil, err
	}
	return raw, nil
}

func extractAtomic(raw []byte, target string) error {
	if err := validateArchiveHash(raw, filepath.Base(target)); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(target), ".skill-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := extractArchive(raw, tmp); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(tmp, "SKILL.md")); err != nil {
		return fmt.Errorf("archive does not contain SKILL.md")
	}
	if _, err := os.Stat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(tmp, target)
}

func extractArchive(raw []byte, target string) error {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var total int64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(header.Name)
		if name == "." || filepath.IsAbs(name) || name != filepath.Clean(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		path := filepath.Join(target, name)
		if !within(target, path) {
			return fmt.Errorf("unsafe archive path %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > MaxFileBytes {
				return fmt.Errorf("archive file %s exceeds size limit", header.Name)
			}
			total += header.Size
			if total > MaxPackageBytes {
				return fmt.Errorf("archive exceeds size limit")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tr, header.Size)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported archive entry %q", header.Name)
		}
	}
	return nil
}

func validateBindingPath(value string) error {
	if value == "" || value == "." || filepath.Base(value) != value || strings.ContainsAny(value, `/\\`) || strings.HasPrefix(value, ".") {
		return fmt.Errorf("unsafe skill binding path %q", value)
	}
	return nil
}

func linkOrCopy(source string, target string) error {
	if runtime.GOOS != "windows" {
		rel, err := filepath.Rel(filepath.Dir(target), source)
		if err == nil {
			return os.Symlink(rel, target)
		}
	}
	if err := os.Symlink(source, target); err == nil {
		return nil
	}
	return copyTree(source, target)
}

func copyTree(source string, target string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputErr := input.Close()
		outputErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputErr != nil {
			return inputErr
		}
		return outputErr
	})
}

func writeAtomic(path string, raw []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
