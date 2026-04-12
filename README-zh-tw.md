# openab-go

繁體中文 | [English](README.md)

一個輕量、安全、雲原生的 **ACP（Agent Client Protocol）橋接器**，連接 **Discord** 和 **Telegram** 與任何 ACP 相容的 coding CLI — [Kiro CLI](https://kiro.dev)、[Claude Code](https://docs.anthropic.com/en/docs/claude-code)、[Codex](https://github.com/openai/codex)、[Gemini CLI](https://github.com/google-gemini/gemini-cli) 等。

這是 [openab](https://github.com/openabdev/openab)（原本以 Rust 撰寫）的 **Go 重寫版本**。

---

##### 功能特色

- **可插拔的 Agent 後端** — Kiro、Claude Code、Codex、Gemini（任何 ACP 相容 CLI）
- **Discord 整合** — @mention 觸發、自動建立討論串、多輪對話
- **Telegram 整合** — 群組中 @mention / 回覆 bot、私聊、語音訊息自動接受
- **語音訊息轉錄** — 透過 OpenAI Whisper API 轉錄語音訊息（Discord & Telegram）
- **即時編輯串流** — Agent 工作時即時更新訊息（Discord: 1.5s、Telegram: 2s）
- **Emoji 狀態反應** — 透過平台原生 reaction 顯示處理進度
- **Session Pool** — 每個討論串/聊天一個 CLI 程序，自動生命週期管理
- **Session 管理** — Bot 指令（`sessions`/`reset`/`info`）、LRU 驅逐、HTTP API 監控
- **ACP 協定** — 基於 stdio 的 JSON-RPC
- **Kubernetes 就緒** — 包含 Dockerfile 供容器化部署

---

##### 可插拔的 Agent 後端

支援 Kiro CLI、Claude Code、Codex、Gemini，以及任何 ACP 相容的 CLI。

| Agent key | CLI | ACP Adapter | 認證方式 |
|---|---|---|---|
| `kiro`（預設） | Kiro CLI | 原生 `kiro-cli acp` | `kiro-cli login --use-device-flow` |
| `codex` | Codex | [@zed-industries/codex-acp](https://github.com/zed-industries/codex-acp) | `codex login --device-auth` |
| `claude` | Claude Code | [@agentclientprotocol/claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp) | `claude auth login` 或 `claude setup-token` |
| `gemini` | Gemini CLI | 原生 `gemini --acp` | Google OAuth 或 `GEMINI_API_KEY` |

---

##### 平台支援

| 平台 | 文字 | 圖片 | 語音 | 狀態 |
|------|------|------|------|------|
| Discord  | ✅ | ✅ | ✅ | 可用 |
| Telegram | ✅ | ✅ | ✅ | 可用 |
| Teams    | — | — | — | 規劃中 |

---

##### 快速開始

```bash
# 複製專案
git clone https://github.com/neilkuan/openab-go.git
cd openab-go

# 複製並編輯設定
cp config.toml.example config.toml
# 編輯 config.toml，填入你的 Discord bot token 和 channel ID

# 執行
go run . config.toml
```

##### 設定

設定使用 TOML 格式，支援環境變數展開（`${VAR_NAME}` 語法）：

```toml
[discord]
bot_token = "${DISCORD_BOT_TOKEN}"
allowed_channels = ["1234567890"]

[telegram]
bot_token = "${TELEGRAM_BOT_TOKEN}"
allowed_chats = [-100123456789]

[agent]
command = "kiro-cli"
args = ["acp", "--trust-all-tools"]
working_dir = "/home/agent"

[pool]
max_sessions = 10
session_ttl_hours = 24

[discord.reactions]
enabled = true
remove_after_reply = false
```

##### 語音轉錄（選用）

要啟用語音訊息支援，在設定中加入 `[transcribe]` 區段和 OpenAI API key：

```toml
[transcribe]
api_key = "${OPENAI_API_KEY}"
# provider = "openai"       # 預設
# model = "whisper-1"       # 預設
# language = "zh"           # ISO-639-1 語言代碼，預設 "zh"
# prompt = "以下是繁體中文語音的逐字稿："  # 繁體中文輸出提示
```

設定後，語音訊息（Discord & Telegram）會自動轉錄為文字送給 Agent。未設定時，純語音訊息會回傳警告給使用者。

完整設定參考請見 [`config.toml.example`](config.toml.example)，包含其他 Agent（Claude、Codex、Gemini）的設定範例。

---

##### Session 管理

內建 Bot 指令和 HTTP API 來管理 Agent session。

###### Bot 指令

在群組中以 @mention 方式傳送這些指令給 bot：

| 指令 | 說明 |
|------|------|
| `sessions` | 列出所有活躍的 session 及統計資訊 |
| `info` | 顯示當前討論串/聊天的 session 詳情 |
| `reset` | 終止當前 session（下一則訊息會建立新的） |

###### HTTP API（選用）

在設定中啟用：

```toml
[api]
enabled = true
listen = ":8080"
```

| 端點 | 方法 | 說明 |
|------|------|------|
| `/api/health` | GET | 健康檢查，含 pool 統計 |
| `/api/sessions` | GET | 以 JSON 列出所有 session |
| `/api/sessions/{key}` | DELETE | 終止指定的 session |

###### Pool 行為

- **LRU 驅逐** — pool 滿時自動淘汰最久沒使用的 session
- **TTL 清理** — 閒置超過 `session_ttl_hours`（預設 24 小時）的 session 會被清理
- **逐 session 統計** — 建立時間、最後活動時間、訊息數

---

##### Discord vs Telegram

| | Discord | Telegram |
|---|---|---|
| **觸發（頻道/群組）** | @mention 或在討論串中 | @mention、回覆 bot 或語音訊息 |
| **觸發（私訊）** | — | 所有訊息 |
| **討論串模型** | 自動建立 Discord 討論串 | 每個聊天一個 session（forum/topic 尚未支援） |
| **訊息上限** | 2,000 字元 | 4,096 字元 |
| **編輯串流間隔** | 1.5 秒 | 2 秒（Telegram 速率限制較嚴格） |
| **Markdown** | 原生 GFM 支援 | `**粗體**` 自動轉換為 `*粗體*`（Telegram Markdown v1） |
| **狀態 reaction** | 逐個 emoji 新增/移除 | `setMessageReaction` 一次替換全部（一次一個 emoji） |
| **Reaction emoji** | 排隊 `👀` → 思考 `🤔` → 完成 `🆗` + 隨機表情 | 排隊 `👌` → 思考 `🤔` → 完成 = 隨機允許表情 |
| **群組語音** | 需要 @mention | 自動接受（錄音時無法 @mention） |
| **圖片處理** | 從 CDN 下載 URL | 透過 Bot API `getFile` 下載（最大 PhotoSize） |
| **Bot 函式庫** | [discordgo](https://github.com/bwmarrin/discordgo) | [telegram-bot-api/v5](https://github.com/go-telegram-bot-api/telegram-bot-api) |
| **更新機制** | WebSocket gateway | Long polling |

##### Telegram 設定注意事項

1. 透過 [@BotFather](https://t.me/BotFather) 建立 bot 並取得 bot token
2. **停用隱私模式**：透過 BotFather（`/setprivacy` → Disable）讓 bot 能在群組中收到 @mention，然後將 bot 移除並重新加入群組
3. 取得群組 chat ID：先不設定 `allowed_chats` 啟動 bot，在群組中傳訊息 — log 會顯示 `🚨👽🚨 telegram message from unlisted chat ... chat_id=XXXXX`
4. 將 `chat_id` 加入設定中的 `allowed_chats`

---

##### Docker

每次 release 會發布四種 image 變體：

| Image | Agent |
|---|---|
| `ghcr.io/neilkuan/openab-go` | Kiro CLI |
| `ghcr.io/neilkuan/openab-go-claude` | Claude Code |
| `ghcr.io/neilkuan/openab-go-codex` | Codex |
| `ghcr.io/neilkuan/openab-go-gemini` | Gemini CLI |

```bash
docker run -v $(pwd)/config.toml:/etc/openab-go/config.toml \
  ghcr.io/neilkuan/openab-go:latest
```

---

##### 開發

###### 前置需求

- Go 1.23+
- Discord bot token（需啟用 `MESSAGE_CONTENT` intent）和/或 Telegram bot token
- 已安裝 ACP 相容 CLI（如 `kiro-cli`、`claude`、`codex`、`gemini`）

###### 編譯

```bash
go build -o openab-go .

# 帶版本資訊
go build -ldflags "-X main.version=$(cat VERSION)" -o openab-go .
```

###### 以 debug logging 執行

```bash
OPENAB_GO_LOG=debug ./openab-go config.toml
```

###### 專案結構

```
openab-go/
├── main.go              # 進入點：設定、平台註冊、graceful shutdown
├── platform/
│   └── platform.go      # Platform 介面、共用訊息分割
├── config/
│   └── config.go        # TOML 設定解析、環境變數展開、驗證
├── acp/
│   ├── protocol.go      # JSON-RPC 類型、ACP 事件分類
│   ├── connection.go    # 子程序管理、stdio JSON-RPC、自動授權
│   └── pool.go          # Session pool：get-or-create、LRU 驅逐、閒置清理
├── command/
│   └── command.go       # Bot 指令解析與執行（sessions/reset/info）
├── api/
│   └── server.go        # HTTP API server，用於 session 監控
├── transcribe/
│   └── transcribe.go    # Transcriber 介面、OpenAI Whisper 實作
├── discord/
│   ├── adapter.go       # Discord 平台 adapter（實作 Platform 介面）
│   ├── handler.go       # Discord 訊息處理、討論串建立、編輯串流
│   └── reactions.go     # 狀態 reaction 控制器：防彈跳、停滯偵測
├── telegram/
│   ├── adapter.go       # Telegram 平台 adapter（實作 Platform 介面）
│   ├── handler.go       # Telegram 訊息處理、mention/reply 偵測、編輯串流
│   └── reactions.go     # Telegram reaction 控制器（setMessageReaction API）
├── scripts/
│   └── release.sh       # Release 自動化（stable PR + RC tag）
├── Dockerfile           # Kiro CLI 變體
├── Dockerfile.claude    # Claude Code 變體
├── Dockerfile.codex     # Codex 變體
├── Dockerfile.gemini    # Gemini CLI 變體
├── config.toml.example  # 設定參考
├── VERSION              # Semver 版本
└── RELEASING.md         # Release 流程文件
```

###### 關鍵設計決策

| 面向 | 選擇 | 原因 |
|------|------|------|
| 語言 | Go | 編譯快速、單一靜態二進位檔、goroutine 並行 |
| Discord 函式庫 | [discordgo](https://github.com/bwmarrin/discordgo) | Go 生態系的標準 Discord 函式庫 |
| Telegram 函式庫 | [telegram-bot-api/v5](https://github.com/go-telegram-bot-api/telegram-bot-api) | 最多人使用的 Go Telegram bot 函式庫 |
| 設定格式 | TOML | 人類可讀，與原始 Rust 版本相同 |
| Logging | `log/slog`（標準函式庫） | 零依賴、結構化 logging |
| 並行處理 | goroutines + `sync.Mutex` / `sync.RWMutex` | 慣用 Go 風格，不需外部 async runtime |

---

##### Release 流程

Release 遵循 **「測試過的就是要發布的」** 哲學，使用 `scripts/release.sh`：

1. **合併 PR 到 main** → `release.yml` 自動開啟 Release PR（`release/v0.4.1`，只更新 `VERSION`）
2. **建立 RC tag** → checkout release 分支 → `./scripts/release.sh --rc` → 完整建置 4 個 image 變體 x 2 平台
3. **合併 Release PR** → `tag-on-merge.yml` 自動打 stable tag → promote pre-release image（不重新建置）

詳細流程請見 [RELEASING.md](RELEASING.md)。

---

##### 授權條款

[MIT](LICENSE)
