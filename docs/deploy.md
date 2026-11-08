# Skill: 把这台机器部署为 SwarmPool 节点

> 本卡供本地 Agent（Claude Code / Cursor 等）执行。目标：一条命令把本机 GPU
> 接入 SwarmPool 算力池，全程不触碰本机系统数据。
> 平台地址：https://swarmpool.duckdns.org

## 安全声明（先向用户确认）

本卡将执行的操作：
- 安装容器运行时（如缺失）与 `spark` 客户端，创建 `~/.swarmpool/` 专属目录
- 下载 AI 模型权重到该目录，在容器内执行平台下发的生成任务
- 上报硬件信息白名单：GPU 型号/显存/带宽/任务统计

**不会做**：读取文件系统内容、进程列表、剪贴板、屏幕、摄像头/麦克风、浏览器数据。

## 步骤（全部非交互，逐步执行并自校验）

### 1. 环境检测

```bash
# GPU 检测（无 N 卡则停止并告知用户）
nvidia-smi --query-gpu=name,memory.total --format=csv,noheader
```

### 2. 安装（Linux，先审阅后执行）

```bash
curl -fsSL https://swarmpool.duckdns.org/install.sh -o install.sh
# 审阅脚本内容（会做什么：GPU 只读检测、下载 spark 二进制、写 systemd 服务）
sh install.sh
```

安装器行为（幂等）：GPU 只读检测 → 下载 spark 二进制到 /usr/local/bin →
写 systemd 服务并启动 → 节点自动注册上线。

Windows (PowerShell) 与 macOS 安装器在下一迭代发布。

### 3. 自校验（每步失败即停止排错）

```bash
spark doctor          # 平台可达性 + 密钥存在性 + runtime 摘要
spark status          # 平台池状态快照
journalctl -u spark -f   # 节点实时日志（接单/执行/回执）
```

### 4. 收尾汇报（Agent 必做）

执行 `spark earnings --preview` 与 `spark status`，向用户汇报：
节点 ID、已接入模型、预估每小时积分、数据目录位置（`~/.swarmpool`）。

## 回滚/卸载

```bash
spark uninstall   # 停止守护进程、删除容器，并提示是否删除 ~/.swarmpool
```

## 故障速查

| 现象 | 处理 |
|------|------|
| `spark doctor` 连接失败 | 检查出站 443；公司网络可能拦截 WSS |
| 显存不足被拒 | 换 `SWARMPOOL_MODELS` 为小模型（如 flux-schnell）|
| Windows 提示需重启 | WSL2 内核更新后重启再跑 `spark up` |
