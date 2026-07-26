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
# 三者均已修，本脚本逐条验收。
#
# 2026-07-25 扩展: 新增 10-12 三项，验收本轮实现的编排能力**在真集群里通电**
#   ⑥ 动作队列消费方是否真注册（此前队列只写不读，提示语是假承诺）
#   ⑦ 拦截器链是否真装上（此前生产恒空链 = 建成未通电）
#   ⑧ 预算台账事件是否真落 journal（budget.consumed / interceptors 进 run.created）
set -uo pipefail
export PATH="$PATH:$HOME/go/bin"
REPO=/home/victor/base/git/temp/ruflo/claude-go
NS=claude-go
CLUSTER=${CLAUDE_GO_E2E_CLUSTER:-dbk8s-e2e}   # kind 集群名; 独立 namespace, 不碰其它项目
IMG=claude-go:e2e
step() { echo; echo "━━━ $* ━━━"; }
ok()   { echo "  ✅ $*"; PASSED=$((PASSED+1)); }
# FAILED 是**计数**不是布尔: 写成 FAILED=1 时结论行恒报"有 1 项未通过",
# 三项全挂也只报 1 —— 一个把坏消息说小的汇总比没有汇总更危险。
bad()  { echo "  ❌ $*"; FAILED=$((FAILED+1)); }
FAILED=0
PASSED=0

step "1/13 构建静态二进制"
cd "$REPO" || exit 1
CGO_ENABLED=0 go build -trimpath -o deploy/claude-go-linux ./cmd/claude-go || exit 1
# 分布式模式的 worker 用**独立二进制** (claude-go 的 worker 子命令仍是回显桩)。
CGO_ENABLED=0 go build -trimpath -o deploy/claude-go-worker-linux ./cmd/claude-go-worker || exit 1
ls -la --block-size=M deploy/claude-go-linux deploy/claude-go-worker-linux | awk '{print "  二进制", $5, $NF}'

step "2/13 构建镜像并导入 kind"
docker build -q -t "$IMG" -f deploy/Dockerfile deploy/ >/dev/null || exit 1
# kind 节点看不到宿主 docker 镜像库，必须 save + ctr import（已知坑）
docker save "$IMG" | docker exec -i "${CLUSTER}-control-plane" ctr -n k8s.io images import - >/dev/null || exit 1
ok "镜像已导入 ${CLUSTER}-control-plane"

step "3/13 创建 namespace / Secret / ConfigMap"
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

step "4/13 部署单体模式"
sed -e "s|image: localhost:5000/claude-go:latest|image: $IMG|" \
    -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|" \
    "$REPO/deploy/k8s/monolith.yaml" \
  | kubectl apply -f - >/dev/null
kubectl -n "$NS" rollout status deploy/claude-go-monolith --timeout=180s || bad "单体 rollout 超时"
POD=$(kubectl -n "$NS" get pod -l app=claude-go-monolith -o jsonpath='{.items[0].metadata.name}')
echo "  pod=$POD"

step "5/13 验收①: ConfigMap 真被读取 + 真 key 生效"
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

step "6/13 验收②: HTTP 面存活"
kubectl -n "$NS" exec "$POD" -- curl -s -o /dev/null -w '  /api/health → %{http_code}\n' localhost:18080/api/health
WF=$(kubectl -n "$NS" exec "$POD" -- curl -s localhost:18080/api/workflows | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))' 2>/dev/null)
echo "  /api/workflows 数量 = ${WF:-取不到}"

step "7/13 验收③: 真实 LLM 团队执行"
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

step "8/13 验收④: 本轮修复的产物是否真落盘"
echo "  — 轨迹 Span（证明 TraceCaptureHook 已注册）:"
kubectl -n "$NS" exec "$POD" -- sh -c 'ls -la /data/.claude-go/statestore/log/ 2>/dev/null | head -5' || bad "无 statestore/log"
SPANS=$(kubectl -n "$NS" exec "$POD" -- sh -c 'cat /data/.claude-go/statestore/log/trace-*.jsonl 2>/dev/null | wc -l')
[ "${SPANS:-0}" -gt 0 ] && ok "trace span 行数 = $SPANS" || bad "无 trace span —— hook 未注册或未采集"
echo "  — 奖励事件:"
RW=$(kubectl -n "$NS" exec "$POD" -- sh -c 'wc -l < /data/.claude-go/evolution/rewards.jsonl 2>/dev/null')
[ "${RW:-0}" -gt 0 ] && ok "rewards.jsonl 行数 = $RW" || echo "  ⚠ 无 rewards（research 工作流未必触发 gate/episode 奖励）"
echo "  — 团队产物:"
kubectl -n "$NS" exec "$POD" -- sh -c 'ls /data/.claude-go/teams/e2e-real/ 2>/dev/null' || bad "无团队目录"

step "9/13 验收⑤: 分布式模式 + 网关真被经过"
# 团队工作区卷 (cwd 的 pvc 档位, design/02 §3.3): distributed.yaml 只引用不创建。
# kind 的 local-path **拒绝 RWX** (NodePath only supports ReadWriteOnce), 所以这里用
# hostPath 版 —— 单节点上它是真共享 (同一个节点目录), 已实测 A 写 B 读。
kubectl apply -f "$REPO/deploy/k8s/workspace-pvc-kind.yaml" >/dev/null \
  && ok "工作区卷 claude-go-teams 已就绪 (hostPath RWX, 单节点)" \
  || bad "工作区卷创建失败 —— pvc 档位的 worker 会卡 Pending"
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
# cwd 档位 (design/02 §3.3) 真通电的三条实证, 缺一条就是装饰品:
#   ① worker 上报 ws:pvc / wsvol:<卷名> 标签 (控制面靠它路由, 队列按标签过滤)
#   ② 控制面日志声明了档位 (说明 --workspace-mode 真被读到)
#   ③ 控制面与 worker 看到的 /workspace 是**同一份数据** (控制面写, worker 读)
WPOD=$(kubectl -n "$NS" get pod -l app=claude-go-worker -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$CTLPOD" ] && [ -n "$WPOD" ]; then
  kubectl -n "$NS" exec "$CTLPOD" -- curl -s localhost:18080/cluster/workers 2>/dev/null \
    | grep -q 'wsvol:claude-go-teams' \
    && ok "worker 上报 cwd 档位标签 (ws:pvc + wsvol:claude-go-teams)" \
    || bad "worker 未上报档位标签 —— 档位路由没通电"
  kubectl -n "$NS" logs "$CTLPOD" --tail=400 2>/dev/null | grep -q 'cwd 档位=pvc' \
    && ok "控制面已按 pvc 档位派活" || bad "控制面未声明 cwd 档位"
  STAMP="ws-probe-$RANDOM"
  kubectl -n "$NS" exec "$CTLPOD" -- sh -c "echo $STAMP > /workspace/.ws-e2e-probe" >/dev/null 2>&1
  kubectl -n "$NS" exec "$WPOD" -- sh -c 'cat /workspace/.ws-e2e-probe 2>/dev/null' 2>/dev/null \
    | grep -q "$STAMP" \
    && ok "控制面与 worker 的 /workspace 是同一份数据 (RWX 真共享)" \
    || bad "控制面与 worker 的 /workspace 不是同一份数据 —— 远程产码门禁看不见 (风险④)"
  kubectl -n "$NS" exec "$CTLPOD" -- rm -f /workspace/.ws-e2e-probe >/dev/null 2>&1
fi
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

step "10/13 验收⑥: 动作队列消费方已注册（design/01 §4.12）"
# 改造前 :7777 动作队列两处写入、全仓零消费方, "等待 claude-go 主进程消费"是假承诺。
LOG2=$(kubectl -n "$NS" logs "$POD" --tail=400 2>/dev/null)
if grep -q '动作队列消费方已启动' <<<"$LOG2"; then
  ok "主进程已注册动作队列消费方"
else
  bad "未见消费方启动日志 —— ConsumeActions 仍是建成未通电"
fi
# 真发一个动作, 看它是否被消费（回包里 consumed 字段是据实回报的）
ACT=$(kubectl -n "$NS" exec "$POD" -- curl -s -X POST \
  localhost:18080/api/actions/team/create/e2e-act -H 'Content-Type: application/json' \
  -d '{"workflow":"research","objective":"验证动作队列消费"}' 2>/dev/null)
echo "  动作回包: $(head -c 200 <<<"$ACT")"

step "11/13 验收⑦⑧: 图引擎 + 拦截器链 + 预算台账（design/01 §4.10）"
# 单体那轮跑的是 pipeline 路径（灰度默认关），这里显式开灰度跑一轮图引擎，
# 才能验证拦截器链与预算事件真的落盘。
kubectl -n "$NS" set env deploy/claude-go-monolith CLAUDE_GO_GRAPH_ENGINE=1 >/dev/null 2>&1
kubectl -n "$NS" rollout status deploy/claude-go-monolith --timeout=180s >/dev/null 2>&1 \
  && ok "已切到图引擎灰度（CLAUDE_GO_GRAPH_ENGINE=1）" || bad "切灰度后 rollout 失败"
GPOD=$(kubectl -n "$NS" get pod -l app=claude-go-monolith -o jsonpath='{.items[0].metadata.name}')
kubectl -n "$NS" exec "$GPOD" -- curl -s -X POST \
  localhost:18080/api/actions/team/create/e2e-graph -H 'Content-Type: application/json' \
  -d '{"workflow":"research","objective":"两句话说明有界展开为什么是必需的"}' >/dev/null 2>&1
kubectl -n "$NS" exec "$GPOD" -- curl -s -X POST \
  localhost:18080/api/actions/team/run/e2e-graph -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1
echo "  等待图引擎团队完成（真实 LLM，最长 12 分钟）..."
GST=""
for i in $(seq 144); do
  GST=$(kubectl -n "$NS" exec "$GPOD" -- curl -s localhost:18080/api/teams/e2e-graph \
        | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null)
  case "$GST" in
    completed|delivered_with_remediation) ok "图引擎团队状态=$GST"; break ;;
    failed) bad "图引擎团队 failed"; break ;;
  esac
  sleep 5
done
JRN=/data/.claude-go/teams/e2e-graph/graph-journal/journal.jsonl
JL=$(kubectl -n "$NS" exec "$GPOD" -- sh -c "wc -l < $JRN 2>/dev/null")
[ "${JL:-0}" -gt 0 ] && ok "graph-journal 行数 = $JL（图引擎真的跑了）" \
  || bad "无 graph-journal —— 图引擎未生效"
# 拦截器链构成进了 run.created 的 Data
IC=$(kubectl -n "$NS" exec "$GPOD" -- sh -c \
  "grep -m1 run.created $JRN 2>/dev/null | python3 -c 'import json,sys;print(\",\".join(json.load(sys.stdin).get(\"data\",{}).get(\"interceptors\",[])))'" 2>/dev/null)
[ -n "${IC:-}" ] && ok "拦截器链已装: [$IC]" || bad "run.created 无 interceptors —— 生产仍是空链"
# 预算台账真记账
BC=$(kubectl -n "$NS" exec "$GPOD" -- sh -c "grep -c budget.consumed $JRN 2>/dev/null")
[ "${BC:-0}" -gt 0 ] && ok "budget.consumed 事件 = $BC 条（预算台账真通电）" \
  || bad "无 budget.consumed —— 预算拦截器未记账"
# 事件类型盘点（本轮把事件从 8 种补到 17 种）
echo "  — journal 事件类型分布:"
kubectl -n "$NS" exec "$GPOD" -- sh -c \
  "python3 -c \"
import json,collections,sys
c=collections.Counter()
for l in open('$JRN'):
    try: c[json.loads(l)['type']]+=1
    except Exception: pass
for k,v in sorted(c.items()): print('    %-24s %d'%(k,v))
\"" 2>/dev/null || echo "    (取不到)"

step "12/13 验收⑨: 本轮新能力真落盘（轨迹 kind / 拦截器链构成）"
NS_POD=$(kubectl -n "$NS" get pod -l app=claude-go-monolith -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "${NS_POD:-}" ]; then
  echo "  — 轨迹 kind 分布（design/03 §4.1 应有 5-6 种，run 是本轮补的产生方）:"
  kubectl -n "$NS" exec "$NS_POD" -- sh -c 'cat /data/.claude-go/statestore/log/trace-*.jsonl 2>/dev/null' \
    | python3 -c "
import json,sys,collections
c=collections.Counter()
for l in sys.stdin:
    try: c[json.loads(l).get('kind','?')]+=1
    except Exception: pass
if not c: print('    (无轨迹)')
for k,v in sorted(c.items()): print('    %-16s %d'%(k,v))
" 2>/dev/null || echo "    (取不到)"
  echo "  — 奖励源分布（design/03 §4.2 八源）:"
  kubectl -n "$NS" exec "$NS_POD" -- sh -c 'cat /data/.claude-go/evolution/rewards.jsonl 2>/dev/null' \
    | python3 -c "
import json,sys,collections
c=collections.Counter()
for l in sys.stdin:
    try: c[json.loads(l).get('source','?')]+=1
    except Exception: pass
if not c: print('    (无奖励事件)')
for k,v in sorted(c.items()): print('    %-20s %d'%(k,v))
" 2>/dev/null || echo "    (取不到)"
fi

step "13/13 验收⑩: 三种 runtime 与 cwd 档位声明（design/01 §4.9 / 02 §3.3）"
kubectl -n "$NS" get pod -l app=claude-go-worker -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].command}{"\n"}{end}' 2>/dev/null | head -3
WCAPS=$(kubectl -n "$NS" exec "$POD" -- curl -s localhost:18080/cluster/workers 2>/dev/null \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print(" | ".join("%s:%s"%(w.get("name"),",".join(w.get("caps") or [])) for w in (d.get("workers") or [])) or "(无 worker)")' 2>/dev/null)
echo "  worker 能力标签: ${WCAPS:-取不到}"
case "${WCAPS:-}" in
  *ws:pvc*|*ws:git*) ok "worker 上报了 cwd 档位标签（跨机产码闭环的前提）" ;;
  *) echo "  ⚠ worker 未上报 ws:* 档位标签（可能是陈旧 Deployment，见报告）" ;;
esac

step "结论"
echo "  通过 $PASSED 项 / 未通过 $FAILED 项"
[ "$FAILED" -eq 0 ] && echo "  ✅ 全部验收通过" || echo "  ❌ 有 $FAILED 项未通过（见上方 ❌）"
echo
echo "清理: kubectl delete ns $NS"
