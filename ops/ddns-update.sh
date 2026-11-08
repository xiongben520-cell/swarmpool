#!/usr/bin/env bash
# DuckDNS 动态域名更新（部署于 vps87 /opt/swarmpool/ddns/update.sh）
# 每 5 分钟由 cron 调用；不带 ip 参数 = DuckDNS 自动探测本机出口 IP
set -euo pipefail

ENV_FILE=/opt/swarmpool/ddns/.token.env
[ -f "$ENV_FILE" ] || { echo "missing $ENV_FILE" >&2; exit 1; }
# shellcheck disable=SC1090
source "$ENV_FILE"

RESULT=$(curl -fsS --max-time 20 \
  "https://www.duckdns.org/update?domains=swarmpool&token=${DUCKDNS_TOKEN}&verbose=true")
echo "$(date '+%F %T') ${RESULT}" >> /var/log/swarmpool-ddns.log
