#!/usr/bin/env bash
# SwarmPool 每日备份脚本（部署于 vps107 /opt/swarmpool-ops/backup.sh）
# 逻辑：每晚 04:17 通过 WireGuard 对 vps87 的 Postgres 做逻辑备份，保留最近 7 份
# 认证：pg_dump 读取 ~/.pgpass（600 权限），本脚本不含任何凭据
set -euo pipefail

PG_HOST=10.10.0.2
PG_PORT=5432
PG_USER=swarmpool
PG_DB=swarmpool

TS=$(date +%Y%m%d-%H%M%S)
DEST=/opt/swarmpool-backups
mkdir -p "$DEST"

pg_dump -h "$PG_HOST" -p "$PG_PORT" -U "$PG_USER" -d "$PG_DB" -Fc -f "$DEST/swarmpool-$TS.dump"

# 保留最近 7 份
ls -1t "$DEST"/swarmpool-*.dump | tail -n +8 | xargs -r rm -f
echo "backup ok: swarmpool-$TS.dump ($(du -h "$DEST/swarmpool-$TS.dump" | cut -f1))"
