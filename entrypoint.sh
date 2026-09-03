#!/bin/sh
# 启动前置：按 PUID/PGID（默认 100:100，即镜像内 app 用户）修正 /data 属主后降权运行。
# 容器以 root 启动本脚本是为了能 chown 挂载进来的宿主机目录（属主任意均可），
# 修正后立刻通过 su-exec 降权，进程实际以非 root 运行。
set -e

PUID="${PUID:-100}"
PGID="${PGID:-100}"

# 非 root 启动（docker run --user=...）：无权 chown，直接运行，属主由调用方保证
if [ "$(id -u)" != "0" ]; then
    exec /usr/local/bin/cline2api "$@"
fi

mkdir -p /data

# 属主已正确则跳过，避免每次启动对 usage.db 全量 chown
owner="$(stat -c '%u:%g' /data 2>/dev/null || echo '')"
if [ "$owner" != "$PUID:$PGID" ]; then
    chown -R "$PUID:$PGID" /data 2>/dev/null || echo "warning: chown /data to $PUID:$PGID failed (NFS root-squash?), continuing"
fi

exec su-exec "$PUID:$PGID" /usr/local/bin/cline2api "$@"
