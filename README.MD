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

Try the full tunnel path with the bundled reference server:

```bash
go run ./cmd/hub-example -listen :8080 -token secret &
device-agent -server ws://127.0.0.1:8080/agent -server-token secret
curl localhost:8080/agents
curl -XPOST localhost:8080/agents/<agentId>/rpc -d '{"method":"device.source","params":{"serial":"<serial>"}}'
```

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
cmd/hub-example      reference web-server side of the tunnel
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
