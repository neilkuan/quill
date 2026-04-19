# Quill

[![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-ghcr.io-blue?logo=docker)](https://github.com/neilkuan/quill/pkgs/container/quill)

繁體中文 | [English](README.md)

一個輕量、安全、雲原生的 **ACP（Agent Client Protocol）橋接器**，連接 **Discord**、**Telegram** 和 **Microsoft Teams** 與任何 ACP 相容的 coding CLI — [Kiro CLI](https://kiro.dev)、[Claude Code](https://docs.anthropic.com/en/docs/claude-code)、[Codex](https://github.com/openai/codex)、[GitHub Copilot CLI](https://github.com/github/copilot-cli) 等。

這是 [openab](https://github.com/openabdev/openab)（原本以 Rust 撰寫）的 **Go 重寫版本**。

---

##### 功能特色

- **可插拔的 Agent 後端** — Kiro、Claude Code、Codex、GitHub Copilot（任何 ACP 相容 CLI）
- **Discord 整合** — @mention 觸發、自動建立討論串、多輪對話
- **Telegram 整合** — 群組中 @mention / 回覆 bot、私聊、語音訊息自動接受、forum topic 支援（每個 topic 一個 session）
- **Microsoft Teams 整合** — 頻道中 @mention 觸發、Bot Framework webhook、串流編輯回覆、圖片/語音/檔案附件
- **語音訊息轉錄** — 透過 OpenAI Whisper API 轉錄語音訊息（Discord、Telegram & Teams）
- **即時編輯串流** — Agent 工作時即時更新訊息（Discord: 1.5s、Telegram: 2s）
- **Emoji 狀態反應** — 透過平台原生 reaction 顯示處理進度
- **中途打斷回覆** — `/stop` 指令或 Discord 點擊 🛑 reaction 即可中斷；session 保留、上下文不會丟失（ACP `session/cancel` + watchdog 保底）
- **Session Pool** — 每個討論串/聊天一個 CLI 程序，自動生命週期管理
- **Session 管理** — Bot 指令（`sessions`/`reset`/`info`/`resume`/`stop`）、LRU 驅逐、HTTP API 監控
- **ACP 協定** — 基於 stdio 的 JSON-RPC
- **Kubernetes 就緒** — 包含 Dockerfile 供容器化部署

---

##### 可插拔的 Agent 後端

支援 Kiro CLI、Claude Code、Codex、GitHub Copilot CLI，以及任何 ACP 相容的 CLI。

| Agent key | CLI | ACP Adapter | 認證方式 |
|---|---|---|---|
| `kiro`（預設） | Kiro CLI | 原生 `kiro-cli acp` | `kiro-cli login --use-device-flow` |
| `codex` | Codex | [@zed-industries/codex-acp](https://github.com/zed-industries/codex-acp) | `codex login --device-auth` |
| `claude` | Claude Code | [@agentclientprotocol/claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp) | `claude auth login` 或 `claude setup-token` |
| `copilot` ⚠️ | GitHub Copilot CLI | 原生 `copilot --acp --stdio` | `gh auth login -p https -w` |

> ⚠️ **copilot**：需付費 GitHub Copilot 訂閱。ACP 支援目前為 public preview — 行為可能會變動。

---

##### 平台支援

| 平台 | 文字 | 圖片 | 語音 | 狀態 |
|------|------|------|------|------|
| Discord  | ✅ | ✅ | ✅ | 可用 |
| Telegram | ✅ | ✅ | ✅ | 可用 |
| ⚠️ Teams    | ✅ | ✅ | ✅ (STT) | **實驗 / Beta** |

> ⚠️ **實驗性 / Beta：** **Microsoft Teams adapter** 與 **Helm chart**（`deploy/helm/quill`）仍在 beta 階段。介面、config key、chart values 可能在未來版本直接調整，恕不另行公告。生產環境請自行評估風險後使用。

---

##### 架構

> 圖中標籤全部使用 ASCII，避免 CJK 字寬度不一致造成對齊跑位；各項中文說明列於圖下方。

```
  _________________                ___________________________                 ______________
 |                 |   message    |                           |   JSON-RPC    |              |
 |    Discord      |------------->|     Platform Adapter      |<------------->|  ACP Agent   |
 |    Telegram     |<-------------|   (handler | reactions)   |     stdio     |  (subprocess)|
 |   MS Teams      |   reply      |             |             |               |              |
 |_________________|              |             v             |               |   kiro-cli   |
                                  |   command.ParseCommand    |               |   claude-acp |
                                  |  sessions | info | reset  |               |   codex-acp  |
                                  |  resume   | stop (cancel) |               |   copilot    |
                                  |             |             |               |______________|
                                  |             v             |
                                  |       SessionPool         |
                                  |   LRU | TTL | per-thread  |
                                  |             |             |
                                  |             v             |
                                  |      AcpConnection        |
                                  |   prompt | cancel | wd    |
                                  |___________________________|
                                        |                |
                              optional  |                |  optional
                                        v                v
                                  _____________     _____________
                                 |             |   |             |
                                 |   STT/TTS   |   |  HTTP API   |
                                 |  (Whisper | |   |  (sessions  |
                                 |   OpenAI |  |   |   health)   |
                                 |   Gemini)   |   |_____________|
                                 |_____________|
```

圖中術語對照：

| 英文 | 中文 |
|------|------|
| `message` / `reply` | 訊息 / 回覆 |
| `Platform Adapter` | 平台轉接層（Discord、Telegram、Teams） |
| `command.ParseCommand` | 指令解析（slash / 純文字） |
| `SessionPool` | Session 池（每個 thread 一個連線、LRU 淘汰、TTL 清理） |
| `AcpConnection` | ACP 連線（`prompt` 送 prompt、`cancel` 送 session/cancel、`wd` 是 watchdog） |
| `ACP Agent (subprocess)` | ACP Agent 子程序（kiro / claude-agent-acp / codex-acp / copilot） |
| `STT/TTS` | 語音轉文字 / 文字轉語音（選配） |
| `HTTP API` | HTTP 監控 API（選配） |

**資料流**：使用者訊息 → platform adapter → command parser（slash / 純文字）→ SessionPool（每個 thread key 一個 AcpConnection）→ JSON-RPC over stdio → agent CLI 子程序。串流回覆循同一路徑逆向返回，每 1.5-2 秒寫回原本的 bot 訊息。

**Cancel 路徑**：使用者 `/stop` 或 🛑 reaction → `AcpConnection.SessionCancel()` 於獨立 goroutine 送 `session/cancel` notification（不鎖 `promptMu`）。Agent 回 `stopReason="cancelled"`；若 agent 沒實作 cancel，10 秒 watchdog 會自行合成同樣的 response，串流永遠不會卡死。

---

##### 快速開始

```bash
# 複製專案
git clone https://github.com/neilkuan/quill.git
cd quill

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
# allowed_user_id = ["*"]                    # 萬用字元：任何使用者
# allowed_user_id = ["823367235137044491"]   # 或指定 Discord user ID

[telegram]
bot_token = "${TELEGRAM_BOT_TOKEN}"
allowed_chats = [-100123456789]
# allowed_user_id = ["*"]             # 萬用字元：任何使用者
# allowed_user_id = ["123456789"]     # 或指定 Telegram user ID（字串形式）

[teams]
app_id = "${TEAMS_APP_ID}"
app_secret = "${TEAMS_APP_SECRET}"
tenant_id = "${TEAMS_TENANT_ID}"
listen = ":3978"
# allowed_user_id = ["*"]             # 萬用字元：任何使用者
# allowed_user_id = ["29:user-id"]    # 或指定 Teams user ID

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

##### 使用者白名單（`allowed_user_id`）

在平台區段設定 `allowed_user_id` 後，其**優先權高於** `allowed_channels`（Discord / Teams）／`allowed_chats`（Telegram）：只有清單中的使用者可以觸發 bot，**不限 channel / chat**。未設定時維持原本的 channel/chat gate 行為。`["*"]` 是萬用字元，代表任何使用者都可以。

比對的是**數字型 user ID**，不是 username — username 會變，ID 不會。

###### Discord 怎麼取得 user ID

- **從 App：** 開啟開發者模式（`使用者設定 → 進階 → 開發者模式`），然後對使用者頭像右鍵 → **複製使用者 ID**。
- **從 log：** 用 `QUILL_LOG=debug` 啟動 bot，傳一則訊息給它，看 `discord message received` 這行 log 的 `author_id=...`。

###### Telegram 怎麼取得 user ID

- **從 Telegram：** 跟 [@userinfobot](https://t.me/userinfobot) 講話，它會回你的數字 ID。
- **從 log：** 用 `QUILL_LOG=debug` 啟動 bot，傳一則訊息，看 `telegram update` 這行 log 的 `user_id=...`。

Telegram 的 ID 在 TOML 中要加引號（`["123456789"]`，不是 `[123456789]`），這樣 `"*"` 才能和數字 ID 並存於同一陣列中。

##### STT — 語音轉文字（選用）

要啟用語音訊息支援，在設定中加入 `[stt]` 區段和 OpenAI API key：

```toml
[stt]
api_key = "${OPENAI_API_KEY}"
# provider = "openai"       # 預設
# model = "whisper-1"       # 預設
# language = "zh"           # ISO-639-1 語言代碼，預設 "zh"
# prompt = "以下是繁體中文語音的逐字稿："  # 繁體中文輸出提示
```

設定後，語音訊息（Discord & Telegram）會自動轉錄為文字送給 Agent。未設定時，純語音訊息會回傳警告給使用者。

##### TTS — 文字轉語音（選用）

要啟用語音回覆功能，在設定中加入 `[tts]` 區段和 OpenAI API key：

```toml
[tts]
api_key = "${OPENAI_API_KEY}"
# model = "tts-1"           # 或 "tts-1-hd"、"gpt-4o-mini-tts"
# voice = "nova"            # alloy, ash, ballad, coral, echo, fable, nova, onyx, sage, shimmer, verse, marin, cedar
# voice_gender = "female"   # "female"（預設，nova）或 "male"（ash）— 未設定 voice 時使用
```

設定後，當使用者傳語音訊息時，bot 會同時回覆文字和語音訊息。使用 OpenAI TTS API。

##### 語音功能計價（OpenAI）

| 服務 | 模型 | 價格 |
|------|------|------|
| **STT** | `whisper-1` | $0.006 / 分鐘 |
| **STT** | `gpt-4o-mini-transcribe` | $0.003 / 分鐘 |
| **STT** | `gpt-4o-transcribe` | $0.006 / 分鐘 |
| **TTS** | `tts-1` | $15.00 / 百萬字元 |
| **TTS** | `tts-1-hd` | $30.00 / 百萬字元 |
| **TTS** | `gpt-4o-mini-tts` | $0.015 / 分鐘 |

一般 chatbot 語音回覆（約 300 字元）使用 `tts-1` 約花費 **$0.0045**。價格為 2026 年資料，最新請見 [OpenAI pricing](https://openai.com/api/pricing/)。

完整設定參考請見 [`config.toml.example`](config.toml.example)，包含其他 Agent（Claude、Codex）的設定範例。

---

##### Session 管理

內建 Bot 指令和 HTTP API 來管理 Agent session。

###### Bot 指令

指令已註冊為平台原生指令 — Discord Slash Commands 和 Telegram BotCommands — 輸入 `/` 即可在自動完成選單中看到。也支援純文字方式（如 `@bot sessions`）作為 fallback。

| 指令 | 說明 |
|------|------|
| `/sessions` | 列出所有活躍的 session 及統計資訊 |
| `/info` | 顯示當前討論串/聊天的 session 詳情 |
| `/reset` | 終止當前 session（下一則訊息會建立新的） |
| `/resume` | 嘗試還原當前討論串的前一個 session |
| `/stop` | 中斷 agent 當前的回覆（session 保留）。`cancel` 為同義指令。Discord 上點擊串流訊息的 🛑 reaction 效果相同。 |
| `/session-picker` | 瀏覽並載入歷史 agent session。無參數時列出當前 cwd 範圍內最近的 session。`/session-picker <N>` 載入前一次列表的第 N 筆；`/session-picker load <id>` 依 session ID 直接載入；`/session-picker all` 跳過 cwd 過濾（適用於 Codex 等無 cwd 欄位的 agent）。`history` 和 `pick` 為同義指令。 |

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

###### Session 歷史紀錄（`/session-picker`）

`/session-picker` 指令讓使用者能直接在聊天平台瀏覽並恢復 agent 的歷史 session。Picker 直接讀取 agent 寫在本機的 session 檔，agent 程序不需要在線：

| Agent | Session 儲存位置 | cwd 過濾 |
|---|---|---|
| Kiro CLI | `~/.kiro/sessions/cli/<uuid>.{json,jsonl}` | ✅ |
| Claude Code | `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl` | ✅ |
| GitHub Copilot CLI | `~/.copilot/session-state/<uuid>/`（`workspace.yaml` + `events.jsonl`） | ✅（best-effort 從 `workspace.yaml` 讀） |
| Codex | `~/.codex/history.jsonl`（扁平索引） | ❌ — Codex history 的每一筆沒有 cwd 欄位。傳入非空 cwd 時 `List` 回空 slice，不會回傳未過濾的結果 |

顯示 Codex session 時，picker UI 會提示「該 agent 不支援 cwd 過濾」，讓使用者知道需要改用空 cwd 才能看到結果。

---

##### 平台比較

| | Discord | Telegram | Teams |
|---|---|---|---|
| **觸發（頻道/群組）** | @mention 或在討論串中 | @mention、回覆 bot 或語音訊息 | @mention |
| **觸發（私訊）** | — | 所有訊息 | 所有訊息 |
| **討論串模型** | 自動建立 Discord 討論串 | 每個聊天一個 session；forum 超級群組每個 topic 一個 session | 每個對話一個 session |
| **訊息上限** | 2,000 字元 | 4,096 字元 | 28,000 字元 |
| **編輯串流間隔** | 1.5 秒 | 2 秒（Telegram 速率限制較嚴格） | 2 秒 |
| **Markdown** | 原生 GFM 支援 | `**粗體**` 自動轉換為 `*粗體*`（Telegram Markdown v1） | 原生 GFM 支援 |
| **狀態 reaction** | 逐個 emoji 新增/移除 | `setMessageReaction` 一次替換全部（一次一個 emoji） | Typing indicator |
| **群組語音** | 需要 @mention | 自動接受（錄音時無法 @mention） | 音訊附件 STT 轉錄 |
| **圖片處理** | 從 CDN 下載 URL | 透過 Bot API `getFile` 下載（最大 PhotoSize） | 從 `contentUrl` 下載（bearer token 驗證） |
| **Bot 函式庫** | [discordgo](https://github.com/bwmarrin/discordgo) | [go-telegram/bot](https://github.com/go-telegram/bot) | 自製（Bot Framework REST API） |
| **更新機制** | WebSocket gateway | Long polling | HTTP webhook（`POST /api/messages`） |

##### Telegram 設定注意事項

1. 透過 [@BotFather](https://t.me/BotFather) 建立 bot 並取得 bot token
2. **停用隱私模式**：透過 BotFather（`/setprivacy` → Disable）讓 bot 能在群組中收到 @mention，然後將 bot 移除並重新加入群組
3. 取得群組 chat ID：先不設定 `allowed_chats` 啟動 bot，在群組中傳訊息 — log 會顯示 `🚨👽🚨 telegram message from unlisted chat ... chat_id=XXXXX`
4. 將 `chat_id` 加入設定中的 `allowed_chats`

##### Teams 設定注意事項

> ⚠️ **Beta：** Teams 支援仍在實驗階段。Inbound JWT 驗證、附件處理、Helm chart ingress 路由仍可能調整。遇到問題請回報 <https://github.com/neilkuan/quill/issues>。

1. 在 [Azure Portal](https://portal.azure.com) 建立 Azure Bot 資源 — 記下 **App ID**、**App Secret** 和 **Tenant ID**
2. 設定 messaging endpoint 為 `https://<your-domain>/api/messages`（Quill 預設監聽 `:3978`）
3. 將 app manifest 上傳到 [Teams Developer Portal](https://dev.teams.microsoft.com/apps) — 打包說明請見 `teams/appmanifest/README.md`
4. 透過 Developer Portal 建立的 bot **預設為 single-tenant** — Quill 的驗證流程使用 tenant-specific token URL

---

##### Docker

每次 release 會發布四種 image 變體：

| Image | Agent |
|---|---|
| `ghcr.io/neilkuan/quill` | Kiro CLI |
| `ghcr.io/neilkuan/quill-claude` | Claude Code |
| `ghcr.io/neilkuan/quill-codex` | Codex |
| `ghcr.io/neilkuan/quill-copilot` | GitHub Copilot CLI |

```bash
docker run -v $(pwd)/config.toml:/etc/quill/config.toml \
  ghcr.io/neilkuan/quill:latest
```

##### Kubernetes（Helm）

> ⚠️ **Beta：** Helm chart 仍在實驗階段，主要在 EKS + AWS Load Balancer Controller 環境下驗證。Values 與 templates 在版本之間可能會改動。

包含 Helm chart 供 EKS 部署（Teams webhook 需要公開 HTTPS endpoint）：

```bash
helm install quill deploy/helm/quill \
  -n quill --create-namespace \
  --set instances.kiro.secrets.TEAMS_APP_ID="<app-id>" \
  --set instances.kiro.secrets.TEAMS_APP_SECRET="<secret>" \
  --set instances.kiro.secrets.TEAMS_TENANT_ID="<tenant>" \
  --set ingress.host="quill.example.com" \
  --set 'ingress.annotations.alb\.ingress\.kubernetes\.io/certificate-arn=arn:aws:acm:...'
```

Chart 支援多 instance 部署 — 在同一個 release 裡同時跑 Kiro、Claude、Codex。詳見 [`deploy/helm/quill/README.md`](deploy/helm/quill/README.md)。

---

##### 開發

###### 前置需求

- Go 1.23+
- Discord bot token（需啟用 `MESSAGE_CONTENT` intent）、Telegram bot token、和/或 Teams Azure Bot 註冊（App ID + App Secret）
- 已安裝 ACP 相容 CLI（如 `kiro-cli`、`claude`、`codex`）

###### 編譯

```bash
go build -o quill .

# 帶版本資訊
go build -ldflags "-X main.version=$(cat VERSION)" -o quill .
```

###### 以 debug logging 執行

```bash
QUILL_LOG=debug ./quill config.toml
```

###### 專案結構

```
quill/
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
│   └── command.go       # Bot 指令解析與執行（sessions/reset/info/resume/stop）
├── api/
│   └── server.go        # HTTP API server，用於 session 監控
├── stt/
│   └── stt.go           # Transcriber 介面、OpenAI Whisper 實作
├── tts/
│   └── openai.go        # Synthesizer 介面、OpenAI TTS 實作
├── discord/
│   ├── adapter.go       # Discord 平台 adapter（實作 Platform 介面）
│   ├── handler.go       # Discord 訊息處理、討論串建立、編輯串流
│   └── reactions.go     # 狀態 reaction 控制器：防彈跳、停滯偵測
├── telegram/
│   ├── adapter.go       # Telegram 平台 adapter（實作 Platform 介面）
│   ├── handler.go       # Telegram 訊息處理、mention/reply 偵測、編輯串流
│   └── reactions.go     # Telegram reaction 控制器（setMessageReaction API）
├── teams/
│   ├── adapter.go       # Teams 平台 adapter（HTTP webhook 伺服器）
│   ├── auth.go          # Azure AD OAuth2 + JWT 驗證
│   ├── client.go        # Bot Framework REST API 用戶端
│   ├── handler.go       # Teams 訊息處理、mention 偵測、ACP 串流
│   ├── types.go         # Bot Framework Activity 類型
│   └── appmanifest/     # Teams App Manifest、圖示、打包指南
├── deploy/
│   └── helm/quill/      # Helm chart 供 EKS 部署（多 instance）
├── scripts/
│   └── release.sh       # Release 自動化（stable PR + RC tag）
├── Dockerfile           # Kiro CLI 變體
├── Dockerfile.claude    # Claude Code 變體
├── Dockerfile.codex     # Codex 變體
├── Dockerfile.copilot   # GitHub Copilot CLI 變體
├── config.toml.example  # 設定參考
├── VERSION              # Semver 版本
└── RELEASING.md         # Release 流程文件
```

###### 關鍵設計決策

| 面向 | 選擇 | 原因 |
|------|------|------|
| 語言 | Go | 編譯快速、單一靜態二進位檔、goroutine 並行 |
| Discord 函式庫 | [discordgo](https://github.com/bwmarrin/discordgo) | Go 生態系的標準 Discord 函式庫 |
| Telegram 函式庫 | [go-telegram/bot](https://github.com/go-telegram/bot) | 積極維護、原生支援 forum topic |
| 設定格式 | TOML | 人類可讀，與原始 Rust 版本相同 |
| Logging | `log/slog`（標準函式庫） | 零依賴、結構化 logging |
| 並行處理 | goroutines + `sync.Mutex` / `sync.RWMutex` | 慣用 Go 風格，不需外部 async runtime |

---

##### Release 流程

Release 遵循 **「測試過的就是要發布的」** 哲學，使用 `scripts/release.sh`：

1. **合併 PR 到 main** → `release.yml` 自動開啟 Release PR（`release/v0.4.1`，只更新 `VERSION`）
2. **建立 RC tag** → checkout release 分支 → `./scripts/release.sh --rc` → 完整建置 5 個 image 變體 x 2 平台
3. **合併 Release PR** → `tag-on-merge.yml` 自動打 stable tag → promote pre-release image（不重新建置）

詳細流程請見 [RELEASING.md](RELEASING.md)。

---

# 授權條款

[MIT](LICENSE)
