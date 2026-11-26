## v0.2.1 — 高密度浅树压缩、纯英文原生编译、章节并行与门禁精确度提升

### 更新亮点 / Highlights

- **更高净压缩比与浅树排版**：去除冗余逐行框线前缀（`│ ├`）与装饰性编号，采用顶层 `├`/`╰` + 2 空格缩进并加强公共前提合并与单位简写（`h`/`min`/`s`/`d`/`req/s`/`<=`/`>=`），在中文规章、运维手册、合同审阅等场景下显著降低 BPE Token 消耗。
- **纯英文输入原生编译 & Token 密度分块**：纯英文输入自动启用全英文零汉字电报式编译提示词与英文续写引导，避免译为中文导致 Token 膨胀；按英文与中文 Token 密度区分自动并行切块阈值（英文 `5000` / `2800` 字符，中文 `2000` / `900` 字符）。
- **章节边界并行提速**：`>900` 字（中文）多章节文本在空白行及 Markdown `#` / 中文顶层标题处自动并行编译，显著缩短中长文本等待时间；并行失败时优先透传真实上游根因错误。
- **语义门禁与字面量校验精确度提升**：
  - 修复 `20req/s` 等速率单位的数值字面量误报、`userId/orderId/tenantId` 与中文紧邻路径（如 `php://filter读源码`）的技术字面量边界误报，以及 `第一条`/`Section 1` 独立标题误锁全文列表编号的问题。
  - 将语义审阅与盲审的 `max_tokens` 提升至 `2048`，避免长文本审计截断返回 `502`；放行等价巴别语简写并禁止在 `missing[]` 中写入非缺陷说明。

---

## 下载 / Download

Windows、macOS 和 Linux 均提供 `amd64` / `arm64` 程序包。页面、图标、海报和 Token 计数器均已内嵌，无需安装 Go 或 Node；模型 API 需自行配置。

| 系统 / System | 安装包 / Package | 说明 / Notes |
| :--- | :--- | :--- |
| **macOS Apple Silicon (桌面安装)** | `babel_v0.2.1_macos_arm64.dmg` | 拖入 Applications 双击打开（自签名 `6wyndoIin`） |
| **macOS Intel (桌面安装)** | `babel_v0.2.1_macos_amd64.dmg` | 拖入 Applications 双击打开（自签名 `6wyndoIin`） |
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
