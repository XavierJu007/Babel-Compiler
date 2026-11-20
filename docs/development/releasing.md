# 发布版本

普通用户从 [Releases](https://github.com/XavierJu007/Babel-Compiler/releases/latest) 下载；本页只给维护者使用。

## 自动发布

1. 完成要发布的代码，更新 `VERSION`（例如 `v0.1.1`）。如需修改通用下载说明，编辑 `.github/RELEASE_NOTES.md`。
2. 将这些改动一起提交到 `main`。包含 `VERSION` 变更的提交会自动触发 Release workflow；也可在 Actions → Release → Run workflow 手动重试。
3. workflow 使用当前稳定版 Go，运行 Go/UI 测试，交叉构建六个平台包，核对 SHA256，并用解包后的 Linux 程序运行真实进程集成测试。这些测试使用本机模拟模型，不需要 API Key。
4. 全部通过后，创建指向该提交的版本标签和 Release。所有附件上传完成后才公开。已有标签不会被覆盖；正式版标记为 Latest，带 `-suffix` 的版本标记为 prerelease。

首次启用需要仓库允许 GitHub Actions 运行，发布 job 的 `GITHUB_TOKEN` 需要 `contents: write`。无需添加个人 Token 或模型 API 密钥。

## 本地打包

在干净的、与指定提交一致的源码目录运行：

```bash
go run ./cmd/babel-release -commit "$(git rev-parse HEAD)"
```

需要 Go 1.22+；实际发布建议使用受支持的 Go 版本。输出到新建或空的 `dist/`，也可用 `-out` 指定空目录。工具拒绝覆盖已有输出。每个 ZIP / tar.gz 只包含程序、`LICENSE`、`THIRD_PARTY_NOTICES.md`、双语 `START_HERE.txt`；不包含配置、缓存或私有词典。

程序通过 `-version` 显示版本、源码提交、平台和 Go 版本。升级时停止旧程序并替换可执行文件；默认用户目录中的配置会保留。

## 验证范围

Linux amd64 包在 CI 中实际启动并测试；其它平台交叉编译，但未在原生操作系统运行验收。Windows 使用控制台程序，macOS/Linux 使用终端入口。安装包暂不包含代码签名、公证或自动更新功能。

`SHA256SUMS` 用于核对文件内容，不是发布者身份签名。打包工具固定归档时间戳、移除本机路径；只有同一 Go 工具链、源码和构建环境下才可比较可重复构建结果。
