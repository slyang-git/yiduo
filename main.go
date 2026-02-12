package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type SyncPayload struct {
	AgentID  string        `json:"agent_id"`
	Tool     string        `json:"tool"`
	Host     string        `json:"host"`
	Device   DeviceInfo    `json:"device"`
	Sessions []SyncSession `json:"sessions"`
}

type DeviceInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

type SyncSession struct {
	ID                     string        `json:"id"`
	Title                  string        `json:"title"`
	Model                  string        `json:"model"`
	Cwd                    string        `json:"cwd"`
	StartedAt              string        `json:"started_at"`
	EndedAt                string        `json:"ended_at"`
	TotalTokens            int           `json:"total_tokens"`
	TotalInputTokens       int           `json:"total_input_tokens"`
	TotalOutputTokens      int           `json:"total_output_tokens"`
	TotalCachedInputTokens int           `json:"total_cached_input_tokens"`
	TotalReasoningTokens   int           `json:"total_reasoning_tokens"`
	Messages               []SyncMessage `json:"messages"`
}

type SyncMessage struct {
	Index             int    `json:"index"`
	Role              string `json:"role"`
	Content           string `json:"content"`
	Timestamp         string `json:"timestamp"`
	InputTokens       int    `json:"input_tokens,omitempty"`
	OutputTokens      int    `json:"output_tokens,omitempty"`
	CachedInputTokens int    `json:"cached_input_tokens,omitempty"`
	ReasoningTokens   int    `json:"reasoning_tokens,omitempty"`
	TotalTokens       int    `json:"total_tokens,omitempty"`
}

type tokenTotals struct {
	totalTokens           int
	inputTokens           int
	outputTokens          int
	cachedInputTokens     int
	reasoningOutputTokens int
}

type syncState struct {
	LastSync map[string]string `json:"last_sync"`
}

type daemonMeta struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

type syncOptions struct {
	incremental bool
	state       *syncState
	logf        func(format string, args ...any)
}

const daemonEnv = "YIDUO_DAEMON"

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "sync" {
		args = args[1:]
	}

	server := flag.String("server", "", "API server base URL")
	tool := flag.String("tool", "", "tool name override")
	source := flag.String("source", "auto", "data source: auto|codex|claude|gemini|qwen|cline|continue|kilocode|cursor|amp|opencode|pi|antigravity|droid (comma-separated)")
	authToken := flag.String("auth-token", "", "auth token for sync authentication")
	deviceToken := flag.String("device-token", "", "deprecated: use --auth-token")
	daemon := flag.Bool("daemon", false, "run periodic sync in background")
	daemonShort := flag.Bool("d", false, "shorthand for --daemon")
	codexRoot := flag.String("codex-root", envOrDefault("CODEX_ROOT", "~/.codex"), "Codex root")
	claudeRoot := flag.String("claude-root", envOrDefault("CLAUDE_ROOT", "~/.claude"), "Claude Code root")
	geminiRoot := flag.String("gemini-root", envOrDefault("GEMINI_ROOT", "~/.gemini"), "Gemini CLI root")
	qwenRoot := flag.String("qwen-root", envOrDefault("QWEN_ROOT", "~/.qwen"), "Qwen root")
	clineRoot := flag.String("cline-root", envOrDefault("CLINE_ROOT", "~/.cline"), "Cline root")
	continueRoot := flag.String("continue-root", envOrDefault("CONTINUE_ROOT", "~/.continue"), "Continue root")
	kiloRoot := flag.String("kilocode-root", envOrDefault("KILOCODE_ROOT", "~/.kilocode"), "KiloCode root")
	cursorRoot := flag.String("cursor-root", envOrDefault("CURSOR_ROOT", "~/.cursor"), "Cursor root")
	ampRoot := flag.String("amp-root", envOrDefault("AMP_ROOT", "~/.local/share/amp"), "Amp root")
	opencodeRoot := flag.String("opencode-root", envOrDefault("OPENCODE_ROOT", "~/.local/share/opencode"), "OpenCode root")
	piRoot := flag.String("pi-root", envOrDefault("PI_ROOT", "~/.pi"), "PI root")
	antigravityRoot := flag.String("antigravity-root", envOrDefault("ANTIGRAVITY_ROOT", "~/.gemini/antigravity"), "Antigravity root")
	droidRoot := flag.String("droid-root", envOrDefault("DROID_ROOT", "~/.factory"), "Droid root")
	agentID := flag.String("agent-id", "local", "agent id")
	host := flag.String("host", hostname(), "host name")
	if err := flag.CommandLine.Parse(args); err != nil {
		os.Exit(2)
	}

	extraArgs := flag.Args()
	forceDaemonStart := false
	isDaemonWorker := os.Getenv(daemonEnv) != ""
	if len(extraArgs) > 0 {
		switch extraArgs[0] {
		case "status":
			printDaemonStatus()
			return
		case "stop":
			if running, pid := daemonRunning(); running {
				fmt.Printf("yiduo sync daemon stopping (pid %d)\n", pid)
				if startedAt, ok := daemonStartedAt(pid); ok {
					fmt.Printf("Started at: %s\n", formatStatusTime(startedAt))
					fmt.Printf("Uptime: %s\n", formatDuration(time.Since(startedAt)))
				}
			}
			if err := stopDaemon(); err != nil {
				fmt.Fprintf(os.Stderr, "failed to stop daemon: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("yiduo sync daemon stopped")
			return
		case "start":
			forceDaemonStart = true
		case "restart":
			if isDaemonWorker {
				forceDaemonStart = true
				break
			}
			if running, pid := daemonRunning(); running {
				fmt.Printf("yiduo sync daemon restarting (pid %d)\n", pid)
				if startedAt, ok := daemonStartedAt(pid); ok {
					fmt.Printf("Started at: %s\n", formatStatusTime(startedAt))
					fmt.Printf("Uptime: %s\n", formatDuration(time.Since(startedAt)))
				}
				if err := stopDaemon(); err != nil {
					fmt.Fprintf(os.Stderr, "failed to stop daemon: %v\n", err)
					os.Exit(1)
				}
				if err := waitForDaemonStop(5 * time.Second); err != nil {
					fmt.Fprintf(os.Stderr, "failed to restart daemon: %v\n", err)
					os.Exit(1)
				}
			} else {
				fmt.Println("yiduo sync daemon not running, starting a new daemon")
			}
			if err := spawnDaemon(); err != nil {
				fmt.Fprintf(os.Stderr, "failed to start daemon: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("yiduo sync daemon restarted")
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", extraArgs[0])
			os.Exit(2)
		}
	}

	daemonEnabled := *daemon || *daemonShort || forceDaemonStart
	daemonWorker := daemonEnabled && isDaemonWorker
	if daemonEnabled && !daemonWorker {
		if running, pid := daemonRunning(); running {
			fmt.Printf("yiduo sync daemon already running (pid %d)\n", pid)
			return
		}
		if err := spawnDaemon(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start daemon: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("yiduo sync daemon started")
		return
	}

	config := loadConfig()
	config, err := ensureDeviceID(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to ensure device id: %v\n", err)
		os.Exit(1)
	}
	resolvedServer := firstNonEmpty(*server, os.Getenv("AI_WRAPPED_SERVER"), config.Server, "https://yiduo.one/")
	resolvedDeviceToken := firstNonEmpty(*authToken, *deviceToken, os.Getenv("AI_WRAPPED_SYNC_TOKEN"), os.Getenv("AI_WRAPPED_DEVICE_TOKEN"), config.AuthToken, config.LegacyDeviceToken)
	deviceInfo := DeviceInfo{
		ID:   config.DeviceID,
		Name: *host,
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	sources, err := parseSources(*source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid source: %v\n", err)
		os.Exit(1)
	}

	toolOverride := strings.TrimSpace(*tool)
	if toolOverride != "" && len(sources) > 1 {
		fmt.Fprintln(os.Stderr, "tool override ignored when syncing multiple sources")
		toolOverride = ""
	}

	options := syncOptions{}
	if daemonWorker {
		options.incremental = true
		state := loadSyncState()
		options.state = &state
	}

	runOnce := func() (int, error) {
		if options.logf != nil {
			options.logf("yiduo sync start")
		}
		return syncOnce(syncParams{
			server:          resolvedServer,
			deviceToken:     resolvedDeviceToken,
			deviceInfo:      deviceInfo,
			agentID:         *agentID,
			host:            *host,
			toolOverride:    toolOverride,
			sources:         sources,
			codexRoot:       expandUser(*codexRoot),
			claudeRoot:      expandUser(*claudeRoot),
			geminiRoot:      expandUser(*geminiRoot),
			qwenRoot:        expandUser(*qwenRoot),
			clineRoot:       expandUser(*clineRoot),
			continueRoot:    expandUser(*continueRoot),
			kiloRoot:        expandUser(*kiloRoot),
			cursorRoot:      expandUser(*cursorRoot),
			ampRoot:         expandUser(*ampRoot),
			opencodeRoot:    expandUser(*opencodeRoot),
			piRoot:          expandUser(*piRoot),
			antigravityRoot: expandUser(*antigravityRoot),
			droidRoot:       expandUser(*droidRoot),
			logf:            options.logf,
		}, options)
	}

	if daemonWorker {
		if err := writeDaemonPid(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write daemon pid: %v\n", err)
		}
		logFile, err := openDaemonLog()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open daemon log: %v\n", err)
		}
		if logFile != nil {
			defer logFile.Close()
			options.logf = func(format string, args ...any) {
				logDaemonf(logFile, format, args...)
			}
		}
		runSyncLoop(runOnce, options.state, logFile)
		return
	}

	options.logf = func(format string, args ...any) {
		fmt.Printf(format+"\n", args...)
	}

	totalSessions, err := runOnce()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if len(sources) == 1 {
		fmt.Printf("Synced %d sessions to %s\n", totalSessions, resolvedServer)
	} else {
		fmt.Printf("Synced %d sessions across %d tools to %s\n", totalSessions, len(sources), resolvedServer)
	}
}

type syncParams struct {
	server          string
	deviceToken     string
	deviceInfo      DeviceInfo
	agentID         string
	host            string
	toolOverride    string
	sources         []string
	codexRoot       string
	claudeRoot      string
	geminiRoot      string
	qwenRoot        string
	clineRoot       string
	continueRoot    string
	kiloRoot        string
	cursorRoot      string
	ampRoot         string
	opencodeRoot    string
	piRoot          string
	antigravityRoot string
	droidRoot       string
	logf            func(format string, args ...any)
}

func syncOnce(params syncParams, options syncOptions) (int, error) {
	totalSessions := 0
	for _, sourceName := range params.sources {
		toolName := defaultToolName(sourceName)
		if params.toolOverride != "" {
			toolName = params.toolOverride
		}

		var sessions []SyncSession
		var err error
		switch sourceName {
		case "codex":
			sessions, err = loadCodexSessions(params.codexRoot)
		case "claude":
			sessions, err = loadClaudeSessions(params.claudeRoot)
		case "gemini":
			sessions, err = loadGeminiSessions(params.geminiRoot)
		case "qwen":
			sessions, err = loadQwenSessions(params.qwenRoot)
		case "cline":
			sessions, err = loadClineSessions(params.clineRoot)
		case "continue":
			sessions, err = loadContinueSessions(params.continueRoot)
		case "kilocode":
			sessions, err = loadKiloCodeSessions(params.kiloRoot)
		case "cursor":
			sessions, err = loadCursorSessions(params.cursorRoot, params.logf)
		case "amp":
			sessions, err = loadAmpSessions(params.ampRoot)
		case "opencode":
			sessions, err = loadOpenCodeSessions(params.opencodeRoot)
		case "pi":
			sessions, err = loadPiSessions(params.piRoot)
		case "antigravity":
			sessions, err = loadAntigravitySessions(params.antigravityRoot)
		case "droid":
			sessions, err = loadDroidSessions(params.droidRoot)
		default:
			return 0, fmt.Errorf("unknown source: %s", sourceName)
		}
		if err != nil {
			return 0, fmt.Errorf("failed to load %s sessions: %v", sourceName, err)
		}

		var maxUpdated time.Time
		if options.incremental {
			since := time.Time{}
			if options.state != nil {
				if ts, ok := options.state.LastSync[sourceName]; ok {
					if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
						since = parsed
					}
				}
			}
			sessions, maxUpdated = filterSessionsSince(sessions, since)
			if len(sessions) == 0 {
				if options.logf != nil {
					options.logf("sync source=%s tool=%s sessions=%d", sourceName, toolName, 0)
				}
				continue
			}
		}
		if options.logf != nil {
			options.logf("sync source=%s tool=%s sessions=%d", sourceName, toolName, len(sessions))
		}

		payload := SyncPayload{
			AgentID:  params.agentID,
			Tool:     toolName,
			Host:     params.host,
			Device:   params.deviceInfo,
			Sessions: sessions,
		}

		if err := syncPayload(params.server, params.deviceToken, payload); err != nil {
			return 0, fmt.Errorf("sync failed for %s: %v", sourceName, err)
		}

		totalSessions += len(sessions)
		if options.incremental && options.state != nil && !maxUpdated.IsZero() {
			if options.state.LastSync == nil {
				options.state.LastSync = map[string]string{}
			}
			options.state.LastSync[sourceName] = maxUpdated.UTC().Format(time.RFC3339)
		}
	}
	return totalSessions, nil
}

func runSyncLoop(runOnce func() (int, error), state *syncState, logFile *os.File) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	defer removeDaemonState()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	logDaemonf(logFile, "daemon started")
	for {
		logDaemonf(logFile, "sync start")
		totalSessions, err := runOnce()
		if err != nil {
			logDaemonf(logFile, "sync failed: %v", err)
			fmt.Fprintf(os.Stderr, "sync failed: %v\n", err)
		} else if state != nil {
			if err := saveSyncState(*state); err != nil {
				logDaemonf(logFile, "failed to save sync state: %v", err)
				fmt.Fprintf(os.Stderr, "failed to save sync state: %v\n", err)
			}
			logDaemonf(logFile, "sync complete: sessions=%d", totalSessions)
		} else {
			logDaemonf(logFile, "sync complete: sessions=%d", totalSessions)
		}

		select {
		case <-quit:
			logDaemonf(logFile, "daemon stopped")
			return
		case <-ticker.C:
		}
	}
}

func filterSessionsSince(sessions []SyncSession, since time.Time) ([]SyncSession, time.Time) {
	if since.IsZero() {
		return sessions, maxSessionTime(sessions)
	}
	filtered := make([]SyncSession, 0, len(sessions))
	var maxUpdated time.Time
	for _, session := range sessions {
		updatedAt := sessionUpdatedAt(session)
		if updatedAt.IsZero() || updatedAt.After(since) {
			filtered = append(filtered, session)
			if updatedAt.After(maxUpdated) {
				maxUpdated = updatedAt
			}
		}
	}
	return filtered, maxUpdated
}

func sessionUpdatedAt(session SyncSession) time.Time {
	if session.EndedAt != "" {
		if ts, err := time.Parse(time.RFC3339, session.EndedAt); err == nil {
			return ts
		}
	}
	if session.StartedAt != "" {
		if ts, err := time.Parse(time.RFC3339, session.StartedAt); err == nil {
			return ts
		}
	}
	return time.Time{}
}

func maxSessionTime(sessions []SyncSession) time.Time {
	var maxUpdated time.Time
	for _, session := range sessions {
		updatedAt := sessionUpdatedAt(session)
		if updatedAt.After(maxUpdated) {
			maxUpdated = updatedAt
		}
	}
	return maxUpdated
}

func loadSyncState() syncState {
	path := syncStatePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return syncState{LastSync: map[string]string{}}
	}
	var state syncState
	if err := json.Unmarshal(raw, &state); err != nil {
		return syncState{LastSync: map[string]string{}}
	}
	if state.LastSync == nil {
		state.LastSync = map[string]string{}
	}
	return state
}

func saveSyncState(state syncState) error {
	path := syncStatePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func syncStatePath() string {
	return filepath.Join(expandUser("~/.yiduo"), "sync-state.json")
}

func spawnDaemon() error {
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(), daemonEnv+"=1")
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err == nil {
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
	}
	if err := cmd.Start(); err != nil {
		if devNull != nil {
			_ = devNull.Close()
		}
		return err
	}
	if err := writeDaemonPidWith(cmd.Process.Pid); err != nil {
		if devNull != nil {
			_ = devNull.Close()
		}
		return err
	}
	if devNull != nil {
		_ = devNull.Close()
	}
	return cmd.Process.Release()
}

func printDaemonStatus() {
	if running, pid := daemonRunning(); running {
		fmt.Printf("yiduo sync daemon running (pid %d)\n", pid)
		if startedAt, ok := daemonStartedAt(pid); ok {
			fmt.Printf("Started at: %s\n", formatStatusTime(startedAt))
			fmt.Printf("Uptime: %s\n", formatDuration(time.Since(startedAt)))
		}
		return
	}
	fmt.Println("yiduo sync daemon not running")
	if meta, ok := loadDaemonMeta(); ok {
		if startedAt, err := time.Parse(time.RFC3339, meta.StartedAt); err == nil {
			fmt.Printf("Last started at: %s\n", formatStatusTime(startedAt))
		}
	}
}

func stopDaemon() error {
	pid, err := readDaemonPid()
	if err != nil {
		return err
	}
	if pid == 0 {
		return fmt.Errorf("daemon pid not found")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	return nil
}

func daemonRunning() (bool, int) {
	pid, err := readDaemonPid()
	if err != nil || pid == 0 {
		return false, 0
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false, 0
	}
	return true, pid
}

func waitForDaemonStop(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		running, _ := daemonRunning()
		if !running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon did not stop within %s", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func readDaemonPid() (int, error) {
	raw, err := os.ReadFile(daemonPidPath())
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return 0, nil
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func writeDaemonPid() error {
	return writeDaemonPidWithStart(os.Getpid(), time.Now().UTC())
}

func writeDaemonPidWith(pid int) error {
	return writeDaemonPidWithStart(pid, time.Now().UTC())
}

func writeDaemonPidWithStart(pid int, startedAt time.Time) error {
	path := daemonPidPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return err
	}
	return saveDaemonMeta(daemonMeta{
		PID:       pid,
		StartedAt: startedAt.UTC().Format(time.RFC3339),
	})
}

func removeDaemonPid() {
	_ = os.Remove(daemonPidPath())
}

func removeDaemonState() {
	removeDaemonPid()
	_ = os.Remove(daemonMetaPath())
}

func daemonPidPath() string {
	return filepath.Join(expandUser("~/.yiduo"), "daemon.pid")
}

func daemonMetaPath() string {
	return filepath.Join(expandUser("~/.yiduo"), "daemon-meta.json")
}

func saveDaemonMeta(meta daemonMeta) error {
	path := daemonMetaPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func loadDaemonMeta() (daemonMeta, bool) {
	raw, err := os.ReadFile(daemonMetaPath())
	if err != nil {
		return daemonMeta{}, false
	}
	var meta daemonMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return daemonMeta{}, false
	}
	if meta.PID == 0 || strings.TrimSpace(meta.StartedAt) == "" {
		return daemonMeta{}, false
	}
	return meta, true
}

func daemonStartedAt(pid int) (time.Time, bool) {
	meta, ok := loadDaemonMeta()
	if ok && meta.PID == pid {
		if startedAt, err := time.Parse(time.RFC3339, meta.StartedAt); err == nil {
			return startedAt, true
		}
	}
	if info, err := os.Stat(daemonPidPath()); err == nil {
		return info.ModTime(), true
	}
	return time.Time{}, false
}

func formatStatusTime(ts time.Time) string {
	return ts.Local().Format("2006-01-02 15:04:05 -0700")
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}
func daemonLogPath() string {
	return filepath.Join(expandUser("~/.yiduo"), "sync.log")
}

func openDaemonLog() (*os.File, error) {
	path := daemonLogPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

func logDaemonf(file *os.File, format string, args ...any) {
	if file == nil {
		return
	}
	timestamp := time.Now().Format(time.RFC3339)
	fmt.Fprintf(file, "%s ", timestamp)
	fmt.Fprintf(file, format, args...)
	fmt.Fprintln(file)
}

func loadCodexSessions(root string) ([]SyncSession, error) {
	sessionsDir := filepath.Join(root, "sessions")
	entries, err := listJSONL(sessionsDir)
	if err != nil {
		return nil, err
	}

	results := make([]SyncSession, 0, len(entries))
	for _, path := range entries {
		session, ok := parseCodexSession(path)
		if !ok {
			continue
		}
		results = append(results, session)
	}

	return results, nil
}

func listJSONL(root string) ([]string, error) {
	var paths []string
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return []string{}, nil
	}

	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".jsonl") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(paths)
	return paths, nil
}

func parseCodexSession(filePath string) (SyncSession, bool) {
	file, err := os.Open(filePath)
	if err != nil {
		return SyncSession{}, false
	}
	defer file.Close()

	stat, _ := file.Stat()
	endedAt := ""
	if stat != nil {
		endedAt = stat.ModTime().Format(time.RFC3339)
	}

	var session SyncSession
	session.EndedAt = endedAt

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineIndex := 0
	msgIndex := 0
	var tokens tokenTotals

	for scanner.Scan() {
		lineText := strings.TrimSpace(scanner.Text())
		if lineText == "" {
			lineIndex++
			continue
		}
		item := map[string]any{}
		dec := json.NewDecoder(strings.NewReader(lineText))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			lineIndex++
			continue
		}

		typeValue := stringFrom(item["type"])
		if lineIndex == 0 && typeValue == "session_meta" {
			payload := mapFrom(item["payload"])
			session.ID = stringFrom(payload["id"])
			session.StartedAt = stringFrom(payload["timestamp"])
			session.Title = firstStringFromMap(payload, "title", "name")
			session.Cwd = firstStringFromMap(
				payload,
				"cwd",
				"workspace",
				"workspaceDirectory",
				"workspace_directory",
				"projectPath",
				"project_path",
				"repo_path",
				"path",
			)
		}

		if typeValue == "turn_context" {
			payload := mapFrom(item["payload"])
			model := stringFrom(payload["model"])
			if model != "" {
				session.Model = model
			}
		}

		if typeValue == "event_msg" {
			payload := mapFrom(item["payload"])
			if stringFrom(payload["type"]) == "token_count" {
				info := mapFrom(payload["info"])
				total := mapFrom(info["total_token_usage"])
				tokens.totalTokens = intFrom(total["total_tokens"], tokens.totalTokens)
				tokens.inputTokens = intFrom(total["input_tokens"], tokens.inputTokens)
				tokens.outputTokens = intFrom(total["output_tokens"], tokens.outputTokens)
				tokens.cachedInputTokens = intFrom(total["cached_input_tokens"], tokens.cachedInputTokens)
				tokens.reasoningOutputTokens = intFrom(total["reasoning_output_tokens"], tokens.reasoningOutputTokens)
			}
			lineIndex++
			continue
		}

		if typeValue == "response_item" {
			payload := mapFrom(item["payload"])
			if stringFrom(payload["type"]) != "message" {
				lineIndex++
				continue
			}
			content := parseMessageContent(payload["content"])
			if content != "" {
				session.Messages = append(session.Messages, SyncMessage{
					Index:   msgIndex,
					Role:    stringFrom(payload["role"]),
					Content: content,
				})
				msgIndex++
			}
		}
		lineIndex++
	}

	session.TotalTokens = tokens.totalTokens
	session.TotalInputTokens = tokens.inputTokens
	session.TotalOutputTokens = tokens.outputTokens
	session.TotalCachedInputTokens = tokens.cachedInputTokens
	session.TotalReasoningTokens = tokens.reasoningOutputTokens

	if session.ID == "" {
		return SyncSession{}, false
	}
	if session.StartedAt == "" && endedAt != "" {
		session.StartedAt = endedAt
	}
	if session.Cwd == "" {
		for _, msg := range session.Messages {
			if msg.Content == "" {
				continue
			}
			if cwd := extractTaggedValue(msg.Content, "cwd"); cwd != "" {
				session.Cwd = cwd
				break
			}
			if cwd := extractWorkspaceDirectory(msg.Content); cwd != "" {
				session.Cwd = cwd
				break
			}
		}
	}
	session.Cwd = normalizeCwd(session.Cwd)
	if session.Title == "" && session.Cwd != "" {
		session.Title = session.Cwd
	}

	return session, true
}

type geminiSession struct {
	SessionID   string          `json:"sessionId"`
	ProjectHash string          `json:"projectHash"`
	StartTime   string          `json:"startTime"`
	LastUpdated string          `json:"lastUpdated"`
	Messages    []geminiMessage `json:"messages"`
}

type geminiMessage struct {
	ID        string        `json:"id"`
	Timestamp string        `json:"timestamp"`
	Type      string        `json:"type"`
	Content   string        `json:"content"`
	Model     string        `json:"model"`
	Tokens    *geminiTokens `json:"tokens"`
}

type geminiTokens struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
	Tool     int `json:"tool"`
	Total    int `json:"total"`
}

type claudeIndex struct {
	Entries []claudeEntry `json:"entries"`
}

type claudeEntry struct {
	SessionID    string `json:"sessionId"`
	FullPath     string `json:"fullPath"`
	Created      string `json:"created"`
	Modified     string `json:"modified"`
	FirstPrompt  string `json:"firstPrompt"`
	ProjectPath  string `json:"projectPath"`
	MessageCount int    `json:"messageCount"`
}

func loadClaudeSessions(root string) ([]SyncSession, error) {
	projectsDir := filepath.Join(root, "projects")
	info, err := os.Stat(projectsDir)
	if err != nil || !info.IsDir() {
		return []SyncSession{}, nil
	}

	indexPaths, err := findClaudeIndexes(projectsDir)
	if err != nil {
		return nil, err
	}

	var sessions []SyncSession
	for _, indexPath := range indexPaths {
		items, err := parseClaudeIndex(indexPath)
		if err != nil {
			continue
		}
		for _, entry := range items {
			if entry.SessionID == "" || entry.FullPath == "" {
				continue
			}
			if isClaudeIndexProbe(entry) {
				continue
			}
			session := SyncSession{
				ID:        entry.SessionID,
				Title:     entry.FirstPrompt,
				Cwd:       entry.ProjectPath,
				StartedAt: entry.Created,
				EndedAt:   entry.Modified,
			}
			ok, skip := parseClaudeSession(entry.FullPath, &session)
			if !ok || skip {
				continue
			}
			if session.TotalTokens == 0 && session.Model == "" {
				title := strings.ToLower(strings.TrimSpace(session.Title))
				if title == "what is 2+2?" || len(session.Messages) <= 2 {
					continue
				}
			}
			sessions = append(sessions, session)
		}
	}

	return sessions, nil
}

func isClaudeIndexProbe(entry claudeEntry) bool {
	title := strings.TrimSpace(entry.FirstPrompt)
	if strings.EqualFold(title, "what is 2+2?") && entry.MessageCount <= 2 && entry.ProjectPath == "/" {
		return true
	}
	return false
}

func findClaudeIndexes(projectsDir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "sessions-index.json" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func parseClaudeIndex(path string) ([]claudeEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx claudeIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, err
	}
	return idx.Entries, nil
}

func parseClaudeSession(filePath string, session *SyncSession) (bool, bool) {
	file, err := os.Open(filePath)
	if err != nil {
		return false, false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	msgIndex := 0
	userMessages := 0
	assistantMessages := 0
	authError := false
	for scanner.Scan() {
		lineText := strings.TrimSpace(scanner.Text())
		if lineText == "" {
			continue
		}
		item := map[string]any{}
		dec := json.NewDecoder(strings.NewReader(lineText))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			continue
		}

		typeValue := stringFrom(item["type"])
		if typeValue != "user" && typeValue != "assistant" {
			continue
		}
		message := mapFrom(item["message"])
		role := stringFrom(message["role"])
		if role == "" {
			role = typeValue
		}
		content := parseClaudeContent(message["content"])
		if content == "" {
			continue
		}
		if typeValue == "assistant" {
			if isTrue(item["isApiErrorMessage"]) || stringFrom(item["error"]) == "authentication_failed" {
				authError = true
			}
			lower := strings.ToLower(content)
			if strings.Contains(lower, "invalid api key") || strings.Contains(lower, "please run /login") {
				authError = true
			}
		}
		timestamp := stringFrom(item["timestamp"])
		msg := SyncMessage{
			Index:     msgIndex,
			Role:      role,
			Content:   content,
			Timestamp: timestamp,
		}
		msgIndex++
		if typeValue == "user" {
			userMessages++
		} else {
			assistantMessages++
		}

		if typeValue == "assistant" {
			model := stringFrom(message["model"])
			if model != "" {
				session.Model = model
			}
			usage := mapFrom(message["usage"])
			inputTokens := intFrom(usage["input_tokens"], 0)
			outputTokens := intFrom(usage["output_tokens"], 0)
			cachedInputTokens := intFrom(usage["cache_read_input_tokens"], 0)
			reasoningTokens := intFrom(usage["reasoning_output_tokens"], 0)
			msg.InputTokens = inputTokens
			msg.OutputTokens = outputTokens
			msg.CachedInputTokens = cachedInputTokens
			msg.ReasoningTokens = reasoningTokens
			msg.TotalTokens = inputTokens + outputTokens + cachedInputTokens + reasoningTokens
			session.TotalInputTokens += inputTokens
			session.TotalOutputTokens += outputTokens
			session.TotalCachedInputTokens += cachedInputTokens
			session.TotalReasoningTokens += reasoningTokens
			session.TotalTokens = session.TotalInputTokens + session.TotalOutputTokens
		}
		session.Messages = append(session.Messages, msg)
	}

	if session.ID == "" {
		return false, false
	}

	if authError && (userMessages+assistantMessages) <= 2 && session.TotalTokens == 0 {
		return true, true
	}

	return true, false
}

func loadGeminiSessions(root string) ([]SyncSession, error) {
	tmpDir := filepath.Join(root, "tmp")
	info, err := os.Stat(tmpDir)
	if err != nil || !info.IsDir() {
		return []SyncSession{}, nil
	}

	sessionFiles, err := findGeminiSessions(tmpDir)
	if err != nil {
		return nil, err
	}

	results := make([]SyncSession, 0, len(sessionFiles))
	for _, path := range sessionFiles {
		session, ok := parseGeminiSession(path)
		if !ok {
			continue
		}
		results = append(results, session)
	}
	return results, nil
}

func findGeminiSessions(tmpDir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(tmpDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, "session-") && strings.HasSuffix(name, ".json") &&
			strings.Contains(path, string(filepath.Separator)+"chats"+string(filepath.Separator)) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func parseGeminiSession(path string) (SyncSession, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SyncSession{}, false
	}
	var sess geminiSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return SyncSession{}, false
	}
	if sess.SessionID == "" {
		return SyncSession{}, false
	}

	startedAt := sess.StartTime
	endedAt := sess.LastUpdated
	if endedAt == "" {
		if stat, err := os.Stat(path); err == nil {
			endedAt = stat.ModTime().Format(time.RFC3339)
		}
	}

	session := SyncSession{
		ID:        sess.SessionID,
		StartedAt: startedAt,
		EndedAt:   endedAt,
	}

	msgIndex := 0
	for _, msg := range sess.Messages {
		role := geminiRole(msg.Type)
		content := strings.TrimSpace(msg.Content)
		if content != "" {
			if session.Title == "" && role == "user" {
				session.Title = content
			}
			message := SyncMessage{
				Index:     msgIndex,
				Role:      role,
				Content:   content,
				Timestamp: msg.Timestamp,
			}
			if msg.Tokens != nil {
				message.InputTokens = msg.Tokens.Input
				message.OutputTokens = msg.Tokens.Output
				message.CachedInputTokens = msg.Tokens.Cached
				message.ReasoningTokens = msg.Tokens.Thoughts
				if msg.Tokens.Total > 0 {
					message.TotalTokens = msg.Tokens.Total
				} else {
					message.TotalTokens = msg.Tokens.Input + msg.Tokens.Output + msg.Tokens.Cached + msg.Tokens.Thoughts
				}
			}
			session.Messages = append(session.Messages, message)
			msgIndex++
		}
		if msg.Model != "" {
			session.Model = msg.Model
		}
		if msg.Tokens != nil {
			session.TotalInputTokens += msg.Tokens.Input
			session.TotalOutputTokens += msg.Tokens.Output
			session.TotalCachedInputTokens += msg.Tokens.Cached
			session.TotalReasoningTokens += msg.Tokens.Thoughts
			if msg.Tokens.Total > 0 {
				session.TotalTokens += msg.Tokens.Total
			} else {
				session.TotalTokens += msg.Tokens.Input + msg.Tokens.Output
			}
		}
		if session.StartedAt == "" && msg.Timestamp != "" {
			session.StartedAt = msg.Timestamp
		}
		if session.EndedAt == "" && msg.Timestamp != "" {
			session.EndedAt = msg.Timestamp
		}
	}

	if session.Title == "" {
		session.Title = "Gemini session"
	}
	if session.TotalTokens == 0 {
		session.TotalTokens = session.TotalInputTokens + session.TotalOutputTokens
	}

	return session, true
}

func geminiRole(value string) string {
	switch value {
	case "user":
		return "user"
	case "gemini", "assistant":
		return "assistant"
	default:
		if value == "" {
			return "assistant"
		}
		return value
	}
}

type qwenLogItem struct {
	SessionID string `json:"sessionId"`
	MessageID int    `json:"messageId"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

type qwenMessage struct {
	MessageID int
	Role      string
	Content   string
	Timestamp string
}

func loadQwenSessions(root string) ([]SyncSession, error) {
	tmpDir := filepath.Join(root, "tmp")
	projectsDir := filepath.Join(root, "projects")
	merged := map[string]SyncSession{}

	if info, err := os.Stat(tmpDir); err == nil && info.IsDir() {
		logs, err := findQwenLogs(tmpDir)
		if err != nil {
			return nil, err
		}
		for _, path := range logs {
			items, err := parseQwenLog(path)
			if err != nil {
				continue
			}
			for _, item := range items {
				merged[item.ID] = pickBetterQwenSession(merged[item.ID], item)
			}
		}
	}

	if info, err := os.Stat(projectsDir); err == nil && info.IsDir() {
		chats, err := findQwenChats(projectsDir)
		if err != nil {
			return nil, err
		}
		for _, path := range chats {
			items, err := parseQwenChat(path)
			if err != nil {
				continue
			}
			for _, item := range items {
				merged[item.ID] = pickBetterQwenSession(merged[item.ID], item)
			}
		}
	}

	if len(merged) == 0 {
		return []SyncSession{}, nil
	}
	sessions := make([]SyncSession, 0, len(merged))
	for _, session := range merged {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt < sessions[j].StartedAt
	})
	return sessions, nil
}

func findQwenLogs(tmpDir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(tmpDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "logs.json" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func parseQwenLog(path string) ([]SyncSession, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var items []qwenLogItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}

	type qwenBucket struct {
		session   SyncSession
		messages  []qwenMessage
		startedAt time.Time
		endedAt   time.Time
	}

	buckets := map[string]*qwenBucket{}
	for _, item := range items {
		if item.SessionID == "" {
			continue
		}
		bucket, ok := buckets[item.SessionID]
		if !ok {
			bucket = &qwenBucket{
				session: SyncSession{
					ID: item.SessionID,
				},
			}
			buckets[item.SessionID] = bucket
		}

		role := "user"
		if item.Type != "" {
			role = item.Type
		}
		content := strings.TrimSpace(item.Message)
		if content != "" {
			bucket.messages = append(bucket.messages, qwenMessage{
				MessageID: item.MessageID,
				Role:      role,
				Content:   content,
				Timestamp: item.Timestamp,
			})
			if bucket.session.Title == "" && role == "user" {
				bucket.session.Title = content
			}
		}

		if item.Timestamp != "" {
			if ts, err := time.Parse(time.RFC3339, item.Timestamp); err == nil {
				if bucket.startedAt.IsZero() || ts.Before(bucket.startedAt) {
					bucket.startedAt = ts
				}
				if bucket.endedAt.IsZero() || ts.After(bucket.endedAt) {
					bucket.endedAt = ts
				}
			}
		}
	}

	var sessions []SyncSession
	for _, bucket := range buckets {
		sort.Slice(bucket.messages, func(i, j int) bool {
			return bucket.messages[i].MessageID < bucket.messages[j].MessageID
		})
		for idx, msg := range bucket.messages {
			bucket.session.Messages = append(bucket.session.Messages, SyncMessage{
				Index:     idx,
				Role:      msg.Role,
				Content:   msg.Content,
				Timestamp: msg.Timestamp,
			})
		}
		if bucket.session.StartedAt == "" && !bucket.startedAt.IsZero() {
			bucket.session.StartedAt = bucket.startedAt.Format(time.RFC3339)
		}
		if bucket.session.EndedAt == "" && !bucket.endedAt.IsZero() {
			bucket.session.EndedAt = bucket.endedAt.Format(time.RFC3339)
		}
		if bucket.session.Title == "" {
			bucket.session.Title = "Qwen session"
		}
		sessions = append(sessions, bucket.session)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt < sessions[j].StartedAt
	})
	return sessions, nil
}

func findQwenChats(projectsDir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".jsonl") && strings.Contains(path, string(filepath.Separator)+"chats"+string(filepath.Separator)) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func parseQwenChat(path string) ([]SyncSession, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	type qwenChatBucket struct {
		session SyncSession
	}

	buckets := map[string]*qwenChatBucket{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		item := map[string]any{}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			continue
		}

		sessionID := strings.TrimSpace(stringFrom(item["sessionId"]))
		if sessionID == "" {
			continue
		}
		bucket, ok := buckets[sessionID]
		if !ok {
			bucket = &qwenChatBucket{
				session: SyncSession{ID: sessionID},
			}
			buckets[sessionID] = bucket
		}

		if bucket.session.Cwd == "" {
			bucket.session.Cwd = normalizeCwd(stringFrom(item["cwd"]))
		}
		if bucket.session.Model == "" {
			bucket.session.Model = strings.TrimSpace(stringFrom(item["model"]))
		}

		ts := strings.TrimSpace(stringFrom(item["timestamp"]))
		if ts != "" {
			if bucket.session.StartedAt == "" || ts < bucket.session.StartedAt {
				bucket.session.StartedAt = ts
			}
			if bucket.session.EndedAt == "" || ts > bucket.session.EndedAt {
				bucket.session.EndedAt = ts
			}
		}

		eventType := strings.TrimSpace(stringFrom(item["type"]))
		usage := mapFrom(item["usageMetadata"])
		inputTokens := intFrom(usage["promptTokenCount"], 0)
		outputTokens := intFrom(usage["candidatesTokenCount"], 0)
		cachedInputTokens := intFrom(usage["cachedContentTokenCount"], 0)
		reasoningTokens := intFrom(usage["thoughtsTokenCount"], 0)
		totalTokens := intFrom(usage["totalTokenCount"], 0)
		if totalTokens == 0 {
			totalTokens = inputTokens + outputTokens + cachedInputTokens + reasoningTokens
		}

		role := ""
		content := ""
		switch eventType {
		case "user", "assistant":
			message := mapFrom(item["message"])
			content = parseQwenMessageParts(message["parts"])
			if content == "" {
				content = strings.TrimSpace(stringFrom(item["message"]))
			}
			role = eventType
		case "system", "about", "model_stats":
			content = parseQwenSystemEvent(item)
			role = "system"
		}
		if content != "" {
			content = appendQwenMeta(content, item)
			bucket.session.Messages = append(bucket.session.Messages, SyncMessage{
				Index:             len(bucket.session.Messages),
				Role:              role,
				Content:           content,
				Timestamp:         ts,
				InputTokens:       inputTokens,
				OutputTokens:      outputTokens,
				CachedInputTokens: cachedInputTokens,
				ReasoningTokens:   reasoningTokens,
				TotalTokens:       totalTokens,
			})
			if bucket.session.Title == "" && role == "user" {
				bucket.session.Title = content
			}
		}

		bucket.session.TotalInputTokens += inputTokens
		bucket.session.TotalOutputTokens += outputTokens
		bucket.session.TotalCachedInputTokens += cachedInputTokens
		bucket.session.TotalReasoningTokens += reasoningTokens
		bucket.session.TotalTokens += totalTokens
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	sessions := make([]SyncSession, 0, len(buckets))
	for _, bucket := range buckets {
		session := bucket.session
		if session.TotalTokens == 0 {
			session.TotalTokens = session.TotalInputTokens + session.TotalOutputTokens + session.TotalCachedInputTokens + session.TotalReasoningTokens
		}
		if session.Title == "" {
			if session.Cwd != "" {
				session.Title = session.Cwd
			} else {
				session.Title = "Qwen session"
			}
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt < sessions[j].StartedAt
	})
	return sessions, nil
}

func parseQwenMessageParts(value any) string {
	items, _ := value.([]any)
	if len(items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		entry := mapFrom(item)
		if text := strings.TrimSpace(stringFrom(entry["text"])); text != "" {
			parts = append(parts, text)
			continue
		}
		if raw := strings.TrimSpace(stringifyJSON(item)); raw != "" {
			parts = append(parts, raw)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func parseQwenSystemEvent(item map[string]any) string {
	eventType := strings.TrimSpace(stringFrom(item["type"]))
	subtype := strings.TrimSpace(stringFrom(item["subtype"]))
	systemPayload := mapFrom(item["systemPayload"])

	if subtype == "slash_command" {
		phase := strings.TrimSpace(stringFrom(systemPayload["phase"]))
		rawCommand := strings.TrimSpace(stringFrom(systemPayload["rawCommand"]))
		if phase == "" && rawCommand == "" {
			return ""
		}
		return strings.TrimSpace(fmt.Sprintf("[slash_command] phase=%s command=%s", phase, rawCommand))
	}

	if subtype == "ui_telemetry" {
		uiEvent := mapFrom(systemPayload["uiEvent"])
		eventName := strings.TrimSpace(stringFrom(uiEvent["event.name"]))
		model := strings.TrimSpace(stringFrom(uiEvent["model"]))
		status := intFrom(uiEvent["status_code"], 0)
		durationMs := intFrom(uiEvent["duration_ms"], 0)
		responseID := strings.TrimSpace(stringFrom(uiEvent["response_id"]))
		parts := make([]string, 0, 5)
		if eventName != "" {
			parts = append(parts, "event="+eventName)
		}
		if model != "" {
			parts = append(parts, "model="+model)
		}
		if status > 0 {
			parts = append(parts, "status="+strconv.Itoa(status))
		}
		if durationMs > 0 {
			parts = append(parts, "duration_ms="+strconv.Itoa(durationMs))
		}
		if responseID != "" {
			parts = append(parts, "response_id="+responseID)
		}
		if len(parts) == 0 {
			return ""
		}
		return "[ui_telemetry] " + strings.Join(parts, " ")
	}

	outputItems, _ := systemPayload["outputHistoryItems"].([]any)
	if len(outputItems) > 0 {
		itemTypes := make([]string, 0, len(outputItems))
		for _, raw := range outputItems {
			entry := mapFrom(raw)
			itemType := strings.TrimSpace(stringFrom(entry["type"]))
			if itemType != "" {
				itemTypes = append(itemTypes, itemType)
			}
		}
		if len(itemTypes) > 0 {
			return fmt.Sprintf("[%s] output_items=%s", eventType, strings.Join(itemTypes, ","))
		}
	}

	if subtype != "" {
		return fmt.Sprintf("[%s] subtype=%s", eventType, subtype)
	}
	if eventType != "" {
		return fmt.Sprintf("[%s]", eventType)
	}
	return ""
}

func appendQwenMeta(content string, item map[string]any) string {
	meta := map[string]string{}

	add := func(key string, value any) {
		text := strings.TrimSpace(stringFrom(value))
		if text == "" {
			return
		}
		meta[key] = text
	}
	addNum := func(key string, value any) {
		num := int64From(value, 0)
		if num <= 0 {
			return
		}
		meta[key] = strconv.FormatInt(num, 10)
	}

	add("event_type", item["type"])
	add("subtype", item["subtype"])
	add("uuid", item["uuid"])
	add("parent_uuid", item["parentUuid"])
	add("session_id", item["sessionId"])
	add("timestamp", item["timestamp"])
	add("cwd", item["cwd"])
	add("version", item["version"])
	add("git_branch", item["gitBranch"])
	add("model", item["model"])

	usage := mapFrom(item["usageMetadata"])
	addNum("usage.prompt_token_count", usage["promptTokenCount"])
	addNum("usage.candidates_token_count", usage["candidatesTokenCount"])
	addNum("usage.cached_content_token_count", usage["cachedContentTokenCount"])
	addNum("usage.thoughts_token_count", usage["thoughtsTokenCount"])
	addNum("usage.total_token_count", usage["totalTokenCount"])

	systemPayload := mapFrom(item["systemPayload"])
	if strings.EqualFold(strings.TrimSpace(stringFrom(item["subtype"])), "slash_command") {
		add("slash.phase", systemPayload["phase"])
		add("slash.raw_command", systemPayload["rawCommand"])
		outputItems, _ := systemPayload["outputHistoryItems"].([]any)
		if len(outputItems) > 0 {
			types := make([]string, 0, len(outputItems))
			for _, raw := range outputItems {
				entry := mapFrom(raw)
				itemType := strings.TrimSpace(stringFrom(entry["type"]))
				if itemType != "" {
					types = append(types, itemType)
				}
			}
			if len(types) > 0 {
				meta["slash.output_item_types"] = strings.Join(types, ",")
			}
		}
	}

	uiEvent := mapFrom(systemPayload["uiEvent"])
	add("telemetry.event_name", uiEvent["event.name"])
	add("telemetry.event_timestamp", uiEvent["event.timestamp"])
	add("telemetry.response_id", uiEvent["response_id"])
	add("telemetry.model", uiEvent["model"])
	addNum("telemetry.status_code", uiEvent["status_code"])
	addNum("telemetry.duration_ms", uiEvent["duration_ms"])
	addNum("telemetry.input_token_count", uiEvent["input_token_count"])
	addNum("telemetry.output_token_count", uiEvent["output_token_count"])
	addNum("telemetry.cached_content_token_count", uiEvent["cached_content_token_count"])
	addNum("telemetry.thoughts_token_count", uiEvent["thoughts_token_count"])
	addNum("telemetry.tool_token_count", uiEvent["tool_token_count"])
	addNum("telemetry.total_token_count", uiEvent["total_token_count"])
	add("telemetry.prompt_id", uiEvent["prompt_id"])
	add("telemetry.auth_type", uiEvent["auth_type"])

	if len(meta) == 0 {
		return content
	}

	keys := make([]string, 0, len(meta))
	for key := range meta {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys)+1)
	lines = append(lines, "[meta]")
	for _, key := range keys {
		lines = append(lines, key+": "+meta[key])
	}
	metaText := strings.Join(lines, "\n")
	if strings.TrimSpace(content) == "" {
		return metaText
	}
	return strings.TrimSpace(content) + "\n\n" + metaText
}

func pickBetterQwenSession(current SyncSession, incoming SyncSession) SyncSession {
	if incoming.ID == "" {
		return current
	}
	if current.ID == "" {
		return incoming
	}

	currentHasAssistant := qwenHasAssistant(current)
	incomingHasAssistant := qwenHasAssistant(incoming)
	if incomingHasAssistant && !currentHasAssistant {
		return qwenMergeSessionMeta(incoming, current)
	}
	if currentHasAssistant && !incomingHasAssistant {
		return qwenMergeSessionMeta(current, incoming)
	}

	currentHasTokens := qwenHasTokens(current)
	incomingHasTokens := qwenHasTokens(incoming)
	if incomingHasTokens && !currentHasTokens {
		return qwenMergeSessionMeta(incoming, current)
	}
	if currentHasTokens && !incomingHasTokens {
		return qwenMergeSessionMeta(current, incoming)
	}

	merged := current
	if qwenSessionScore(incoming) > qwenSessionScore(current) {
		return qwenMergeSessionMeta(incoming, current)
	}

	return qwenMergeSessionMeta(merged, incoming)
}

func qwenSessionScore(session SyncSession) int {
	score := len(session.Messages)
	if qwenHasAssistant(session) {
		score += 1000
	}
	if qwenHasTokens(session) {
		score += 500
	}
	return score
}

func qwenHasAssistant(session SyncSession) bool {
	for _, msg := range session.Messages {
		if msg.Role == "assistant" {
			return true
		}
	}
	return false
}

func qwenHasTokens(session SyncSession) bool {
	return session.TotalTokens > 0 ||
		session.TotalInputTokens > 0 ||
		session.TotalOutputTokens > 0 ||
		session.TotalCachedInputTokens > 0 ||
		session.TotalReasoningTokens > 0
}

func qwenMergeSessionMeta(primary SyncSession, secondary SyncSession) SyncSession {
	merged := primary
	if merged.Title == "" {
		merged.Title = secondary.Title
	}
	if merged.Cwd == "" {
		merged.Cwd = secondary.Cwd
	}
	if merged.Model == "" {
		merged.Model = secondary.Model
	}
	if merged.StartedAt == "" || (secondary.StartedAt != "" && secondary.StartedAt < merged.StartedAt) {
		merged.StartedAt = secondary.StartedAt
	}
	if merged.EndedAt == "" || (secondary.EndedAt != "" && secondary.EndedAt > merged.EndedAt) {
		merged.EndedAt = secondary.EndedAt
	}
	return merged
}

func loadClineSessions(root string) ([]SyncSession, error) {
	path := filepath.Join(root, "data", "state", "taskHistory.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return []SyncSession{}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}

	var sessions []SyncSession
	for _, item := range items {
		id := stringFrom(item["ulid"])
		if id == "" {
			id = stringFrom(item["id"])
		}
		if id == "" {
			continue
		}
		ts := int64From(item["ts"], 0)
		startedAt := formatMillis(ts)
		session := SyncSession{
			ID:                     id,
			Title:                  stringFrom(item["task"]),
			Cwd:                    stringFrom(item["cwdOnTaskInitialization"]),
			StartedAt:              startedAt,
			EndedAt:                startedAt,
			TotalInputTokens:       intFrom(item["tokensIn"], 0),
			TotalOutputTokens:      intFrom(item["tokensOut"], 0),
			TotalCachedInputTokens: intFrom(item["cacheReads"], 0),
		}
		session.TotalTokens = session.TotalInputTokens + session.TotalOutputTokens
		if session.Title == "" {
			session.Title = "Cline task"
		}
		if session.Title != "" {
			session.Messages = append(session.Messages, SyncMessage{
				Index:     0,
				Role:      "user",
				Content:   session.Title,
				Timestamp: startedAt,
			})
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

func loadContinueSessions(root string) ([]SyncSession, error) {
	sessionsDir := filepath.Join(root, "sessions")
	indexPath := filepath.Join(sessionsDir, "sessions.json")
	index := map[string]map[string]any{}

	if raw, err := os.ReadFile(indexPath); err == nil {
		var items []map[string]any
		if err := json.Unmarshal(raw, &items); err == nil {
			for _, item := range items {
				id := stringFrom(item["sessionId"])
				if id != "" {
					index[id] = item
				}
			}
		}
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return []SyncSession{}, nil
	}

	var sessions []SyncSession
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || entry.Name() == "sessions.json" {
			continue
		}
		path := filepath.Join(sessionsDir, entry.Name())
		session, ok := parseContinueSession(path, index)
		if ok {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

type continueHistory struct {
	Message    map[string]any   `json:"message"`
	PromptLogs []map[string]any `json:"promptLogs"`
}

type continueSession struct {
	SessionID          string            `json:"sessionId"`
	Title              string            `json:"title"`
	WorkspaceDirectory string            `json:"workspaceDirectory"`
	History            []continueHistory `json:"history"`
}

func parseContinueSession(path string, index map[string]map[string]any) (SyncSession, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SyncSession{}, false
	}
	var sess continueSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return SyncSession{}, false
	}
	if sess.SessionID == "" {
		return SyncSession{}, false
	}

	var startedAt string
	if meta, ok := index[sess.SessionID]; ok {
		ts := int64From(meta["dateCreated"], 0)
		startedAt = formatMillis(ts)
		if sess.Title == "" {
			sess.Title = stringFrom(meta["title"])
		}
		if sess.WorkspaceDirectory == "" {
			sess.WorkspaceDirectory = stringFrom(meta["workspaceDirectory"])
		}
	}

	endedAt := ""
	if stat, err := os.Stat(path); err == nil {
		endedAt = stat.ModTime().Format(time.RFC3339)
	}

	session := SyncSession{
		ID:        sess.SessionID,
		Title:     sess.Title,
		Cwd:       strings.TrimPrefix(sess.WorkspaceDirectory, "file://"),
		StartedAt: startedAt,
		EndedAt:   endedAt,
	}

	msgIndex := 0
	for _, entry := range sess.History {
		role := stringFrom(entry.Message["role"])
		content := parseContinueContent(entry.Message["content"])
		if content == "" {
			continue
		}
		if session.Title == "" && role == "user" {
			session.Title = content
		}
		session.Messages = append(session.Messages, SyncMessage{
			Index:   msgIndex,
			Role:    role,
			Content: content,
		})
		msgIndex++

		if session.Model == "" {
			for _, log := range entry.PromptLogs {
				model := stringFrom(log["modelTitle"])
				if model != "" {
					session.Model = model
					break
				}
			}
		}
	}

	if session.Title == "" {
		session.Title = "Continue session"
	}
	if session.StartedAt == "" {
		session.StartedAt = session.EndedAt
	}

	return session, true
}

func parseContinueContent(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			entry := mapFrom(item)
			if text := stringFrom(entry["text"]); text != "" {
				parts = append(parts, text)
				continue
			}
			if entryType := stringFrom(entry["type"]); entryType == "text" {
				if text := stringFrom(entry["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

type kiloUiMessage struct {
	Ts   int64  `json:"ts"`
	Type string `json:"type"`
	Say  string `json:"say"`
	Ask  string `json:"ask"`
	Text string `json:"text"`
}

type kiloUsage struct {
	TokensIn    int     `json:"tokensIn"`
	TokensOut   int     `json:"tokensOut"`
	CacheReads  int     `json:"cacheReads"`
	CacheWrites int     `json:"cacheWrites"`
	Cost        float64 `json:"cost"`
}

type kiloSessionIndex struct {
	TaskSessionMap map[string]string `json:"taskSessionMap"`
}

type kiloStoragePaths struct {
	tasksDir    string
	sessionsDir string
}

func loadKiloCodeSessions(root string) ([]SyncSession, error) {
	candidates := kiloStorageCandidates(root)
	parsed := make(map[string]SyncSession)
	order := make([]string, 0)

	for _, candidate := range candidates {
		info, err := os.Stat(candidate.tasksDir)
		if err != nil || !info.IsDir() {
			continue
		}

		taskSessionMap, err := loadKiloTaskSessionMap(candidate.sessionsDir)
		if err != nil {
			return nil, err
		}

		entries, err := os.ReadDir(candidate.tasksDir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			taskID := entry.Name()
			taskDir := filepath.Join(candidate.tasksDir, taskID)
			session, ok := parseKiloTask(taskDir, taskID, taskSessionMap[taskID])
			if !ok {
				continue
			}
			if existing, ok := parsed[session.ID]; ok {
				if kiloSessionScore(session) > kiloSessionScore(existing) {
					parsed[session.ID] = session
				}
				continue
			}
			parsed[session.ID] = session
			order = append(order, session.ID)
		}
	}

	sessions := make([]SyncSession, 0, len(order))
	for _, id := range order {
		sessions = append(sessions, parsed[id])
	}
	return sessions, nil
}

func parseKiloTask(taskDir string, fallbackID string, mappedSessionID string) (SyncSession, bool) {
	uiPath := filepath.Join(taskDir, "ui_messages.json")
	apiPath := filepath.Join(taskDir, "api_conversation_history.json")

	var uiMessages []kiloUiMessage
	if raw, err := os.ReadFile(uiPath); err == nil {
		_ = json.Unmarshal(raw, &uiMessages)
	}

	var tokens tokenTotals
	var startedMs int64
	var endedMs int64
	for _, msg := range uiMessages {
		if startedMs == 0 || msg.Ts < startedMs {
			startedMs = msg.Ts
		}
		if endedMs == 0 || msg.Ts > endedMs {
			endedMs = msg.Ts
		}
		if msg.Say == "api_req_started" && msg.Text != "" {
			var usage kiloUsage
			if err := json.Unmarshal([]byte(msg.Text), &usage); err == nil {
				tokens.inputTokens += usage.TokensIn
				tokens.outputTokens += usage.TokensOut
				tokens.cachedInputTokens += usage.CacheReads
			}
		}
	}
	tokens.totalTokens = tokens.inputTokens + tokens.outputTokens

	var messages []SyncMessage
	var title string
	var model string
	var cwd string

	if raw, err := os.ReadFile(apiPath); err == nil {
		var entries []map[string]any
		if err := json.Unmarshal(raw, &entries); err == nil {
			msgIndex := 0
			for _, entry := range entries {
				role := stringFrom(entry["role"])
				contentList, _ := entry["content"].([]any)
				content := parseKiloContentList(contentList)
				ts := int64From(entry["ts"], 0)
				if startedMs == 0 || ts < startedMs {
					startedMs = ts
				}
				if endedMs == 0 || ts > endedMs {
					endedMs = ts
				}
				if model == "" {
					model = extractTaggedValue(content, "model")
				}
				if cwd == "" {
					cwd = extractWorkspaceDirectory(content)
				}
				if content == "" {
					continue
				}
				if title == "" && role == "user" {
					title = content
				}
				messages = append(messages, SyncMessage{
					Index:     msgIndex,
					Role:      role,
					Content:   content,
					Timestamp: formatMillis(ts),
				})
				msgIndex++
			}
		}
	}

	if len(messages) == 0 && len(uiMessages) > 0 {
		msgIndex := 0
		for _, entry := range uiMessages {
			if entry.Say != "text" || entry.Text == "" {
				continue
			}
			if title == "" {
				title = entry.Text
			}
			messages = append(messages, SyncMessage{
				Index:     msgIndex,
				Role:      "user",
				Content:   entry.Text,
				Timestamp: formatMillis(entry.Ts),
			})
			msgIndex++
		}
	}

	id := strings.TrimSpace(mappedSessionID)
	if id == "" {
		id = fallbackID
	}
	if id == "" {
		id = strconv.FormatInt(startedMs, 10)
	}

	session := SyncSession{
		ID:                     id,
		Title:                  title,
		Model:                  model,
		Cwd:                    cwd,
		StartedAt:              formatMillis(startedMs),
		EndedAt:                formatMillis(endedMs),
		TotalTokens:            tokens.totalTokens,
		TotalInputTokens:       tokens.inputTokens,
		TotalOutputTokens:      tokens.outputTokens,
		TotalCachedInputTokens: tokens.cachedInputTokens,
		Messages:               messages,
	}
	if session.Title == "" {
		session.Title = "KiloCode session"
	}
	if session.StartedAt == "" {
		session.StartedAt = session.EndedAt
	}
	return session, session.ID != ""
}

func kiloStorageCandidates(root string) []kiloStoragePaths {
	seen := make(map[string]bool)
	var candidates []kiloStoragePaths

	add := func(base string) {
		base = strings.TrimSpace(base)
		if base == "" {
			return
		}
		base = expandUser(base)
		paths := []kiloStoragePaths{
			{
				tasksDir:    filepath.Join(base, "tasks"),
				sessionsDir: filepath.Join(base, "sessions"),
			},
			{
				tasksDir:    filepath.Join(base, "cli", "global", "tasks"),
				sessionsDir: filepath.Join(base, "cli", "global", "sessions"),
			},
		}
		for _, path := range paths {
			key := path.tasksDir + "::" + path.sessionsDir
			if !seen[key] {
				seen[key] = true
				candidates = append(candidates, path)
			}
		}
	}

	add(root)

	switch runtime.GOOS {
	case "darwin":
		add("~/Library/Application Support/Code/User/globalStorage/kilocode.kilo-code")
	case "linux":
		add("~/.vscode-server/data/User/globalStorage/kilocode.kilo-code")
		add("~/.config/Code/User/globalStorage/kilocode.kilo-code")
	}

	return candidates
}

func loadKiloTaskSessionMap(sessionsDir string) (map[string]string, error) {
	mapping := make(map[string]string)
	info, err := os.Stat(sessionsDir)
	if err != nil || !info.IsDir() {
		return mapping, nil
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(sessionsDir, entry.Name(), "session.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var idx kiloSessionIndex
		if err := json.Unmarshal(raw, &idx); err != nil {
			continue
		}
		for taskID, sessionID := range idx.TaskSessionMap {
			taskID = strings.TrimSpace(taskID)
			sessionID = strings.TrimSpace(sessionID)
			if taskID == "" || sessionID == "" {
				continue
			}
			mapping[taskID] = sessionID
		}
	}
	return mapping, nil
}

func kiloSessionScore(session SyncSession) int {
	score := len(session.Messages) * 10
	score += session.TotalTokens
	if session.Cwd != "" {
		score += 20
	}
	if session.Model != "" {
		score += 10
	}
	return score
}

func parseKiloContentList(items []any) string {
	if len(items) == 0 {
		return ""
	}
	var parts []string
	for _, item := range items {
		entry := mapFrom(item)
		entryType := stringFrom(entry["type"])
		switch entryType {
		case "text":
			if text := stringFrom(entry["text"]); text != "" {
				parts = append(parts, text)
			}
		case "tool_result":
			if content := entry["content"]; content != nil {
				if contentList, ok := content.([]any); ok {
					for _, sub := range contentList {
						subEntry := mapFrom(sub)
						if text := stringFrom(subEntry["text"]); text != "" {
							parts = append(parts, text)
						}
					}
				}
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func extractTaggedValue(content string, tag string) string {
	if content == "" {
		return ""
	}
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(content, open)
	if start == -1 {
		return ""
	}
	start += len(open)
	end := strings.Index(content[start:], close)
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(content[start : start+end])
}

func extractWorkspaceDirectory(content string) string {
	needle := "Current Workspace Directory ("
	start := strings.Index(content, needle)
	if start == -1 {
		return ""
	}
	start += len(needle)
	end := strings.Index(content[start:], ")")
	if end == -1 {
		return ""
	}
	return content[start : start+end]
}

func normalizeCwd(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "file://")
	return strings.TrimRight(trimmed, `/\`)
}

func firstStringFromMap(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringFrom(payload[key]); value != "" {
			return value
		}
	}
	return ""
}

func loadCursorSessions(root string, logf func(format string, args ...any)) ([]SyncSession, error) {
	logCursorf(logf, "cursor: root=%s", root)
	trackingSessions, err := loadCursorTrackingSessions(root, logf)
	if err != nil {
		logCursorf(logf, "cursor: tracking error: %v", err)
		trackingSessions = nil
	}
	chatSessions, err := loadCursorChatSessions(root, logf)
	if err != nil {
		logCursorf(logf, "cursor: chat error: %v", err)
		chatSessions = nil
	}
	logCursorf(logf, "cursor: tracking sessions=%d chat sessions=%d", len(trackingSessions), len(chatSessions))
	return append(trackingSessions, chatSessions...), nil
}

func logCursorf(logf func(format string, args ...any), format string, args ...any) {
	if logf == nil {
		return
	}
	logf(format, args...)
}

func logCursorSchema(logf func(format string, args ...any), dbPath string, prefix string) {
	if logf == nil {
		return
	}
	tables, err := sqliteRows(dbPath, "SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		logCursorf(logf, "%s schema error: %v", prefix, err)
		return
	}
	tableNames := make([]string, 0, len(tables))
	for _, row := range tables {
		if len(row) == 0 {
			continue
		}
		tableNames = append(tableNames, row[0])
	}
	logCursorf(logf, "%s tables=%v", prefix, tableNames)

	tokenColumns := make([]string, 0)
	for _, table := range tableNames {
		cols, err := sqliteRows(dbPath, fmt.Sprintf("PRAGMA table_info(%s)", table))
		if err != nil {
			continue
		}
		for _, col := range cols {
			if len(col) < 2 {
				continue
			}
			colName := strings.ToLower(col[1])
			if strings.Contains(colName, "token") {
				tokenColumns = append(tokenColumns, fmt.Sprintf("%s.%s", table, col[1]))
			}
		}
	}
	if len(tokenColumns) == 0 {
		logCursorf(logf, "%s token columns not found", prefix)
	} else {
		logCursorf(logf, "%s token columns=%v", prefix, tokenColumns)
	}
}

func loadCursorTrackingSessions(root string, logf func(format string, args ...any)) ([]SyncSession, error) {
	dbPath := filepath.Join(root, "ai-tracking", "ai-code-tracking.db")
	if _, err := os.Stat(dbPath); err != nil {
		logCursorf(logf, "cursor: tracking db missing at %s", dbPath)
		return []SyncSession{}, nil
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		logCursorf(logf, "cursor: sqlite3 not found; skip tracking")
		return []SyncSession{}, nil
	}
	logCursorSchema(logf, dbPath, "cursor: tracking")

	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, "SELECT conversationId, title, model, updatedAt FROM conversation_summaries")
	output, err := cmd.Output()
	if err != nil {
		logCursorf(logf, "cursor: tracking sqlite error: %v", err)
		return []SyncSession{}, nil
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	var sessions []SyncSession
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		updatedMs, _ := strconv.ParseInt(parts[3], 10, 64)
		timestamp := formatMillis(updatedMs)
		session := SyncSession{
			ID:        parts[0],
			Title:     parts[1],
			Model:     parts[2],
			StartedAt: timestamp,
			EndedAt:   timestamp,
		}
		if session.Title == "" {
			session.Title = session.ID
		}
		sessions = append(sessions, session)
	}
	logCursorf(logf, "cursor: tracking parsed sessions=%d", len(sessions))
	return sessions, nil
}

type cursorChatMeta struct {
	AgentID        string `json:"agentId"`
	LatestRootBlob string `json:"latestRootBlobId"`
	Name           string `json:"name"`
	Mode           string `json:"mode"`
	CreatedAt      int64  `json:"createdAt"`
	LastUsedModel  string `json:"lastUsedModel"`
}

type cursorChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func loadCursorChatSessions(root string, logf func(format string, args ...any)) ([]SyncSession, error) {
	chatsRoot := filepath.Join(root, "chats")
	info, err := os.Stat(chatsRoot)
	if err != nil || !info.IsDir() {
		logCursorf(logf, "cursor: chats dir missing at %s", chatsRoot)
		return []SyncSession{}, nil
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		logCursorf(logf, "cursor: sqlite3 not found; skip chats")
		return []SyncSession{}, nil
	}

	storePaths := []string{}
	err = filepath.WalkDir(chatsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() != "store.db" {
			return nil
		}
		storePaths = append(storePaths, path)
		return nil
	})
	if err != nil {
		logCursorf(logf, "cursor: chat walk error: %v", err)
		return []SyncSession{}, nil
	}
	logCursorf(logf, "cursor: chat stores found=%d", len(storePaths))

	var sessions []SyncSession
	for _, dbPath := range storePaths {
		session, ok := parseCursorChatStore(dbPath, logf)
		if ok {
			sessions = append(sessions, session)
		}
	}
	logCursorf(logf, "cursor: chat parsed sessions=%d", len(sessions))
	return sessions, nil
}

func parseCursorChatStore(dbPath string, logf func(format string, args ...any)) (SyncSession, bool) {
	logCursorSchema(logf, dbPath, "cursor: chat")
	metaHex, err := sqliteScalar(dbPath, "SELECT value FROM meta WHERE key='0'")
	if err != nil || metaHex == "" {
		logCursorf(logf, "cursor: chat meta missing at %s", dbPath)
		return SyncSession{}, false
	}
	metaBytes, err := hex.DecodeString(strings.TrimSpace(metaHex))
	if err != nil {
		logCursorf(logf, "cursor: chat meta decode error at %s: %v", dbPath, err)
		return SyncSession{}, false
	}
	var meta cursorChatMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		logCursorf(logf, "cursor: chat meta unmarshal error at %s: %v", dbPath, err)
		return SyncSession{}, false
	}
	if meta.AgentID == "" || meta.LatestRootBlob == "" {
		logCursorf(logf, "cursor: chat meta incomplete at %s", dbPath)
		return SyncSession{}, false
	}

	rootHex, err := sqliteScalar(dbPath, fmt.Sprintf("SELECT hex(data) FROM blobs WHERE id='%s'", meta.LatestRootBlob))
	if err != nil || rootHex == "" {
		logCursorf(logf, "cursor: chat root blob missing at %s (id=%s)", dbPath, meta.LatestRootBlob)
		return SyncSession{}, false
	}
	rootBytes, err := hex.DecodeString(strings.TrimSpace(rootHex))
	if err != nil {
		logCursorf(logf, "cursor: chat root blob decode error at %s: %v", dbPath, err)
		return SyncSession{}, false
	}
	blobIDs := parseCursorRootBlobIDs(rootBytes)
	if len(blobIDs) == 0 {
		logCursorf(logf, "cursor: chat no message blob ids at %s", dbPath)
		return SyncSession{}, false
	}

	messageByID := make(map[string]cursorChatMessage, len(blobIDs))
	for _, chunk := range chunkStrings(blobIDs, 200) {
		rows, err := sqliteRows(dbPath, fmt.Sprintf("SELECT id, hex(data) FROM blobs WHERE id IN (%s)", quoteStrings(chunk)))
		if err != nil {
			logCursorf(logf, "cursor: chat blob batch error at %s: %v", dbPath, err)
			continue
		}
		for _, row := range rows {
			if len(row) < 2 {
				continue
			}
			dataBytes, err := hex.DecodeString(strings.TrimSpace(row[1]))
			if err != nil {
				continue
			}
			var msg cursorChatMessage
			if err := json.Unmarshal(dataBytes, &msg); err != nil {
				continue
			}
			if msg.Role == "" {
				continue
			}
			messageByID[row[0]] = msg
		}
	}
	logCursorf(logf, "cursor: chat message blobs decoded=%d", len(messageByID))

	startedAt := formatMillis(meta.CreatedAt)
	endedAt := ""
	if stat, err := os.Stat(dbPath); err == nil {
		endedAt = stat.ModTime().UTC().Format(time.RFC3339)
	}
	if startedAt == "" {
		startedAt = endedAt
	}
	if endedAt == "" {
		endedAt = startedAt
	}

	session := SyncSession{
		ID:        "cursor-chat:" + meta.AgentID,
		Title:     strings.TrimSpace(meta.Name),
		Model:     strings.TrimSpace(meta.LastUsedModel),
		StartedAt: startedAt,
		EndedAt:   endedAt,
	}
	if session.Title == "" {
		session.Title = session.ID
	}

	index := 0
	for _, id := range blobIDs {
		msg, ok := messageByID[id]
		if !ok {
			continue
		}
		content := cursorContentToString(msg.Content)
		if session.Cwd == "" {
			if cwd := extractCursorWorkspacePath(content); cwd != "" {
				session.Cwd = normalizeCwd(cwd)
			}
			if session.Cwd == "" {
				if cwd := extractWorkspaceDirectory(content); cwd != "" {
					session.Cwd = normalizeCwd(cwd)
				}
			}
		}
		if content == "" {
			continue
		}
		session.Messages = append(session.Messages, SyncMessage{
			Index:   index,
			Role:    msg.Role,
			Content: content,
		})
		index++
	}

	if session.Title == session.ID && session.Cwd != "" {
		session.Title = session.Cwd
	}
	logCursorf(logf, "cursor: chat session id=%s title=%s messages=%d", session.ID, session.Title, len(session.Messages))
	return session, session.ID != ""
}

func sqliteScalar(dbPath string, query string) (string, error) {
	cmd := exec.Command("sqlite3", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func sqliteRows(dbPath string, query string) ([][]string, error) {
	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, query)
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	// Cursor blobs can be large; raise scan limit to avoid token too long.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	rows := make([][]string, 0)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		rows = append(rows, strings.Split(line, "\t"))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func parseCursorRootBlobIDs(data []byte) []string {
	ids := make([]string, 0, len(data)/34)
	for i := 0; i+2 <= len(data); {
		if data[i] == 0x0a && data[i+1] == 0x20 {
			start := i + 2
			end := start + 32
			if end > len(data) {
				break
			}
			ids = append(ids, hex.EncodeToString(data[start:end]))
			i = end
			continue
		}
		i++
	}
	return ids
}

func cursorContentToString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			switch part := item.(type) {
			case string:
				b.WriteString(part)
			case map[string]any:
				if text, ok := part["text"].(string); ok {
					b.WriteString(text)
					continue
				}
				if text, ok := part["content"].(string); ok {
					b.WriteString(text)
					continue
				}
				if raw, err := json.Marshal(part); err == nil {
					b.WriteString(string(raw))
				}
			default:
				if raw, err := json.Marshal(part); err == nil {
					b.WriteString(string(raw))
				}
			}
		}
		return b.String()
	default:
		if raw, err := json.Marshal(v); err == nil {
			return string(raw)
		}
	}
	return ""
}

func extractCursorWorkspacePath(content string) string {
	needle := "Workspace Path:"
	if idx := strings.Index(content, needle); idx != -1 {
		trimmed := strings.TrimSpace(content[idx+len(needle):])
		if trimmed == "" {
			return ""
		}
		if end := strings.Index(trimmed, "\n"); end != -1 {
			return strings.TrimSpace(trimmed[:end])
		}
		return strings.TrimSpace(trimmed)
	}
	return ""
}

func chunkStrings(values []string, size int) [][]string {
	if size <= 0 || len(values) == 0 {
		return nil
	}
	chunks := make([][]string, 0, (len(values)+size-1)/size)
	for i := 0; i < len(values); i += size {
		end := i + size
		if end > len(values) {
			end = len(values)
		}
		chunks = append(chunks, values[i:end])
	}
	return chunks
}

func quoteStrings(values []string) string {
	if len(values) == 0 {
		return "''"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		safe := strings.ReplaceAll(value, "'", "''")
		quoted = append(quoted, "'"+safe+"'")
	}
	return strings.Join(quoted, ",")
}

func loadAmpSessions(root string) ([]SyncSession, error) {
	threadsDir := filepath.Join(root, "threads")
	info, err := os.Stat(threadsDir)
	if (err != nil || !info.IsDir()) && strings.TrimSpace(root) != "" {
		legacyRoot := expandUser("~/.amp")
		if root == legacyRoot {
			fallback := expandUser("~/.local/share/amp")
			fallbackDir := filepath.Join(fallback, "threads")
			if stat, statErr := os.Stat(fallbackDir); statErr == nil && stat.IsDir() {
				threadsDir = fallbackDir
			}
		}
	}

	entries, err := os.ReadDir(threadsDir)
	if err != nil {
		return []SyncSession{}, nil
	}

	sessions := make([]SyncSession, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(threadsDir, entry.Name())
		session, ok := parseAmpThread(path)
		if ok {
			sessions = append(sessions, session)
		}
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt < sessions[j].StartedAt
	})
	return sessions, nil
}

func parseAmpThread(path string) (SyncSession, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SyncSession{}, false
	}

	var thread map[string]any
	if err := json.Unmarshal(raw, &thread); err != nil {
		return SyncSession{}, false
	}

	sessionID := strings.TrimSpace(stringFrom(thread["id"]))
	if sessionID == "" {
		sessionID = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	if sessionID == "" {
		return SyncSession{}, false
	}

	startedMs := int64From(thread["created"], 0)
	endedMs := startedMs
	cwd := extractAmpCwd(thread)

	session := SyncSession{
		ID:    sessionID,
		Title: strings.TrimSpace(stringFrom(thread["title"])),
		Cwd:   cwd,
	}

	entries, _ := thread["messages"].([]any)
	msgIndex := 0
	for _, item := range entries {
		msg := mapFrom(item)
		role := strings.TrimSpace(stringFrom(msg["role"]))
		if role == "" {
			continue
		}

		tsMs := int64From(mapFrom(msg["meta"])["sentAt"], 0)
		if tsMs > 0 {
			if startedMs == 0 || tsMs < startedMs {
				startedMs = tsMs
			}
			if tsMs > endedMs {
				endedMs = tsMs
			}
		}

		content := parseAmpContent(msg["content"])
		if content != "" {
			session.Messages = append(session.Messages, SyncMessage{
				Index:     msgIndex,
				Role:      role,
				Content:   content,
				Timestamp: formatMillis(tsMs),
			})
			if session.Title == "" && role == "user" {
				session.Title = content
			}
			msgIndex++
		}

		if role == "assistant" {
			usage := mapFrom(msg["usage"])
			model := strings.TrimSpace(stringFrom(usage["model"]))
			if model != "" {
				session.Model = model
			}
			inputTokens := intFrom(usage["inputTokens"], 0)
			outputTokens := intFrom(usage["outputTokens"], 0)
			cacheCreate := intFrom(usage["cacheCreationInputTokens"], 0)
			cacheRead := intFrom(usage["cacheReadInputTokens"], 0)
			session.TotalInputTokens += inputTokens
			session.TotalOutputTokens += outputTokens
			session.TotalCachedInputTokens += cacheCreate + cacheRead
		}
	}

	if session.TotalInputTokens == 0 && session.TotalOutputTokens == 0 {
		accumulateAmpUsageLedger(&session, thread)
	}

	if endedMs == 0 {
		if stat, statErr := os.Stat(path); statErr == nil {
			endedMs = stat.ModTime().UnixMilli()
		}
	}
	if startedMs == 0 {
		startedMs = endedMs
	}
	session.StartedAt = formatMillis(startedMs)
	session.EndedAt = formatMillis(endedMs)
	session.TotalTokens = session.TotalInputTokens + session.TotalOutputTokens + session.TotalCachedInputTokens

	session.Cwd = normalizeCwd(session.Cwd)
	if session.Title == "" {
		if session.Cwd != "" {
			session.Title = session.Cwd
		} else {
			session.Title = "AMP session"
		}
	}
	if len(session.Messages) == 0 {
		return SyncSession{}, false
	}
	return session, true
}

func extractAmpCwd(thread map[string]any) string {
	env := mapFrom(thread["env"])
	initial := mapFrom(env["initial"])
	trees, _ := initial["trees"].([]any)
	for _, item := range trees {
		tree := mapFrom(item)
		uri := strings.TrimSpace(stringFrom(tree["uri"]))
		if uri == "" {
			continue
		}
		if strings.HasPrefix(uri, "file://") {
			return normalizeCwd(strings.TrimPrefix(uri, "file://"))
		}
		return normalizeCwd(uri)
	}
	return ""
}

func parseAmpContent(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			entry := mapFrom(item)
			entryType := stringFrom(entry["type"])
			switch entryType {
			case "text":
				if text := strings.TrimSpace(stringFrom(entry["text"])); text != "" {
					parts = append(parts, text)
				}
			case "thinking":
				if text := strings.TrimSpace(stringFrom(entry["thinking"])); text != "" {
					parts = append(parts, "[thinking]\n"+text)
				}
			case "tool_use":
				name := strings.TrimSpace(stringFrom(entry["name"]))
				inputText := stringifyJSON(entry["input"])
				if inputText == "" {
					if name != "" {
						parts = append(parts, fmt.Sprintf("[tool_use] %s", name))
					} else {
						parts = append(parts, "[tool_use]")
					}
				} else if name != "" {
					parts = append(parts, fmt.Sprintf("[tool_use] %s\n%s", name, inputText))
				} else {
					parts = append(parts, fmt.Sprintf("[tool_use]\n%s", inputText))
				}
			case "tool_result":
				contentText := stringifyJSON(entry["content"])
				if contentText == "" {
					contentText = stringFrom(entry["content"])
				}
				if contentText == "" {
					parts = append(parts, "[tool_result]")
				} else {
					parts = append(parts, fmt.Sprintf("[tool_result]\n%s", contentText))
				}
			default:
				if text := strings.TrimSpace(stringFrom(entry["text"])); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func accumulateAmpUsageLedger(session *SyncSession, thread map[string]any) {
	ledger := mapFrom(thread["usageLedger"])
	events, _ := ledger["events"].([]any)
	for _, item := range events {
		event := mapFrom(item)
		if strings.TrimSpace(stringFrom(event["operationType"])) != "inference" {
			continue
		}
		model := strings.TrimSpace(stringFrom(event["model"]))
		if model != "" && session.Model == "" {
			session.Model = model
		}
		tokens := mapFrom(event["tokens"])
		session.TotalInputTokens += intFrom(tokens["input"], 0)
		session.TotalOutputTokens += intFrom(tokens["output"], 0)
	}
}

func loadOpenCodeSessions(root string) ([]SyncSession, error) {
	dataRoot := filepath.Join(root, "storage")
	sessionsDir := filepath.Join(dataRoot, "session")
	messagesDir := filepath.Join(dataRoot, "message")
	partsDir := filepath.Join(dataRoot, "part")
	projectsDir := filepath.Join(dataRoot, "project")

	// Load projects map
	projects := make(map[string]opencodeProject)
	if err := loadOpenCodeProjects(projectsDir, projects); err != nil {
		return nil, fmt.Errorf("failed to load projects: %v", err)
	}

	// Load sessions from session directory
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return []SyncSession{}, nil
	}

	var sessions []SyncSession
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		projectDir := filepath.Join(sessionsDir, entry.Name())
		sessionFiles, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}

		for _, sessionFile := range sessionFiles {
			if sessionFile.IsDir() || !strings.HasSuffix(sessionFile.Name(), ".json") {
				continue
			}

			sessionPath := filepath.Join(projectDir, sessionFile.Name())
			session, ok := parseOpenCodeSession(sessionPath, projects, messagesDir, partsDir)
			if ok {
				sessions = append(sessions, session)
			}
		}
	}

	// Sort sessions by start time
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt < sessions[j].StartedAt
	})

	return sessions, nil
}

func loadPiSessions(root string) ([]SyncSession, error) {
	sessionsDir := filepath.Join(root, "agent", "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil || !info.IsDir() {
		fallback := filepath.Join(root, "sessions")
		if stat, statErr := os.Stat(fallback); statErr == nil && stat.IsDir() {
			sessionsDir = fallback
		}
	}

	entries, err := listJSONL(sessionsDir)
	if err != nil {
		return nil, err
	}

	sessions := make([]SyncSession, 0, len(entries))
	for _, path := range entries {
		session, ok := parsePiSession(path)
		if ok {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func parsePiSession(path string) (SyncSession, bool) {
	file, err := os.Open(path)
	if err != nil {
		return SyncSession{}, false
	}
	defer file.Close()

	session := SyncSession{}
	if stat, statErr := file.Stat(); statErr == nil {
		session.EndedAt = stat.ModTime().UTC().Format(time.RFC3339)
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	msgIndex := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		item := map[string]any{}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			continue
		}

		switch stringFrom(item["type"]) {
		case "session":
			session.ID = strings.TrimSpace(stringFrom(item["id"]))
			if ts := parsePiTimestamp(item["timestamp"]); ts != "" {
				session.StartedAt = ts
			}
			if session.Cwd == "" {
				session.Cwd = normalizeCwd(stringFrom(item["cwd"]))
			}
		case "model_change":
			model := strings.TrimSpace(stringFrom(item["modelId"]))
			if model != "" {
				session.Model = model
			}
		case "message":
			msg := mapFrom(item["message"])
			role := strings.TrimSpace(stringFrom(msg["role"]))
			if role == "" {
				continue
			}

			content := parsePiMessageContent(msg)
			timestamp := parsePiTimestamp(msg["timestamp"])
			if timestamp == "" {
				timestamp = parsePiTimestamp(item["timestamp"])
			}
			if content != "" {
				session.Messages = append(session.Messages, SyncMessage{
					Index:     msgIndex,
					Role:      role,
					Content:   content,
					Timestamp: timestamp,
				})
				if session.Title == "" && role == "user" {
					session.Title = content
				}
				msgIndex++
			}

			usage := mapFrom(msg["usage"])
			session.TotalInputTokens += intFrom(usage["input"], 0)
			session.TotalOutputTokens += intFrom(usage["output"], 0)
			session.TotalCachedInputTokens += intFrom(usage["cacheRead"], 0)
			if session.Model == "" {
				if model := strings.TrimSpace(stringFrom(msg["model"])); model != "" {
					session.Model = model
				}
			}
			if ts := parsePiTimestamp(item["timestamp"]); ts != "" {
				if session.EndedAt == "" || ts > session.EndedAt {
					session.EndedAt = ts
				}
			}
		}
	}

	if session.ID == "" {
		return SyncSession{}, false
	}
	if session.Cwd == "" {
		session.Cwd = normalizeCwd(session.Cwd)
	}
	if session.StartedAt == "" {
		session.StartedAt = session.EndedAt
	}
	if session.EndedAt == "" {
		session.EndedAt = session.StartedAt
	}
	session.TotalTokens = session.TotalInputTokens + session.TotalOutputTokens + session.TotalCachedInputTokens + session.TotalReasoningTokens
	if session.Title == "" {
		if session.Cwd != "" {
			session.Title = session.Cwd
		} else {
			session.Title = "PI session"
		}
	}
	return session, true
}

func parsePiMessageContent(message map[string]any) string {
	parts := make([]string, 0)
	contentItems, _ := message["content"].([]any)
	for _, item := range contentItems {
		entry := mapFrom(item)
		switch strings.TrimSpace(stringFrom(entry["type"])) {
		case "text":
			if text := strings.TrimSpace(stringFrom(entry["text"])); text != "" {
				parts = append(parts, text)
			}
		case "toolCall":
			name := strings.TrimSpace(stringFrom(entry["name"]))
			inputText := stringifyJSON(entry["arguments"])
			if name == "" {
				name = "tool"
			}
			if inputText == "" {
				parts = append(parts, fmt.Sprintf("[tool_use] %s", name))
			} else {
				parts = append(parts, fmt.Sprintf("[tool_use] %s\n%s", name, inputText))
			}
		default:
			if text := strings.TrimSpace(stringFrom(entry["text"])); text != "" {
				parts = append(parts, text)
			}
		}
	}

	if len(parts) == 0 {
		if text := strings.TrimSpace(stringFrom(message["errorMessage"])); text != "" {
			parts = append(parts, "[error]\n"+text)
		}
	}

	result := strings.TrimSpace(strings.Join(parts, "\n"))
	if role := strings.TrimSpace(stringFrom(message["role"])); role == "toolResult" && result != "" {
		toolName := strings.TrimSpace(stringFrom(message["toolName"]))
		if toolName == "" {
			toolName = "tool"
		}
		return fmt.Sprintf("[tool_result] %s\n%s", toolName, result)
	}
	return result
}

func parsePiTimestamp(value any) string {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return ""
		}
		if _, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return trimmed
		}
		if ms := int64From(trimmed, 0); ms > 0 {
			return formatMillis(ms)
		}
		return ""
	default:
		return formatMillis(int64From(v, 0))
	}
}

func loadOpenCodeProjects(projectsDir string, projects map[string]opencodeProject) error {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		projectPath := filepath.Join(projectsDir, entry.Name())
		raw, err := os.ReadFile(projectPath)
		if err != nil {
			continue
		}

		var project opencodeProject
		if err := json.Unmarshal(raw, &project); err != nil {
			continue
		}

		if project.ID != "" {
			projects[project.ID] = project
		}
	}

	return nil
}

func parseOpenCodeSession(sessionPath string, projects map[string]opencodeProject, messagesDir string, partsDir string) (SyncSession, bool) {
	raw, err := os.ReadFile(sessionPath)
	if err != nil {
		return SyncSession{}, false
	}

	var sess opencodeSession
	if err := json.Unmarshal(raw, &sess); err != nil {
		return SyncSession{}, false
	}

	if sess.ID == "" {
		return SyncSession{}, false
	}

	startedAt := formatMillis(sess.Time.Created)
	endedAt := formatMillis(sess.Time.Updated)
	if endedAt == "" {
		if stat, err := os.Stat(sessionPath); err == nil {
			endedAt = stat.ModTime().UTC().Format(time.RFC3339)
		}
	}

	session := SyncSession{
		ID:        sess.ID,
		Title:     sess.Title,
		StartedAt: startedAt,
		EndedAt:   endedAt,
	}

	// Set directory from session or project
	if sess.Directory != "" {
		session.Cwd = normalizeCwd(sess.Directory)
	} else if project, exists := projects[sess.ProjectID]; exists {
		session.Cwd = normalizeCwd(project.Worktree)
	}

	// Load messages and usage for this session.
	content, err := loadOpenCodeSessionContent(sess.ID, messagesDir, partsDir)
	if err == nil {
		session.Messages = content.Messages
		session.TotalInputTokens = content.TotalInputTokens
		session.TotalOutputTokens = content.TotalOutputTokens
		session.TotalCachedInputTokens = content.TotalCachedInputTokens
		session.TotalReasoningTokens = content.TotalReasoningTokens
		session.TotalTokens = content.TotalInputTokens + content.TotalOutputTokens + content.TotalCachedInputTokens + content.TotalReasoningTokens
		if session.Model == "" {
			session.Model = content.Model
		}
		if session.Cwd == "" {
			session.Cwd = content.Cwd
		}
		if session.EndedAt == "" {
			session.EndedAt = content.LastTimestamp
		}
	}

	// Set title if empty
	if session.Title == "" {
		session.Title = "OpenCode session"
		if session.Cwd != "" {
			session.Title = session.Cwd
		}
	}

	return session, true
}

type opencodeSessionContent struct {
	Messages               []SyncMessage
	TotalInputTokens       int
	TotalOutputTokens      int
	TotalCachedInputTokens int
	TotalReasoningTokens   int
	Model                  string
	Cwd                    string
	LastTimestamp          string
}

type opencodeStoredMessage struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	ModelID   string `json:"modelID"`
	Path      struct {
		Cwd string `json:"cwd"`
	} `json:"path"`
	Model struct {
		ModelID string `json:"modelID"`
	} `json:"model"`
	Summary struct {
		Title string `json:"title"`
	} `json:"summary"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Tokens struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read int `json:"read"`
		} `json:"cache"`
	} `json:"tokens"`
}

func loadOpenCodeSessionContent(sessionID string, messagesDir string, partsDir string) (opencodeSessionContent, error) {
	sessionDir := filepath.Join(messagesDir, sessionID)
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return opencodeSessionContent{}, err
	}

	type messageEntry struct {
		path string
		msg  opencodeStoredMessage
	}
	messageEntries := make([]messageEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(sessionDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var msg opencodeStoredMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.ID == "" {
			continue
		}
		messageEntries = append(messageEntries, messageEntry{path: path, msg: msg})
	}

	sort.Slice(messageEntries, func(i, j int) bool {
		li := messageEntries[i].msg.Time.Created
		lj := messageEntries[j].msg.Time.Created
		if li == lj {
			return messageEntries[i].msg.ID < messageEntries[j].msg.ID
		}
		return li < lj
	})

	result := opencodeSessionContent{}
	for _, item := range messageEntries {
		msg := item.msg
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			role = "assistant"
		}
		timestampMs := msg.Time.Created
		if timestampMs == 0 {
			timestampMs = msg.Time.Completed
		}
		if msg.Time.Completed > timestampMs {
			timestampMs = msg.Time.Completed
		}

		parts, partTimestampMs := loadOpenCodeMessageParts(msg.ID, partsDir)
		if partTimestampMs > timestampMs {
			timestampMs = partTimestampMs
		}
		content := strings.TrimSpace(strings.Join(parts, "\n"))
		if content == "" && role == "user" {
			content = strings.TrimSpace(msg.Summary.Title)
		}
		if content != "" {
			result.Messages = append(result.Messages, SyncMessage{
				Index:     len(result.Messages),
				Role:      role,
				Content:   content,
				Timestamp: formatMillis(timestampMs),
			})
		}

		if role == "assistant" {
			result.TotalInputTokens += msg.Tokens.Input
			result.TotalOutputTokens += msg.Tokens.Output
			result.TotalCachedInputTokens += msg.Tokens.Cache.Read
			result.TotalReasoningTokens += msg.Tokens.Reasoning
		}
		if result.Model == "" {
			if model := strings.TrimSpace(msg.ModelID); model != "" {
				result.Model = model
			} else if model := strings.TrimSpace(msg.Model.ModelID); model != "" {
				result.Model = model
			}
		}
		if result.Cwd == "" {
			result.Cwd = normalizeCwd(msg.Path.Cwd)
		}
		if ts := formatMillis(timestampMs); ts != "" {
			if result.LastTimestamp == "" || ts > result.LastTimestamp {
				result.LastTimestamp = ts
			}
		}
	}

	return result, nil
}

type opencodePartContent struct {
	OrderMs int64
	ID      string
	Text    string
}

func loadOpenCodeMessageParts(messageID string, partsDir string) ([]string, int64) {
	msgPartDir := filepath.Join(partsDir, messageID)
	entries, err := os.ReadDir(msgPartDir)
	if err != nil {
		return nil, 0
	}

	parts := make([]opencodePartContent, 0, len(entries))
	var maxTimestamp int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(msgPartDir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		payload := map[string]any{}
		if err := json.Unmarshal(raw, &payload); err != nil {
			continue
		}

		timeObj := mapFrom(payload["time"])
		start := int64From(timeObj["start"], 0)
		end := int64From(timeObj["end"], 0)
		order := start
		if order == 0 {
			order = end
		}
		if start > maxTimestamp {
			maxTimestamp = start
		}
		if end > maxTimestamp {
			maxTimestamp = end
		}

		content := parseOpenCodePartContent(payload)
		if content == "" {
			continue
		}
		parts = append(parts, opencodePartContent{
			OrderMs: order,
			ID:      stringFrom(payload["id"]),
			Text:    content,
		})
	}

	sort.Slice(parts, func(i, j int) bool {
		if parts[i].OrderMs == parts[j].OrderMs {
			return parts[i].ID < parts[j].ID
		}
		return parts[i].OrderMs < parts[j].OrderMs
	})

	result := make([]string, 0, len(parts))
	for _, part := range parts {
		result = append(result, part.Text)
	}
	return result, maxTimestamp
}

func parseOpenCodePartContent(payload map[string]any) string {
	partType := strings.TrimSpace(stringFrom(payload["type"]))
	switch partType {
	case "text":
		return strings.TrimSpace(stringFrom(payload["text"]))
	case "reasoning":
		text := strings.TrimSpace(stringFrom(payload["text"]))
		if text == "" {
			return ""
		}
		return "[reasoning]\n" + text
	case "tool":
		tool := strings.TrimSpace(stringFrom(payload["tool"]))
		state := mapFrom(payload["state"])
		inputText := stringifyJSON(state["input"])
		if tool == "" {
			tool = "tool"
		}
		if inputText == "" {
			return fmt.Sprintf("[tool] %s", tool)
		}
		return fmt.Sprintf("[tool] %s\n%s", tool, inputText)
	case "patch":
		files, _ := payload["files"].([]any)
		fileNames := make([]string, 0, len(files))
		for _, f := range files {
			if name := strings.TrimSpace(stringFrom(f)); name != "" {
				fileNames = append(fileNames, name)
			}
		}
		if len(fileNames) == 0 {
			return "[patch]"
		}
		return fmt.Sprintf("[patch]\n%s", strings.Join(fileNames, "\n"))
	default:
		return ""
	}
}

func loadAntigravitySessions(root string) ([]SyncSession, error) {
	brainDir := filepath.Join(root, "brain")
	info, err := os.Stat(brainDir)
	if err != nil || !info.IsDir() {
		return []SyncSession{}, nil
	}

	var sessions []SyncSession
	err = filepath.WalkDir(brainDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".metadata.json") {
			return nil
		}
		session, ok := parseAntigravityMetadata(path)
		if ok {
			sessions = append(sessions, session)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

type antigravityMetadata struct {
	ArtifactType string `json:"artifactType"`
	Summary      string `json:"summary"`
	UpdatedAt    string `json:"updatedAt"`
	Version      string `json:"version"`
}

type opencodeProject struct {
	ID       string       `json:"id"`
	Worktree string       `json:"worktree"`
	VCS      string       `json:"vcs"`
	Time     opencodeTime `json:"time"`
}

type opencodeSession struct {
	ID        string          `json:"id"`
	Version   string          `json:"version"`
	ProjectID string          `json:"projectID"`
	Directory string          `json:"directory"`
	ParentID  string          `json:"parentID,omitempty"`
	Title     string          `json:"title"`
	Time      opencodeTime    `json:"time"`
	Summary   opencodeSummary `json:"summary"`
}

type opencodeMessage struct {
	ID        string           `json:"id"`
	SessionID string           `json:"sessionID"`
	MessageID string           `json:"messageID"`
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	CallID    string           `json:"callID,omitempty"`
	Tool      string           `json:"tool,omitempty"`
	Snapshot  string           `json:"snapshot,omitempty"`
	State     opencodeState    `json:"state,omitempty"`
	Title     string           `json:"title,omitempty"`
	Metadata  opencodeMetadata `json:"metadata,omitempty"`
	Time      opencodeTime     `json:"time,omitempty"`
}

type opencodeTime struct {
	Created int64 `json:"created"`
	Updated int64 `json:"updated"`
	Start   int64 `json:"start,omitempty"`
	End     int64 `json:"end,omitempty"`
}

type opencodeSummary struct {
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
	Files     int `json:"files"`
}

type opencodeState struct {
	Status string         `json:"status"`
	Input  map[string]any `json:"input,omitempty"`
	Output string         `json:"output,omitempty"`
}

type opencodeMetadata struct {
	Count     int  `json:"count,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

func parseAntigravityMetadata(path string) (SyncSession, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return SyncSession{}, false
	}
	var meta antigravityMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return SyncSession{}, false
	}

	updatedAt := meta.UpdatedAt
	if updatedAt == "" {
		if stat, err := os.Stat(path); err == nil {
			updatedAt = stat.ModTime().Format(time.RFC3339)
		}
	}

	id := filepath.Base(filepath.Dir(path))
	title := strings.TrimSpace(meta.Summary)
	if title == "" {
		title = "Antigravity artifact"
	}

	session := SyncSession{
		ID:        id,
		Title:     title,
		StartedAt: updatedAt,
		EndedAt:   updatedAt,
	}
	if updatedAt != "" {
		session.Messages = append(session.Messages, SyncMessage{
			Index:     0,
			Role:      "assistant",
			Content:   title,
			Timestamp: updatedAt,
		})
	}
	return session, session.ID != ""
}

type droidSessionStart struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Cwd   string `json:"cwd"`
}

type droidMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type droidTokenUsage struct {
	InputTokens         int `json:"inputTokens"`
	OutputTokens        int `json:"outputTokens"`
	CacheCreationTokens int `json:"cacheCreationTokens"`
	CacheReadTokens     int `json:"cacheReadTokens"`
	ThinkingTokens      int `json:"thinkingTokens"`
}

type droidSettings struct {
	Model      string          `json:"model"`
	TokenUsage droidTokenUsage `json:"tokenUsage"`
}

func loadDroidSessions(root string) ([]SyncSession, error) {
	sessionsDir := filepath.Join(root, "sessions")
	info, err := os.Stat(sessionsDir)
	if err != nil || !info.IsDir() {
		return []SyncSession{}, nil
	}

	var sessions []SyncSession
	err = filepath.WalkDir(sessionsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		session, ok := parseDroidSession(path)
		if ok {
			sessions = append(sessions, session)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

func parseDroidSession(path string) (SyncSession, bool) {
	file, err := os.Open(path)
	if err != nil {
		return SyncSession{}, false
	}
	defer file.Close()

	var session SyncSession
	var startedAt time.Time
	var endedAt time.Time
	msgIndex := 0

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		item := map[string]any{}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			continue
		}
		switch stringFrom(item["type"]) {
		case "session_start":
			var start droidSessionStart
			if err := decodeInto(item, &start); err == nil {
				if start.ID != "" {
					session.ID = start.ID
				}
				if start.Title != "" {
					session.Title = start.Title
				}
				if start.Cwd != "" {
					session.Cwd = start.Cwd
				}
			}
		case "message":
			var message droidMessage
			if err := decodeInto(item["message"], &message); err != nil {
				message = droidMessage{Role: stringFrom(mapFrom(item["message"])["role"]), Content: mapFrom(item["message"])["content"]}
			}
			content := parseMessageContent(message.Content)
			if content == "" {
				continue
			}
			role := message.Role
			if role == "" {
				role = "user"
			}
			timestamp := stringFrom(item["timestamp"])
			session.Messages = append(session.Messages, SyncMessage{
				Index:     msgIndex,
				Role:      role,
				Content:   content,
				Timestamp: timestamp,
			})
			msgIndex++
			if session.Title == "" && role == "user" {
				session.Title = content
			}
			if timestamp != "" {
				if ts, err := time.Parse(time.RFC3339, timestamp); err == nil {
					if startedAt.IsZero() || ts.Before(startedAt) {
						startedAt = ts
					}
					if endedAt.IsZero() || ts.After(endedAt) {
						endedAt = ts
					}
				}
			}
		}
	}

	if session.ID == "" {
		session.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if session.Title == "" {
		session.Title = "Droid session"
	}

	settingsPath := strings.TrimSuffix(path, ".jsonl") + ".settings.json"
	if raw, err := os.ReadFile(settingsPath); err == nil {
		var settings droidSettings
		if err := json.Unmarshal(raw, &settings); err == nil {
			if settings.Model != "" {
				session.Model = settings.Model
			}
			cached := settings.TokenUsage.CacheReadTokens + settings.TokenUsage.CacheCreationTokens
			session.TotalInputTokens = settings.TokenUsage.InputTokens
			session.TotalOutputTokens = settings.TokenUsage.OutputTokens
			session.TotalCachedInputTokens = cached
			session.TotalReasoningTokens = settings.TokenUsage.ThinkingTokens
			session.TotalTokens = settings.TokenUsage.InputTokens + settings.TokenUsage.OutputTokens + cached + settings.TokenUsage.ThinkingTokens
		}
	}

	if session.TotalTokens == 0 {
		session.TotalTokens = session.TotalInputTokens + session.TotalOutputTokens + session.TotalCachedInputTokens + session.TotalReasoningTokens
	}

	if session.StartedAt == "" && !startedAt.IsZero() {
		session.StartedAt = startedAt.Format(time.RFC3339)
	}
	if session.EndedAt == "" && !endedAt.IsZero() {
		session.EndedAt = endedAt.Format(time.RFC3339)
	}
	if session.StartedAt == "" {
		if stat, err := os.Stat(path); err == nil {
			session.StartedAt = stat.ModTime().Format(time.RFC3339)
			session.EndedAt = session.StartedAt
		}
	}

	return session, session.ID != ""
}

func parseClaudeContent(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			entryType := stringFrom(entry["type"])
			switch entryType {
			case "text":
				text := stringFrom(entry["text"])
				if text != "" {
					parts = append(parts, text)
				}
			case "tool_use":
				name := stringFrom(entry["name"])
				input := entry["input"]
				inputText := stringifyJSON(input)
				if inputText == "" {
					parts = append(parts, fmt.Sprintf("[tool_use] %s", name))
				} else {
					parts = append(parts, fmt.Sprintf("[tool_use] %s\n%s", name, inputText))
				}
			case "tool_result":
				content := entry["content"]
				contentText := stringifyJSON(content)
				if contentText == "" {
					contentText = stringFrom(content)
				}
				if contentText != "" {
					parts = append(parts, fmt.Sprintf("[tool_result]\n%s", contentText))
				} else {
					parts = append(parts, "[tool_result]")
				}
			case "image":
				parts = append(parts, "[image]")
			default:
				raw := stringifyJSON(entry)
				if raw != "" {
					parts = append(parts, raw)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func stringifyJSON(value any) string {
	if value == nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}

func decodeInto(value any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func parseMessageContent(value any) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entryType := stringFrom(entry["type"])
		switch entryType {
		case "text":
			if text := stringFrom(entry["text"]); text != "" {
				parts = append(parts, text)
			}
		case "tool_use":
			name := stringFrom(entry["name"])
			input := entry["input"]
			inputText := stringifyJSON(input)
			if inputText == "" {
				if name != "" {
					parts = append(parts, fmt.Sprintf("[tool_use] %s", name))
				} else {
					parts = append(parts, "[tool_use]")
				}
			} else if name != "" {
				parts = append(parts, fmt.Sprintf("[tool_use] %s\n%s", name, inputText))
			} else {
				parts = append(parts, fmt.Sprintf("[tool_use]\n%s", inputText))
			}
		case "tool_result":
			content := entry["content"]
			contentText := stringifyJSON(content)
			if contentText == "" {
				contentText = stringFrom(content)
			}
			if contentText != "" {
				parts = append(parts, fmt.Sprintf("[tool_result]\n%s", contentText))
			} else {
				parts = append(parts, "[tool_result]")
			}
		default:
			// Fallback: include any inline text we can extract.
			if text := stringFrom(entry["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func syncPayload(server string, deviceToken string, payload SyncPayload) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := strings.TrimRight(server, "/") + "/v1/sync"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if deviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+deviceToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server returned %s: %s", resp.Status, string(body))
	}
	return nil
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func expandUser(path string) string {
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		if path == "~" {
			return home
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return name
}

type yiduoConfig struct {
	AuthToken         string `json:"auth_token,omitempty"`
	LegacyDeviceToken string `json:"device_token,omitempty"`
	Server            string `json:"server"`
	DeviceID          string `json:"device_id"`
}

func loadConfig() yiduoConfig {
	path := configPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return yiduoConfig{}
	}
	var cfg yiduoConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return yiduoConfig{}
	}
	if strings.TrimSpace(cfg.AuthToken) == "" && strings.TrimSpace(cfg.LegacyDeviceToken) != "" {
		cfg.AuthToken = strings.TrimSpace(cfg.LegacyDeviceToken)
	}
	return cfg
}

func saveConfig(cfg yiduoConfig) error {
	path := configPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if strings.TrimSpace(cfg.AuthToken) == "" && strings.TrimSpace(cfg.LegacyDeviceToken) != "" {
		cfg.AuthToken = strings.TrimSpace(cfg.LegacyDeviceToken)
	}
	// Keep reading old `device_token`, but only write canonical `auth_token`.
	cfg.LegacyDeviceToken = ""
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func configPath() string {
	return filepath.Join(expandUser("~/.yiduo"), "config.json")
}

func ensureDeviceID(cfg yiduoConfig) (yiduoConfig, error) {
	if strings.TrimSpace(cfg.DeviceID) != "" {
		return cfg, nil
	}
	deviceID, err := newDeviceID()
	if err != nil {
		return cfg, err
	}
	cfg.DeviceID = deviceID
	if fileExists(configPath()) {
		if err := saveConfig(cfg); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func newDeviceID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func stringFrom(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func mapFrom(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func intFrom(value any, fallback int) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return int(i)
		}
		if f, err := v.Float64(); err == nil {
			return int(f)
		}
	case string:
		if v == "" {
			return fallback
		}
		if i, err := json.Number(v).Int64(); err == nil {
			return int(i)
		}
	}
	return fallback
}

func isTrue(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i != 0
		}
	}
	return false
}

func int64From(value any, fallback int64) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i
		}
		if f, err := v.Float64(); err == nil {
			return int64(f)
		}
	case string:
		if v == "" {
			return fallback
		}
		if i, err := json.Number(v).Int64(); err == nil {
			return i
		}
	}
	return fallback
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func parseSources(value string) ([]string, error) {
	normalized := strings.TrimSpace(value)
	if normalized == "" || normalized == "auto" || normalized == "default" || normalized == "all" {
		return []string{
			"codex",
			"claude",
			"gemini",
			"qwen",
			"cline",
			"continue",
			"kilocode",
			"cursor",
			"amp",
			"opencode",
			"pi",
			"antigravity",
			"droid",
		}, nil
	}
	parts := strings.Split(normalized, ",")
	allowed := map[string]bool{
		"codex":       true,
		"claude":      true,
		"gemini":      true,
		"qwen":        true,
		"cline":       true,
		"continue":    true,
		"kilocode":    true,
		"cursor":      true,
		"amp":         true,
		"opencode":    true,
		"pi":          true,
		"antigravity": true,
		"droid":       true,
	}
	seen := map[string]bool{}
	var sources []string
	for _, part := range parts {
		source := strings.TrimSpace(part)
		if source == "" {
			continue
		}
		if source == "all" {
			return []string{
				"codex",
				"claude",
				"gemini",
				"qwen",
				"cline",
				"continue",
				"kilocode",
				"cursor",
				"amp",
				"opencode",
				"pi",
				"antigravity",
				"droid",
			}, nil
		}
		if !allowed[source] {
			return nil, fmt.Errorf("unsupported source %q", source)
		}
		if !seen[source] {
			sources = append(sources, source)
			seen[source] = true
		}
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no valid sources provided")
	}
	return sources, nil
}

func defaultToolName(source string) string {
	switch source {
	case "codex":
		return "codex"
	case "claude":
		return "claude-code"
	case "gemini":
		return "gemini"
	case "qwen":
		return "qwen"
	case "cline":
		return "cline"
	case "continue":
		return "continue"
	case "kilocode":
		return "kilocode"
	case "cursor":
		return "cursor"
	case "amp":
		return "amp"
	case "opencode":
		return "opencode"
	case "pi":
		return "pi"
	case "antigravity":
		return "antigravity"
	case "droid":
		return "droid"
	default:
		return source
	}
}
