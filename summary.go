package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const claudePath = "claude" // Claude CLI 경로 — 환경에 맞게 수정 (예: /usr/local/bin/claude)

var (
	summaryDir string
	summaryMu  sync.Mutex
	summaryRunning bool
	lastSummarized string // 마지막으로 요약한 시간대 키 (YYYYMMDD_HH)
)

func initSummary() {
	summaryDir = filepath.Join(logDir, "요약")
	os.MkdirAll(summaryDir, 0755)
}

// trySummarizePreviousHour: 새 시간대 파일이 생기면 이전 시간대를 요약
// writeHourlyFiles()에서 호출
func trySummarizePreviousHour(currentHourKey string) {
	// 이전 시간대 계산
	t, err := time.ParseInLocation("20060102_15", currentHourKey, time.Local)
	if err != nil {
		return
	}
	prevHour := t.Add(-1 * time.Hour)
	prevKey := prevHour.Format("20060102_15")
	prevFileName := fmt.Sprintf("%s_대화록_%s시.md", prevHour.Format("20060102"), prevHour.Format("15"))
	prevPath := filepath.Join(logDir, prevFileName)

	// 이전 시간대 파일이 있는지 확인
	if _, err := os.Stat(prevPath); os.IsNotExist(err) {
		return
	}

	// 이미 요약했으면 스킵
	summaryFileName := fmt.Sprintf("%s_요약_%s시.md", prevHour.Format("20060102"), prevHour.Format("15"))
	summaryPath := filepath.Join(summaryDir, summaryFileName)
	if _, err := os.Stat(summaryPath); err == nil {
		return
	}

	// 이미 같은 시간대 요약 중이면 스킵
	summaryMu.Lock()
	if summaryRunning || lastSummarized == prevKey {
		summaryMu.Unlock()
		return
	}
	summaryRunning = true
	summaryMu.Unlock()

	go func() {
		defer func() {
			summaryMu.Lock()
			summaryRunning = false
			lastSummarized = prevKey
			summaryMu.Unlock()
		}()

		summarizeAndEmbed(prevPath, summaryPath)
	}()
}

// summarizeAndEmbed: 대화록 파일을 요약하고 임베딩
func summarizeAndEmbed(mdPath, summaryPath string) {
	data, err := os.ReadFile(mdPath)
	if err != nil {
		log.Printf("요약 실패 — 파일 읽기: %v", err)
		return
	}

	content := string(data)
	if len(content) < 200 {
		log.Printf("요약 스킵 — 내용 너무 짧음: %s", filepath.Base(mdPath))
		return
	}

	// 도구 결과(⎿) 제거하여 노이즈 줄이기
	lines := strings.Split(content, "\n")
	var filtered []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "⎿") {
			continue
		}
		filtered = append(filtered, line)
	}
	cleanContent := strings.Join(filtered, "\n")

	// 길이 제한 (Claude 컨텍스트)
	if len(cleanContent) > 50000 {
		cleanContent = cleanContent[:50000]
	}

	prompt := fmt.Sprintf(`도구(Bash, Read, Write, Edit, Glob, Grep 등)를 사용하지 마. 텍스트로만 응답해.

아래는 사용자와 Claude의 대화록이다.
이 요약은 나중에 벡터 검색(임베딩)으로 찾을 수 있도록 작성해야 한다.

## 요약 규칙

1. 주제별로 "## 주제명" 헤더로 구분
2. 각 주제 아래:
   - **지시**: 사용자가 시킨 것 (원문 표현 그대로)
   - **결정**: 사용자가 결정/승인한 것
   - **완료**: 실제 수행된 작업 (파일 경로, 명령어, 설정값 포함)
   - **미완료**: 중단되거나 보류된 것
3. 기술 용어, 파일명, 경로, 포트번호, 설정값 등 구체적 정보는 반드시 포함
4. 사용자 승인 맥락도 무엇을 승인한 건지 명시
5. 검색 키워드가 될 만한 단어를 자연스럽게 포함
6. 분량 제한 없음. 빠뜨리지 말고 충실하게 작성

대화록:
%s`, cleanContent)

	log.Printf("요약 시작: %s", filepath.Base(mdPath))

	cmd := exec.Command(claudePath, "-p", prompt, "--output-format", "text", "--max-turns", "1", "--dangerously-skip-permissions")
	output, err := cmd.Output()
	if err != nil {
		log.Printf("요약 실패 — claude: %v", err)
		return
	}

	summary := strings.TrimSpace(string(output))
	if len(summary) < 20 {
		log.Printf("요약 실패 — 결과 너무 짧음")
		return
	}

	// 요약 파일 헤더 추가
	header := fmt.Sprintf("# 요약: %s\n\n원본: %s\n생성: %s\n\n---\n\n",
		filepath.Base(mdPath),
		filepath.Base(mdPath),
		time.Now().Format("2006-01-02 15:04"))

	if err := os.WriteFile(summaryPath, []byte(header+summary), 0644); err != nil {
		log.Printf("요약 파일 저장 실패: %v", err)
		return
	}

	log.Printf("요약 완료: %s → %s", filepath.Base(mdPath), filepath.Base(summaryPath))

}

// runFullSummary: 전체 대화록 스캔 → 미요약 파일만 요약
func runFullSummary() (int, int, error) {
	// 기존 요약 파일 세트
	existingSummaries := make(map[string]bool)
	summaryEntries, _ := os.ReadDir(summaryDir)
	for _, e := range summaryEntries {
		existingSummaries[e.Name()] = true
	}

	// 전체 대화록 스캔
	files, _ := filepath.Glob(filepath.Join(logDir, "*_대화록_*시.md"))
	sort.Strings(files)

	var newFiles []string
	for _, f := range files {
		baseName := filepath.Base(f)
		summaryName := strings.Replace(baseName, "_대화록_", "_요약_", 1)
		if !existingSummaries[summaryName] {
			newFiles = append(newFiles, f)
		}
	}

	if len(newFiles) == 0 {
		return 0, 0, nil
	}

	log.Printf("요약 시작: %d개 파일", len(newFiles))

	successCount := 0
	skipCount := 0

	for i, f := range newFiles {
		baseName := filepath.Base(f)
		summaryName := strings.Replace(baseName, "_대화록_", "_요약_", 1)
		summaryPath := filepath.Join(summaryDir, summaryName)

		summarizeAndEmbed(f, summaryPath)

		// 요약 파일이 생성됐는지 확인
		if _, err := os.Stat(summaryPath); err == nil {
			successCount++
		} else {
			skipCount++
		}

		if (i+1)%10 == 0 {
			log.Printf("요약 진행: %d/%d", i+1, len(newFiles))
		}
	}

	log.Printf("요약 완료: %d개 성공, %d개 스킵", successCount, skipCount)
	return successCount, successCount, nil
}

// ========== 클린본 생성 (노이즈 제거) ==========

var (
	cleanDir     string
	cleanMu      sync.Mutex
	cleanRunning bool
)

func initClean() {
	cleanDir = filepath.Join(logDir, "클린")
	os.MkdirAll(cleanDir, 0755)
}

// cleanChatlog: 대화록 원본에서 노이즈 제거 → 클린본 생성
func cleanChatlog(mdPath string) {
	data, err := os.ReadFile(mdPath)
	if err != nil {
		log.Printf("클린본 실패 — 파일 읽기: %v", err)
		return
	}

	content := string(data)
	if len(content) < 100 {
		return
	}

	lines := strings.Split(content, "\n")
	var cleaned []string
	inToolResult := false // ⎿ 블록 안에 있는지

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// 도구 결과 블록 (⎿ 로 시작하는 들여쓰기 블록)
		if strings.HasPrefix(trimmed, "⎿") {
			inToolResult = true
			continue
		}
		// ⎿ 블록 내부 (들여쓰기된 줄) — 빈 줄이나 새 마커가 나오면 탈출
		if inToolResult {
			if trimmed == "" || strings.HasPrefix(trimmed, "❯") || strings.HasPrefix(trimmed, "⏺") {
				inToolResult = false
			} else if strings.HasPrefix(line, "        ") || strings.HasPrefix(line, "\t\t") {
				continue // 여전히 도구 결과 내부
			} else {
				inToolResult = false
			}
		}

		// 도구 호출 줄 제거
		if strings.HasPrefix(trimmed, "⏺ Read(") ||
			strings.HasPrefix(trimmed, "⏺ Write(") ||
			strings.HasPrefix(trimmed, "⏺ Edit(") ||
			strings.HasPrefix(trimmed, "⏺ Bash(") ||
			strings.HasPrefix(trimmed, "⏺ Glob(") ||
			strings.HasPrefix(trimmed, "⏺ Grep(") ||
			strings.HasPrefix(trimmed, "⏺ Task(") ||
			strings.HasPrefix(trimmed, "⏺ AskUserQuestion(") ||
			strings.HasPrefix(trimmed, "⏺ TodoWrite(") ||
			strings.HasPrefix(trimmed, "⏺ WebFetch(") ||
			strings.HasPrefix(trimmed, "⏺ WebSearch(") {
			continue
		}

		// 요약 요청 프롬프트 블록 제거
		if strings.HasPrefix(trimmed, "❯ 아래는 1시간 동안의 대화록이다") ||
			strings.HasPrefix(trimmed, "❯ 아래 대화록 파일을 읽고") {
			continue
		}
		// 요약 규칙 블록 제거
		if strings.HasPrefix(trimmed, "## 요약 규칙") ||
			strings.HasPrefix(trimmed, "## 대화록") {
			continue
		}
		if len(trimmed) > 0 && trimmed[0] >= '1' && trimmed[0] <= '6' && strings.Contains(trimmed, "주제별로") {
			continue
		}
		if strings.HasPrefix(trimmed, "- 인사,") || strings.HasPrefix(trimmed, "- 실제 작업") ||
			strings.HasPrefix(trimmed, "- 한국어로") || strings.HasPrefix(trimmed, "- 제목과") ||
			strings.HasPrefix(trimmed, "- 불필요한") {
			continue
		}

		// 메타데이터 제거
		if strings.HasPrefix(trimmed, "agentId:") ||
			strings.HasPrefix(trimmed, "<usage>") ||
			strings.HasPrefix(trimmed, "tool_uses:") ||
			strings.HasPrefix(trimmed, "duration_ms:") ||
			strings.HasPrefix(trimmed, "total_tokens:") {
			continue
		}

		// "Continue from where you left off" 제거
		if trimmed == "Continue from where you left off." ||
			trimmed == "❯ Continue from where you left off." {
			continue
		}

		// 출력 형식 프롬프트 제거
		if strings.HasPrefix(trimmed, "출력 형식 (정확히") ||
			strings.HasPrefix(trimmed, "파일:") && strings.Contains(trimmed, "대화록") ||
			strings.HasPrefix(trimmed, "제목: [") ||
			strings.HasPrefix(trimmed, "내용: [") ||
			trimmed == "규칙:" {
			continue
		}

		cleaned = append(cleaned, line)
	}

	// 연속 빈 줄 압축 (3줄 이상 → 2줄)
	var result []string
	blankCount := 0
	for _, line := range cleaned {
		if strings.TrimSpace(line) == "" {
			blankCount++
			if blankCount <= 2 {
				result = append(result, line)
			}
		} else {
			blankCount = 0
			result = append(result, line)
		}
	}

	output := strings.Join(result, "\n")
	output = strings.TrimSpace(output)
	if len(output) < 50 {
		log.Printf("클린본 스킵 — 내용 없음: %s", filepath.Base(mdPath))
		return
	}

	// 클린본 저장
	baseName := filepath.Base(mdPath)
	cleanName := strings.Replace(baseName, "_대화록_", "_클린_", 1)
	cleanPath := filepath.Join(cleanDir, cleanName)

	header := fmt.Sprintf("# 클린본: %s\n\n원본: %s\n생성: %s\n\n---\n\n",
		baseName, baseName, time.Now().Format("2006-01-02 15:04"))

	if err := os.WriteFile(cleanPath, []byte(header+output), 0644); err != nil {
		log.Printf("클린본 저장 실패: %v", err)
		return
	}

	log.Printf("클린본 생성: %s → %s (원본 %d자 → 클린 %d자, %.0f%% 감소)",
		baseName, cleanName, len(content), len(output),
		float64(len(content)-len(output))/float64(len(content))*100)
}

// tryCleanPreviousHour: 새 시간대 파일이 생기면 이전 시간대 클린본 생성
func tryCleanPreviousHour(currentHourKey string) {
	t, err := time.ParseInLocation("20060102_15", currentHourKey, time.Local)
	if err != nil {
		return
	}
	prevHour := t.Add(-1 * time.Hour)
	prevFileName := fmt.Sprintf("%s_대화록_%s시.md", prevHour.Format("20060102"), prevHour.Format("15"))
	prevPath := filepath.Join(logDir, prevFileName)

	if _, err := os.Stat(prevPath); os.IsNotExist(err) {
		return
	}

	cleanName := strings.Replace(prevFileName, "_대화록_", "_클린_", 1)
	cleanPath := filepath.Join(cleanDir, cleanName)
	if _, err := os.Stat(cleanPath); err == nil {
		return // 이미 존재
	}

	go cleanChatlog(prevPath)
}

// runFullClean: 전체 대화록 스캔 → 미생성 클린본만 처리
func runFullClean() {
	cleanMu.Lock()
	if cleanRunning {
		cleanMu.Unlock()
		log.Println("클린본 생성 이미 진행 중")
		return
	}
	cleanRunning = true
	cleanMu.Unlock()

	defer func() {
		cleanMu.Lock()
		cleanRunning = false
		cleanMu.Unlock()
	}()

	entries, err := os.ReadDir(logDir)
	if err != nil {
		log.Printf("클린본 전체 스캔 실패: %v", err)
		return
	}

	// 기존 클린본 세트
	existingClean := make(map[string]bool)
	cleanEntries, _ := os.ReadDir(cleanDir)
	for _, e := range cleanEntries {
		existingClean[e.Name()] = true
	}

	var newFiles []string
	for _, e := range entries {
		name := e.Name()
		if !strings.Contains(name, "_대화록_") || !strings.HasSuffix(name, ".md") {
			continue
		}
		cleanName := strings.Replace(name, "_대화록_", "_클린_", 1)
		if existingClean[cleanName] {
			continue
		}
		newFiles = append(newFiles, filepath.Join(logDir, name))
	}

	if len(newFiles) == 0 {
		log.Println("클린본 생성할 파일 없음")
		return
	}

	log.Printf("클린본 생성 시작: %d개 파일", len(newFiles))
	for i, f := range newFiles {
		cleanChatlog(f)
		if (i+1)%10 == 0 {
			log.Printf("클린본 진행: %d/%d", i+1, len(newFiles))
		}
	}
	log.Printf("클린본 전체 완료: %d개", len(newFiles))
}
