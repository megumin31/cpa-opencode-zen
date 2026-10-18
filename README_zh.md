<div align="right">
  <a href="README.md">English</a> | <strong>简体中文</strong>
</div>

# OpenCode CLIProxyAPI (CPA) 插件

专为 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) 设计的高性能 C-shared 动态库插件，将 OpenCode LLM 生态系统（包含免费模型与 Console 工作区模型）无缝接入标准 OpenAI 与 Anthropic 兼容端点。

---

## 特性

- **公开免费模型发现**：支持 public 模式，仅注册目录明确标为零费用且协议、目标地址受支持的 OpenCode 模型。
- **自动化网关适配**：
  - 自动合成符合 OpenCode 规范的 EID 会话标识符（`ses_<12位hex><14位base62>`）。
  - 透明注入并隔离网关必须的指纹验证工具，对下游客户端自定义工具调用（Tool Calls）零干扰并完整透传。
  - 原生流式自适应，确保流式（streaming）与非流式（non-streaming）下游客户端均能稳定接收响应。
- **全协议跨端兼容**：全面兼容 OpenAI Chat Completions（`/v1/chat/completions`）与 OpenAI Responses API（`/v1/responses`），自动完成协议规范化与推理控制字段映射。
- **模型能力元数据**：接入目录明确声明的推理选项、上下文与输出限制、输入输出模态；缺失值不猜测。详见[能力元数据、宿主接口可见范围及请求边界](docs/MODEL-CAPABILITIES.md)。
- **Console 工作区与 Service Key**：支持配置 OpenCode Console 服务密钥，使用会话持久化和 Prompt 缓存调用自定义付费模型。

---

## 可用免费模型

本插件筛选公开目录中的免费候选模型，在 CPA 中通过 `opencode-zen/<model-id>` 别名进行调用：

模型目录随上游变化；请以 CPA 的 /v1/models 实际列表为准。2026-10-17 的一次独立脚本 public 请求返回 HTTP 403 FreeTierError，并提示仅允许官方客户端。该脚本未使用本插件完整的请求构造、会话头及传输链路，不能据此判断实际插件请求是否成功；插件线上端到端推理仍未验证。目录标为零费用并不保证匿名调用获准；上游仍可限流、停用模型或要求账户认证。

---

## 安装与部署

### 1. 下载预编译动态库

推送版本标签即可自动构建并发布 [Releases](https://github.com/megumin31/cpa-opencode-zen/releases) 均在原生验证门禁通过后自动打包：
- `opencode-<tag>-linux-amd64.tar.gz`（适用于 x86_64）
- `opencode-<tag>-linux-arm64.tar.gz`（适用于 aarch64）

下载对应架构的压缩包，校验 SHA-256 后解压出 `opencode-v0.1.0.so`（注意：**请勿重命名该 .so 文件**，保持版本号命名不变），详见 [发布说明与重试机制文档](docs/RELEASE_zh.md)。

此外，每次代码提交均会通过 GitHub Actions 自动构建最新的 Linux 二进制：
1. 访问项目的 [Actions](https://github.com/megumin31/cpa-opencode-zen/actions) 页面；
2. 选择最近一次成功的 **Build Linux plugins** 工作流；
3. 在 **Artifacts** 下下载 `opencode-linux-amd64-<commit>` 或 `opencode-linux-arm64-<commit>`；
4. 将解压得到的 `opencode-v0.1.0.so` 放置于 CPA 的 `plugins/` 目录中：
   ```bash
   cp opencode-v0.1.0.so /path/to/cliproxyapi/plugins/
   chmod 700 /path/to/cliproxyapi/plugins/opencode-v0.1.0.so
   ```

### 2. 配置 CPA

在 CPA 的配置文件 `config.yaml` 中添加插件声明：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    opencode:
      enabled: true
      request_timeout_seconds: 0
      allowed_destinations:
        - https://opencode.ai/console
        - https://opencode.ai/inference
```

### 3. 配置 OpenCode 认证凭据

在 CPA 的 `auth-dir` 目录（默认路径为 `~/.cli-proxy-api/`）下创建认证文件 `opencode.json`：

```json
{
  "type": "opencode",
  "key": "public",
  "server": "https://opencode.ai/console",
  "label": "OpenCode Service"
}
```

> **说明**：
> - 若仅使用公开免费模型，`"key"` 填写精确字符串 `"public"`。此模式跳过需要账户认证的 Console 配置接口，仅读取公开目录；推理请求仍发送 public 认证值。输入与输出价格必须显式为 0，其他列出的费用也必须为 0；缺失或无法确认的价格不会被当成免费。
> - 真实账户 key 仍使用 Console 发现；401/403 会清除该认证的缓存路由并返回失败，不会退回 public。两类认证缓存彼此隔离。直连和配置 Worker 的请求遵守相同规则。
> - 若拥有 OpenCode Console Service Key（`oc_sk_...`），将其填入 `"key"` 字段即可无缝解锁工作区付费与自定义模型。

### 4. 重启并验证 CPA

```bash
systemctl --user restart cliproxyapi
# 或直接重启 CPA 主进程
```

调用模型列表端点确认模型是否已正确挂载：
```bash
curl http://localhost:8317/v1/models -H "Authorization: Bearer <your-cpa-api-key>" | jq '.data[].id'
```

---

## 调用示例

### 对话补全（Chat Completions，`/v1/chat/completions`）

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "opencode-zen/cat-jam-free",
    "messages": [{"role": "user", "content": "用 Go 语言写一个快速排序算法。"}],
    "stream": false
  }'
```

### 工具调用（Tool Calling）

客户端声明的 tools 会被完整保留并由模型原生调用，插件会在网关层自动隔离下游工具与平台指纹工具：

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cpa-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "opencode-zen/space-bunny-free",
    "messages": [{"role": "user", "content": "使用 calc 工具计算 123 * 456。"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "calc",
        "description": "计算数学表达式",
        "parameters": {
          "type": "object",
          "properties": {"expr": {"type": "string"}},
          "required": ["expr"]
        }
      }
    }],
    "tool_choice": "auto"
  }'
```

---

## 源码构建与本地验证

构建环境要求：Go **1.26+**（CI 固定使用 1.26.9）、C 编译器（GCC 或 Clang）、Python 3、GNU Make、Bash 及 GNU `timeout`。原生宿主测试需要在启用了 CGO 的 Linux 环境下运行。

```bash
# 在允许联网的环境下预先准备两个 Go 模块的依赖缓存
go mod download
go -C cmd/hostcheck mod download

# 运行完整的离线验证门禁（包含针对本地 mock 的原生真实宿主测试）
make check-offline

# 仅编译共享动态库（C ABI 入口保留在根目录包）
make build
# 编译产物位于：build/opencode-v0.1.0.so

# 从共享常量打印插件提供商标识、版本号及文件名元数据
make metadata
```

`check-offline` 包含了两个 Go 模块的单元测试、vet 静态分析与 race 竞态检测、Python 协议用例、Shell 脚本预检、SDK 复现脚本，以及原生宿主在 `--quiesce`、`--cancel-first`、同实例 `--reconfigure` 和空闲 `--reenable` 模式下的完整验证。该命令禁用模块外网下载并使用本地 loopback 服务。

已知的 CPA 上游宿主在活跃请求下的 disable 路径缺陷作为独立的 `make hostcheck-known-bug` 目标保留，不会干扰主门禁。详见 [构建与验证文档](docs/BUILD.md)。

项目架构将根目录的 C-ABI 包装层与 Go 插件核心引擎及其内部的 catalog、protocol、diagnostics、fault 子包严格分离。供开发者抓包的 Python 工具位于 `tools/officialwire`；`cmd/hostcheck` 保持为独立的 Go 子模块，以便安全白盒测试 CPA 原生宿主内部逻辑。详见 [架构设计与依赖边界文档](docs/ARCHITECTURE.md)。

---

## 开源协议

[AGPL-3.0](LICENSE)
