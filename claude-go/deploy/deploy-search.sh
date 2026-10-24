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
STAMP=$(date -u +%Y%m%d-%H%M%S)
IMG=${IMG:-claude-go:search-$STAMP}
DEPLOYMENTS=${DEPLOYMENTS:-"claude-go-control claude-go-worker claude-go-mcp claude-go-dashboard"}

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

# search-mcp sidecar: 把数据面作为独立 MCP 端点暴露 (与 codeintel MCP 同 Pod,
# 端口 29235)。用 strategic merge 幂等写入, 已存在时只更新镜像与 env。
SEARCH_ENV=$(cat <<'JSON'
[
  {"name":"SEARCH_PROXY_URL","value":"http://100.96.50.63:8888"},
  {"name":"SEARCH_NO_PROXY","value":"127.0.0.1,localhost,::1,100.96.50.63"},
  {"name":"SEARCH_MAX_TIER","value":"L2"}
]
JSON
)

echo "==> 注入 search/market 数据面配置 + sidecar"
for pair in "claude-go-control:control" "claude-go-worker:worker"; do
  d=${pair%%:*}; c=${pair##*:}
  kubectl -n "$NS" patch deploy "$d" --type=strategic \
    -p "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"$c\",\"env\":$SEARCH_ENV}]}}}}" >/dev/null
done

kubectl -n "$NS" patch deploy claude-go-mcp --type=strategic -p "$(cat <<JSON
{"spec":{"template":{"spec":{"containers":[{"name":"search-mcp","image":"$IMG","imagePullPolicy":"Never","args":["search-mcp-server","--transport","http","--addr","0.0.0.0:29235"],"env":[{"name":"HOME","value":"/tmp"},{"name":"SEARCH_PROXY_URL","value":"http://100.96.50.63:8888"},{"name":"SEARCH_NO_PROXY","value":"127.0.0.1,localhost,::1,100.96.50.63"},{"name":"SEARCH_MAX_TIER","value":"L2"}], "ports":[{"name":"search","containerPort":29235}],"resources":{"requests":{"cpu":"100m","memory":"256Mi"},"limits":{"cpu":"1","memory":"1Gi"}}}]}}}}
JSON
)" >/dev/null

# 独立端口供集群内其它服务/外部 MCP 客户端挂载。
kubectl -n "$NS" patch svc claude-go-mcp --type=strategic \
  -p '{"spec":{"ports":[{"name":"search","port":29235,"targetPort":29235}]}}' >/dev/null

echo "==> 滚动更新: $DEPLOYMENTS"
for d in $DEPLOYMENTS; do
  # 一个 Pod 可能有多个容器 (mcp 有 codeintel + search-mcp), 全部切到同一镜像。
  while IFS= read -r c; do
    [ -n "$c" ] || continue
    kubectl -n "$NS" set image "deploy/$d" "$c=$IMG"
  done < <(kubectl -n "$NS" get deploy "$d" -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\n"}{end}')
done

echo "==> 等待 rollout"
for d in $DEPLOYMENTS; do
  kubectl -n "$NS" rollout status "deploy/$d" --timeout=180s
done

echo "==> 结果"
kubectl -n "$NS" get pods -o wide

# 镜像名与启动时间都要对得上, 否则可能验的是旧二进制。
echo "==> 各 Deployment 当前镜像"
for d in $DEPLOYMENTS; do
  printf '   %-24s %s\n' "$d" \
    "$(kubectl -n "$NS" get deploy "$d" -o jsonpath='{.spec.template.spec.containers[0].image}')"
done
