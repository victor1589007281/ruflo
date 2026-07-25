#!/usr/bin/env bash
# 用法: bash deploy/k8s-e2e.sh   (需 kind 集群 + 宿主 ~/.claude-go/config/config.json 内有真实 kimi key)
# 集群名可用 CLAUDE_GO_E2E_CLUSTER 覆盖。清理: kubectl delete ns claude-go
# claude-go K8s 真实 LLM 端到端验证
#
# 与此前那次「双模式冒烟」的区别：这次要证明 Pod 真的接触了 LLM。
# 此前不可能——三个死变量挡着：
#   ① K8s ConfigMap 从未被读取（发现路径不含 /etc/claude-go）
#   ② <PROVIDER>_API_KEY 无人读取（ConfigMap 里 apiKey 是占位符）
#   ③ CLAUDE_GO_LLM_GATEWAY 无人读取（分布式里网关是装饰品）
# 三者本轮均已修，本脚本逐条验收。
set -uo pipefail
export PATH="$PATH:$HOME/go/bin"
REPO=/home/victor/base/git/temp/ruflo/claude-go
NS=claude-go
CLUSTER=${CLAUDE_GO_E2E_CLUSTER:-dbk8s-e2e}   # kind 集群名; 独立 namespace, 不碰其它项目
IMG=claude-go:e2e
step() { echo; echo "━━━ $* ━━━"; }
ok()   { echo "  ✅ $*"; }
bad()  { echo "  ❌ $*"; FAILED=1; }
FAILED=0

step "1/9 构建静态二进制"
cd "$REPO" || exit 1
CGO_ENABLED=0 go build -trimpath -o deploy/claude-go-linux ./cmd/claude-go || exit 1
ls -la --block-size=M deploy/claude-go-linux | awk '{print "  二进制", $5}'

step "2/9 构建镜像并导入 kind"
docker build -q -t "$IMG" -f deploy/Dockerfile deploy/ >/dev/null || exit 1
# kind 节点看不到宿主 docker 镜像库，必须 save + ctr import（已知坑）
docker save "$IMG" | docker exec -i "${CLUSTER}-control-plane" ctr -n k8s.io images import - >/dev/null || exit 1
ok "镜像已导入 ${CLUSTER}-control-plane"

step "3/9 创建 namespace / Secret / ConfigMap"
kubectl create ns "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# 真实 kimi key 从宿主配置取，只进 Secret，不写进仓库任何文件
KEY=$(python3 -c "
import json;d=json.load(open('$HOME/.claude-go/config/config.json'))
print(d['providers']['kimi']['apiKey'])")
[ -n "$KEY" ] || { bad "宿主配置里取不到 kimi apiKey"; exit 1; }
kubectl -n "$NS" create secret generic claude-go-llm \
  --from-literal=kimi-api-key="$KEY" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
ok "Secret 已建（key 长度 ${#KEY}，不落盘不入库）"

# ConfigMap 保留占位符 apiKey —— 正是要验证 <PROVIDER>_API_KEY 覆盖生效
kubectl -n "$NS" create configmap claude-go-config --from-literal=config.json='{
  "stateDir": "/data/.claude-go",
  "cwd": "/data",
  "providers": {
    "kimi": {
      "name": "kimi",
      "baseUrl": "https://api.kimi.com/coding/v1",
      "apiKey": "PLACEHOLDER_OVERRIDE_VIA_ENV",
      "models": { "kimi:k3": {} }
    }
  },
  "ai": { "modelAlias": "kimi:k3", "maxTurns": 12, "maxTokens": 8192 },
  "wiki": { "enabled": true, "apiPort": 18080 }
}' --dry-run=client -o yaml | kubectl apply -f - >/dev/null
ok "ConfigMap 已建（apiKey 故意留占位符）"

step "4/9 部署单体模式"
sed -e "s|image: localhost:5000/claude-go:latest|image: $IMG|" \
    -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|" \
    "$REPO/deploy/k8s/monolith.yaml" \
  | kubectl apply -f - >/dev/null
kubectl -n "$NS" rollout status deploy/claude-go-monolith --timeout=180s || bad "单体 rollout 超时"
POD=$(kubectl -n "$NS" get pod -l app=claude-go-monolith -o jsonpath='{.items[0].metadata.name}')
echo "  pod=$POD"

step "5/9 验收①: ConfigMap 真被读取 + 真 key 生效"
sleep 3
LOG=$(kubectl -n "$NS" logs "$POD" --tail=200 2>/dev/null)
if grep -q 'apiKey 取自环境变量 KIMI_API_KEY' <<<"$LOG"; then
  ok "provider key 来自 Secret 注入的环境变量（<PROVIDER>_API_KEY 覆盖生效）"
else
  bad "未见 key 覆盖日志 —— ConfigMap 或环境变量覆盖没生效"
fi
if grep -qiE '占位|placeholder' <<<"$LOG"; then
  bad "日志出现占位 provider —— 仍落在假 provider 分支"
else
  ok "未落占位 provider 分支"
fi

step "6/9 验收②: HTTP 面存活"
kubectl -n "$NS" exec "$POD" -- curl -s -o /dev/null -w '  /api/health → %{http_code}\n' localhost:18080/api/health
WF=$(kubectl -n "$NS" exec "$POD" -- curl -s localhost:18080/api/workflows | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))' 2>/dev/null)
echo "  /api/workflows 数量 = ${WF:-取不到}"

step "7/9 验收③: 真实 LLM 团队执行"
# 真实端点是 /api/actions/team/{action}/{target}, 载荷 {workflow, objective}
kubectl -n "$NS" exec "$POD" -- curl -s -X POST \
  localhost:18080/api/actions/team/create/e2e-real \
  -H 'Content-Type: application/json' \
  -d '{"workflow":"research","objective":"用三句话说明事件溯源(event sourcing)与快照恢复的区别"}' \
  | head -c 300; echo
kubectl -n "$NS" exec "$POD" -- curl -s -X POST \
  localhost:18080/api/actions/team/run/e2e-real -H 'Content-Type: application/json' -d '{}' \
  | head -c 200; echo
echo "  等待团队完成（真实 LLM，research=fanout 共 6 阶段，最长 12 分钟）..."
for i in $(seq 144); do
  ST=$(kubectl -n "$NS" exec "$POD" -- curl -s localhost:18080/api/teams/e2e-real \
        | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null)
  case "$ST" in
    completed|delivered_with_remediation) ok "团队状态=$ST"; break ;;
    failed) bad "团队 failed"; break ;;
  esac
  sleep 5
done
[ -n "${ST:-}" ] && echo "  最终状态: $ST"

step "8/9 验收④: 本轮修复的产物是否真落盘"
echo "  — 轨迹 Span（证明 TraceCaptureHook 已注册）:"
kubectl -n "$NS" exec "$POD" -- sh -c 'ls -la /data/.claude-go/statestore/log/ 2>/dev/null | head -5' || bad "无 statestore/log"
SPANS=$(kubectl -n "$NS" exec "$POD" -- sh -c 'cat /data/.claude-go/statestore/log/trace-*.jsonl 2>/dev/null | wc -l')
[ "${SPANS:-0}" -gt 0 ] && ok "trace span 行数 = $SPANS" || bad "无 trace span —— hook 未注册或未采集"
echo "  — 奖励事件:"
RW=$(kubectl -n "$NS" exec "$POD" -- sh -c 'wc -l < /data/.claude-go/evolution/rewards.jsonl 2>/dev/null')
[ "${RW:-0}" -gt 0 ] && ok "rewards.jsonl 行数 = $RW" || echo "  ⚠ 无 rewards（research 工作流未必触发 gate/episode 奖励）"
echo "  — 团队产物:"
kubectl -n "$NS" exec "$POD" -- sh -c 'ls /data/.claude-go/teams/e2e-real/ 2>/dev/null' || bad "无团队目录"

step "9/9 验收⑤: 分布式模式 + 网关真被经过"
sed -e "s|image: localhost:5000/claude-go:latest|image: $IMG|g" \
    -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|g" \
    "$REPO/deploy/k8s/distributed.yaml" \
  | kubectl apply -f - >/dev/null
for d in claude-go-gateway claude-go-control claude-go-worker; do
  kubectl -n "$NS" rollout status "deploy/$d" --timeout=150s 2>/dev/null \
    && ok "$d Running" || bad "$d 未就绪"
done
GWPOD=$(kubectl -n "$NS" get pod -l app=claude-go-gateway -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
CTLPOD=$(kubectl -n "$NS" get pod -l app=claude-go-control -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$CTLPOD" ]; then
  kubectl -n "$NS" exec "$CTLPOD" -- curl -s -X POST \
    localhost:18080/api/actions/team/create/e2e-gw -H 'Content-Type: application/json' \
    -d '{"workflow":"research","objective":"一句话说明什么是租约(lease)"}' >/dev/null 2>&1
  kubectl -n "$NS" exec "$CTLPOD" -- curl -s -X POST \
    localhost:18080/api/actions/team/run/e2e-gw -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1
  sleep 240
  # 网关的 access.jsonl 有行 = 流量真的过了网关（CLAUDE_GO_LLM_GATEWAY 生效）
  ACC=$(kubectl -n "$NS" exec "$GWPOD" -- sh -c 'cat /data/.claude-go/llm-gateway/access.jsonl 2>/dev/null | wc -l' 2>/dev/null)
  [ "${ACC:-0}" -gt 0 ] \
    && ok "网关 access.jsonl 行数 = $ACC（CLAUDE_GO_LLM_GATEWAY 真生效，流量经网关）" \
    || bad "网关无访问记录 —— 流量没经网关（或路径不同）"
fi

step "结论"
[ "$FAILED" -eq 0 ] && echo "  全部验收通过" || echo "  有 $FAILED 项未通过（见上方 ❌）"
echo
echo "清理: kubectl delete ns $NS"
