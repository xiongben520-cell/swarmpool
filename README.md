# SwarmPool

> 开源的个人算力聚合调度协议：任何人一条命令把闲置 GPU 变成池中节点，
> 任何人一条命令/一段提示词拿到一个生成式 AI API。
> License: Apache-2.0

## 快速开始（供方，Linux）

```bash
# 1. 下载安装脚本并审阅（推荐；脚本做什么见文件头注释）
curl -fsSL https://raw.githubusercontent.com/swarmpool/swarmpool/main/spark/install.sh -o install.sh
# 2. 执行（无 N 卡自动进入 sim 模式；环境变量见脚本头）
sh install.sh
# 3. 自检
spark doctor && spark status
```

## 快速开始（需方，拿 API Key + 发任务）

```bash
# 平台管理端签发（P0 阶段管理员操作），需方拿到 sk-sp-... 后：
curl -H "Authorization: Bearer sk-sp-..." \
  -d '{"type":"image.gen","model":"sim-model","input":{"prompt":"a red apple"}}' \
  https://swarmpool.duckdns.org/v1/jobs
# 轮询 GET /v1/jobs/{id}，产物经 /v1/jobs/{id}/artifacts/0 下载
```

## 定位

- **供方**：个人 PC/显卡，一键部署，零穿透本地系统，只出算力
- **需方**：个人/Agent，一键拿 Key，MCP 接入，batch 任务自动切片多卡并行
- **平台**：只做协议 + 调度 + 记账，积分制，MVP 零抽成零现金流

## 仓库结构

```
protocol/    协议规范（RFC 流程）——约束全系统的唯一契约
docs/        deploy.md（供方技能卡）/ connect.md（需方技能卡）
deploy/      控制面 compose 骨架（自建池入口）
ops/         服务器运维手册 + 备份脚本（当前官方池：vps87/vps107）
```

## 当前进展（2026-11-08）

- [x] 竞品调研与设计定稿（设计文档见上级目录）
- [x] 官方池控制面上线：vps87（Postgres16 + Redis7 + swarmpool-core:dev）
- [x] 公网入口：https://swarmpool.duckdns.org（DuckDNS + DDNS cron + Let's Encrypt TLS）
- [x] schema v0.1 迁移（nodes/jobs/artifacts/ledger/task_types）
- [x] **P0 最小闭环打通**：WSS 派发协议（hello/welcome/heartbeat/lease/assign/result）
  + spark 节点客户端（sim 运行时）+ 调度（重试/冗余）+ quick-checks 校验（sha256 三方一致）
  + 积分账本（burn 需方 / mint 供方）—— 全链路公网实测通过
- [x] vps107：Uptime Kuma 监控（PG/ICMP 正常）+ 每日 pg_dump 备份（实测通过）
- [x] 节点一键安装流程就绪：spark 二进制+install.sh+双技能卡公网分发
  （spark-linux-amd64 / deploy.md / connect.md 实测 200）；
  vps107 实机完成等效安装 → **双节点在线（tier 2/1）**，tier 落库正确
- [x] 多节点调度验证：tier 优先 + 故障转移（vps87 节点停止 → 任务自动落 vps107 ✓）
- [ ] P0 收尾：runtime-comfy 容器（真实 GPU 推理）+ 产物直传存储层（MinIO/R2）
- [ ] P1：spark 一键安装脚本 + deploy.md/connect.md 实装 + MCP 包 + 10-20 节点公测

## 下一步

1. `protocol/spec.md` 冻结 v0.1 → 实现 `swarmpool-core`（Go 单体）
2. ComfyUI 文生图任务全链路：自己两台机器互跑通
3. deploy.md / connect.md 技能卡实机打磨
