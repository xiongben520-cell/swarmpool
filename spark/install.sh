#!/bin/sh
# SwarmPool 节点安装脚本（幂等、非交互）
# 用法：先下载审阅再执行（快速开始命令见仓库 README）
# 环境变量：SWARMPOOL_API / SWARMPOOL_RUNTIME / SWARMPOOL_MODELS / SWARMPOOL_TIER
set -eu

API="${SWARMPOOL_API:-https://swarmpool.duckdns.org}"
MODELS="${SWARMPOOL_MODELS:-sim-model}"
RUNTIME="${SWARMPOOL_RUNTIME:-auto}"
TIER="${SWARMPOOL_TIER:-1}"
# 二进制从 GitHub Release 分发（版本化+可校验），不再走平台服务器
BASE_URL="${SWARMPOOL_BASE:-https://github.com/xiongben520-cell/swarmpool/releases/latest/download}"

# ---- 1. GPU 检测（只读检测，不触碰系统其它数据）----
GPU_INFO=""
if command -v nvidia-smi >/dev/null 2>&1; then
    GPU_INFO=$(nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null | head -1) || GPU_INFO=""
fi
if [ -n "$GPU_INFO" ]; then
    echo "==> [OK] 检测到 GPU: $GPU_INFO"
    if [ "$RUNTIME" = "auto" ]; then RUNTIME="comfy"; fi
else
    echo "==> [WARN] 未检测到 NVIDIA GPU：将以 sim 运行时接入（仅体验用，收益预期极低）"
    if [ "$RUNTIME" = "auto" ]; then RUNTIME="sim"; fi
fi

# ---- 2. 下载 spark 二进制 ----
ARCH=$(uname -m)
if [ "$ARCH" != "x86_64" ]; then
    echo "!! 暂不支持架构 $ARCH（当前发布 linux-amd64）"
    exit 1
fi

echo "==> 下载 spark ..."
SPARK_URL="$BASE_URL/spark-linux-amd64"
if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$SPARK_URL" -o /tmp/spark-download
elif command -v wget >/dev/null 2>&1; then
    wget -qO /tmp/spark-download "$SPARK_URL"
else
    echo "!! 需要 curl 或 wget"
    exit 1
fi
install -m 0755 /tmp/spark-download /usr/local/bin/spark
rm -f /tmp/spark-download
echo "==> [OK] spark -> /usr/local/bin/spark"

# ---- 3. systemd 服务（幂等覆盖）----
SVC_USER="root"
SERVICE_DIR="/etc/systemd/system"
if [ "$(id -u)" != "0" ]; then
    SVC_USER="$(id -un)"
    SERVICE_DIR="$HOME/.config/systemd/user"
    mkdir -p "$SERVICE_DIR"
fi

cat > "$SERVICE_DIR/spark.service" <<EOF
[Unit]
Description=SwarmPool Spark Node
After=network-online.target

[Service]
Type=simple
User=$SVC_USER
Environment=SWARMPOOL_API=$API
Environment=SWARMPOOL_RUNTIME=$RUNTIME
Environment=SWARMPOOL_MODELS=$MODELS
Environment=SWARMPOOL_TIER=$TIER
ExecStart=/usr/local/bin/spark up
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

if [ "$(id -u)" = "0" ]; then
    systemctl daemon-reload
    systemctl enable --now spark.service
    LOG_HINT="journalctl -u spark -f"
else
    systemctl --user daemon-reload
    systemctl --user enable --now spark.service
    LOG_HINT="journalctl --user -u spark -f"
fi

echo "==> [OK] spark 服务已启动（runtime=$RUNTIME models=$MODELS tier=$TIER）"
echo "==> 状态自检: spark doctor && spark status"
echo "==> 实时日志: $LOG_HINT"
echo "==> 卸载: systemctl disable --now spark.service && rm /usr/local/bin/spark && rm -rf ~/.swarmpool"
