# claude-chatlog

Claude Code 대화 자동 기록 도구 — macOS 메뉴바 위젯

## 기능

- **JSONL 감시**: `~/.claude/projects/` 디렉토리의 Claude Code 세션 파일을 30초 간격으로 감시
- **자동 변환**: JSONL → 사람이 읽을 수 있는 마크다운 형식으로 변환
- **시간대별 정리**: 같은 시간대(1시간) 세션을 하나의 파일로 통합
- **자동 요약**: 이전 시간대 대화록을 Claude CLI로 자동 요약 (벡터 검색용)
- **클린본 생성**: 도구 호출/결과 노이즈 제거한 깔끔한 버전 생성
- **일일 평가**: 매일 저녁 대화 기반 자기 분석 리포트 생성
- **생각 추출**: 대화에서 전략적 생각/인사이트 자동 추출
- **업무일지**: 대화록 기반 자동 업무일지 생성
- **HTTP API**: localhost:7758 에서 상태 조회, 수동 트리거 가능
- **macOS 메뉴바**: systray로 실시간 상태 표시

## 저장 형식

```
❯ 사용자 메시지
⏺ Claude 응답
⏺ Bash(ls -la)
  ⎿ 도구 결과
```

파일명: `YYYYMMDD_대화록_HH시.md`

## 폴더 구조

```
대화록/
├── 20260406_대화록_15시.md    ← 원본 대화 기록
├── 요약/                     ← AI 요약본
├── 클린/                     ← 노이즈 제거본
├── 생각/                     ← 전략적 생각 추출
├── 업무일지/                 ← 자동 업무일지
└── 평가/                     ← 일일 자기 분석
```

## 요구 사항

- macOS
- Go 1.21+
- [Claude CLI](https://docs.anthropic.com/en/docs/claude-code) (`claude` 명령어)

## 설치

### 1. Claude CLI 설치

[Claude Code](https://docs.anthropic.com/en/docs/claude-code) 설치 후 `claude` 명령어가 PATH에 있는지 확인:

```bash
which claude
```

### 2. 빌드

```bash
git clone https://github.com/StudioGMM/claude-chatlog.git
cd claude-chatlog
make build
```

### 3. 설정 (선택)

대화록 저장 디렉토리를 지정하려면 환경변수 `CHATLOG_DIR`을 설정합니다.
미설정 시 기본값은 `~/claude-chatlog` 입니다.

```bash
export CHATLOG_DIR="$HOME/my-chatlog"
```

Claude CLI가 PATH에 없는 경우, `main.go`와 `summary.go`의 `claudeBin`/`claudePath` 상수를 직접 수정하세요.

## 실행

```bash
./claude-chatlog
```

실행하면 macOS 메뉴바에 `🟢 CHAT` 아이콘이 나타납니다.

- `~/.claude/projects/` 디렉토리를 30초 간격으로 감시
- 새 대화가 감지되면 `$CHATLOG_DIR/대화록/` 에 마크다운으로 자동 저장
- 시간대가 바뀌면 이전 시간대의 요약·클린본을 자동 생성

### macOS 자동 시작 (LaunchAgent) — 상시 실행 가이드

부팅 시 자동 실행 + 크래시 시 자동 재시작되도록 LaunchAgent를 등록합니다.

#### Step 1. plist 경로 수정

`com.chatlog.plist` 파일의 바이너리 경로를 본인 환경에 맞게 수정합니다:

```bash
# 빌드된 바이너리의 절대경로 확인
realpath claude-chatlog
```

plist 파일을 열어서 `ProgramArguments`의 경로를 위 결과로 수정:

```xml
<key>ProgramArguments</key>
<array>
    <string>/Users/yourname/claude-chatlog/claude-chatlog</string>
</array>
```

`CHATLOG_DIR` 환경변수를 사용하려면 `EnvironmentVariables`를 추가:

```xml
<key>EnvironmentVariables</key>
<dict>
    <key>CHATLOG_DIR</key>
    <string>/Users/yourname/my-chatlog</string>
</dict>
```

#### Step 2. LaunchAgent 등록

```bash
cp com.chatlog.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.chatlog.plist
```

#### Step 3. 동작 확인

```bash
# 프로세스 확인
launchctl list | grep chatlog

# 메뉴바에 🟢 CHAT 아이콘 확인

# API로 상태 확인
curl http://localhost:7758/status
```

#### LaunchAgent 관리 명령어

```bash
# 중지
launchctl unload ~/Library/LaunchAgents/com.chatlog.plist

# 재시작
launchctl unload ~/Library/LaunchAgents/com.chatlog.plist
launchctl load ~/Library/LaunchAgents/com.chatlog.plist

# 로그 확인
tail -f /tmp/com.chatlog.out
tail -f /tmp/com.chatlog.err
```

### 중복 실행 방지

프로그램 내부에 PID 파일 기반 중복 방지가 구현되어 있습니다:

- 시작 시 `~/.chat.pid` 파일을 확인
- 이미 동일한 프로세스가 실행 중이면 `"이미 실행 중입니다."` 출력 후 종료
- 비정상 종료 시 PID 파일이 남아 있더라도, 해당 PID의 프로세스가 실제로 살아있는지 검증
- 따라서 수동 실행 + LaunchAgent가 동시에 떠도 **하나만 실행**됩니다

## API

| 엔드포인트 | 메서드 | 설명 |
|-----------|--------|------|
| `/status` | GET | 위젯 상태 조회 |
| `/logs` | GET | 오늘 대화록 목록 |
| `/sync` | POST | 수동 동기화 |
| `/summarize` | POST | 전체 요약 실행 |
| `/clean` | POST | 전체 클린본 생성 |
| `/evaluate` | POST | 일일 평가 실행 |
| `/saengak` | POST | 생각 추출 실행 |
| `/ilji` | POST | 업무일지 작성 |

## 라이선스

MIT
