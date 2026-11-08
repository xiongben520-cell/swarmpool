# SwarmPool 服务器运维手册

> 两台 VPS 的分工、部署位置、日常操作与故障恢复。2026-11-08 实测配置。

## 机器分工

| | vps87 (yeziyun, 87.83.207.37) | vps107 (DediRock, 107.172.186.165) |
|---|---|---|
| 角色 | **主控面**：调度器+API+账本 DB+网站 | **边缘+灾备**：监控、备份、待命机 |
| 已部署 | Docker 29.1.3、compose 2.40、/opt/swarmpool（Postgres16 + Redis7） | Docker 29.1.3、Uptime Kuma(:3001 回环)、备份脚本+cron |
| 网络 | NAT 机（入站 80/443 可用），WG=10.10.0.2 | 直连公网，WG hub=10.10.0.1，1Gbps |
| 约束 | 内存 2G（已加 2G swap，swappiness=20）；国际出口仅 ~4Mbps | CPU 老（E5-2670v2），30G 盘 |

## 目录约定

- vps87: `/opt/swarmpool/`（compose.yml + .env + data/）— 控制面全家桶
- vps107: `/opt/swarmpool-ops/`（backup.sh + .pg.env 主密钥副本）、`/opt/swarmpool-backups/`（备份产物，保留7份）、`/opt/uptime-kuma/`（监控数据）；pg_dump 认证走 `~/.pgpass`（600）
- WG 内网：vps87=10.10.0.2，vps107=10.10.0.1；ufw 已放行 wg0 全量入站

## 日常操作

```bash
# vps87: 控制面状态
ssh yeziyun 'cd /opt/swarmpool && docker compose ps && docker compose logs --tail 20'

# vps107: 监控面板（SSH 隧道后浏览器打开 http://localhost:3001）
ssh -L 3001:127.0.0.1:3001 vps107
# 首次访问在页面里创建管理员，建号后建议在设置里关闭"允许注册"

# 手动触发一次备份
ssh vps107 'bash /opt/swarmpool-ops/backup.sh'

# vps107 跑大下载任务时借道 vps87 出口（可选）
ssh yeziyun 'sudo ip route add <目标网段> via 10.10.0.1'
```

## 故障恢复

| 场景 | 操作 |
|------|------|
| vps87 挂机 | 重买/换 IP 后：装 Docker → scp compose + .env → `docker compose up -d` → 用 vps107 上的最新 dump 恢复：`pg_restore -h localhost -U swarmpool -d swarmpool <最新dump>` |
| vps107 挂机 | 无状态损失（Kuma 数据可丢可重），重装 Docker + Kuma + 备份 cron 即可 |
| 备份失败 | 先 `ssh vps107 'ping -c2 10.10.0.2'` 查 WG 隧道，再查 `~/.pgpass` 与 PG 容器状态 |

## 安全注意

- `.env` / `.pg.env` 含 DB 密码（600 权限，永不入库）
- Postgres/Redis 只绑定 127.0.0.1 + WG 内网，公网不可达（ufw 默认 deny incoming，仅 80/443/22/11150/2096/8787 放行）
- Uptime Kuma 绑 127.0.0.1:3001，外部访问走 SSH 隧道，不暴露公网
- 转发端口与 xray 共机共存：给 swarmpool 用 127.0.0.1/内网绑定，不占公网端口
