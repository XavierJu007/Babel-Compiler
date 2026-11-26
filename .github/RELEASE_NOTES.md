## v0.2.0 — 极速本地压缩、流式可中断工作台、macOS 桌面安装包

### 更新亮点 / Highlights

- **极速本地快路径 (<1ms) & 智能路由**：短文本（`<180` 字或 `<80` Token）与纯 JSON / 代码自动走零 LLM 本地确定性无损压缩或原样直通，消除等待与短文本膨胀；透明代理冷启动同样即时生效并同步后台异步预热。
- **超长文档并行分块 + 流式 UI**：`>1200` 字文档自动多块并行编译，Web 控制台全面切换为 `/api/compile/stream` SSE 流式渲染，支持实时计时与**随时点击「停止」中止编译**。
- **体验全面打磨**：首屏精简直达双栏编辑器；输入即预判路由与估算 Token；多场景样本预设、本地编译历史、一键「强制深度编译」、代理日志过滤与一键 `Dry-Run` 链路自检。
- **全新图标与 macOS `.dmg` 桌面安装**：为 Windows `.exe`（内嵌 `.ico`）、macOS `Babel.app`（`.icns`）及 Web 控制台适配 BABEL 深色几何图标；macOS 产物统一使用自签名标识 **`6wyndoIin`**。

---

## 下载 / Download

Windows、macOS 和 Linux 均提供 `amd64` / `arm64` 程序包。页面、图标、海报和 Token 计数器均已内嵌，无需安装 Go 或 Node；模型 API 需自行配置。

| 系统 / System | 安装包 / Package | 说明 / Notes |
| :--- | :--- | :--- |
| **macOS Apple Silicon (桌面安装)** | `babel_v0.2.0_macos_arm64.dmg` | 拖入 Applications 双击打开（自签名 `6wyndoIin`） |
| **macOS Intel (桌面安装)** | `babel_v0.2.0_macos_amd64.dmg` | 拖入 Applications 双击打开（自签名 `6wyndoIin`） |
| **macOS Apple Silicon / Intel (CLI)** | `macos_arm64.tar.gz` / `macos_amd64.tar.gz` | 终端运行 `./babel`（自签名 `6wyndoIin`） |
| **Windows x64 / ARM64** | `windows_amd64.zip` / `windows_arm64.zip` | 解压后双击带图标的 `babel.exe` 运行 |
| **Linux x86-64 / ARM64** | `linux_amd64.tar.gz` / `linux_arm64.tar.gz` | 终端运行 `./babel` |

### macOS 首次启动提示说明
macOS 版本的 `.app` 与 CLI 二进制已使用 ad-hoc 签名（identifier：`6wyndoIin`）。因未经 Apple 公证（Notarization），从浏览器下载后首次打开若系统拦截「无法验证开发者」，可在终端执行一次：
```bash
xattr -cr /Applications/Babel.app
```
或点击「系统设置 → 隐私与安全性 → 仍要打开」。

校验下载完整性请对照 `SHA256SUMS`。GitHub 自动生成的 **Source code** 仅包含构建源码（已剔除文档）。
