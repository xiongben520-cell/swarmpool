# SwarmPool 协议规范 v0.1

> 状态：草稿（RFC 流程）｜ 本文档是约束全系统的唯一契约。
> 域名 `swarmpool.dev` 为占位符。许可：Apache-2.0。

## 1. 概述

SwarmPool 是一个开源的个人算力聚合调度协议：供方节点通过出站连接接入池，
平台将生成式 AI 任务（首发为图像/视频生成）切片调度到节点执行，产出经对象存储
直传直下，控制面只搬运 KB 级信令。积分（Credits）记账，MVP 阶段不涉及任何法币。

三条不可动摇的架构铁律：

1. **Origin 不碰字节**：任务产物与输入资产一律通过预签名 URL 在节点与对象存储之间
   直传直下，控制面永不代理大流量。
2. **节点只出不进**：节点到平台只有出站 WSS 连接，无公网入站端口要求，NAT 后即插即用。
3. **沙箱即边界**：节点容器只挂载专属数据目录，遥测采集以第 8 节白名单为唯一依据。

## 2. 术语

| 术语 | 含义 |
|------|------|
| Node / 节点 | 提供方机器上运行的容器化 worker（`spark` 客户端托管） |
| Provider / 供方 | 运行节点、贡献算力的个人 |
| Consumer / 需方 | 通过 API 消费算力的个人或其 Agent |
| Task Type | 一种可调度的负载（如 `image.gen`、`video.i2v`），见第 7 节 |
| Job / 任务 | 一次具体的算力消费请求 |
| Lease / 租约 | 调度器授予节点执行任务的限时凭证 |
| Credit / 积分 | 内部记账单位，MVP 不承诺兑付现金 |

## 3. 身份与密钥

- **节点身份** = Ed25519 密钥对，私钥存于 `~/.swarmpool/node.key`（600），
  公钥即节点 ID（`node_<32hex>`）。注册时提交公钥 + 首次基准测试结果。
- **消费者身份** = API Key（`sk-sp-` 前缀），由平台签发，可随时吊销。
- 无邮箱/手机/KYC。所有身份皆可吊销、信誉可重置（新 key = 新身份，但积分清零）。
- 协议消息全部带 `sig` 字段（Ed25519 签名）+ `ts` 时间戳（±60s 抗重放）。

## 4. 节点生命周期

```
spark up
  → 基准测试（跑一组标准推理，产出 benchmark_score 与 gpu_tier）
  → 注册  POST /node/register {pubkey, hw, benchmark, models[]}
  → WSS 连接 wss://api.<host>/node/ws
  → 心跳（每 15s：空闲显存/磁盘/在线窗口/负载）
  → 收到 lease → 拉取任务包（工作流 JSON + 输入资产预签名URL）
  → 容器执行（runtime image 按 TaskType 决定）
  → 产物上传存储（预签名 PUT）→ 签名回执 → 调度器验证 → 记账
心跳丢失 30s → 租约作废 → 任务重派第二节点（冗余重试）
```

### 4.1 节点准入

- 注册时基准测试分 < 3090 级 → 必须显式 `--i-know-power-cost` 才能加入
  （协议要求平台把"收益可能低于电费"的信息给足，但不设硬门槛）
- 节点声明 `models[]`（已缓存/愿意缓存的模型清单）与 `online_windows`（在线时段）

## 5. WSS 消息协议

JSON envelope，字段：`{v, type, ts, sig, payload}`。v=1。

| type | 方向 | payload 要点 |
|------|------|-------------|
| `hello` | node→pool | pubkey, hw摘要, models, 协议版本 |
| `welcome` | pool→node | node_id, 调度策略版本, 心跳间隔 |
| `heartbeat` | node→pool | 空闲VRAM/磁盘, 队列深度, 在线窗口 |
| `lease.offer` | pool→node | job_id, lease_ttl(30s), 任务包摘要 |
| `lease.accept` | node→pool | 确认接单 |
| `job.assign` | pool→node | 完整任务包引用（资产走预签名URL） |
| `job.progress` | node→pool | 可选，进度百分比 |
| `job.result` | node→pool | 产物清单（存储key+sha256）+ 执行元数据 |
| `job.fail` | node→pool | 错误码+原因（触发重派） |
| `credit.mint` | pool→node | 本任务积分入账通知 |

兼容规则：未知 `type` 忽略并告警；未知 payload 字段保留透传；
两端版本号不等时走协商降级，拒绝跨大版本（v1≠v2）。

## 6. 任务 Schema

```jsonc
// 单任务
{
  "id": "job_01J...",              // 平台生成
  "type": "video.i2v",             // 见任务类型注册表
  "model": "wan2.1-i2v-1.3b",
  "input": {                       // 结构由 TaskType.input_schema 定义
    "image_url": "...", "prompt": "...", "frames": 81, "seed": 42
  },
  "constraints": {                 // 可选
    "vram_min_gb": 12, "regions": ["eu","us"], "max_latency_s": 120
  },
  "redundancy": 1,                 // 冗余份数（1-3）
  "webhook": "https://...",        // 可选完成回调
  "ttl_s": 3600                    // 最长排队时间
}
```

**batch/fanout**（池化集群的核心卖点）：

```jsonc
{
  "type": "batch",
  "fanout": 16,                    // 最多并行节点数
  "items": [ /* N 个标准任务 */ ],
  "assemble": {                    // 可选：完成后拼接
    "kind": "video.concat", "then": {"audio_url": "..."}
  }
}
```

调度器将 batch 切成子任务入队，逐个按普通 Job 派发；assemble 作为最后一个
依赖节点执行（CPU 负载，可在小节点跑）。

## 7. 任务类型注册表

每种 TaskType 是注册表中的一条记录，**平台核心代码不因新增负载而改动**：

```jsonc
{
  "type": "video.i2v",
  "runtime_image": "ghcr.io/swarmpool/runtime-comfy:1.0",
  "models": [{
    "id": "wan2.1-i2v-1.3b",
    "repo": "Wan-AI/Wan2.1-I2V-14B"/* 示例 */,
    "files": [...], "vram_gb": 12, "tier": "high"
  }],
  "input_schema": {...},           // JSON Schema
  "pricing": { "gpu_tier_factor": 10, "per_second": 0.01, "per_gb_artifact": 20 },
  "verifier": "quick-checks",      // quick-checks | sampling | none
  "sandbox_profile": "comfy"       // 对应第9节沙箱约束模板
}
```

路线图：v0.1 `image.gen`(SDXL/Flux-schnell)、`video.i2v`(Wan2.1/LTX-Video)、
`video.t2v`；v0.2 `tts`、`asr`、OpenAI 兼容聊天；v0.3 `custom.container`(gVisor)。

## 8. 遥测白名单（隐私契约）

节点客户端**仅可**上报以下字段，实现不得超出：

**硬件（注册时+心跳）：**
GPU 型号、显存总量与空闲值、驱动/CUDA 版本、CPU 逻辑核数、内存总量、
节点数据目录磁盘用量、出口带宽测速结果。

**运行（心跳+任务回执）：**
在线窗口、任务执行统计（成功/失败/时长）、故障记录。

**明确禁止采集**：文件系统内容与路径、进程列表、环境变量、剪贴板、屏幕、
摄像头/麦克风、浏览器数据、任何个人身份信息。节点开源可审计，违反即生态除名。

## 9. 沙箱要求（安全契约）

节点容器最低要求：

- 仅挂载 `~/.swarmpool/`（模型权重、任务临时产物），不挂载 /、/home、/etc
- 出站白名单：`api.<host>`、模型源（HuggingFace）、对象存储端点
- seccomp 默认 profile；无特权模式；GPU 仅设备透传（nvidia-container-toolkit）
- 平台侧默认无法进入节点容器；远程排障须供方显式 `spark debug --allow 30m`
- 供方卸载 = 删除容器 + 删除 `~/.swarmpool` 目录，无其他残留

## 10. 验证与信誉

- **快检（默认）**：产物存在性/大小/时长/帧率、采样参数一致性、感知哈希去重
- **抽样（p%，按节点信誉调节）**：同任务在验证节点复跑，产物比对（SSIM/感知哈希）
- **信誉分 0-100**：初始 50；成功 +1，快检失败 -10，抽样不一致 -30，
  租约超时 -5，恢复到 50+ 需连续成功
- 信誉分 < 20 → 自动降级出池（暂停派单），可申诉重置

## 11. 积分账本

```
credit_cost(j)   = Σ (gpu_tier × runtime_s / 100) + artifact_gb × per_gb
provider_credit  = credit_cost × (1 − platform_fee)   // MVP platform_fee = 0
```

- `gpu_tier` 由注册基准分映射（示例：4090=10, 3090=7, 3080=6）
- 账本事件：`mint`（供方入账）/ `burn`（需方出账）/ `grant`（新用户赠送）/
  `slash`（违约扣减）——只增不改不删，全部公开可审计
- **MVP 阶段积分不承诺兑付现金**；新需方赠送 500 credits；
  结算模块（Stripe/加密支付）在 P3 接入，接口预留，不改账本结构

## 12. HTTP API v1

```
POST /v1/jobs              创建任务（含 batch）      → {id, status, estimate}
GET  /v1/jobs/{id}         查询状态                  → {status, progress, artifacts[]}
GET  /v1/jobs/{id}/artifacts/{n}   产物预签名下载URL
GET  /v1/models            任务类型与模型清单
GET  /v1/pool/stats        池子状态（节点数/各型号卡数/排队）
POST /v1/keys              创建 API Key（需方）
```

- 认证：`Authorization: Bearer sk-sp-...`
- 限流：免费账户 10 req/min，赠送 credits 用尽后需攒积分或等 P3 充值
- **OpenAI 兼容端点**（v0.2）：`POST /v1/images/generations` 等，存量生态零改造接入

## 13. 升级与兼容

- 协议版本随 envelope `v` 字段走；小版本向后兼容，大版本需双端协商
- 本规范的任何变更走 RFC 流程：`protocol/rfcs/NNN-title.md`，
  冻结期 7 天，无反对即合入
