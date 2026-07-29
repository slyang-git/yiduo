package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Root struct {
	Tool string
	Path string
}

type DiscoverOptions struct {
	Home  string
	Roots []Root
}

var skillNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func DefaultRoots(home string) []Root {
	return []Root{
		{Tool: "agents", Path: filepath.Join(home, ".agents", "skills")},
		{Tool: "codex", Path: filepath.Join(home, ".codex", "skills")},
		{Tool: "claude", Path: filepath.Join(home, ".claude", "skills")},
		{Tool: "cursor", Path: filepath.Join(home, ".cursor", "skills")},
		{Tool: "gemini", Path: filepath.Join(home, ".gemini", "skills")},
		{Tool: "opencode", Path: filepath.Join(home, ".config", "opencode", "skills")},
	}
}

func Discover(options DiscoverOptions) ([]Package, error) {
	if options.Home == "" {
		return nil, fmt.Errorf("home directory is required")
	}
	realHome, err := filepath.EvalSymlinks(options.Home)
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	if len(options.Roots) == 0 {
		options.Roots = DefaultRoots(options.Home)
	}

	byHash := map[string]*Package{}
	for _, root := range options.Roots {
		entries, err := os.ReadDir(root.Path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("scan %s skills: %w", root.Tool, err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			logicalPath := entry.Name()
			candidate := filepath.Join(root.Path, logicalPath)
			realPath, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return nil, fmt.Errorf("resolve %s skill %s: %w", root.Tool, logicalPath, err)
			}
			if !within(realHome, realPath) {
				return nil, fmt.Errorf("refusing %s skill %s: symlink target is outside the home directory", root.Tool, logicalPath)
			}
			info, err := os.Stat(realPath)
			if err != nil || !info.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(realPath, "SKILL.md")); err != nil {
				continue
			}

			archive, name, err := archiveSkill(realPath, logicalPath)
			if err != nil {
				return nil, fmt.Errorf("package %s skill %s: %w", root.Tool, logicalPath, err)
			}
			hash, err := ArchiveHash(archive)
			if err != nil {
				return nil, fmt.Errorf("hash %s skill %s: %w", root.Tool, logicalPath, err)
			}
			binding := Binding{Tool: root.Tool, Path: logicalPath}
			if existing := byHash[hash]; existing != nil {
				existing.Manifest.Bindings = appendBinding(existing.Manifest.Bindings, binding)
				continue
			}
			byHash[hash] = &Package{
				Manifest: Manifest{Name: name, Hash: hash, Size: int64(len(archive)), Bindings: []Binding{binding}},
				Archive:  archive,
			}
		}
	}

	packages := make([]Package, 0, len(byHash))
	for _, pkg := range byHash {
		sort.Slice(pkg.Manifest.Bindings, func(i, j int) bool {
			return BindingKey(pkg.Manifest.Bindings[i]) < BindingKey(pkg.Manifest.Bindings[j])
		})
		packages = append(packages, *pkg)
	}
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Manifest.Name == packages[j].Manifest.Name {
			return packages[i].Manifest.Hash < packages[j].Manifest.Hash
		}
		return packages[i].Manifest.Name < packages[j].Manifest.Name
	})
	return packages, nil
}

func archiveSkill(root string, fallbackName string) ([]byte, string, error) {
	name, err := readSkillName(filepath.Join(root, "SKILL.md"), fallbackName)
	if err != nil {
		return nil, "", err
	}

	paths := make([]string, 0, 16)
	total := int64(0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if shouldExclude(rel, entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("nested symlink %s is not portable", rel)
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("special file %s is not supported", rel)
		}
		if info.Mode().IsRegular() {
			if info.Size() > MaxFileBytes {
				return fmt.Errorf("file %s exceeds %d bytes", rel, MaxFileBytes)
			}
			total += info.Size()
			if total > MaxPackageBytes {
				return fmt.Errorf("skill exceeds %d bytes", MaxPackageBytes)
			}
			if err := scanFileForSecrets(path, rel, info.Size()); err != nil {
				return err
			}
		}
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Strings(paths)

	var output bytes.Buffer
	zw, err := gzip.NewWriterLevel(&output, gzip.BestCompression)
	if err != nil {
		return nil, "", err
	}
	zw.Header.ModTime = time.Unix(0, 0)
	zw.Header.OS = 255
	tw := tar.NewWriter(zw)
	for _, rel := range paths {
		path := filepath.Join(root, rel)
		info, err := os.Stat(path)
		if err != nil {
			return nil, "", err
		}
		mode := int64(0o644)
		if info.IsDir() {
			mode = 0o755
		} else if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		header := &tar.Header{
			Name:       filepath.ToSlash(rel),
			Mode:       mode,
			Size:       info.Size(),
			ModTime:    time.Unix(0, 0),
			AccessTime: time.Unix(0, 0),
			ChangeTime: time.Unix(0, 0),
			Uid:        0,
			Gid:        0,
		}
		if info.IsDir() {
			header.Typeflag = tar.TypeDir
			header.Size = 0
		} else {
			header.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(header); err != nil {
			return nil, "", err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return nil, "", err
			}
			_, copyErr := io.Copy(tw, file)
			closeErr := file.Close()
			if copyErr != nil {
				return nil, "", copyErr
			}
			if closeErr != nil {
				return nil, "", closeErr
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	if err := zw.Close(); err != nil {
		return nil, "", err
	}
	if output.Len() > MaxPackageBytes {
		return nil, "", fmt.Errorf("compressed skill exceeds %d bytes", MaxPackageBytes)
	}
	return output.Bytes(), name, nil
}

func readSkillName(path string, fallback string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", fmt.Errorf("SKILL.md must start with YAML frontmatter")
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return "", fmt.Errorf("SKILL.md has unterminated YAML frontmatter")
	}
	name := ""
	for _, line := range strings.Split(text[4:4+end], "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "name" {
			name = strings.Trim(strings.TrimSpace(value), "\"'")
			break
		}
	}
	if name == "" {
		name = fallback
	}
	if !skillNamePattern.MatchString(name) || len(name) > 64 {
		return "", fmt.Errorf("invalid skill name %q", name)
	}
	return name, nil
}

func shouldExclude(rel string, entry fs.DirEntry) bool {
	base := strings.ToLower(entry.Name())
	if base == ".git" || base == ".svn" || base == "node_modules" || base == "__pycache__" || base == ".ds_store" {
		return true
	}
	if base == ".env" || strings.HasPrefix(base, ".env.") || base == "credentials" || base == "credentials.json" || base == "id_rsa" || base == "id_ed25519" {
		return true
	}
	return strings.HasSuffix(base, ".pyc") || strings.HasSuffix(base, "~") || strings.Contains(filepath.ToSlash(rel), "/.git/")
}

func scanFileForSecrets(path string, rel string, size int64) error {
	if size > 1<<20 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	text := string(raw)
	patterns := []string{"-----BEGIN PRIVATE KEY-----", "-----BEGIN OPENSSH PRIVATE KEY-----", "AKIA", "ghp_", "sk-proj-"}
	for _, pattern := range patterns {
		if strings.Contains(text, pattern) {
			return fmt.Errorf("possible secret in %s (%s); remove it before syncing", rel, pattern)
		}
	}
	return nil
}

func appendBinding(bindings []Binding, candidate Binding) []Binding {
	key := BindingKey(candidate)
	for _, binding := range bindings {
		if BindingKey(binding) == key {
			return bindings
		}
	}
	return append(bindings, candidate)
}

func within(parent string, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func validateArchiveHash(raw []byte, expected string) error {
	actual, err := ArchiveHash(raw)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("archive hash mismatch")
	}
	return nil
}
