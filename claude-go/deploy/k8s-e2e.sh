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
#
# 2026-07-25 二次修正（一次真机跑暴露出来的三个**脚本自身**缺陷，不是产品缺陷）:
#   ⓐ 镜像标签固定 ⇒ apply 空操作 ⇒ 整轮在验一个 18 小时前的二进制而照常报成功。
#      改为每轮唯一标签 + 一道"Pod 启动时刻必须晚于本轮构建"的硬断言。
#   ⓑ 团队名固定 ⇒ 第二次跑撞"团队已存在", 且 run 可能走 resume 直接返回上一轮产出
#      而一次 LLM 都不调 —— "真实 LLM 端到端"退化成读缓存。改为每轮唯一团队名。
#   ⓒ 两条断言看的窗口不对（--tail=200 滑过了启动首几行）: 一条假红, 另一条
#      在错误窗口上做否定断言从而**恒真**。改为读全量日志 + 否定断言先自证窗口有效。
#   同时补上本轮最后三项（ExternalHook / 图级 watchdog / 黑板 Watch）的真集群验收,
#   搭第 11 步本来就要跑的图引擎那轮, 不额外花 LLM 预算。
set -uo pipefail
export PATH="$PATH:$HOME/go/bin"
REPO=/home/victor/base/git/temp/ruflo/claude-go
NS=claude-go
CLUSTER=${CLAUDE_GO_E2E_CLUSTER:-dbk8s-e2e}   # kind 集群名; 独立 namespace, 不碰其它项目

# ⚠️ 镜像标签**每轮唯一** —— 这一条是本脚本最重要的正确性前提, 曾静默失效过一整轮:
#
# 固定用 `claude-go:e2e` 时, 重建镜像并 ctr import 之后 Deployment 的 spec **一个字节
# 都没变**, 于是 `kubectl apply` 是空操作、`rollout status` 立刻对着**上一轮的旧 Pod**
# 报 "successfully rolled out"。表现极其隐蔽: 全部验收照常跑、大部分照常通过, 而验的是
# 一个 18 小时前的二进制 —— 一次**通过了却什么都没证明**的 E2E 比失败危险得多。
# (2026-07-25 实测: Pod startTime=07-25T11:40Z, 镜像 Created=07-26T06:24Z, 差 18.8h。
#  此前那次"worker Deployment 是陈旧的"也是同一个根因。)
#
# 唯一标签让 spec 必变 ⇒ 新 ReplicaSet ⇒ 新 Pod ⇒ 跑的一定是刚构建的二进制。
# 下面第 4 步还有一道**硬断言**兜底 (Pod 启动时刻必须晚于本轮构建时刻)。
STAMP=$(date +%s)
IMG=claude-go:e2e-$STAMP
BUILD_EPOCH=$STAMP
# 团队名也每轮唯一: 同名团队在 PVC 上是**持久**的, 复用会撞 "团队已存在" (create 失败),
# 且更糟的是 run 会走 resume 路径 —— 有可能直接返回上一轮的产出而**一次 LLM 都不调**,
# 于是"真实 LLM 端到端"变成了读缓存。唯一名字保证每轮都是冷跑。
T_MONO=e2e-real-$STAMP
T_GW=e2e-gw-$STAMP
T_ACT=e2e-act-$STAMP
T_GRAPH=e2e-graph-$STAMP
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
ok "镜像已导入 ${CLUSTER}-control-plane（标签 $IMG，每轮唯一）"
# 清掉往轮的 e2e 镜像: 每轮一个唯一标签, 不清就是在共享 kind 节点上无界涨盘。
# 只匹配 `claude-go:e2e-*` 且排除本轮 —— 绝不碰其它项目的镜像 (同集群有 databases 等)。
docker exec "${CLUSTER}-control-plane" sh -c "
  ctr -n k8s.io images ls -q 2>/dev/null \
    | grep -E '^docker\.io/library/claude-go:e2e-[0-9]+\$' \
    | grep -v ':e2e-$STAMP\$' \
    | xargs -r ctr -n k8s.io images rm >/dev/null 2>&1" 2>/dev/null || true
# 宿主侧同理 (docker build 每轮留一个 tag)。
docker images --format '{{.Repository}}:{{.Tag}}' 2>/dev/null \
  | grep -E '^claude-go:e2e-[0-9]+$' | grep -v ":e2e-$STAMP\$" \
  | xargs -r docker rmi -f >/dev/null 2>&1 || true

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

# —— 硬断言: 跑的必须是本轮构建的二进制 ——
# 唯一镜像标签已经让"复用旧 Pod"在结构上不可能, 但这条断言仍要有: 它是**直接观测**
# (Pod 启动时刻 vs 本轮构建时刻), 与"我相信 apply 会触发滚动"这种推理无关。
# 一次静默跑旧二进制的 E2E 会通过大半验收却什么都没证明 —— 那种失败方式必须被断言挡住,
# 不能靠下一个人记得检查。
assert_fresh_pod() {
  local pod=$1 what=$2
  local st img
  st=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.startTime}' 2>/dev/null)
  img=$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].image}' 2>/dev/null)
  local st_epoch
  st_epoch=$(date -d "$st" +%s 2>/dev/null || echo 0)
  if [ "$img" != "$IMG" ]; then
    bad "$what 用的镜像是 $img，不是本轮的 $IMG —— 在验一个陈旧二进制"
    return 1
  fi
  if [ "$st_epoch" -lt "$BUILD_EPOCH" ]; then
    bad "$what 启动于 $st，早于本轮构建（$(date -d @"$BUILD_EPOCH" -u +%FT%TZ)）—— 在验一个陈旧二进制"
    return 1
  fi
  ok "$what 跑的是本轮二进制（启动 $st / 镜像 $IMG）"
}
assert_fresh_pod "$POD" "单体 Pod"

step "5/13 验收①: ConfigMap 真被读取 + 真 key 生效"
sleep 3
# ⚠️ **读全量日志, 不用 --tail** —— 这两条断言要找的是**启动首几行**
# (`[modelconfig] provider "kimi" 的 apiKey 取自环境变量` 落在第 1 行与第 10 行),
# 而 Pod 启动到这里已经能刷出 300+ 行 (28 个技能 / 飞书重连 / 各子系统就绪)。
# 用 --tail=200 时窗口早已滑过那两行 ⇒ 第一条断言**假红**;
# 更糟的是第二条 (`grep -qiE '占位|placeholder'`) 会因为"窗口里没有占位字样"而**假绿** ——
# 一条在错误窗口上做否定断言的检查, 恒真, 属"许愿式测试"。
LOG=$(kubectl -n "$NS" logs "$POD" 2>/dev/null)
if grep -q 'apiKey 取自环境变量 KIMI_API_KEY' <<<"$LOG"; then
  ok "provider key 来自 Secret 注入的环境变量（<PROVIDER>_API_KEY 覆盖生效）"
else
  bad "未见 key 覆盖日志 —— ConfigMap 或环境变量覆盖没生效"
fi
# 否定断言先自证窗口有效: 拿不到 modelconfig 行就说明日志没读到, 此时"没有占位字样"
# 不构成证据 —— 必须报不确定而不是记通过。
if ! grep -q '\[modelconfig\]' <<<"$LOG"; then
  bad "日志里连 [modelconfig] 都没有 —— 无法判断是否落占位 provider（窗口无效, 不记通过）"
elif grep -qiE '占位|placeholder' <<<"$LOG"; then
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
  localhost:18080/api/actions/team/create/$T_MONO \
  -H 'Content-Type: application/json' \
  -d '{"workflow":"research","objective":"用三句话说明事件溯源(event sourcing)与快照恢复的区别"}' \
  | head -c 300; echo
kubectl -n "$NS" exec "$POD" -- curl -s -X POST \
  localhost:18080/api/actions/team/run/$T_MONO -H 'Content-Type: application/json' -d '{}' \
  | head -c 200; echo
echo "  等待团队完成（真实 LLM，research=fanout 共 6 阶段，最长 12 分钟）..."
for i in $(seq 144); do
  ST=$(kubectl -n "$NS" exec "$POD" -- curl -s localhost:18080/api/teams/$T_MONO \
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
# 双引号: 单引号里 $T_MONO 不会展开, 会变成 `ls /data/.claude-go/teams//`
# —— 那会列出**整个 teams 目录**并照常返回 0, 于是"团队目录存在"恒真 (又一条许愿式断言)。
kubectl -n "$NS" exec "$POD" -- sh -c "ls /data/.claude-go/teams/$T_MONO/ 2>/dev/null" \
  | grep -q . && ok "团队目录有产物 ($T_MONO)" || bad "无团队目录或目录为空 ($T_MONO)"

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
# 这三个也要断言新鲜: **上一轮真机验证就是在这里被咬过** —— worker Deployment 是陈旧的
# (还跑着旧回显桩、caps 为空), 而 rollout status 照常报成功。
for p in "$GWPOD" "$CTLPOD"; do
  [ -n "$p" ] && assert_fresh_pod "$p" "分布式 Pod $p"
done
# cwd 档位 (design/02 §3.3) 真通电的三条实证, 缺一条就是装饰品:
#   ① worker 上报 ws:pvc / wsvol:<卷名> 标签 (控制面靠它路由, 队列按标签过滤)
#   ② 控制面日志声明了档位 (说明 --workspace-mode 真被读到)
#   ③ 控制面与 worker 看到的 /workspace 是**同一份数据** (控制面写, worker 读)
WPOD=$(kubectl -n "$NS" get pod -l app=claude-go-worker -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -n "$WPOD" ] && assert_fresh_pod "$WPOD" "worker Pod $WPOD"
if [ -n "$CTLPOD" ] && [ -n "$WPOD" ]; then
  kubectl -n "$NS" exec "$CTLPOD" -- curl -s localhost:18080/cluster/workers 2>/dev/null \
    | grep -q 'wsvol:claude-go-teams' \
    && ok "worker 上报 cwd 档位标签 (ws:pvc + wsvol:claude-go-teams)" \
    || bad "worker 未上报档位标签 —— 档位路由没通电"
  kubectl -n "$NS" logs "$CTLPOD" --tail=400 2>/dev/null | grep -q 'cwd 档位=pvc' \
    && ok "控制面已按 pvc 档位派活" || bad "控制面未声明 cwd 档位"
  # ⚠️ 变量名**不能**叫 STAMP: 那是顶上那个"每轮唯一标签/团队名"用的全局量,
  # 在这里覆写等于让后续步骤的镜像与团队名判据换了个值 (曾是潜伏的踩坑点)。
  WSPROBE="ws-probe-$RANDOM"
  kubectl -n "$NS" exec "$CTLPOD" -- sh -c "echo $WSPROBE > /workspace/.ws-e2e-probe" >/dev/null 2>&1
  kubectl -n "$NS" exec "$WPOD" -- sh -c 'cat /workspace/.ws-e2e-probe 2>/dev/null' 2>/dev/null \
    | grep -q "$WSPROBE" \
    && ok "控制面与 worker 的 /workspace 是同一份数据 (RWX 真共享)" \
    || bad "控制面与 worker 的 /workspace 不是同一份数据 —— 远程产码门禁看不见 (风险④)"
  kubectl -n "$NS" exec "$CTLPOD" -- rm -f /workspace/.ws-e2e-probe >/dev/null 2>&1
fi
if [ -n "$CTLPOD" ]; then
  kubectl -n "$NS" exec "$CTLPOD" -- curl -s -X POST \
    localhost:18080/api/actions/team/create/$T_GW -H 'Content-Type: application/json' \
    -d '{"workflow":"research","objective":"一句话说明什么是租约(lease)"}' >/dev/null 2>&1
  kubectl -n "$NS" exec "$CTLPOD" -- curl -s -X POST \
    localhost:18080/api/actions/team/run/$T_GW -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1
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
  localhost:18080/api/actions/team/create/$T_ACT -H 'Content-Type: application/json' \
  -d '{"workflow":"research","objective":"验证动作队列消费"}' 2>/dev/null)
echo "  动作回包: $(head -c 200 <<<"$ACT")"

step "11/13 验收⑦⑧: 图引擎 + 拦截器链 + 预算台账（design/01 §4.10）"
# 单体那轮跑的是 pipeline 路径（灰度默认关），这里显式开灰度跑一轮图引擎，
# 才能验证拦截器链与预算事件真的落盘。
# 顺带把本轮最后三项里两个**默认关**的开关也打开 —— 它们默认关是刻意的 (自动判失败会
# 杀掉合法长阶段 / 会改写盘频率), 但"默认关"不等于"没验过": 不在真集群开一次跑一轮,
# 就只有单测证据。搭本来就要跑的这轮图引擎真实 LLM, 不额外花一次 LLM 预算。
kubectl -n "$NS" set env deploy/claude-go-monolith \
  CLAUDE_GO_GRAPH_ENGINE=1 CLAUDE_GO_GRAPH_WATCHDOG=1 CLAUDE_GO_BOARD_WATCH=1 >/dev/null 2>&1
kubectl -n "$NS" rollout status deploy/claude-go-monolith --timeout=180s >/dev/null 2>&1 \
  && ok "已切到图引擎灰度（GRAPH_ENGINE=1 + WATCHDOG=1 观测档 + BOARD_WATCH=1）" || bad "切灰度后 rollout 失败"
GPOD=$(kubectl -n "$NS" get pod -l app=claude-go-monolith -o jsonpath='{.items[0].metadata.name}')
assert_fresh_pod "$GPOD" "图引擎 Pod"
kubectl -n "$NS" exec "$GPOD" -- curl -s -X POST \
  localhost:18080/api/actions/team/create/$T_GRAPH -H 'Content-Type: application/json' \
  -d '{"workflow":"research","objective":"两句话说明有界展开为什么是必需的"}' >/dev/null 2>&1
kubectl -n "$NS" exec "$GPOD" -- curl -s -X POST \
  localhost:18080/api/actions/team/run/$T_GRAPH -H 'Content-Type: application/json' -d '{}' >/dev/null 2>&1
echo "  等待图引擎团队完成（真实 LLM，最长 12 分钟）..."
GST=""
for i in $(seq 144); do
  GST=$(kubectl -n "$NS" exec "$GPOD" -- curl -s localhost:18080/api/teams/$T_GRAPH \
        | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null)
  case "$GST" in
    completed|delivered_with_remediation) ok "图引擎团队状态=$GST"; break ;;
    failed) bad "图引擎团队 failed"; break ;;
  esac
  sleep 5
done
JRN=/data/.claude-go/teams/$T_GRAPH/graph-journal/journal.jsonl
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

# —— 本轮最后三项在真集群的验收（三项默认关，这里已显式开启两项）——
# ① 黑板 Watch 生产订阅方: 收尾日志带 applied/board_drops 两个数。
#    只断言"有订阅方跑过"而不断言 applied>0: 阶段落 <stage>-status 的时机与订阅方
#    起停有真实竞争, 硬要求条数会造出一个偶发红的断言 (第二类假测试)。
GLOG=$(kubectl -n "$NS" logs "$GPOD" 2>/dev/null)
if grep -q '黑板进度订阅收尾' <<<"$GLOG"; then
  ok "黑板 Watch 订阅方真跑过: $(grep -o '黑板进度订阅收尾.*' <<<"$GLOG" | tail -1 | head -c 160)"
else
  bad "未见黑板订阅收尾日志 —— CLAUDE_GO_BOARD_WATCH=1 没生效（§4.11 订阅方未通电）"
fi
# ② 图级 watchdog 观测档: **断言它没有误报**。一个跑得正常的团队若被判停滞,
#    说明阈值/进展时钟接错了 —— 而观测档下这种错误不会让团队失败, 只会静默污染 journal,
#    正是需要断言来抓的形态。同时确认它真的在跑 (fail 档才会干预, 这里不该出现干预)。
GS=$(kubectl -n "$NS" exec "$GPOD" -- sh -c "grep -c graph.stalled $JRN 2>/dev/null")
if [ "${GS:-0}" -eq 0 ]; then
  ok "图级 watchdog 未误报停滞（正常完成的团队不该被判停滞）"
else
  bad "图级 watchdog 误报 $GS 次停滞 —— 阈值或进展时钟接错了"
fi
# ③ ExternalHook 适配器: 镜像里没有用户配置的外部 hook ⇒ findHooks 恒空 ⇒ 一条事件都不该有。
#    这一条**验的正是"零成本"那一半**（没配 hook 的部署日志逐字节不变）；
#    "配了 hook 能看见事件"那一半由单测 + 真实日志钉住, 本脚本不造 hook 配置。
if grep -q 'ext:' <<<"$GLOG"; then
  bad "未配置外部 hook 却出现 ext: 事件 —— 空 hook 时应零事件"
else
  ok "未配外部 hook ⇒ 零 ext: 事件（ExternalHook 的零成本那一半；另一半见单测）"
fi

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
