# Quill

[![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-ghcr.io-blue?logo=docker)](https://github.com/neilkuan/quill/pkgs/container/quill)

繁體中文 | [English](README.md)

一個輕量、安全、雲原生的 **ACP（Agent Client Protocol）橋接器**，連接 **Discord**、**Telegram** 和 **Microsoft Teams** 與任何 ACP 相容的 coding CLI — [Kiro CLI](https://kiro.dev)、[Claude Code](https://docs.anthropic.com/en/docs/claude-code)、[Codex](https://github.com/openai/codex)、[GitHub Copilot CLI](https://github.com/github/copilot-cli)、[Gemini CLI](https://github.com/google-gemini/gemini-cli) 等。

這是 [openab](https://github.com/openabdev/openab)（原本以 Rust 撰寫）的 **Go 重寫版本**。

---

##### 功能特色

- **可插拔的 Agent 後端** — Kiro、Claude Code、Codex、GitHub Copilot、Gemini CLI（任何 ACP 相容 CLI）
- **Discord 整合** — @mention 觸發、自動建立討論串、多輪對話
- **Telegram 整合** — 群組中 @mention / 回覆 bot、私聊、語音訊息自動接受、forum topic 支援（每個 topic 一個 session）
- **Microsoft Teams 整合** — 頻道中 @mention 觸發、Bot Framework webhook、串流編輯回覆、圖片/語音/檔案附件
- **語音訊息轉錄** — 透過 OpenAI Whisper API 轉錄語音訊息（Discord、Telegram & Teams）
- **即時編輯串流** — Agent 工作時即時更新訊息（Discord: 1.5s、Telegram: 2s）
- **Emoji 狀態反應** — 透過平台原生 reaction 顯示處理進度
- **中途打斷回覆** — `/stop` 指令或 Discord 點擊 🛑 reaction 即可中斷；session 保留、上下文不會丟失（ACP `session/cancel` + watchdog 保底）
- **Session Pool** — 每個討論串/聊天一個 CLI 程序，自動生命週期管理
- **Session 管理** — Bot 指令（`sessions`/`reset`/`info`/`resume`/`stop`）、LRU 驅逐、HTTP API 監控
- **排程提示（Scheduled Prompts）** — `/cron` 讓使用者排程 cron／interval／一次性 prompt，到時間自動 fire 到聊天視窗，並在 `<sender_context>` 帶上 `trigger:"cron"` 標記，agent 一眼就能分辨是排程觸發還是人類訊息
- **ACP 協定** — 基於 stdio 的 JSON-RPC
- **Kubernetes 就緒** — 包含 Dockerfile 供容器化部署

---

##### 可插拔的 Agent 後端

支援 Kiro CLI、Claude Code、Codex、GitHub Copilot CLI、Gemini CLI，以及任何 ACP 相容的 CLI。

| Agent key | CLI | ACP Adapter | 認證方式 |
|---|---|---|---|
| `kiro`（預設） | Kiro CLI | 原生 `kiro-cli acp` | `kiro-cli login --use-device-flow` |
| `codex` | Codex | [@zed-industries/codex-acp](https://github.com/zed-industries/codex-acp) | `codex login --device-auth` |
| `claude` | Claude Code | [@agentclientprotocol/claude-agent-acp](https://github.com/agentclientprotocol/claude-agent-acp) | `claude auth login` 或 `claude setup-token` |
| `copilot` ⚠️ | GitHub Copilot CLI | 原生 `copilot --acp --stdio` | `gh auth login -p https -w` |
| `gemini` ⚠️ | Gemini CLI | 原生 `gemini --experimental-acp` | `gemini` 首次執行 / `GEMINI_API_KEY` |

> ⚠️ **copilot**：需付費 GitHub Copilot 訂閱。ACP 支援目前為 public preview — 行為可能會變動。
>
> ⚠️ **gemini**：ACP 支援透過 `--experimental-acp` flag 暴露，行為可能變動。

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
# allowed_user_id = ["*"]                                          # 萬用字元：任何使用者
# allowed_user_id = ["921f866a-c005-4e5f-b520-38e6dbe29a5f"]       # Entra ID (AAD) Object ID — 建議使用
# allowed_user_id = ["29:1abcd..."]                                # 舊版 Bot Framework channel ID — 仍可接受

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

###### Teams 怎麼取得 user ID

Teams 支援兩種 ID 格式，擇一即可：

- **Entra ID (AAD) Object ID** *（建議）* — Teams profile 卡片上的 **Object ID** GUID（點使用者頭像 → **Copy object ID**），例如 `921f866a-c005-4e5f-b520-38e6dbe29a5f`。跨租戶穩定且人類可讀。
- **Bot Framework channel ID** *（舊版、向下相容）* — Bot Framework 在 inbound activity 上掛的 `29:xxxxxx` 字串。用 `QUILL_LOG=debug` 啟動 bot，傳訊息給它，看 `teams message received` 這行 log 的 `user_id=...`；同一行的 `aad_object_id=...` 即上方的 GUID。

兩種格式比對的是同一個 `allowed_user_id` 陣列，現有用 `29:xxx` 的設定不需要改動就會繼續運作。

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
| `/pick` | 瀏覽並載入歷史 agent session。無參數或 `all` 時 Discord 會回 select menu、Telegram 會回 inline keyboard，點選即可 resume；`/pick <N>` 載入前一次列表的第 N 筆；`/pick load <id>` 依 session ID 直接載入（文字路徑仍保留給熟手）；`/pick all` 跳過 cwd 過濾（適用於 Codex 等無 cwd 欄位的 agent）。`history`、`session-picker`、`session_picker`、`sessionpicker` 為相容舊寫法的同義指令。 |
| `/mode` | 列出或切換 session 的 agent mode（ACP `session/set_mode`）。無參數時 Discord 會回 select menu、Telegram 會回 inline keyboard，可點選切換；`/mode <id>` 或 `/mode <N>` 直接切。需要當前 thread 已有活著的 session（先傳訊息觸發），且 agent 在 session setup 時回報 `modes` 物件。 |
| `/model` | 列出或切換 session 使用的 LLM model（ACP `session/set_model`）。互動 UX 與 `/mode` 相同，Discord／Telegram 互動、Teams 純文字。需要 agent 在 session setup 時回報 `models` 物件。 |
| `/cron` | 排程訊息自動 fire 到當前聊天。子指令：`/cron add <schedule> <prompt>`、`/cron list`、`/cron rm <id>`。排程格式支援標準 5-field cron（`0 9 * * *`）、`every 5m`、`at 09:00`、`at 2026-05-05 09:00`、`in 30m`。詳見 [排程提示（`/cron`）](#排程提示cron) 一節。可在 `[cronjob] disabled = true` 全域關閉。 |

每則 agent 回覆末尾會附上一行小字 footer，顯示當下 session 使用的 mode 與 model，例如 `— mode: `卡卡西` · model: `claude-sonnet-4.6``，讓使用者不必 `/info` 就知道這則回覆是哪個 persona、哪個後端模型產出的。agent 沒回報 modes／models 時 footer 會省略。

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

###### Session 歷史紀錄（`/pick`）

`/pick` 指令讓使用者能直接在聊天平台瀏覽並恢復 agent 的歷史 session。Picker 直接讀取 agent 寫在本機的 session 檔，agent 程序不需要在線：

| Agent | Session 儲存位置 | cwd 過濾 |
|---|---|---|
| Kiro CLI | `~/.kiro/sessions/cli/<uuid>.{json,jsonl}` | ✅ |
| Claude Code | `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl` | ✅ |
| GitHub Copilot CLI | `~/.copilot/session-state/<uuid>/`（`workspace.yaml` + `events.jsonl`） | ✅（best-effort 從 `workspace.yaml` 讀） |
| Codex | `~/.codex/history.jsonl`（扁平索引） | ❌ — Codex history 的每一筆沒有 cwd 欄位。傳入非空 cwd 時 `List` 回空 slice，不會回傳未過濾的結果 |
| Gemini CLI | `~/.gemini/tmp/<project-tmp-id>/chats/session-*.jsonl` | ✅ — 比對每筆 session 內嵌的 `projectHash`（`sha256(cwd)`），以及任何透過 `/dir add` 加入的 cwd |

顯示 Codex session 時，picker UI 會提示「該 agent 不支援 cwd 過濾」，讓使用者知道需要改用空 cwd 才能看到結果。

---

##### 排程提示（`/cron`）

讓使用者排程 prompt 自動 fire 到聊天視窗。每次觸發都走跟一般使用者訊息一樣的 agent 路徑，但會在既有的 `quill.sender.v1` `<sender_context>` 信封裡多帶 `trigger:"cron"`（加上 `cron_id`、`cron_schedule`、`cron_fire_time`），agent 因此能分辨這次是排程觸發還是人類訊息。

###### 排程格式

| 形式 | 範例 | 行為 |
|---|---|---|
| 標準 5-field cron | `0 9 * * *` | 重複觸發；最小粒度 1 分鐘 |
| 固定間隔 | `every 5m`、`every 2h` | 重複觸發；低於 `min_interval_seconds`（預設 60 秒）會被拒絕 |
| 相對一次性 | `in 30m`、`in 2h` | 觸發一次後從 store 自動刪除 |
| 今天／明天 HH:MM | `at 09:00` | 在設定時區下、下次到達 HH:MM 時觸發一次 |
| 絕對時間 | `at 2026-05-05 09:00` | 在設定時區下的指定時刻觸發一次 |

###### 範例

```
/cron add 0 9 * * * 每日 standup 摘要
/cron add every 30m 看一下 staging 最新部署狀態
/cron add in 2h 提醒我去 deploy
/cron add at 09:00 拉昨天的 CI 失敗摘要
/cron list
/cron rm a3f5b201
```

###### 各平台 UX

- **Telegram** — `/cron list` 顯示純文字，每筆下方一個 🗑️ InlineKeyboard 按鈕，點按即刪除。
- **Discord** — 原生 slash command，子指令 `add` / `list` / `rm`；`list` 回純文字（cron ID、schedule、prompt）。
- **Teams** — 純文字 `/cron` 指令，子指令同上。

###### 設定

```toml
[cronjob]
# disabled = false                  # 設為 true 可完全停用 /cron
# max_per_thread = 20               # 每個 thread 上限
# min_interval_seconds = 60         # 拒絕「every 30s」；one-shot 不受此限
# queue_size = 50                   # 每 thread 觸發緩衝；溢出會 drop 並回 chat marker
# timezone = "UTC"                  # 顯示與解析「at HH:MM」用的時區
# store_path = "./.quill/cronjobs.json"
```

###### V1 已知限制

- **純 FIFO queue** — 使用者跟 agent 講久一點，期間累積的 interval 觸發會在使用者結束後一口氣灌進來。V2 可能加 coalesce。
- **沒有 pause／resume** — V1 只支援 `add` / `list` / `rm`；`Job` 結構裡的 `Disabled` 欄位保留給 V2 用。
- **沒有執行歷史** — 觸發紀錄只能從 `slog` 看；可查詢的歷史是 V2 工作。
- **Best-effort 投遞** — bot 在預定時間離線的話，那次觸發**會丟失**（重啟後不會補打 missed fire）。
- **單一 instance 假設** — 兩個 Quill 程序共用同一個 `cronjobs.json` 會雙觸發。每個 store 只跑一個 instance。
- **Teams `serviceURL` 快取** — Teams 的 Bot Framework 每次發送都需要 `serviceURL`，Quill 把每個 conversation 最近一次的 `serviceURL` 持久化到 `./.quill/teams-serviceurls.json`（可用 `teams.service_url_store_path` 改路徑；設成空字串則退回純記憶體模式），process／pod 重啟後 cron 立刻可以 post，不用使用者重新發訊息。在 Kubernetes 上這個檔案落在 `agent.workingDir`，所以 Helm chart 開 `backup.enabled=true` 時會跟 `cronjobs.json` 一起 sync 到 S3。

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

##### Discord 設定注意事項

1. 在 [Discord Developer Portal](https://discord.com/developers/applications) 建立 application 與 bot，複製 **bot token**
2. 在 bot 頁面啟用下方必要的 **Privileged Gateway Intents**（見下表）
3. 透過 OAuth2 URL Generator 產生邀請連結，勾選下方列出的 scopes 與 permissions，再把 bot 邀請進你的 server
4. 取得 channel/thread ID：開啟開發者模式（`使用者設定 → 進階 → 開發者模式`），對 channel 右鍵 → **複製頻道 ID**，加到設定中的 `allowed_channels`

###### 必要的 Gateway Intents

設定於 `discord/adapter.go`：

| Intent | 特權 intent？ | 用途 |
|---|---|---|
| `GUILDS` | 否 | Guild／channel 生命週期事件 |
| `GUILD_MESSAGES` | 否 | 接收 guild channel 與 thread 中的訊息 |
| `MESSAGE_CONTENT` | ✅ **是** | 讀取訊息內容（解析 prompt 與 @mention 必需） |
| `GUILD_MESSAGE_REACTIONS` | 否 | 接收 reaction-add 事件（點擊 🛑 取消的流程） |

> ⚠️ `MESSAGE_CONTENT` 是 **privileged intent** — 必須在 Developer Portal 中手動開啟（**Bot → Privileged Gateway Intents → Message Content Intent**）。Bot 加入 100+ servers 時還需要 Discord 審核通過。
>
> 已部署的環境如果從不含 `GUILD_MESSAGE_REACTIONS` 的版本升級，可能需要**重新邀請 bot**，新的 intent 才會生效。

![](./docs/discord-bot-page.png)

###### 必要的 OAuth2 Scopes

在 **OAuth2 → URL Generator** 頁面同時勾選兩個 scope：

| Scope | 用途 |
|---|---|
| `bot` | 標準 bot 安裝 scope |
| `applications.commands` | 註冊 Slash Commands（`/sessions`、`/info`、`/reset`、`/resume`、`/stop`、`/pick`、`/mode`、`/model`），透過 `ApplicationCommandCreate` |

###### 必要的 Bot Permissions

在同一頁面的 **Bot Permissions** 區塊勾選以下權限（權限整數：`397284474944`，可視需求調整）：

| 權限 | 用途 |
|---|---|
| View Channels | 在允許的 channel 中接收訊息 |
| Send Messages | 發送回覆與 `💭 thinking...` 佔位訊息 |
| Send Messages in Threads | 在自動建立的 thread 中串流回覆 |
| Create Public Threads | `MessageThreadStartComplex` — 在非 thread 的 channel 中為每則 prompt 自動建立 thread |
| Manage Threads | 重新命名／封存 bot 建立的 thread |
| Embed Links | 正確渲染連結與 session footer |
| Attach Files | `ChannelFileSend`，用於 TTS 語音回覆（`voice_reply.mp3`） |
| Read Message History | 編輯串流回覆、解析 reaction 時查詢原訊息 |
| Add Reactions | `MessageReactionAdd` — 狀態 emoji（queued／thinking／tool／done）與 🛑 點擊取消 |
| Use Slash Commands | 在 server 中執行已註冊的 application commands |

![](./docs/discord-oauth2-page.png)

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

每次 release 會發布五種 image 變體：

| Image | Agent |
|---|---|
| `ghcr.io/neilkuan/quill` | Kiro CLI |
| `ghcr.io/neilkuan/quill-claude` | Claude Code |
| `ghcr.io/neilkuan/quill-codex` | Codex |
| `ghcr.io/neilkuan/quill-copilot` | GitHub Copilot CLI |
| `ghcr.io/neilkuan/quill-gemini` | Gemini CLI |

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
