package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"yiduo/internal/skills"
)

const skillBackupInterval = 5 * time.Minute

func runSkillsCommand(args []string) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		printSkillsHelp()
		return 0
	}
	subcommand := strings.TrimSpace(args[0])
	flags := flag.NewFlagSet("skills "+subcommand, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	server := flags.String("server", "", "API server base URL")
	authToken := flags.String("auth-token", "", "sync auth token")
	if err := flags.Parse(args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\n", flags.Arg(0))
		return 2
	}

	cfg := loadConfig()
	resolvedServer := firstNonEmpty(*server, os.Getenv("AI_WRAPPED_SERVER"), cfg.Server, "https://yiduo.one/")
	resolvedToken := firstNonEmpty(*authToken, os.Getenv("AI_WRAPPED_SYNC_TOKEN"), os.Getenv("AI_WRAPPED_DEVICE_TOKEN"), cfg.AuthToken, cfg.LegacyDeviceToken)
	client := skills.Client{Server: resolvedServer, AuthToken: resolvedToken}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to resolve home directory: %v\n", err)
		return 1
	}
	statePath := skillsStatePath()
	state := skills.LoadState(statePath)

	switch subcommand {
	case "list":
		packages, err := discoverSkills(home)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printSkillPackages(packages)
		return 0
	case "status":
		packages, err := discoverSkills(home)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		remote, remoteErr := client.Head()
		fmt.Printf("Local packages: %d\n", len(packages))
		fmt.Printf("Local bindings: %d\n", countPackageBindings(packages))
		if errors.Is(remoteErr, skills.ErrNoRemoteSnapshot) {
			fmt.Println("Remote snapshot: none")
			return 0
		}
		if remoteErr != nil {
			fmt.Fprintf(os.Stderr, "failed to load remote snapshot: %v\n", remoteErr)
			return 1
		}
		fmt.Printf("Remote revision: %s\n", remote.Revision)
		fmt.Printf("Remote packages: %d\n", len(remote.Skills))
		return 0
	case "diff":
		packages, err := discoverSkills(home)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		remote, err := client.Head()
		if errors.Is(err, skills.ErrNoRemoteSnapshot) {
			fmt.Println("Remote snapshot is empty; all local skills are new.")
			printSkillPackages(packages)
			return 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printSkillsDiff(skills.SnapshotFromPackages(packages, cfg.DeviceID), remote)
		return 0
	case "push", "sync":
		committed, changed, err := backupSkills(client, cfg.DeviceID, home, statePath, state)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if !changed {
			fmt.Printf("Skills already backed up at revision %s\n", committed.Revision)
			return 0
		}
		fmt.Printf("Backed up %d skill packages at revision %s\n", len(committed.Skills), committed.Revision)
		return 0
	case "pull", "restore":
		remote, err := client.Head()
		if err != nil {
			if errors.Is(err, skills.ErrNoRemoteSnapshot) {
				fmt.Fprintln(os.Stderr, "No remote skill backup found. Run `yiduo skills sync` on an existing device first.")
			} else {
				fmt.Fprintln(os.Stderr, err)
			}
			return 1
		}
		result, err := skills.Install(remote, skills.InstallOptions{
			Home:      home,
			State:     state,
			StatePath: statePath,
			Download:  client.Download,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("Restored skills: installed=%d unchanged=%d revision=%s\n", result.Installed, result.Unchanged, remote.Revision)
		if len(result.Backups) > 0 {
			fmt.Printf("Backups saved under %s\n", filepath.Dir(filepath.Dir(result.Backups[0])))
		}
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown skills command: %s\n", subcommand)
		printSkillsHelp()
		return 2
	}
}

func backupSkills(client skills.Client, deviceID string, home string, statePath string, state skills.State) (skills.Snapshot, bool, error) {
	packages, err := discoverSkills(home)
	if err != nil {
		return skills.Snapshot{}, false, err
	}
	if len(packages) == 0 {
		return skills.Snapshot{}, false, fmt.Errorf("no user-level skills found; refusing to replace the remote snapshot")
	}
	local := skills.SnapshotFromPackages(packages, deviceID)
	remote, err := client.Head()
	if errors.Is(err, skills.ErrNoRemoteSnapshot) {
		remote = skills.Snapshot{}
	} else if err != nil {
		return skills.Snapshot{}, false, err
	}
	if remote.Revision != "" {
		if state.Revision == "" {
			return skills.Snapshot{}, false, fmt.Errorf("remote skills already exist; run `yiduo skills restore` before backing up from this device")
		}
		if state.Revision != remote.Revision {
			return skills.Snapshot{}, false, fmt.Errorf("remote skills changed from revision %s to %s; run `yiduo skills pull` before pushing", state.Revision, remote.Revision)
		}
	}
	merged := skills.MergePreservingRemote(remote, local)
	if skills.SameContent(merged, remote) {
		state.Revision = remote.Revision
		state.LastBackupAt = time.Now().UTC().Format(time.RFC3339Nano)
		state.LastAttemptAt = state.LastBackupAt
		if err := skills.SaveState(statePath, state); err != nil {
			return skills.Snapshot{}, false, err
		}
		return remote, false, nil
	}

	missing, err := client.Plan(skills.Hashes(merged))
	if err != nil {
		return skills.Snapshot{}, false, err
	}
	archiveByHash := map[string][]byte{}
	for _, pkg := range packages {
		archiveByHash[pkg.Manifest.Hash] = pkg.Archive
	}
	for _, hash := range missing {
		archive, ok := archiveByHash[hash]
		if !ok {
			return skills.Snapshot{}, false, fmt.Errorf("server is missing remote package %s and no local copy is available", hash)
		}
		if err := client.Upload(hash, archive); err != nil {
			return skills.Snapshot{}, false, fmt.Errorf("upload package %s: %w", hash, err)
		}
	}
	committed, err := client.Commit(merged)
	if err != nil {
		return skills.Snapshot{}, false, err
	}
	localState := skills.StateFromSnapshot(local)
	localState.Revision = committed.Revision
	localState.LastBackupAt = time.Now().UTC().Format(time.RFC3339Nano)
	localState.LastAttemptAt = localState.LastBackupAt
	if err := skills.SaveState(statePath, localState); err != nil {
		return skills.Snapshot{}, false, err
	}
	return committed, true, nil
}

func maybeAutoBackupSkills(server string, token string, deviceID string, logf func(string, ...any)) {
	statePath := skillsStatePath()
	state := skills.LoadState(statePath)
	if last := skills.ParseTime(state.LastAttemptAt); !last.IsZero() && time.Since(last) < skillBackupInterval {
		return
	}
	state.LastAttemptAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := skills.SaveState(statePath, state); err != nil {
		logf("skills backup skipped: %v", err)
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		logf("skills backup skipped: %v", err)
		return
	}
	committed, changed, err := backupSkills(skills.Client{Server: server, AuthToken: token}, deviceID, home, statePath, state)
	if err != nil {
		logf("skills backup skipped: %v", err)
		return
	}
	if changed {
		logf("skills backup complete: revision=%s packages=%d", committed.Revision, len(committed.Skills))
	}
}

func discoverSkills(home string) ([]skills.Package, error) {
	packages, err := skills.Discover(skills.DiscoverOptions{Home: home})
	if err != nil {
		return nil, fmt.Errorf("failed to discover skills: %w", err)
	}
	return packages, nil
}

func skillsStatePath() string {
	return filepath.Join(expandUser("~/.yiduo"), "skills-state.json")
}

func isHelpArg(value string) bool {
	return value == "help" || value == "--help" || value == "-h"
}

func printSkillsHelp() {
	fmt.Print(`yiduo skills - Back up and restore user-level agent skills

Usage:
  yiduo skills status
  yiduo skills list
  yiduo skills diff
  yiduo skills sync
  yiduo skills push
  yiduo skills pull
  yiduo skills restore

Commands:
  sync, push      Back up local changes without propagating deletions
  pull, restore   Install the latest remote snapshot without overwriting conflicts
  status          Show local and remote package counts
  list            List locally discovered skill packages
  diff            Compare local bindings with the remote snapshot
`)
}

func printSkillPackages(packages []skills.Package) {
	if len(packages) == 0 {
		fmt.Println("No user-level skills found.")
		return
	}
	for _, pkg := range packages {
		bindings := make([]string, 0, len(pkg.Manifest.Bindings))
		for _, binding := range pkg.Manifest.Bindings {
			bindings = append(bindings, skills.BindingKey(binding))
		}
		sort.Strings(bindings)
		fmt.Printf("%s  %s  %s\n", pkg.Manifest.Name, pkg.Manifest.Hash[:12], strings.Join(bindings, ", "))
	}
}

func printSkillsDiff(local skills.Snapshot, remote skills.Snapshot) {
	localBindings := snapshotBindings(local)
	remoteBindings := snapshotBindings(remote)
	keys := map[string]bool{}
	for key := range localBindings {
		keys[key] = true
	}
	for key := range remoteBindings {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	changes := 0
	for _, key := range ordered {
		localHash, localOK := localBindings[key]
		remoteHash, remoteOK := remoteBindings[key]
		switch {
		case localOK && !remoteOK:
			fmt.Printf("+ %s %s\n", key, localHash[:12])
			changes++
		case !localOK && remoteOK:
			fmt.Printf("- %s %s\n", key, remoteHash[:12])
			changes++
		case localHash != remoteHash:
			fmt.Printf("~ %s %s -> %s\n", key, remoteHash[:12], localHash[:12])
			changes++
		}
	}
	if changes == 0 {
		fmt.Println("Local skills match the remote snapshot.")
	}
}

func snapshotBindings(snapshot skills.Snapshot) map[string]string {
	values := map[string]string{}
	for _, skill := range snapshot.Skills {
		for _, binding := range skill.Bindings {
			values[skills.BindingKey(binding)] = skill.Hash
		}
	}
	return values
}

func countPackageBindings(packages []skills.Package) int {
	total := 0
	for _, pkg := range packages {
		total += len(pkg.Manifest.Bindings)
	}
	return total
}
