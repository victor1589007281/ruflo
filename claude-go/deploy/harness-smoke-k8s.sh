#!/usr/bin/env bash
# harness 契约层集成验收：构建镜像 → 载入 kind → 跑 Job → 取回 JSON 报告。
#
# 用法:
#   bash deploy/harness-smoke-k8s.sh            # 真集群（kind-dbk8s-e2e）
#   LOCAL=1 bash deploy/harness-smoke-k8s.sh    # 只在宿主跑二进制（快速回归，不碰集群）
#
# 为什么镜像标签每轮唯一: 固定标签时 Deployment/Job 的 spec 一个字节都没变，
# kubectl apply 是空操作 —— 于是"验收通过"可能验的是上一次的二进制。
# 本脚本在 Job 侧还有一道硬断言（Pod startTime 晚于本轮构建），与 k8s-e2e.sh 同规。
set -uo pipefail

REPO=/home/victor/base/git/temp/ruflo/claude-go
NS=claude-go
# CLUSTER 是 kind 集群名（kubectl context = kind-$CLUSTER，容器 = $CLUSTER-control-plane），
# 与 k8s-e2e.sh 保持同一约定：默认 dbk8s-e2e。写成 kind-dbk8s-e2e 会拼出 kind-kind-...
CLUSTER=${CLAUDE_GO_E2E_CLUSTER:-dbk8s-e2e}
STAMP=$(date +%s)
IMG="claude-go:harness-$STAMP"
BUILD_EPOCH=$STAMP
MODEL=${HARNESS_SMOKE_MODEL:-opencode:deepseek-v4.1-flash}
REPORT=deploy/harness-smoke-report.json

step() { echo; echo "━━━ $* ━━━"; }
ok()   { echo "  ✅ $*"; PASSED=$((PASSED+1)); }
bad()  { echo "  ❌ $*"; FAILED=$((FAILED+1)); }
PASSED=0; FAILED=0

cd "$REPO" || exit 1

# ────────────────────────────────────────────────────────────
# 1. 构建二进制（镜像只打包，不在容器内拉依赖 —— 与 Dockerfile 的约定一致）
# ────────────────────────────────────────────────────────────
step "1/6 构建二进制"
env -u CLAUDE_GO_LLM_GATEWAY go build -o deploy/claude-go-linux ./cmd/claude-go || exit 1
env -u CLAUDE_GO_LLM_GATEWAY go build -o deploy/claude-go-worker-linux ./cmd/claude-go-worker || exit 1
env -u CLAUDE_GO_LLM_GATEWAY go build -o deploy/anthropic-gateway-linux ./cmd/anthropic-gateway || exit 1
ok "三个二进制构建完成"

# ────────────────────────────────────────────────────────────
# 2. 本地模式（不碰集群）
# ────────────────────────────────────────────────────────────
if [ "${LOCAL:-0}" = "1" ]; then
  step "2/6 本地跑 harness-smoke"
  # 必须摘掉 ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN: buildEngine 的优先级是
  # --api-key > ANTHROPIC_API_KEY > provider.apiKey，而本机 shell 里的这两个变量
  # 是给 Claude Code 自己用的（不是 opencode 的 key）。留着它们，冒烟会拿着
  # 一个不相干的 token 去调 opencode 并得到 401 —— 看起来像"provider key 过期"，
  # 实际是取错了凭据。容器里的 Job 只有 OPENCODE_API_KEY，不存在这个问题。
  env -u CLAUDE_GO_LLM_GATEWAY -u ANTHROPIC_API_KEY -u ANTHROPIC_AUTH_TOKEN \
    ./deploy/claude-go-linux harness-smoke \
    --model "$MODEL" --out "$REPORT" --turn-timeout 240s
  RC=$?
  [ $RC -eq 0 ] && ok "本地 harness-smoke 通过" || bad "本地 harness-smoke 失败（exit $RC）"
  echo; echo "  报告: $REPO/$REPORT"
  echo "  通过 $PASSED 项 / 未通过 $FAILED 项"
  exit $([ $FAILED -eq 0 ] && echo 0 || echo 1)
fi

# ────────────────────────────────────────────────────────────
# 2.5 准备 Secret / ConfigMap
# ────────────────────────────────────────────────────────────
# Job 侧只认 OPENCODE_API_KEY（Secret 注入）。provider 名必须叫 opencode，
# modelconfig.applyProviderKeyFromEnv 才会把它映射到那个环境变量；ConfigMap 里
# 的 apiKey 故意留占位符，正是要验证"Secret 覆盖占位符"这条路真的通。
step "2/6 准备 Secret 与 ConfigMap"
kubectl --context "kind-${CLUSTER}" get ns "$NS" >/dev/null 2>&1 \
  || kubectl --context "kind-${CLUSTER}" create ns "$NS" >/dev/null
KEY=$(python3 -I -c 'import json,os;print(json.load(open(os.path.expanduser("~/.claude-go/config/config.json")))["providers"]["opencode"]["apiKey"])')
[ -n "$KEY" ] || { echo "  ❌ 宿主配置里取不到 opencode apiKey"; exit 1; }
kubectl --context "kind-${CLUSTER}" -n "$NS" create secret generic claude-go-llm \
  --from-literal=opencode-api-key="$KEY" --dry-run=client -o yaml \
  | kubectl --context "kind-${CLUSTER}" apply -f - >/dev/null || exit 1
unset KEY
ok "Secret 已建（key 只经管道进集群，不落盘不回显）"

MODEL_NAME=${MODEL#*:}
kubectl --context "kind-${CLUSTER}" -n "$NS" create configmap claude-go-config --from-literal=config.json='{
  "stateDir": "/data/.claude-go",
  "cwd": "/data",
  "providers": {
    "opencode": {
      "name": "opencode",
      "baseUrl": "https://opencode.ai/zen/go/v1",
      "apiKey": "PLACEHOLDER_OVERRIDE_VIA_ENV",
      "models": { "'"$MODEL"'": { "maxTokens": 4096, "maxTurns": 8, "promptCacheMode": "on", "contextWindow": 131072 } }
    }
  },
  "ai": { "modelAlias": "'"$MODEL"'", "maxTurns": 8, "maxTokens": 4096 }
}' --dry-run=client -o yaml \
  | kubectl --context "kind-${CLUSTER}" apply -f - >/dev/null || exit 1
ok "ConfigMap 已建（provider=opencode model=$MODEL 出站名=$MODEL_NAME，apiKey 留占位符）"

# ────────────────────────────────────────────────────────────
# 3. 构建镜像并载入 kind（kind 节点看不到宿主 docker 镜像库，必须 save+import）
# ────────────────────────────────────────────────────────────
step "3/6 构建镜像并载入 kind"
docker build -q -t "$IMG" -f deploy/Dockerfile deploy/ >/dev/null || exit 1
docker save "$IMG" | docker exec -i "${CLUSTER}-control-plane" ctr -n k8s.io images import - >/dev/null || exit 1
ok "镜像 $IMG 已载入 ${CLUSTER}"

# ────────────────────────────────────────────────────────────
# 4. 起 Job
# ────────────────────────────────────────────────────────────
step "4/6 提交 Job"
kubectl --context "kind-${CLUSTER}" -n "$NS" delete job claude-go-harness-smoke --ignore-not-found >/dev/null 2>&1
sed -e "s|IMAGE_PLACEHOLDER|$IMG|" \
    -e "s|opencode:deepseek-v4.1-flash|$MODEL|" \
    deploy/k8s/harness-smoke-job.yaml \
  | kubectl --context "kind-${CLUSTER}" apply -f - >/dev/null || exit 1
ok "Job 已创建（镜像 $IMG）"

# 轮询 Job 状态：succeeded==1 才算通过；failed>0 立刻停，不要等满超时。
for i in $(seq 120); do
  S=$(kubectl --context "kind-${CLUSTER}" -n "$NS" get job claude-go-harness-smoke \
        -o jsonpath='{.status.succeeded}' 2>/dev/null)
  F=$(kubectl --context "kind-${CLUSTER}" -n "$NS" get job claude-go-harness-smoke \
        -o jsonpath='{.status.failed}' 2>/dev/null)
  [ "${S:-0}" -ge 1 ] 2>/dev/null && break
  [ "${F:-0}" -ge 1 ] 2>/dev/null && break
  sleep 5
done

# ────────────────────────────────────────────────────────────
# 5. 取回报告
# ────────────────────────────────────────────────────────────
step "5/6 取回 Pod 日志与 JSON 报告"
POD=$(kubectl --context "kind-${CLUSTER}" -n "$NS" get pod -l app=claude-go-harness-smoke \
        --field-selector=status.phase!=Pending \
        -o go-template='{{range .items}}{{.status.startTime}} {{.metadata.name}} {{(index .spec.containers 0).image}}{{"\n"}}{{end}}' 2>/dev/null \
      | awk -v img="$IMG" '$3==img' | sort -r | head -1 | awk '{print $2}')

if [ -z "$POD" ]; then
  bad "找不到跑本轮镜像（$IMG）的 Pod"
else
  # 硬断言: Pod 启动时刻必须晚于本轮构建 —— 直接观测，不靠"我相信滚动生效"。
  START=$(kubectl --context "kind-${CLUSTER}" -n "$NS" get pod "$POD" -o jsonpath='{.status.startTime}')
  START_EPOCH=$(date -d "$START" +%s 2>/dev/null || echo 0)
  if [ "$START_EPOCH" -ge "$BUILD_EPOCH" ]; then
    ok "Pod 启动时刻 $START 晚于本轮构建（跑的一定是新二进制）"
  else
    bad "Pod 启动时刻 $START 早于本轮构建 —— 在验陈旧二进制"
  fi

  LOG=$(kubectl --context "kind-${CLUSTER}" -n "$NS" logs "$POD" 2>&1)
  echo "$LOG" | tail -60
  # JSON 报告从日志末段的标记行提取: 命令同时把报告打到 stdout，
  # 避免依赖 Pod 内部文件（Job 结束 Pod 仍可 logs，但 exec 需要 Running）。
  if echo "$LOG" | grep -q '^HARNESS_SMOKE_REPORT_JSON '; then
    echo "$LOG" | sed -n 's/^HARNESS_SMOKE_REPORT_JSON //p' | tail -1 > "$REPORT"
    ok "报告已写入 $REPO/$REPORT"
    python3 -I - "$REPO/$REPORT" <<'PY'
import json,sys
r=json.load(open(sys.argv[1]))
checks=r.get("checks",[])
failed=[c for c in checks if not c.get("pass")]
print(f"  报告: {len(checks)} 项检查，失败 {len(failed)} 项")
for c in failed:
    print(f"    ❌ {c.get('name')}: {c.get('detail','')[:200]}")
sys.exit(0 if not failed and r.get("pass") else 1)
PY
    [ $? -eq 0 ] && ok "报告内所有检查通过" || bad "报告内有检查未通过"
  else
    bad "日志里没有 HARNESS_SMOKE_REPORT_JSON 行 —— 取不到报告（不能凭 Job 完成记通过）"
  fi
fi

step "6/6 结论"
# succeeded==1 是进程退出码的集群侧回执；报告检查是内容侧判据。两者都要。
S=$(kubectl --context "kind-${CLUSTER}" -n "$NS" get job claude-go-harness-smoke -o jsonpath='{.status.succeeded}' 2>/dev/null)
[ "${S:-0}" -ge 1 ] && ok "Job succeeded=1" || bad "Job 未成功（succeeded=${S:-0}）"

echo; echo "  通过 $PASSED 项 / 未通过 $FAILED 项"
exit $([ $FAILED -eq 0 ] && echo 0 || echo 1)
