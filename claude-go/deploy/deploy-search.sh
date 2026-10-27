#!/usr/bin/env bash
# 构建并发布 search/market 数据面到 agents 命名空间。
#
# 约定与 deploy/k8s-e2e.sh 一致: 二进制在宿主机构建, 镜像只打包;
# kind 节点看不到宿主 docker 仓库, 必须 save + ctr import。
#
# 用法: deploy/deploy-search.sh [--no-build]
set -euo pipefail

CLUSTER=${CLUSTER:-hakit}
NS=${NS:-agents}
KUBE_CONTEXT=${KUBE_CONTEXT:-kind-$CLUSTER}
KUBECTL=(kubectl --context "$KUBE_CONTEXT")
STAMP=$(date -u +%Y%m%d-%H%M%S)
IMG=${IMG:-claude-go:search-$STAMP}
DEPLOYMENTS=${DEPLOYMENTS:-"claude-go-control claude-go-worker claude-go-mcp claude-go-dashboard"}

# 免费检索也共享 Redis 缓存；付费预算默认开启但 MaxTier=L2 不会启用付费源。
# max_uses 是会话级机械上限，当前状态在 MCP 进程内，所以下面强制单副本。
SEARCH_REDIS_URL=${SEARCH_REDIS_URL:-"redis://search-redis.${NS}.svc.cluster.local:6379/0"}
SEARCH_REDIS_KEY_PREFIX=${SEARCH_REDIS_KEY_PREFIX:-"claude-go:search"}
SEARCH_REDIS_TIMEOUT=${SEARCH_REDIS_TIMEOUT:-250ms}
SEARCH_REDIS_STALE_TTL=${SEARCH_REDIS_STALE_TTL:-1h}
SEARCH_SHARED_BUDGET_LIMIT_USD=${SEARCH_SHARED_BUDGET_LIMIT_USD:-1.00}
SEARCH_SHARED_BUDGET_WINDOW=${SEARCH_SHARED_BUDGET_WINDOW:-1h}
SEARCH_SHARED_BUDGET_RESERVATION_TTL=${SEARCH_SHARED_BUDGET_RESERVATION_TTL:-5m}
SEARCH_MCP_MAX_USES=${SEARCH_MCP_MAX_USES:-100}

REPO=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO"

BUILD=1
[ "${1:-}" = "--no-build" ] && BUILD=0

if [ "$BUILD" = 1 ]; then
  echo "==> 构建静态二进制"
  CGO_ENABLED=0 go build -trimpath -o deploy/claude-go-linux ./cmd/claude-go
  CGO_ENABLED=0 go build -trimpath -o deploy/claude-go-worker-linux ./cmd/claude-go-worker
  CGO_ENABLED=0 go build -trimpath -o deploy/anthropic-gateway-linux ./cmd/anthropic-gateway

  echo "==> 构建镜像 $IMG"
  docker build -q -t "$IMG" -f deploy/Dockerfile deploy/ >/dev/null

  echo "==> 导入 kind 节点 ${CLUSTER}-control-plane"
  docker save "$IMG" | docker exec -i "${CLUSTER}-control-plane" ctr -n k8s.io images import - >/dev/null
fi

echo "==> 部署 search 专用 Redis ($NS/search-redis)"
"${KUBECTL[@]}" -n "$NS" apply -f deploy/k8s/hakit/search-redis.yaml >/dev/null
"${KUBECTL[@]}" -n "$NS" rollout status deploy/search-redis --timeout=180s
"${KUBECTL[@]}" -n "$NS" exec deploy/search-redis -- redis-cli ping | grep -qx PONG

# search-mcp sidecar: 把数据面作为独立 MCP 端点暴露 (与 codeintel MCP 同 Pod,
# 端口 29235)。用 strategic merge 幂等写入, 已存在时只更新镜像与 env。
# GitHub token 从 ConfigMap 的 config.json → search.githubToken 读取 (不走 env):
# 没有配 token 时 GitHub 搜索源按未认证的 10 req/min 限流。写入用
# deploy/set-github-token.sh。search-mcp 容器必须挂 config 卷才能读到这份配置;
# 旧版本只在 codeintel 容器上挂了它, 所以这里显式补 volumeMounts。
SEARCH_ENV=$(cat <<JSON
[
  {"name":"SEARCH_PROXY_URL","value":"http://100.96.50.63:8888"},
  {"name":"SEARCH_NO_PROXY","value":"127.0.0.1,localhost,::1,100.96.50.63"},
  {"name":"SEARCH_MAX_TIER","value":"L2"},
  {"name":"SEARCH_REDIS_URL","value":"$SEARCH_REDIS_URL"},
  {"name":"SEARCH_REDIS_KEY_PREFIX","value":"$SEARCH_REDIS_KEY_PREFIX"},
  {"name":"SEARCH_REDIS_TIMEOUT","value":"$SEARCH_REDIS_TIMEOUT"},
  {"name":"SEARCH_REDIS_STALE_TTL","value":"$SEARCH_REDIS_STALE_TTL"},
  {"name":"SEARCH_SHARED_BUDGET_LIMIT_USD","value":"$SEARCH_SHARED_BUDGET_LIMIT_USD"},
  {"name":"SEARCH_SHARED_BUDGET_WINDOW","value":"$SEARCH_SHARED_BUDGET_WINDOW"},
  {"name":"SEARCH_SHARED_BUDGET_RESERVATION_TTL","value":"$SEARCH_SHARED_BUDGET_RESERVATION_TTL"}
]
JSON
)
SEARCH_MCP_ENV=$(printf '%s' "$SEARCH_ENV" | MAX_USES="$SEARCH_MCP_MAX_USES" python3 -c '
import json, os, sys
items = json.load(sys.stdin)
for item in items:
    if item.get("value", "").startswith("$SEARCH_"):
        raise SystemExit("unexpanded search environment variable: " + item["name"])
max_uses = os.environ["MAX_USES"]
if not max_uses.isdigit():
    raise SystemExit("SEARCH_MCP_MAX_USES must be a non-negative integer")
items.extend([
    {"name": "HOME", "value": "/tmp"},
    {"name": "SEARCH_MCP_MAX_USES", "value": max_uses},
])
print(json.dumps(items, separators=(",", ":")))
')

echo "==> 注入 search/market 数据面配置 + sidecar"
for pair in "claude-go-control:control" "claude-go-worker:worker"; do
  d=${pair%%:*}; c=${pair##*:}
  "${KUBECTL[@]}" -n "$NS" patch deploy "$d" --type=strategic \
    -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"$c\",\"env\":$SEARCH_ENV}]}}}}" >/dev/null
  # 迁移: 旧版本把 token 以 env 注入; 现在唯一入口是 config.json。删掉残留 env,
  # 否则"改了配置却没生效"会被这个更高优先级的旧值掩盖。
  "${KUBECTL[@]}" -n "$NS" set env "deploy/$d" -c "$c" GITHUB_TOKEN- >/dev/null 2>&1 || true
done

# max_uses 的签发会话与计数目前在进程内；未做会话级路由/外置状态前不能扩多副本。
"${KUBECTL[@]}" -n "$NS" patch deploy claude-go-mcp --type=strategic -p "$(cat <<JSON
{"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"search-mcp","image":"$IMG","imagePullPolicy":"Never","args":["search-mcp-server","--transport","http","--addr","0.0.0.0:29235","--config","/etc/claude-go/config.json"],"env":$SEARCH_MCP_ENV,"volumeMounts":[{"name":"config","mountPath":"/etc/claude-go"}],"ports":[{"name":"search","containerPort":29235}],"resources":{"requests":{"cpu":"100m","memory":"256Mi"},"limits":{"cpu":"1","memory":"1Gi"}}}]}}}}
JSON
)" >/dev/null
# codeintel 容器也曾经被注入 GITHUB_TOKEN; 它不消费这个变量, 一并清掉。
"${KUBECTL[@]}" -n "$NS" set env deploy/claude-go-mcp -c mcp GITHUB_TOKEN- >/dev/null 2>&1 || true

# 独立端口供集群内其它服务/外部 MCP 客户端挂载。
"${KUBECTL[@]}" -n "$NS" patch svc claude-go-mcp --type=strategic \
  -p '{"spec":{"ports":[{"name":"search","port":29235,"targetPort":29235}]}}' >/dev/null

# 把 search MCP 登记进 config.json 的 mcpServers。
#
# 只暴露端口不登记, 能力中心的 MCP 页仍是空的 —— 那里读的是 bot 启动时建立的
# 连接 (/api/mcp/servers), 不是 k8s Service 清单。控制面与 mcp Pod 在同一个
# 命名空间, 走 Service DNS 即可; 用 127.0.0.1 会把连接指到控制面自己的 Pod。
#
# ConfigMap 里含凭据, 所以整份内容只在管道里流转, 不写临时文件、不落日志。
echo "==> 登记 search MCP 到 mcpServers"
NEW_CM=$("${KUBECTL[@]}" -n "$NS" get cm agents-config -o json | NS="$NS" python3 -c '
import json, os, sys
cm = json.load(sys.stdin)
cfg = json.loads(cm["data"]["config.json"])
servers = cfg.setdefault("mcpServers", {})
want = {
    "transport": "http",
    "url": "http://claude-go-mcp.%s.svc.cluster.local:29235" % os.environ.get("NS", "agents"),
}
if servers.get("claude-go-search") == want:
    sys.exit(0)
servers["claude-go-search"] = want
cm["data"]["config.json"] = json.dumps(cfg, ensure_ascii=False, indent=2)
print(json.dumps(cm))
')
if [ -n "$NEW_CM" ]; then
  printf '%s' "$NEW_CM" | "${KUBECTL[@]}" -n "$NS" replace -f - >/dev/null
  echo "    mcpServers.claude-go-search 已写入"
else
  echo "    mcpServers.claude-go-search 已存在"
fi
unset NEW_CM

# 先切 search-mcp 并等它就绪, 再切控制面。
#
# 控制面在启动时只尝试连接 mcpServers 一次, 失败不重试 (pkg/feishu/bot.go:
# addMCPServerWithTimeout)。两者同时滚动时, 控制面会碰上仍在 Terminating 的旧
# mcp Pod, 拿到旧二进制返回的 method not found, 于是能力中心一直显示"没有连接
# 的 MCP 服务器" —— 而镜像、Service、配置全都对。
switch_image() {
  local d=$1
  # 一个 Pod 可能有多个容器 (mcp 有 codeintel + search-mcp), 全部切到同一镜像。
  while IFS= read -r c; do
    [ -n "$c" ] || continue
    "${KUBECTL[@]}" -n "$NS" set image "deploy/$d" "$c=$IMG"
  done < <("${KUBECTL[@]}" -n "$NS" get deploy "$d" -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}')
}

echo "==> 滚动更新: $DEPLOYMENTS"
rest=""
for d in $DEPLOYMENTS; do
  if [ "$d" = "claude-go-mcp" ]; then
    switch_image "$d"
    "${KUBECTL[@]}" -n "$NS" rollout status "deploy/$d" --timeout=180s
  else
    rest="$rest $d"
  fi
done

echo "==> 等待其余 rollout"
for d in $rest; do
  switch_image "$d"
  "${KUBECTL[@]}" -n "$NS" rollout status "deploy/$d" --timeout=180s
done

echo "==> 结果"
"${KUBECTL[@]}" -n "$NS" get pods -o wide

# 镜像名与启动时间都要对得上, 否则可能验的是旧二进制。
echo "==> 各 Deployment 当前镜像"
for d in $DEPLOYMENTS; do
  printf '   %-24s %s\n' "$d" \
    "$("${KUBECTL[@]}" -n "$NS" get deploy "$d" -o jsonpath='{.spec.template.spec.containers[0].image}')"
done
