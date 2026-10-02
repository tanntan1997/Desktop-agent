# Mobile Device Agent

A single-binary desktop agent (Windows + macOS) that discovers Android and iOS
devices plugged in over USB and lets a web server drive them remotely:
screenshots, native element hierarchy, tap/swipe/type, app install/launch, and
running test suites (pytest/Appium, Patrol, Gradle, xcodebuild, ...).

```
 ┌──────────── QA desktop (Win / Mac) ────────────┐            ┌──── Web server ────┐
 │                                                 │  outbound  │                    │
 │  USB ── adb ─────────┐                          │  wss://    │  /agent (WS)       │
 │  USB ── go-ios/      ├── device-agent ──────────┼───────────►│  request/response  │
 │         libimobile-  │     │   ▲                │  tunnel    │  + events          │
 │         device + WDA─┘     │   │                │            │                    │
 │                     local REST API              │            └────────────────────┘
 │                     127.0.0.1:7100              │
 │                     (+ /appium proxy, SSE)      │
 └─────────────────────────────────────────────────┘
```

The agent **dials out** to the server, so no inbound firewall/NAT rules are
needed on QA machines. The same method set is also available on a
token-protected localhost API for local tooling.

## Quick start

```bash
make dist                         # dist/device-agent-{darwin,windows}-{amd64,arm64}
./dist/device-agent-darwin-arm64 -init            # writes example config
./dist/device-agent-darwin-arm64 -server wss://hub.example.com/agent -server-token XXX
```

Local only (no server): just run `device-agent`, then

```bash
TOKEN=$(device-agent -show-token)
curl -H "Authorization: Bearer $TOKEN" localhost:7100/api/devices
curl -o s.png "localhost:7100/api/devices/<serial>/screenshot?token=$TOKEN"
curl -H "Authorization: Bearer $TOKEN" localhost:7100/api/devices/<serial>/source
```

Run the web server and an agent together on this machine in one step
(builds both, prints the dashboard link, Ctrl+C stops both; logs in `dist/logs/`):

```bash
make run-local                              # dashboard: http://localhost:8080/?token=local-api-token
PORT=9090 AGENT_PORT=7101 make run-local    # if 8080 / 7100 are taken
```

Or start them by hand with the bundled web server (see [Web server](#web-server-device-hub)):

```bash
go run ./cmd/server -agent-token secret -api-token api &
device-agent -server ws://127.0.0.1:8080/agent -server-token secret
curl -H "Authorization: Bearer api" localhost:8080/api/devices
curl -H "Authorization: Bearer api" -XPOST localhost:8080/api/agents/<agentId>/rpc -d '{"method":"device.source","params":{"serial":"<serial>"}}'
```

## Web server (`device-hub`)

`cmd/server` is the server agents connect to. It keeps a registry of
connected agents and their devices, routes requests down each tunnel (with
cancel on timeout or client disconnect), and exposes everything over a
token-protected REST API, an SSE event stream, and a small dashboard at `/`
(live screen, click to tap, drag to swipe, keys, text, app launch/stop).

```bash
make build-server                 # or: make dist-server (linux/darwin/windows)
./dist/device-hub -agent-token AGENT_SECRET -api-token API_SECRET          # :8080
./dist/device-agent -server ws://hub:8080/agent -server-token AGENT_SECRET
open http://hub:8080/             # paste API_SECRET into the token box
```

Use `-tls-cert/-tls-key` (or a reverse proxy) so agents connect over `wss://`.
Settings come from flags, then `HUB_*` env vars (`HUB_LISTEN`,
`HUB_AGENT_TOKEN`, `HUB_API_TOKEN`, `HUB_TLS_CERT`, `HUB_TLS_KEY`,
`HUB_LOG_LEVEL`), then `-config` ([server.config.example.json](server.config.example.json)).
`agentTokens` maps agent ID → token for per-machine credentials; the shared
`agentToken` is accepted from any agent. If no API token is set, one is
generated and logged at startup. `-insecure` turns all auth off (local only).

API (auth: `Authorization: Bearer <token>` or `?token=`). `{agent}` is the
agent ID from `/api/agents`; every agent call accepts `?timeoutSec=` (default 120).

| Route | |
|---|---|
| `GET /api/agents`, `GET /api/agents/{agent}` | connected agents (hello info + devices) |
| `GET /api/devices` | devices of all agents, each with `agentId`, `agentName` |
| `POST /api/agents/{agent}/rpc` | any method: `{"method","params","timeoutSec"}` → `{"result"}` |
| `GET …/devices/{serial}` | `device.info` |
| `GET …/devices/{serial}/screenshot` | PNG |
| `GET …/devices/{serial}/source` | XML |
| `POST …/devices/{serial}/tap\|swipe\|text\|key` | body = method params |
| `POST …/devices/{serial}/apps/install\|launch\|terminate` | body = method params |
| `GET\|POST /api/agents/{agent}/jobs` | `job.list` / `job.start` |
| `GET …/jobs/{id}`, `POST …/jobs/{id}/cancel`, `GET …/jobs/{id}/logs?since=&limit=` | |
| `GET …/jobs/{id}/artifacts`, `GET …/jobs/{id}/artifacts/{name}` | list / download file |
| `GET /api/events?agent=&event=a,b` | SSE: `agent.connected`, `agent.disconnected` + all agent events, data `{"agentId","event","data"}` |

Errors are `{"error":{"code","message"}}` with the status mapped from the
code (`not_found` 404, `busy` 409, `unavailable` 503, `timeout` 504, …).
State is in memory: agents re-register on reconnect, so a restart only loses
in-flight requests. Run one instance (or pin agents to instances) — calls
must reach the instance holding the agent's socket.

## AI tests (explore once, replay without AI)

Write a test in plain language; an OpenAI model explores it once on a real device and
records the steps; every later run replays those steps deterministically,
with no model calls.

```
Verify Dashboard menu of Exchange is visible
1. Open MBB app
2. Log in with PIN ${PIN}
3. Verify the Exchange menu is visible on the dashboard
```

1. **Set up**: export `OPENAI_API_KEY` before starting `device-hub` (or
   `make run-local`). Open **AI Tests** in the dashboard (`/tests.html`).
   For Azure OpenAI or another OpenAI-compatible endpoint, also set
   `OPENAI_BASE_URL` (or `-ai-base-url`) and pick a vision + tool-calling model
   with `-ai-model`.
2. **Create** a test: prompt, optional app id (package / bundle id) and
   variables (`PIN=123456`). Variable values are typed on the device but are
   **never sent to the model** — it only sees `${PIN}`; values are masked in
   API responses and redacted from on-screen text the model reads.
3. **Explore with AI** on a device. The model (default `gpt-4o-mini`, Chat
   Completions API, `store: false`) sees each screen
   as a screenshot plus the UI hierarchy and calls tools (`launch_app`, `tap`,
   `input_text`, `swipe`, `scroll_to`, `press_key`, `wait`, `assert_visible`,
   `assert_not_visible`, `finish`). The timeline shows its messages and each
   action live. Each successful action is recorded as a step with a **stable
   locator** (resource-id → content-desc → text, plus index only when needed);
   coordinates are used only when no element exists and are stored as screen
   fractions. At the end it keeps only the steps that form a clean script
   (detours dropped).
4. **Verify** (on by default): the kept steps are immediately replayed from a
   fresh app start without the model. Passing marks the test *verified*.
5. **Run recorded steps** any time — from the dashboard, CI, or the API. Each
   step waits up to 10s for its element, failures stop the run with the step,
   reason and a screenshot. Steps are editable as JSON.

API (same auth as the rest of `/api`):

| Route | |
|---|---|
| `GET/POST /api/tests`, `GET/PUT/DELETE /api/tests/{id}` | manage tests (`PUT` without `steps` keeps them; `"******"` keeps a variable) |
| `POST /api/tests/{id}/explore` | `{"agentId","serial","verify":true}` → run (202) |
| `POST /api/tests/{id}/run` | `{"agentId","serial"}` → replay run (202) |
| `GET /api/runs?testId=`, `GET /api/runs/{id}` | run status, step results, timeline, token usage |
| `POST /api/runs/{id}/cancel`, `POST /api/runs/{id}/save-steps` | stop a run / use an exploration's steps |
| `GET /api/runs/{id}/files/{n}.jpg` | screenshot after step *n* |
| SSE `run.updated`, `run.log` on `/api/events` | live progress |

CI example — replay and wait for the verdict:

```bash
RUN=$(curl -s -H "Authorization: Bearer $HUB_TOKEN" -XPOST $HUB/api/tests/$TEST/run \
  -d "{\"agentId\":\"$AGENT\",\"serial\":\"$SERIAL\"}" | jq -r .id)
until S=$(curl -s -H "Authorization: Bearer $HUB_TOKEN" $HUB/api/runs/$RUN | jq -r .status); [ "$S" != running ]; do sleep 3; done
echo "$S"; [ "$S" = passed ]
```

Settings: `-ai-model` (e.g. `gpt-5.5` for long or tricky flows),
`-ai-effort` (reasoning effort; sent only to reasoning models such as gpt-5 /
o-series, default `medium` there), `-ai-base-url`,
`-no-ai`, `-data` (default `./hub-data`: tests, runs, screenshots as
JSON/JPEG files), or `ai.*` / `dataDir` in the config file. One run per device
at a time; runs time out after 30 minutes; an exploration stops after 60 model
turns. Only the latest screen (screenshot + element list) is kept in the
model's context — older screens are replaced by a placeholder — so long
explorations fit gpt-4o-mini's 128K window.

Tips: give the app id so runs start from a fresh launch; phrase expectations
as "verify …" so they become assertions; banking apps that block screenshots
(`FLAG_SECURE`) still work through the UI hierarchy, but Flutter screens need
Semantics labels to be targetable (otherwise steps fall back to coordinates).

## Host prerequisites

| | macOS | Windows |
|---|---|---|
| Android | Android platform-tools (`adb`); auto-detected from PATH, `ANDROID_HOME`, `~/Library/Android/sdk` | platform-tools + OEM USB driver (Samsung/Google); auto-detected from `%LOCALAPPDATA%\Android\Sdk` |
| iOS discovery / install | [go-ios](https://github.com/danielpaulus/go-ios) (`ios`) **or** `brew install libimobiledevice ideviceinstaller` | go-ios + Apple Devices app (or iTunes) for the usbmux driver |
| iOS 17+ | go-ios with `ios.startTunnel: true` | same |
| iOS UI (source/tap/type) | WebDriverAgent signed & installed on the device | same (WDA built once on a Mac, run with `ios runwda`) |
| Appium proxy (optional) | `appium` + drivers | same |

### iOS / WebDriverAgent

Element hierarchy and input on iOS go through WDA. The agent forwards a host
port per device (`8100`, `8101`, ...) to WDA's port on the device. You can
either:

* set `ios.wdaBundleId` (e.g. `com.facebook.WebDriverAgentRunner.xctrunner`)
  and the agent launches WDA via `ios runwda` whenever the device is attached
  (works on Windows too); or
* on macOS set `ios.wdaProjectPath` to `WebDriverAgent.xcodeproj` to launch via
  `xcodebuild test`; or
* start WDA yourself (Xcode / Appium) — the agent just forwards the port.

Screenshots fall back to the `ios screenshot` / `idevicescreenshot` CLI when
WDA is down.

**Coordinates are always screenshot pixels on both platforms.** On iOS the
agent converts to WDA points automatically, so a click on the rendered
screenshot maps 1:1 to `device.tap`. Note iOS page-source bounds are in points.

## Configuration

`config.json` lives in the user config dir (`~/Library/Application
Support/mobile-device-agent/` on macOS, `%AppData%\mobile-device-agent\` on
Windows). See [config.example.json](config.example.json). Env overrides:
`DA_SERVER_URL`, `DA_SERVER_TOKEN`, `DA_LISTEN`, `DA_API_TOKEN`,
`DA_AGENT_NAME`, `DA_ADB_PATH`, `DA_APPIUM_URL`, `DA_LOG_LEVEL`.

| Key | Purpose |
|---|---|
| `serverUrl`, `serverToken` | Tunnel endpoint and bearer token sent on connect |
| `listenAddr`, `apiToken` | Local API (default `127.0.0.1:7100`; token auto-generated into `api-token`) |
| `allowedOrigins` | CORS origins allowed to call the local API from a browser |
| `runners` | **Allow-list** of commands `job.start` may run, with fixed args / workDir / env |
| `allowShell` | Enables `device.shell` (Android raw shell). Off by default |
| `ios.*` | Backend selection, WDA launch, port range, iOS 17 tunnel |
| `appium.*` | Upstream Appium URL; `autoStart` runs and supervises it |

## Tunnel protocol (v1)

JSON text frames over one WebSocket. The agent sends headers
`Authorization: Bearer <serverToken>` and `X-Agent-Id: <stable id>`.

```jsonc
// agent -> server, first frame
{"type":"hello","data":{"protocol":1,"agentId":"e0715d…","name":"qa-mac-01","os":"darwin",
  "methods":["device.screenshot", …],"runners":["pytest"],"devices":[…]}}

// server -> agent
{"type":"request","id":"42","method":"device.tap","params":{"serial":"R5CW…","x":540,"y":1200}}
{"type":"cancel","id":"42"}                         // abort an in-flight request

// agent -> server
{"type":"response","id":"42","result":{"ok":true}}
{"type":"response","id":"43","error":{"code":"unavailable","message":"device … is unauthorized"}}
{"type":"event","event":"devices.changed","data":[…]}
```

The agent pings every 20s and reconnects with jittered backoff (1s → 30s).
Requests run concurrently (bounded by `maxConcurrentRequests`).

Error codes: `bad_request`, `not_found`, `not_supported`, `unavailable`,
`busy`, `forbidden`, `timeout`, `canceled`, `internal`.

### Methods

| Method | Params | Result |
|---|---|---|
| `agent.info` | – | hello, uptime, tool paths, discovery errors |
| `devices.list` | `refresh?` | `[DeviceInfo]` |
| `device.info` | `serial` | `DeviceInfo` |
| `device.screenshot` | `serial` | `{mime, width, height, data(base64 PNG)}` |
| `device.source` | `serial` | `{format:"xml", platform, source}` — uiautomator XML / WDA XML |
| `device.tap` | `serial, x, y` | `{ok}` |
| `device.swipe` | `serial, x1, y1, x2, y2, durationMs?` | `{ok}` |
| `device.text` | `serial, text` | `{ok}` (Android: printable ASCII; use Appium for unicode) |
| `device.key` | `serial, key` | `home, back, enter, delete, volume_up, volume_down, power, menu, app_switch` or Android keycode |
| `device.shell` | `serial, command` | `{output}` — requires `allowShell` |
| `app.install` | `serial, path` or `url` | `{ok}` — `.apk` / `.ipa`; `url` is downloaded by the agent |
| `app.launch` / `app.terminate` | `serial, appId` | package, `pkg/.Activity`, or bundle id |
| `wda.request` | `serial, method, path, body?` | `{status, body}` raw WDA passthrough (iOS) |
| `appium.request` | `method, path, body?` | `{status, body}` raw WebDriver passthrough to local Appium |
| `job.start` | `runner, serial?, args?, env?, timeoutSec?` | `JobSnapshot` |
| `job.get` / `job.cancel` | `id` | `JobSnapshot` |
| `job.list` | – | `[JobSnapshot]` |
| `job.logs` | `id, since?, limit?` | `[{seq, stream, text, time}]` |
| `job.artifacts` / `job.artifact` | `id` / `id, name` | file list / `{name, data(base64)}` |

Events: `device.added`, `device.updated`, `device.removed`,
`devices.changed` (full list), `job.updated`, `job.log`.

### Running tests

Jobs run an allow-listed runner with these env vars injected so the suite
targets the right phone:

`DEVICE_SERIAL`, `DEVICE_UDID`, `DEVICE_PLATFORM`, `DEVICE_NAME`,
`DEVICE_OS`, `APPIUM_URL`, `JOB_ID`, `ARTIFACTS_DIR`, plus `ANDROID_SERIAL`
(adb, Patrol and Flutter honour it) or `IOS_UDID` / `WDA_URL`.

Only one job holds a device at a time (`busy` otherwise). Cancelling kills the
whole process tree (process group on macOS, `taskkill /T` on Windows). Write
reports/screenshots into `$ARTIFACTS_DIR`; `output.log` is always captured.

```json
{"method":"job.start","params":{"runner":"pytest","serial":"R5CWC3QFR0B",
  "args":["tests/test_login.py","--junitxml=$ARTIFACTS_DIR/junit.xml"],"timeoutSec":1800}}
```

> Args are passed verbatim (no shell), so `$ARTIFACTS_DIR` above is not
> expanded; read it from the environment inside the test, or use a runner like
> `{"command":"sh","args":["-c"]}` if you deliberately want shell expansion.

## Security model

* The tunnel server is trusted to control the attached devices. Use `wss://`
  and per-agent tokens.
* Arbitrary host commands are **not** exposed: `job.start` only runs
  configured runners; `device.shell` is opt-in; app IDs are validated before
  reaching the device shell; artifact reads are confined to the job dir.
* The local API binds to loopback and requires the API token
  (`Bearer`, `?token=`, or Basic auth password for Appium clients:
  `http://agent:<token>@127.0.0.1:7100/appium`).

## Known limitations

* Android `device.source` uses `uiautomator dump`, which fails while an Appium
  UiAutomator2 session holds the accessibility service — use
  `appium.request` (`GET /session/{id}/source`) during Appium sessions.
  Flutter apps expose widgets only via Semantics labels.
* No live video stream yet; poll `device.screenshot` (≈1–3 fps over adb).
  Adding scrcpy (Android) / WDA MJPEG (iOS, port 9100) is the natural next step.
* Running as a background service: use `launchd` (macOS) or NSSM / Task
  Scheduler (Windows). Don't run as a Windows *service* account if you need
  the user's adb keys and Apple pairing records — run at user logon instead.

## Layout

```
cmd/agent            entrypoint, flags
cmd/server           web server (device-hub): tunnel endpoint, REST, SSE, dashboard
cmd/hub-example      minimal reference server (superseded by cmd/server)
internal/hub         agent registry, request/response routing, event fan-out
internal/autotest    AI tests: OpenAI explorer, step recorder/replayer, UI tree locators, store
internal/server      device-hub HTTP API + embedded dashboard (web/)
internal/agent       wiring + RPC method table
internal/device      Driver interface, polling Manager
  android/           adb driver
  ios/               go-ios / libimobiledevice + WDA driver
internal/wda         WebDriverAgent client
internal/tunnel      outbound WebSocket client (reconnect, ping, cancel)
internal/httpapi     local REST, /api/rpc, SSE, /appium proxy
internal/jobs        allow-listed test runner, logs, artifacts
internal/appium      WebDriver passthrough
internal/proc        helper process supervisor (iproxy, runwda, appium)
internal/execx       exec helpers (no console windows, process-tree kill)
```
