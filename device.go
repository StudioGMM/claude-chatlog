package main

import (
	"os"
	"path/filepath"
	"strings"
)

// detectProjectDirs: ~/.claude/projects/ 하위의 모든 프로젝트 디렉토리 반환
// JSONL 파일이 여러 디렉토리에 분산되어 있으므로 전부 스캔한다.
func detectProjectDirs(home string) []string {
	projectsBase := filepath.Join(home, ".claude", "projects")
	entries, err := os.ReadDir(projectsBase)
	if err != nil {
		return nil
	}

	var dirs []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(projectsBase, e.Name())
		// JSONL 파일이 하나라도 있는 디렉토리만 포함
		jsonls, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		if len(jsonls) > 0 {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

func isStudio() bool {
	return true
}
