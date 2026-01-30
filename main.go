package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
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
	source := flag.String("source", "auto", "data source: auto|codex|claude|gemini|qwen|cline|continue|kilocode|cursor|amp|opencode|antigravity|droid (comma-separated)")
	deviceToken := flag.String("device-token", "", "device token for sync authentication")
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
	ampRoot := flag.String("amp-root", envOrDefault("AMP_ROOT", "~/.amp"), "Amp root")
	opencodeRoot := flag.String("opencode-root", envOrDefault("OPENCODE_ROOT", "~/.opencode"), "OpenCode root")
	antigravityRoot := flag.String("antigravity-root", envOrDefault("ANTIGRAVITY_ROOT", "~/.gemini/antigravity"), "Antigravity root")
	droidRoot := flag.String("droid-root", envOrDefault("DROID_ROOT", "~/.factory"), "Droid root")
	agentID := flag.String("agent-id", "local", "agent id")
	host := flag.String("host", hostname(), "host name")
	if err := flag.CommandLine.Parse(args); err != nil {
		os.Exit(2)
	}

	extraArgs := flag.Args()
	forceDaemonStart := false
	if len(extraArgs) > 0 {
		switch extraArgs[0] {
		case "status":
			printDaemonStatus()
			return
		case "stop":
			if err := stopDaemon(); err != nil {
				fmt.Fprintf(os.Stderr, "failed to stop daemon: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("yiduo sync daemon stopped")
			return
		case "start":
			forceDaemonStart = true
		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s\n", extraArgs[0])
			os.Exit(2)
		}
	}

	daemonEnabled := *daemon || *daemonShort || forceDaemonStart
	daemonWorker := daemonEnabled && os.Getenv(daemonEnv) != ""
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
	resolvedServer := firstNonEmpty(*server, os.Getenv("AI_WRAPPED_SERVER"), config.Server, "http://localhost:8000")
	resolvedDeviceToken := firstNonEmpty(*deviceToken, os.Getenv("AI_WRAPPED_SYNC_TOKEN"), os.Getenv("AI_WRAPPED_DEVICE_TOKEN"), config.DeviceToken)
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
			server:           resolvedServer,
			deviceToken:      resolvedDeviceToken,
			deviceInfo:       deviceInfo,
			agentID:          *agentID,
			host:             *host,
			toolOverride:     toolOverride,
			sources:          sources,
			codexRoot:        expandUser(*codexRoot),
			claudeRoot:       expandUser(*claudeRoot),
			geminiRoot:       expandUser(*geminiRoot),
			qwenRoot:         expandUser(*qwenRoot),
			clineRoot:        expandUser(*clineRoot),
			continueRoot:     expandUser(*continueRoot),
			kiloRoot:         expandUser(*kiloRoot),
			cursorRoot:       expandUser(*cursorRoot),
			ampRoot:          expandUser(*ampRoot),
			opencodeRoot:     expandUser(*opencodeRoot),
			antigravityRoot:  expandUser(*antigravityRoot),
			droidRoot:        expandUser(*droidRoot),
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
	antigravityRoot string
	droidRoot       string
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
			sessions, err = loadCursorSessions(params.cursorRoot)
		case "amp":
			sessions, err = loadAmpSessions(params.ampRoot)
		case "opencode":
			sessions, err = loadOpenCodeSessions(params.opencodeRoot)
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
	defer removeDaemonPid()

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
		return
	}
	fmt.Println("yiduo sync daemon not running")
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
	return writeDaemonPidWith(os.Getpid())
}

func writeDaemonPidWith(pid int) error {
	path := daemonPidPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0o600)
}

func removeDaemonPid() {
	_ = os.Remove(daemonPidPath())
}

func daemonPidPath() string {
	return filepath.Join(expandUser("~/.yiduo"), "daemon.pid")
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
	SessionID   string `json:"sessionId"`
	FullPath    string `json:"fullPath"`
	Created     string `json:"created"`
	Modified    string `json:"modified"`
	FirstPrompt string `json:"firstPrompt"`
	ProjectPath string `json:"projectPath"`
	MessageCount int   `json:"messageCount"`
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
	info, err := os.Stat(tmpDir)
	if err != nil || !info.IsDir() {
		return []SyncSession{}, nil
	}

	logs, err := findQwenLogs(tmpDir)
	if err != nil {
		return nil, err
	}

	var sessions []SyncSession
	for _, path := range logs {
		items, err := parseQwenLog(path)
		if err != nil {
			continue
		}
		sessions = append(sessions, items...)
	}
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
			ID:        id,
			Title:     stringFrom(item["task"]),
			Cwd:       stringFrom(item["cwdOnTaskInitialization"]),
			StartedAt: startedAt,
			EndedAt:   startedAt,
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
	Message    map[string]any `json:"message"`
	PromptLogs []map[string]any `json:"promptLogs"`
}

type continueSession struct {
	SessionID          string           `json:"sessionId"`
	Title              string           `json:"title"`
	WorkspaceDirectory string           `json:"workspaceDirectory"`
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
	TokensIn   int     `json:"tokensIn"`
	TokensOut  int     `json:"tokensOut"`
	CacheReads int     `json:"cacheReads"`
	CacheWrites int    `json:"cacheWrites"`
	Cost       float64 `json:"cost"`
}

func loadKiloCodeSessions(root string) ([]SyncSession, error) {
	tasksDir := filepath.Join(root, "cli", "global", "tasks")
	info, err := os.Stat(tasksDir)
	if err != nil || !info.IsDir() {
		return []SyncSession{}, nil
	}

	entries, err := os.ReadDir(tasksDir)
	if err != nil {
		return nil, err
	}

	var sessions []SyncSession
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		taskDir := filepath.Join(tasksDir, entry.Name())
		session, ok := parseKiloTask(taskDir, entry.Name())
		if ok {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func parseKiloTask(taskDir string, fallbackID string) (SyncSession, bool) {
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

	id := fallbackID
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

func loadCursorSessions(root string) ([]SyncSession, error) {
	dbPath := filepath.Join(root, "ai-tracking", "ai-code-tracking.db")
	if _, err := os.Stat(dbPath); err != nil {
		return []SyncSession{}, nil
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return []SyncSession{}, nil
	}

	cmd := exec.Command("sqlite3", "-separator", "\t", dbPath, "SELECT conversationId, title, model, updatedAt FROM conversation_summaries")
	output, err := cmd.Output()
	if err != nil {
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
	return sessions, nil
}

func loadAmpSessions(root string) ([]SyncSession, error) {
	return []SyncSession{}, nil
}

func loadOpenCodeSessions(root string) ([]SyncSession, error) {
	return []SyncSession{}, nil
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
	InputTokens        int `json:"inputTokens"`
	OutputTokens       int `json:"outputTokens"`
	CacheCreationTokens int `json:"cacheCreationTokens"`
	CacheReadTokens    int `json:"cacheReadTokens"`
	ThinkingTokens     int `json:"thinkingTokens"`
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
	DeviceToken string `json:"device_token"`
	Server      string `json:"server"`
	DeviceID    string `json:"device_id"`
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
	return cfg
}

func saveConfig(cfg yiduoConfig) error {
	path := configPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
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
	case "antigravity":
		return "antigravity"
	case "droid":
		return "droid"
	default:
		return source
	}
}
