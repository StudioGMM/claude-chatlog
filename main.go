// 대화록 위젯 — Claude Code 대화 자동 기록
//
// Claude Code 대화 자동 기록 도구.
// Claude Code JSONL 대화 기록을 감시하여 터미널 형식으로 자동 변환·저장한다.
// macOS 메뉴바 위젯으로 상태 표시 (systray).
//
// 동작 흐름:
//   1. ~/.claude/projects/ 디렉토리의 JSONL 파일 감시 (24/7)
//   2. 변경된 세션만 재파싱, 캐시 유지
//   3. 같은 시간대(1시간) 세션은 하나의 파일로 묶어 저장
//   4. 대화 없는 시간대는 파일 생성 안 함
//
// 저장 형식: ❯ 사용자, ⏺ 클로드, ⎿ 도구결과 — 터미널 출력 그대로
// 파일명: YYYYMMDD_대화록_HH시.md (시간대별 1개)
//
// 로그: <작업디렉토리>/chatlog.log
// PID: ~/.chat.pid
// 로컬 API: localhost:7758

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/getlantern/systray"
)

var (
	home        string
	logDir      string
	progDir     string
	logPath     string
	pidFile     string
	projectDirs []string
	evalDir     string
	carcare     string

	// 세션 추적
	knownSessions map[string]int64 // sessionID → last processed size
	sessionMu     sync.Mutex

	// 세션 캐시 (파싱 결과)
	sessionCache map[string]*sessionData
	cacheMu      sync.Mutex

	// 위젯 상태
	activeSessions int
	totalToday     int
	lastSession    string
	stateMu        sync.RWMutex

	// 평가 상태
	evalRunning bool
	evalMu      sync.Mutex

	// 생각 추출 상태
	saengakRunning bool
	saengakMu      sync.Mutex

	// systray 메뉴
	mStatus   *systray.MenuItem
	mCount    *systray.MenuItem
	mLast     *systray.MenuItem
	mOpenLog  *systray.MenuItem
	mEval     *systray.MenuItem
	mSaengak  *systray.MenuItem
	mIlji     *systray.MenuItem
)

const (
	serverPort    = "7758"
	pollInterval  = 30 * time.Second
	activeTimeout = 5 * time.Minute
	claudeBin     = "claude" // Claude CLI 경로 — 환경에 맞게 수정 (예: /usr/local/bin/claude)
	evalHour      = 22 // 매일 저녁 10시
	saengakHour   = 23 // 매일 밤 11시 — 생각 추출
)

type sessionData struct {
	sessionID string
	startTime time.Time
	endTime   time.Time
	content   string
	topic     string // 첫 사용자 메시지 (캘린더 제목용)
}

// JSONL 메시지 구조체
type Message struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
	UUID      string          `json:"uuid"`
}

type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	ID    string          `json:"id"`
}

// 도구 입력 구조체
type BashInput struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type ReadInput struct {
	FilePath string `json:"file_path"`
}

type EditInput struct {
	FilePath  string `json:"file_path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

type WriteInput struct {
	FilePath string `json:"file_path"`
}

type GlobInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

type GrepInput struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

type TaskInput struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
}

type reopenWriter struct{}

func (w reopenWriter) Write(p []byte) (int, error) {
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(p)
}

func init() {
	home, _ = os.UserHomeDir()
	// 작업 디렉토리 — 환경변수 CHATLOG_DIR 또는 기본값 ~/claude-chatlog
	carcare = os.Getenv("CHATLOG_DIR")
	if carcare == "" {
		carcare = filepath.Join(home, "claude-chatlog")
	}
	logDir = filepath.Join(carcare, "대화록")
	progDir = carcare
	logPath = filepath.Join(progDir, "chatlog.log")
	pidFile = filepath.Join(home, ".chat.pid")
	projectDirs = detectProjectDirs(home)
	evalDir = filepath.Join(logDir, "평가")
	knownSessions = make(map[string]int64)
	sessionCache = make(map[string]*sessionData)
}

func main() {
	log.SetOutput(reopenWriter{})
	log.SetFlags(log.Ldate | log.Ltime)

	if isAlreadyRunning() {
		log.Println("이미 실행 중 — 종료")
		fmt.Println("이미 실행 중입니다.")
		os.Exit(1)
	}
	writePID()
	initSummary()
	initClean()
	log.Printf("대화록 위젯 시작 — JSONL 감시: %d개 디렉토리, MD 저장: %s", len(projectDirs), logDir)
	for _, d := range projectDirs {
		log.Printf("  감시: %s", d)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		systray.Quit()
	}()

	systray.Run(onReady, onExit)
}

// ========== JSONL 감시 ==========

func watchLoop() {
	processNewSessions()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		processNewSessions()
	}
}

func processNewSessions() {
	if len(projectDirs) == 0 {
		log.Printf("프로젝트 디렉토리 없음")
		return
	}

	now := time.Now()
	anyChanged := false
	affectedHours := make(map[string]bool)

	for _, dir := range projectDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}

		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}

			fullPath := filepath.Join(dir, e.Name())
			info, err := os.Stat(fullPath)
			if err != nil {
				continue
			}

			sessionID := strings.TrimSuffix(e.Name(), ".jsonl")

			// 변경 감지
			sessionMu.Lock()
			lastSize := knownSessions[sessionID]
			currentSize := info.Size()
			if currentSize != lastSize {
				knownSessions[sessionID] = currentSize
				anyChanged = true

				// 변경된 세션 재파싱
				result := parseSessionContent(fullPath, sessionID)
				if result.content != "" {
					cacheMu.Lock()
					sessionCache[sessionID] = &sessionData{
						sessionID: sessionID,
						startTime: result.startTime,
						endTime:   result.endTime,
						content:   result.content,
						topic:     result.topic,
					}
					hourKey := result.startTime.Format("20060102_15")
					affectedHours[hourKey] = true
					cacheMu.Unlock()
				}
			}
			sessionMu.Unlock()
		}
	}

	if anyChanged {
		// 처리 중 표시
		systray.SetTitle("🔄 CHAT")
		writeHourlyFiles(affectedHours)
	}

	// 상태 업데이트
	stateMu.Lock()
	today := now.Format("20060102")
	pattern := filepath.Join(logDir, today+"_대화록_*시.md")
	matches, _ := filepath.Glob(pattern)
	totalToday = len(matches)
	if len(matches) > 0 {
		sort.Strings(matches)
		latest := matches[len(matches)-1]
		if fi, err := os.Stat(latest); err == nil {
			lastSession = fi.ModTime().Format("15:04")
		}
	}
	stateMu.Unlock()

	// 처리 끝 → 정상 표시
	systray.SetTitle("🟢 CHAT")
	updateUI()
}

// writeHourlyFiles: 변경된 시간대의 파일만 재생성
func writeHourlyFiles(affectedHours map[string]bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	// 시간대별 그룹핑
	hourGroups := make(map[string][]*sessionData)
	for _, sd := range sessionCache {
		hourKey := sd.startTime.Format("20060102_15")
		hourGroups[hourKey] = append(hourGroups[hourKey], sd)
	}

	os.MkdirAll(logDir, 0755)

	for hourKey, sessions := range hourGroups {
		// 변경된 시간대만 파일 재생성
		if !affectedHours[hourKey] {
			continue
		}

		// 새 시간대 파일 생성 시 이전 시간대 요약 + 클린본 트리거
		trySummarizePreviousHour(hourKey)
		tryCleanPreviousHour(hourKey)

		// 시작 시간순 정렬
		sort.Slice(sessions, func(i, j int) bool {
			return sessions[i].startTime.Before(sessions[j].startTime)
		})

		t, err := time.ParseInLocation("20060102_15", hourKey, time.Local)
		if err != nil {
			continue
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("# 대화록 %s\n\n", t.Format("2006-01-02 15시")))

		for i, s := range sessions {
			if i > 0 {
				sb.WriteString("\n---\n\n")
			}
			if len(sessions) > 1 {
				sb.WriteString(fmt.Sprintf("> 세션 %d: %s (%s)\n\n", i+1, s.sessionID[:8], s.startTime.Format("15:04")))
			}
			sb.WriteString(s.content)
		}

		outName := fmt.Sprintf("%s_대화록_%s시.md", t.Format("20060102"), t.Format("15"))
		outPath := filepath.Join(logDir, outName)
		if err := os.WriteFile(outPath, []byte(sb.String()), 0644); err != nil {
			log.Printf("파일 저장 실패 [%s]: %v", outName, err)
		} else {
			log.Printf("대화록 저장: %s (%d 세션)", outName, len(sessions))
		}
	}
}

// ========== JSONL → 터미널 형식 변환 ==========

type parseResult struct {
	startTime time.Time
	endTime   time.Time
	content   string
	topic     string // 첫 사용자 메시지 (캘린더 제목용)
}

func parseSessionContent(jsonlPath, sessionID string) parseResult {
	// 파일 크기 제한: 5MB 이상은 스킵 (비정상적으로 큰 세션)
	if info, err := os.Stat(jsonlPath); err == nil && info.Size() > 5*1024*1024 {
		log.Printf("세션 스킵 (파일 크기 %dMB > 5MB): %s", info.Size()/1024/1024, filepath.Base(jsonlPath))
		return parseResult{}
	}

	f, err := os.Open(jsonlPath)
	if err != nil {
		return parseResult{}
	}
	defer f.Close()

	var sb strings.Builder
	var sessionStart, sessionEnd time.Time
	var topic string
	msgCount := 0
	firstUserChecked := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		var msg Message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}

		if msg.Timestamp != "" {
			if t, err := time.Parse(time.RFC3339Nano, msg.Timestamp); err == nil {
				local := t.Local()
				if sessionStart.IsZero() {
					sessionStart = local
				}
				sessionEnd = local
			}
		}

		switch msg.Type {
		case "user":
			if !firstUserChecked {
				firstUserChecked = true
				text := peekUserText(msg.Message)
				if isAutomatedSession(text) {
					return parseResult{}
				}
			}
			// topic이 아직 없으면 의미있는 사용자 메시지를 찾아 저장
			if topic == "" {
				text := peekUserText(msg.Message)
				if text != "" && !isSystemContent(text) && !isSkippableTopic(text) {
					topic = extractTopic(text)
				}
			}
			if processUserMessage(msg.Message, &sb) {
				msgCount++
			}
		case "assistant":
			if processAssistantMessage(msg.Message, &sb) {
				msgCount++
			}
		}
	}

	if msgCount == 0 {
		return parseResult{}
	}

	if sessionStart.IsZero() {
		sessionStart = time.Now()
	}
	if sessionEnd.IsZero() {
		sessionEnd = sessionStart
	}

	return parseResult{
		startTime: sessionStart,
		endTime:   sessionEnd,
		content:   sb.String(),
		topic:     topic,
	}
}

// processUserMessage: 사용자 입력 → ❯, 도구결과 → ⎿
func processUserMessage(raw json.RawMessage, sb *strings.Builder) bool {
	var msg ChatMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return false
	}

	wrote := false

	// content가 문자열인 경우
	var textStr string
	if err := json.Unmarshal(msg.Content, &textStr); err == nil {
		text := strings.TrimSpace(textStr)
		if text != "" && !isSystemContent(text) {
			sb.WriteString(fmt.Sprintf("❯ %s\n\n", text))
			wrote = true
		}
		return wrote
	}

	// content가 배열인 경우
	var blocks []json.RawMessage
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return false
	}

	for _, blockRaw := range blocks {
		var block struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}

		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text != "" && !isSystemContent(text) {
				sb.WriteString(fmt.Sprintf("❯ %s\n\n", text))
				wrote = true
			}
		case "tool_result":
			result := extractToolResultContent(block.Content, block.IsError)
			if result != "" {
				if len(result) > 3000 {
					result = safeTruncate(result, 3000)
				}
				lines := strings.Split(result, "\n")
				sb.WriteString(fmt.Sprintf("  ⎿ %s\n", lines[0]))
				for _, line := range lines[1:] {
					sb.WriteString(fmt.Sprintf("    %s\n", line))
				}
				sb.WriteString("\n")
				wrote = true
			}
		}
	}

	return wrote
}

// processAssistantMessage: 텍스트 → ⏺, 도구호출 → ⏺ ToolName(...)
func processAssistantMessage(raw json.RawMessage, sb *strings.Builder) bool {
	var msg ChatMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return false
	}

	wrote := false

	// content가 문자열인 경우
	var textStr string
	if err := json.Unmarshal(msg.Content, &textStr); err == nil {
		text := strings.TrimSpace(textStr)
		if text != "" {
			sb.WriteString(fmt.Sprintf("⏺ %s\n\n", text))
			wrote = true
		}
		return wrote
	}

	// content가 배열인 경우
	var blocks []json.RawMessage
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return false
	}

	for _, blockRaw := range blocks {
		var block ContentBlock
		if err := json.Unmarshal(blockRaw, &block); err != nil {
			continue
		}

		switch block.Type {
		case "text":
			text := strings.TrimSpace(block.Text)
			if text != "" {
				sb.WriteString(fmt.Sprintf("⏺ %s\n\n", text))
				wrote = true
			}
		case "tool_use":
			formatToolUse(block.Name, block.Input, sb)
			wrote = true
		}
	}

	return wrote
}

// formatToolUse: 도구 호출을 터미널 형식으로 포맷
func formatToolUse(name string, input json.RawMessage, sb *strings.Builder) {
	switch name {
	case "Bash":
		var bi BashInput
		if err := json.Unmarshal(input, &bi); err == nil {
			cmd := bi.Command
			if len(cmd) > 300 {
				cmd = safeTruncate(cmd, 300)
			}
			sb.WriteString(fmt.Sprintf("⏺ Bash(%s)\n\n", cmd))
		}

	case "Read":
		var ri ReadInput
		if err := json.Unmarshal(input, &ri); err == nil {
			sb.WriteString(fmt.Sprintf("⏺ Read(%s)\n\n", ri.FilePath))
		}

	case "Edit":
		var ei EditInput
		if err := json.Unmarshal(input, &ei); err == nil {
			sb.WriteString(fmt.Sprintf("⏺ Update(%s)\n", ei.FilePath))
			if ei.OldString != "" || ei.NewString != "" {
				oldLines := strings.Split(strings.TrimRight(ei.OldString, "\n"), "\n")
				newLines := strings.Split(strings.TrimRight(ei.NewString, "\n"), "\n")
				maxLines := 10
				for i, l := range oldLines {
					if i >= maxLines {
						sb.WriteString("    ...\n")
						break
					}
					sb.WriteString(fmt.Sprintf("  - %s\n", l))
				}
				for i, l := range newLines {
					if i >= maxLines {
						sb.WriteString("    ...\n")
						break
					}
					sb.WriteString(fmt.Sprintf("  + %s\n", l))
				}
			}
			sb.WriteString("\n")
		}

	case "Write":
		var wi WriteInput
		if err := json.Unmarshal(input, &wi); err == nil {
			sb.WriteString(fmt.Sprintf("⏺ Write(%s)\n\n", wi.FilePath))
		}

	case "Glob":
		var gi GlobInput
		if err := json.Unmarshal(input, &gi); err == nil {
			if gi.Path != "" {
				sb.WriteString(fmt.Sprintf("⏺ Glob(%s in %s)\n\n", gi.Pattern, gi.Path))
			} else {
				sb.WriteString(fmt.Sprintf("⏺ Glob(%s)\n\n", gi.Pattern))
			}
		}

	case "Grep":
		var gri GrepInput
		if err := json.Unmarshal(input, &gri); err == nil {
			if gri.Path != "" {
				sb.WriteString(fmt.Sprintf("⏺ Grep(%s in %s)\n\n", gri.Pattern, gri.Path))
			} else {
				sb.WriteString(fmt.Sprintf("⏺ Grep(%s)\n\n", gri.Pattern))
			}
		}

	case "Task":
		var ti TaskInput
		if err := json.Unmarshal(input, &ti); err == nil {
			desc := ti.Description
			if desc == "" {
				desc = ti.Prompt
				if len(desc) > 150 {
					desc = safeTruncate(desc, 150)
				}
			}
			sb.WriteString(fmt.Sprintf("⏺ Task(%s)\n\n", desc))
		}

	default:
		inputStr := string(input)
		if len(inputStr) > 200 {
			inputStr = safeTruncate(inputStr, 200)
		}
		sb.WriteString(fmt.Sprintf("⏺ %s(%s)\n\n", name, inputStr))
	}
}

// extractToolResultContent: tool_result의 content 추출
func extractToolResultContent(raw json.RawMessage, isError bool) string {
	if raw == nil || string(raw) == "null" {
		return ""
	}

	prefix := ""
	if isError {
		prefix = "❌ "
	}

	// content가 문자열인 경우
	var contentStr string
	if err := json.Unmarshal(raw, &contentStr); err == nil {
		return prefix + strings.TrimSpace(contentStr)
	}

	// content가 배열인 경우
	var contentBlocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &contentBlocks); err == nil {
		var texts []string
		for _, cb := range contentBlocks {
			if strings.TrimSpace(cb.Text) != "" {
				texts = append(texts, strings.TrimSpace(cb.Text))
			}
		}
		if len(texts) > 0 {
			return prefix + strings.Join(texts, "\n")
		}
	}

	return ""
}

// isSystemContent: 시스템 주입 콘텐츠 필터
func isSystemContent(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "<system-reminder>") ||
		strings.HasPrefix(t, "<context>") ||
		strings.HasPrefix(t, "<system>") ||
		strings.HasPrefix(t, "<local-command") ||
		strings.HasPrefix(t, "<command-name>")
}

// peekUserText: user 메시지에서 첫 텍스트 추출 (자동화 세션 판별용)
func peekUserText(raw json.RawMessage) string {
	var msg ChatMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}

	// content가 문자열인 경우
	var textStr string
	if err := json.Unmarshal(msg.Content, &textStr); err == nil {
		return strings.TrimSpace(textStr)
	}

	// content가 배열인 경우 — 첫 text 블록
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(msg.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}

	return ""
}

// isAutomatedSession: 자동화 프롬프트(claude -p)로 시작된 세션인지 판별
func isAutomatedSession(text string) bool {
	if text == "" {
		return false
	}
	automatedPrefixes := []string{
		"당신은 ",
		"You are a context log writer",
		"당신은 사용자의 일일 자기 객관화 분석가입니다",
		"아래는 1시간 동안의 대화록",
		"아래는 사용자와 Claude의 대화록",
		"아래 대화록 파일을 읽고",
		"도구(Bash, Read, Write, Edit, Glob, Grep 등)를 사용하지 마",
		"아래 대화 내역을 바탕으로 인수인계",
		"다음 세션에 인수인계할 요약을",
		"[세션 인수인계]",
		"Continue from where you left off",
		"아래 대화 목록의 각 항목을",
	}
	for _, prefix := range automatedPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	// Claude Code 데스크탑 세션 (터미널 아트로 시작)
	if strings.Contains(text[:min(len(text), 200)], "Claude Code") {
		return true
	}
	// local-command-caveat (CLI 내부 세션)
	if strings.HasPrefix(text, "<local-command-caveat>") {
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isSkippableTopic: 캘린더 제목으로 쓸 수 없는 메시지 필터
func isSkippableTopic(text string) bool {
	t := strings.TrimSpace(text)
	lower := strings.ToLower(t)

	// 인사/짧은 응답
	skipPrefixes := []string{
		"클로드", "안녕", "ㅎㅇ", "하이", "hello", "hi ",
		"ㅇㅇ", "ㄱㄱ", "굳", "네", "응", "좋아",
	}
	for _, p := range skipPrefixes {
		if strings.HasPrefix(lower, p) && len(t) < 20 {
			return true
		}
	}

	// 시스템 생성 메시지
	if strings.HasPrefix(t, "Implement the following plan:") ||
		strings.HasPrefix(t, "If you need specific details") ||
		strings.HasPrefix(t, "## Context") {
		return true
	}

	return false
}

// extractTopic: 사용자 메시지에서 캘린더 제목 추출
func extractTopic(text string) string {
	t := strings.TrimSpace(text)

	// "Implement the following plan:" 이후 제목 추출
	if strings.Contains(t, "# ") {
		lines := strings.Split(t, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "# ") {
				title := strings.TrimPrefix(line, "# ")
				if len(title) > 40 {
					title = safeTruncate(title, 40)
				}
				return title
			}
		}
	}

	// 일반 메시지
	// 첫 줄만 사용
	if idx := strings.Index(t, "\n"); idx > 0 {
		t = t[:idx]
	}
	if len(t) > 40 {
		t = safeTruncate(t, 40)
	}
	return t
}

// safeTruncate: UTF-8 안전 자르기 (멀티바이트 문자 중간에서 안 잘림)
func safeTruncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// maxBytes 위치에서 뒤로 가면서 유효한 rune 경계 찾기
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes] + "..."
}

// ========== 위젯 모드 ==========

func onReady() {

	systray.SetTitle("🟢 CHAT")
	systray.SetTooltip("Claude Code 대화 자동 기록 + 일일 평가")

	mStatus = systray.AddMenuItem("✅ 대기 중", "현재 상태")
	mStatus.Disable()
	mCount = systray.AddMenuItem("📊 오늘: 0개", "오늘 대화록 수")
	mCount.Disable()
	mLast = systray.AddMenuItem("🕐 마지막: -", "마지막 세션")
	mLast.Disable()

	systray.AddSeparator()

	mEval = systray.AddMenuItem("📊 일일 평가 실행", "오늘 대화 기반 전방위 분석")
	mSaengak = systray.AddMenuItem("💭 생각 추출 실행", "오늘 대화록에서 전략적 생각 추출")
	mIlji = systray.AddMenuItem("📝 업무일지 작성", "오늘 업무일지 생성")
	mOpenLog = systray.AddMenuItem("📂 대화록 폴더 열기", "대화록 폴더")
	mQuit := systray.AddMenuItem("🚪 종료", "위젯 종료")

	go startServer()
	go watchLoop()
	go scheduleEvaluation()
	go scheduleSaengak()
	go scheduleIlji()

	go func() {
		for {
			select {
			case <-mEval.ClickedCh:
				go runEvaluation()
			case <-mSaengak.ClickedCh:
				go runSaengak()
			case <-mIlji.ClickedCh:
				go runIlji()
			case <-mOpenLog.ClickedCh:
				exec.Command("open", logDir).Run()
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	removePID()
	log.Println("대화록 위젯 종료")
}

func updateUI() {
	stateMu.RLock()
	defer stateMu.RUnlock()

	mStatus.SetTitle("✅ 대기 중")
	mCount.SetTitle(fmt.Sprintf("📊 오늘: %d개", totalToday))

	if lastSession != "" {
		mLast.SetTitle(fmt.Sprintf("🕐 마지막: %s", lastSession))
	}
}

// ========== PID 관리 ==========

func isAlreadyRunning() bool {
	// 1. PID 파일 체크
	data, err := os.ReadFile(pidFile)
	if err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			process, err := os.FindProcess(pid)
			if err == nil && process.Signal(syscall.Signal(0)) == nil {
				out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
				if err == nil && strings.Contains(string(out), filepath.Base(os.Args[0])) {
					return true
				}
			}
		}
		os.Remove(pidFile)
	}

	return false
}

func writePID() {
	os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644)
}

func removePID() {
	os.Remove(pidFile)
}

// ========== HTTP API ==========

func startServer() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("startServer panic: %v", r)
		}
	}()
	mux := http.NewServeMux()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		stateMu.RLock()
		defer stateMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"active_sessions": activeSessions,
			"today_count":     totalToday,
			"last_session":    lastSession,
			"project_dirs":    projectDirs,
			"md_dir":          logDir,
		})
	})

	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		today := time.Now().Format("20060102")
		pattern := filepath.Join(logDir, today+"_대화록_*시.md")
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)

		var logs []map[string]string
		for _, m := range matches {
			info, err := os.Stat(m)
			if err != nil {
				continue
			}
			logs = append(logs, map[string]string{
				"file":     filepath.Base(m),
				"size":     fmt.Sprintf("%d", info.Size()),
				"modified": info.ModTime().Format("15:04:05"),
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(logs)
	})

	mux.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		go processNewSessions()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"result": "동기화 시작"})
	})

	mux.HandleFunc("/evaluate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		evalMu.Lock()
		running := evalRunning
		evalMu.Unlock()
		if running {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"result": "이미 진행 중"})
			return
		}
		go runEvaluation()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"result": "평가 시작"})
	})

	mux.HandleFunc("/saengak", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		saengakMu.Lock()
		running := saengakRunning
		saengakMu.Unlock()
		if running {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"result": "이미 진행 중"})
			return
		}
		go runSaengak()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"result": "생각 추출 시작"})
	})

	mux.HandleFunc("/summarize", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		summaryMu.Lock()
		running := summaryRunning
		summaryMu.Unlock()
		if running {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"result": "이미 진행 중"})
			return
		}
		go runSummaryUI()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"result": "요약 시작"})
	})

	mux.HandleFunc("/clean", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		cleanMu.Lock()
		running := cleanRunning
		cleanMu.Unlock()
		if running {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"result": "이미 진행 중"})
			return
		}
		go runFullClean()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"result": "클린본 전체 생성 시작"})
	})


	mux.HandleFunc("/ilji", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if _, err := os.Stat(iljiLockFile); err == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"result": "이미 진행 중"})
			return
		}
		go runIlji()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"result": "업무일지 작성 시작"})
	})

	log.Printf("API 서버 시작: 0.0.0.0:%s", serverPort)
	http.ListenAndServe("0.0.0.0:"+serverPort, mux)
}

// ========== 일일 평가 ==========

func scheduleEvaluation() {
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), evalHour, 0, 0, 0, now.Location())
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}
		log.Printf("다음 일일 평가 예정: %s", next.Format("2006-01-02 15:04"))
		time.Sleep(time.Until(next))
		runEvaluation()
	}
}

func scheduleSaengak() {
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), saengakHour, 0, 0, 0, now.Location())
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}
		log.Printf("다음 생각 추출 예정: %s", next.Format("2006-01-02 15:04"))
		time.Sleep(time.Until(next))
		runSaengak()
	}
}

const saengakLockFile = "/tmp/.saengak.running"
const iljiLockFile = "/tmp/.ilji.running"

func runSaengak() {
	// 락 파일로 중복 실행 방지
	if _, err := os.Stat(saengakLockFile); err == nil {
		log.Println("생각 추출 이미 진행 중 (락 파일 존재)")
		return
	}

	mSaengak.SetTitle("💭 생각 추출 중...")
	log.Println("생각 추출 시작")

	today := time.Now().Format("20060102")
	saengakDir := filepath.Join(logDir, "생각")

	// 오늘 대화록 파일 목록
	chatPattern := filepath.Join(logDir, today+"_대화록_*시.md")
	chatFiles, _ := filepath.Glob(chatPattern)
	sort.Strings(chatFiles)

	if len(chatFiles) == 0 {
		log.Println("오늘 대화록 없음 — 생각 추출 스킵")
		mSaengak.SetTitle("💭 생각 추출 실행")
		return
	}

	// 오늘 이미 저장된 생각 파일 목록
	existingPattern := filepath.Join(saengakDir, today+"_*.md")
	existingFiles, _ := filepath.Glob(existingPattern)

	// 작업 지시 파일 작성
	taskContent := buildSaengakPrompt(today, saengakDir, chatFiles, existingFiles)
	taskFile := "/tmp/saengak_task.md"
	if err := os.WriteFile(taskFile, []byte(taskContent), 0644); err != nil {
		log.Printf("작업 파일 생성 실패: %v", err)
		mSaengak.SetTitle("💭 생각 추출 (실패)")
		return
	}

	// claude에 task를 stdin으로 주입해서 백그라운드 실행
	cmd := exec.Command(claudeBin, "--dangerously-skip-permissions", "--max-turns", "20")
	cmd.Dir = carcare
	cmd.Stdin = strings.NewReader(taskContent)
	cmd.Env = append(os.Environ(),
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:"+os.Getenv("PATH"),
		"DISABLE_PROMPT_CACHING=1",
	)
	logFile, _ := os.OpenFile("/tmp/saengak.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		log.Printf("claude 실행 실패: %v", err)
		mSaengak.SetTitle("💭 생각 추출 (실패)")
		os.Remove(saengakLockFile)
		logFile.Close()
		return
	}
	mSaengak.SetTitle("💭 백그라운드 진행 중...")
	log.Printf("생각 추출 백그라운드 시작 (PID %d)", cmd.Process.Pid)

	// 완료 대기 고루틴
	go func() {
		defer logFile.Close()
		defer os.Remove(saengakLockFile)
		err := cmd.Wait()
		if err != nil {
			log.Printf("생각 추출 종료 (오류): %v", err)
			mSaengak.SetTitle("💭 추출 실패")
		} else {
			log.Printf("생각 추출 완료")
			mSaengak.SetTitle(fmt.Sprintf("💭 추출 완료 (%s)", time.Now().Format("15:04")))
			exec.Command("osascript", "-e",
				`display notification "생각 추출 완료" with title "대화록 위젯" subtitle "생각/ 폴더 확인"`).Run()
		}
	}()
}

func buildSaengakPrompt(today, saengakDir string, chatFiles, existingFiles []string) string {
	var b strings.Builder

	b.WriteString("# 생각 추출 작업\n\n")
	b.WriteString("오늘 대화록을 읽고 사용자(❯ 로 표시)의 전략적 생각을 추출해서 파일로 저장하라.\n\n")

	b.WriteString("## 읽을 파일\n")
	for _, f := range chatFiles {
		b.WriteString(fmt.Sprintf("- %s\n", f))
	}

	b.WriteString("\n## 추출 기준\n")
	b.WriteString("- 저장 O: 사업 방향, 전략적 고민, 구상, 아이디어, 방향 결정, 인사이트\n")
	b.WriteString("- 저장 X: 작업 지시, 기술 질의, 잡담, 단순 확인\n\n")

	b.WriteString("## 이미 저장된 파일 (중복 주제 시 append)\n")
	if len(existingFiles) == 0 {
		b.WriteString("(없음)\n")
	} else {
		for _, f := range existingFiles {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
	}

	b.WriteString(fmt.Sprintf("\n## 저장\n"))
	b.WriteString(fmt.Sprintf("- 경로: %s\n", saengakDir))
	b.WriteString(fmt.Sprintf("- 파일명: %s_주제키워드.md\n", today))
	b.WriteString("- 같은 주제 → 기존 파일에 append / 새 주제 → 신규 생성\n\n")

	b.WriteString("## 파일 내용 형식\n")
	b.WriteString("```\n")
	b.WriteString("## 배경\n")
	b.WriteString("이 생각이 나온 맥락. 사용자 발언 직접 인용 포함.\n")
	b.WriteString("예) 사용자가 \"훅이 지나치게 많이 작동하지 않겠지?\"라고 말하며 자동화 범위에 의문을 품었다.\n\n")
	b.WriteString("## 핵심 생각\n")
	b.WriteString("사용자 발언 원문 인용 후 해석. 요약 대체 금지.\n")
	b.WriteString("예) \"프롬프트로 하지말고 실제 클로드 코드를 백그라운드 터미널에서 호출해서 결과를 받아와\"\n")
	b.WriteString("→ 단순 프롬프트 기반이 아닌 실제 에이전트 실행으로 품질을 높이려는 의도.\n\n")
	b.WriteString("## 방향\n")
	b.WriteString("대화에서 나온 결론 또는 다음 스텝. 발언 인용. 미결이면 '미결' 표기.\n")
	b.WriteString("```\n\n")

	b.WriteString("추출된 생각이 없으면 아무 파일도 생성하지 말고 '추출된 생각 없음'만 출력.\n")

	return b.String()
}

// ========== 업무일지 ==========

func scheduleIlji() {
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), 23, 30, 0, 0, now.Location())
		if now.After(next) {
			next = next.Add(24 * time.Hour)
		}
		log.Printf("다음 업무일지 예정: %s", next.Format("2006-01-02 15:04"))
		time.Sleep(time.Until(next))
		runIlji()
	}
}

func runIlji() {
	if _, err := os.Stat(iljiLockFile); err == nil {
		log.Println("업무일지 이미 진행 중")
		return
	}

	mIlji.SetTitle("📝 업무일지 작성 중...")
	log.Println("업무일지 작성 시작")

	today := time.Now().Format("20060102")
	todayFmt := time.Now().Format("2006-01-02")
	iljiDir := filepath.Join(logDir, "업무일지")
	iljiPath := filepath.Join(iljiDir, today+"_업무일지.md")

	// 오늘 대화록 파일 목록
	chatPattern := filepath.Join(logDir, today+"_대화록_*시.md")
	chatFiles, _ := filepath.Glob(chatPattern)
	sort.Strings(chatFiles)

	if len(chatFiles) == 0 {
		log.Println("오늘 대화록 없음 — 업무일지 스킵")
		mIlji.SetTitle("📝 업무일지 작성")
		return
	}

	prompt := buildIljiPrompt(todayFmt, iljiPath, chatFiles)

	cmd := exec.Command(claudeBin, "--dangerously-skip-permissions", "--max-turns", "20")
	cmd.Dir = carcare
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Env = append(os.Environ(),
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:"+os.Getenv("PATH"),
		"DISABLE_PROMPT_CACHING=1",
	)
	logFile, _ := os.OpenFile("/tmp/ilji.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	os.WriteFile(iljiLockFile, []byte(""), 0644)
	if err := cmd.Start(); err != nil {
		log.Printf("업무일지 실행 실패: %v", err)
		mIlji.SetTitle("📝 업무일지 (실패)")
		os.Remove(iljiLockFile)
		logFile.Close()
		return
	}
	log.Printf("업무일지 백그라운드 시작 (PID %d)", cmd.Process.Pid)
	mIlji.SetTitle("📝 백그라운드 진행 중...")

	go func() {
		defer logFile.Close()
		defer os.Remove(iljiLockFile)
		err := cmd.Wait()
		if err != nil {
			log.Printf("업무일지 종료 (오류): %v", err)
			mIlji.SetTitle("📝 업무일지 (실패)")
		} else {
			log.Println("업무일지 작성 완료")
			mIlji.SetTitle(fmt.Sprintf("📝 완료 (%s)", time.Now().Format("15:04")))
			exec.Command("osascript", "-e",
				fmt.Sprintf(`display notification "업무일지 작성 완료" with title "대화록 위젯" subtitle "%s"`, today+"_업무일지.md")).Run()
		}
	}()
}

func buildIljiPrompt(todayFmt, iljiPath string, chatFiles []string) string {
	var b strings.Builder

	b.WriteString("# 업무일지 작성\n\n")
	b.WriteString(fmt.Sprintf("오늘(%s) 대화록을 읽고 업무일지를 작성해서 저장하라.\n\n", todayFmt))

	b.WriteString("## 읽을 파일\n")
	for _, f := range chatFiles {
		b.WriteString(fmt.Sprintf("- %s\n", f))
	}

	b.WriteString("\n## 저장 경로\n")
	b.WriteString(fmt.Sprintf("- %s\n\n", iljiPath))

	b.WriteString("## 작성 형식\n")
	b.WriteString(fmt.Sprintf("```\n# %s 업무일지\n\n", todayFmt))
	b.WriteString("## 오늘 한 일\n\n")
	b.WriteString("### 1. [작업명]\n")
	b.WriteString("- 구체적으로 무엇을 했는지\n")
	b.WriteString("- 결과물 또는 산출물\n\n")
	b.WriteString("### 2. [작업명]\n")
	b.WriteString("- ...\n\n")
	b.WriteString("## 자동화 프로그램 현황 (변경 있을 때만)\n\n")
	b.WriteString("| 프로그램 | 포트 | 상태 |\n")
	b.WriteString("|---------|------|------|\n\n")
	b.WriteString("## 미완료\n\n")
	b.WriteString("- [ ] 내일 이어서 할 것\n")
	b.WriteString("```\n\n")

	b.WriteString("## 작성 규칙\n")
	b.WriteString("- 대화록의 실제 작업 내용 기반으로 작성. 추측 금지.\n")
	b.WriteString("- 기술적 작업(Go 코드, 빌드, 설정 등)은 구체적으로 기록\n")
	b.WriteString("- 단순 대화·잡담은 제외\n")
	b.WriteString("- 파일 저장은 Write 도구 사용\n")

	return b.String()
}

func runEvaluation() {
	evalMu.Lock()
	if evalRunning {
		evalMu.Unlock()
		log.Println("평가 이미 진행 중")
		return
	}
	evalRunning = true
	evalMu.Unlock()
	defer func() {
		evalMu.Lock()
		evalRunning = false
		evalMu.Unlock()
	}()

	systray.SetTitle("🔄 CHAT")
	mEval.SetTitle("📊 평가 진행 중...")
	log.Println("일일 평가 시작")

	today := time.Now().Format("20060102")
	todayFmt := time.Now().Format("2006-01-02")

	// 1. 오늘 대화록 파일 목록
	chatPattern := filepath.Join(logDir, today+"_대화록_*시.md")
	chatFiles, _ := filepath.Glob(chatPattern)
	sort.Strings(chatFiles)

	if len(chatFiles) == 0 {
		log.Println("오늘 대화록 없음 — 평가 스킵")
		systray.SetTitle("🟢 CHAT")
		mEval.SetTitle("📊 일일 평가 실행")
		return
	}

	// 2. 최근 평가 파일 (트렌드 비교용, 최대 3개)
	os.MkdirAll(evalDir, 0755)
	evalPattern := filepath.Join(evalDir, "*_평가.md")
	evalFiles, _ := filepath.Glob(evalPattern)
	sort.Strings(evalFiles)
	if len(evalFiles) > 3 {
		evalFiles = evalFiles[len(evalFiles)-3:]
	}

	// 3. 최근 수정된 파일 (git)
	gitOut, _ := exec.Command("git", "-C", carcare, "diff", "--name-only", "HEAD~3").Output()

	// 4. 프롬프트 구성
	prompt := buildEvalPrompt(todayFmt, chatFiles, evalFiles, string(gitOut))

	// 5. claude -p 실행
	cmd := exec.Command(claudeBin, "-p", prompt, "--max-turns", "30", "--dangerously-skip-permissions")
	cmd.Dir = carcare
	// launchd 환경에서 PATH 부족 → node, git 등 포함
	cmd.Env = append(os.Environ(),
		"PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:"+os.Getenv("PATH"),
		"DISABLE_PROMPT_CACHING=1",
	)

	output, err := cmd.Output()
	if err != nil {
		log.Printf("평가 실행 실패: %v", err)
		if exitErr, ok := err.(*exec.ExitError); ok {
			log.Printf("stderr: %s", string(exitErr.Stderr))
		}
		systray.SetTitle("🟡 CHAT")
		mEval.SetTitle("📊 일일 평가 (실패)")
		exec.Command("osascript", "-e",
			`display notification "일일 평가 실행에 실패했습니다." with title "대화록 위젯" subtitle "로그를 확인하세요"`).Run()
		return
	}

	// 6. 결과 저장
	outPath := filepath.Join(evalDir, today+"_평가.md")
	if err := os.WriteFile(outPath, output, 0644); err != nil {
		log.Printf("평가 저장 실패: %v", err)
		return
	}

	log.Printf("일일 평가 완료: %s (%d bytes)", filepath.Base(outPath), len(output))
	systray.SetTitle("🟢 CHAT")
	mEval.SetTitle(fmt.Sprintf("📊 평가 완료 (%s)", time.Now().Format("15:04")))

	// 7. macOS 알림
	exec.Command("osascript", "-e",
		fmt.Sprintf(`display notification "일일 평가가 완료되었습니다." with title "대화록 위젯" subtitle "%s"`,
			filepath.Base(outPath))).Run()
}

func buildEvalPrompt(todayFmt string, chatFiles, evalFiles []string, gitChanges string) string {
	var b strings.Builder

	b.WriteString("당신은 사용자의 일일 자기 객관화 분석가입니다.\n\n")
	b.WriteString("## 분석 대상\n\n")
	b.WriteString("아래 파일들을 모두 Read 도구로 읽은 뒤 전방위 분석을 수행하세요.\n\n")

	// 오늘 대화록
	b.WriteString("### 오늘 대화록\n")
	for _, f := range chatFiles {
		b.WriteString(fmt.Sprintf("- %s\n", f))
	}

	// 프로젝트별 설정 파일 — 환경에 맞게 수정
	b.WriteString(fmt.Sprintf("\n### 메모리\n- %s\n", filepath.Join(carcare, "MEMORY.md")))

	// 프로젝트별 설정 파일 — 환경에 맞게 수정
	b.WriteString(fmt.Sprintf("\n### 운영 규칙 (KPI 기준)\n- %s\n", filepath.Join(carcare, "CLAUDE.md")))

	// 최근 평가
	if len(evalFiles) > 0 {
		b.WriteString("\n### 이전 평가 (트렌드 비교용)\n")
		for _, f := range evalFiles {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
	}

	// 최근 수정 파일
	if strings.TrimSpace(gitChanges) != "" {
		b.WriteString("\n### 최근 수정된 파일 (git)\n```\n")
		b.WriteString(gitChanges)
		b.WriteString("```\n")
	}

	// 분석 지시
	b.WriteString("\n## 분석 항목\n\n")
	b.WriteString("### 1. 오늘의 활동 요약\n")
	b.WriteString("- 어떤 작업을 했는지, 주요 성과와 미완료 항목\n\n")
	b.WriteString("### 2. 장점 (오늘 드러난)\n")
	b.WriteString("- 구체적 근거와 함께 3~5개\n\n")
	b.WriteString("### 3. 단점/개선점 (오늘 드러난)\n")
	b.WriteString("- 구체적 근거와 함께 3~5개\n\n")
	b.WriteString("### 4. 패턴 분석\n")
	b.WriteString("- 작업 스타일, 의사결정, 실행력, 멀티태스킹 패턴\n\n")
	b.WriteString("### 5. KPI 진척도\n")
	b.WriteString("- CLAUDE.md에 정의된 KPI 대비 현재 상황 추정\n\n")
	b.WriteString("### 6. 성장 추적\n")
	b.WriteString("- 이전 평가와 비교하여 개선된 점, 반복되는 문제, 새로 발견된 패턴\n")
	b.WriteString("- 이전 평가가 없으면 이 항목은 '첫 평가'로 표기\n\n")
	b.WriteString("### 7. 내일을 위한 제안\n")
	b.WriteString("- 구체적이고 실행 가능한 제안 2~3개\n\n")
	b.WriteString("## 출력 규칙\n\n")
	b.WriteString(fmt.Sprintf("- 제목: `# 일일 평가 %s`\n", todayFmt))
	b.WriteString("- 마크다운 형식\n")
	b.WriteString("- 솔직하고 직접적으로 작성. 빈말/칭찬 금지. 팩트와 근거만.\n")
	b.WriteString("- 각 항목에 대화록의 구체적 발언이나 행동을 인용할 것\n")
	b.WriteString("- 한국어로 작성\n")
	b.WriteString("- **중요: 파일을 저장(Write)하지 마라. 분석 결과를 텍스트로 직접 출력만 해라. 파일 생성/저장 도구 사용 금지.**\n")

	return b.String()
}

// runSummaryUI: 전체 요약 실행
func runSummaryUI() {
	summaryMu.Lock()
	if summaryRunning {
		summaryMu.Unlock()
		return
	}
	summaryRunning = true
	summaryMu.Unlock()
	defer func() {
		summaryMu.Lock()
		summaryRunning = false
		summaryMu.Unlock()
	}()

	log.Println("전체 요약 시작")

	count, _, err := runFullSummary()
	if err != nil {
		log.Printf("요약 실패: %v", err)
		return
	}

	if count == 0 {
		log.Println("미요약 파일 없음")
	} else {
		log.Printf("요약 완료: +%d개", count)
		exec.Command("osascript", "-e",
			fmt.Sprintf(`display notification "+%d개 요약 완료" with title "대화록 위젯"`, count)).Run()
	}
}
