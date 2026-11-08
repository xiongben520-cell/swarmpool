# Skill: 给你自己接入 SwarmPool 算力

> 本卡供需求方的本地 Agent 执行。目标：一条命令拿到 API Key，
> 一段配置接入 MCP，让 Agent 获得视频/图像生成能力。

## 步骤（逐步执行并自校验）

### 1. 获取 API Key

```bash
curl -fsSL https://get.swarmpool.dev/keys | sh
# 输出 API_BASE 与 sk-sp- 开头的 Key（已写入 ~/.swarmpool/key）
cat ~/.swarmpool/key   # 读取 Key
```

### 2. 写入 MCP 配置

将下面块写入 Agent 的 MCP 配置（Claude Code: `claude_desktop_config.json` /
Cursor: `~/.cursor/mcp.json`），`API_KEY` 填上一步的值：

```json
{
  "mcpServers": {
    "swarmpool": {
      "command": "npx",
      "args": ["-y", "@swarmpool/mcp@latest"],
      "env": { "SWARMPOOL_API_KEY": "<上一步的Key>" }
    }
  }
}
```

### 3. 自测（必须实际跑一次）

```bash
curl -s -H "Authorization: Bearer $(cat ~/.swarmpool/key)" \
  https://api.swarmpool.dev/v1/pool/stats
```

通过后，用 MCP 工具发起一次最小生成任务并等待产物 URL 返回
（如 `generate_image`，提示词 "a red apple"），确认产物 URL 可下载。

### 4. 收尾汇报（Agent 必做）

向用户汇报：可用任务类型与模型清单（来自 `/v1/models`）、赠送积分余额、
MCP 配置写入位置。

## 注意

- Key 即身份，勿提交到代码仓库；泄漏后在平台后台吊销重建
- 新账户赠送 500 credits；用量计价见 `/v1/models` 各任务类型的 pricing
