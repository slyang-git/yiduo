package skills

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDiscoverDeduplicatesSymlinkBindings(t *testing.T) {
	home := t.TempDir()
	skillPath := filepath.Join(home, ".agents", "skills", "sample-skill")
	writeTestSkill(t, skillPath, "sample-skill", "instructions")
	claudeRoot := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(claudeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", ".agents", "skills", "sample-skill"), filepath.Join(claudeRoot, "sample-skill")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatal(err)
	}

	first, err := Discover(DiscoverOptions{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Discover(DiscoverOptions{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(first[0].Manifest.Bindings) != 2 {
		t.Fatalf("expected one package with two bindings, got %#v", first)
	}
	if first[0].Manifest.Hash != second[0].Manifest.Hash {
		t.Fatalf("archive hash is not deterministic: %s != %s", first[0].Manifest.Hash, second[0].Manifest.Hash)
	}
}

func TestDiscoverRejectsPossibleSecret(t *testing.T) {
	home := t.TempDir()
	skillPath := filepath.Join(home, ".agents", "skills", "secret-skill")
	writeTestSkill(t, skillPath, "secret-skill", "-----BEGIN OPENSSH PRIVATE KEY-----")
	_, err := Discover(DiscoverOptions{Home: home})
	if err == nil || !strings.Contains(err.Error(), "possible secret") {
		t.Fatalf("expected possible secret error, got %v", err)
	}
}

func TestInstallRestoresAndProtectsConflicts(t *testing.T) {
	sourceHome := t.TempDir()
	skillPath := filepath.Join(sourceHome, ".agents", "skills", "sample-skill")
	writeTestSkill(t, skillPath, "sample-skill", "instructions")
	packages, err := Discover(DiscoverOptions{Home: sourceHome})
	if err != nil {
		t.Fatal(err)
	}
	packages[0].Manifest.Bindings = []Binding{{Tool: "agents", Path: "sample-skill"}, {Tool: "codex", Path: "sample-skill"}}
	snapshot := SnapshotFromPackages(packages, "device")
	snapshot.Revision = "revision-1"

	targetHome := t.TempDir()
	result, err := Install(snapshot, InstallOptions{
		Home:      targetHome,
		State:     State{BindingHashes: map[string]string{}},
		StatePath: filepath.Join(targetHome, ".yiduo", "skills-state.json"),
		Download: func(hash string) ([]byte, error) {
			return packages[0].Archive, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Installed != 2 {
		t.Fatalf("expected two bindings installed, got %#v", result)
	}
	for _, path := range []string{
		filepath.Join(targetHome, ".agents", "skills", "sample-skill", "SKILL.md"),
		filepath.Join(targetHome, ".codex", "skills", "sample-skill", "SKILL.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("restored file missing at %s: %v", path, err)
		}
	}

	conflictHome := t.TempDir()
	conflictPath := filepath.Join(conflictHome, ".agents", "skills", "sample-skill")
	writeTestSkill(t, conflictPath, "sample-skill", "different local instructions")
	_, err = Install(snapshot, InstallOptions{
		Home:      conflictHome,
		State:     State{BindingHashes: map[string]string{}},
		StatePath: filepath.Join(conflictHome, ".yiduo", "skills-state.json"),
		Download: func(hash string) ([]byte, error) {
			return packages[0].Archive, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "skill conflict") {
		t.Fatalf("expected conflict protection, got %v", err)
	}
}

func writeTestSkill(t *testing.T, dir string, name string, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: Test skill\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
