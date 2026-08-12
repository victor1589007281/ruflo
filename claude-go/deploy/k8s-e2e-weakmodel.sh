#!/usr/bin/env bash
# 用法: bash deploy/k8s-e2e-weakmodel.sh [模型子集: lfm|gemma|all] [任务子集: smoke|full]
# 方案三 (手册 13.3) 真集群在线验证: 冻结权重的 harness 增强 (schema 校验修复环 /
# tool_result 后处理 / 工具掩码 / 规划指引) 对本地弱模型的实际效果 A/B。
#
# 与 deploy/k8s-e2e.sh 的关系: 复用其镜像构建/kind 导入/新鲜度断言纪律,
# 但模型走**集群内 Ollama** (宿主防火墙 DROP 全部 bridge 流量, 直连宿主 11434 不可行,
# 见 deploy/k8s/ollama.yaml 头注释)。
#
# A/B 设计: 同一 Deployment 内逐 run 用 CLAUDE_GO_WEAK_MODEL=0|1 切换基线/增强,
# 确定性 checker (文件内容/测试通过/轮数上限) 判分, 不依赖模型自评。
set -uo pipefail
REPO=/home/victor/base/git/temp/ruflo/claude-go
NS=claude-go
CLUSTER=${CLAUDE_GO_E2E_CLUSTER:-dbk8s-e2e}
MODELS=${1:-all}        # lfm | gemma | all
TASKS=${2:-full}        # smoke(T1 only) | full
export KUBECONFIG=${KUBECONFIG:-$HOME/.kube/config}

STAMP=$(date +%s)
IMG=claude-go:e2e-wml-$STAMP
BUILD_EPOCH=$STAMP
FAILED=0; PASSED=0
RESULTS_FILE=/tmp/wml-e2e-results-$STAMP.jsonl
step() { echo; echo "━━━ $* ━━━"; }
ok()   { echo "  ✅ $*"; PASSED=$((PASSED+1)); }
bad()  { echo "  ❌ $*"; FAILED=$((FAILED+1)); }
record() { echo "$*" >> "$RESULTS_FILE"; }

K="kubectl --context kind-$CLUSTER -n $NS"

# ━━━ 0. 工具函数 ━━━
newest_pod() {
  local app=$1 i name
  for i in $(seq 20); do
    name=$($K get pod -l "app=$app" --field-selector=status.phase=Running \
      -o go-template='{{range .items}}{{.status.startTime}} {{.metadata.name}} {{(index .spec.containers 0).image}}{{"\n"}}{{end}}' 2>/dev/null \
      | awk -v img="$IMG" '$3==img' | sort -r | head -1 | awk '{print $2}')
    [ -n "$name" ] && { echo "$name"; return 0; }
    sleep 2
  done
  return 1
}

# ━━━ 1. 构建 + 镜像 + 导入 kind ━━━
step "1/6 构建二进制与镜像 ($IMG)"
cd "$REPO" || exit 1
CGO_ENABLED=0 go build -trimpath -o deploy/claude-go-linux ./cmd/claude-go || exit 1
CGO_ENABLED=0 go build -trimpath -o deploy/claude-go-worker-linux ./cmd/claude-go-worker || exit 1
docker build -q -t "$IMG" -f deploy/Dockerfile deploy/ >/dev/null || exit 1
docker save "$IMG" | docker exec -i "${CLUSTER}-control-plane" ctr -n k8s.io images import - >/dev/null || exit 1
ok "镜像已导入 kind (唯一标签, 防验到陈旧二进制)"

# ━━━ 2. 集群内 Ollama + 模型装载 (幂等) ━━━
step "2/6 部署集群内 Ollama 并装载模型"
$K apply -f "$REPO/deploy/k8s/ollama.yaml" >/dev/null
$K rollout status deploy/claude-go-ollama --timeout=300s >/dev/null || { bad "ollama rollout 超时"; exit 1; }
OPOD=$($K get pod -l app=claude-go-ollama --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
ok "ollama pod=$OPOD"

load_model() { # $1=注册名 $2=宿主 GGUF 路径 $3=额外 Modelfile 行
  local name=$1 gguf=$2 extra=$3
  if $K exec "$OPOD" -- ollama list 2>/dev/null | grep -q "^$name"; then
    ok "模型已在集群内: $name (跳过拷贝)"
    return 0
  fi
  echo "  装载 $name ← $gguf (docker cp 进 kind 节点, 免 API server 传输)"
  docker cp "$gguf" "${CLUSTER}-control-plane:/var/ollama-weakmodel/files/" || { bad "docker cp $name 失败"; return 1; }
  local base; base=$(basename "$gguf")
  $K exec "$OPOD" -- sh -c "printf 'FROM /var/ollama/files/%s\n%b\n' '$base' '$extra' > /tmp/Modelfile.$name && ollama create '$name' -f /tmp/Modelfile.$name" >/dev/null 2>&1 \
    || { bad "ollama create $name 失败"; return 1; }
  $K exec "$OPOD" -- ollama list | grep -q "^$name" && ok "模型已注册: $name" || { bad "模型注册后不可见: $name"; return 1; }
}

mkdir -p /tmp/wml-models && docker exec "${CLUSTER}-control-plane" mkdir -p /var/ollama-weakmodel/files
if [ "$MODELS" = "lfm" ] || [ "$MODELS" = "all" ]; then
  # LFM 的原始 GGUF 已被清理, 从宿主 ollama blob 仓直接取 (ollama show --modelfile 可证)。
  load_model "lfm2.5:2.6b-q4_k_m" "/mnt/data/ollama_models/blobs/sha256-79fdf00351b46cf26f020aead28d01889886be87c55fa0eb907e6f9b00bfee14" 'PARAMETER num_ctx 16384'
fi
if [ "$MODELS" = "gemma" ] || [ "$MODELS" = "all" ]; then
  load_model "gemma4-weak:q8_0" "/home/victor/gguf/gemma4-v2/gemma4-v2-Q8_0.gguf" 'RENDERER gemma4\nPARSER gemma4\nTEMPLATE {{ .Prompt }}\nPARAMETER temperature 1.0\nPARAMETER top_p 0.95\nPARAMETER top_k 64\nPARAMETER repeat_penalty 1.1\nPARAMETER num_ctx 16384'
fi

# ━━━ 3. 部署单体 + ConfigMap (ollama provider) ━━━
# 顺序纪律: monolith.yaml 内嵌 kimi 版 ConfigMap, 必须先 apply yaml 再建 ConfigMap,
# 否则内嵌版后写覆盖 ollama 版 (第三轮实测踩中: run 解析不到 ollama provider 报缺 API key)。
step "3/6 部署单体并覆盖 ollama 配置"
sed -e "s|image: localhost:5000/claude-go:latest|image: $IMG|" \
    -e "s|imagePullPolicy: IfNotPresent|imagePullPolicy: Never|" \
    "$REPO/deploy/k8s/monolith.yaml" | $K apply -f - >/dev/null
$K create configmap claude-go-config --from-literal=config.json='{
  "stateDir": "/data/.claude-go",
  "cwd": "/data",
  "providers": {
    "ollama": {
      "name": "ollama",
      "baseUrl": "http://claude-go-ollama:11434/v1",
      "apiKey": "ollama",
      "models": {
        "ollama:lfm2.5:2.6b-q4_k_m": { "maxTokens": 2048, "maxTurns": 10, "promptCacheMode": "auto", "contextWindow": 16384, "maxParallel": 1, "rpm": 120, "firstTokenTimeoutSec": 600, "callTimeoutSec": 2400 },
        "ollama:gemma4-weak:q8_0":  { "maxTokens": 2048, "maxTurns": 10, "promptCacheMode": "auto", "contextWindow": 16384, "maxParallel": 1, "rpm": 60,  "firstTokenTimeoutSec": 600, "callTimeoutSec": 2400 }
      }
    }
  },
  "ai": { "modelAlias": "ollama:lfm2.5:2.6b-q4_k_m", "maxTurns": 10, "maxTokens": 2048 },
  "wiki": { "enabled": true, "apiPort": 18080 }
}' --dry-run=client -o yaml | $K apply -f - >/dev/null
$K rollout restart deploy/claude-go-monolith >/dev/null 2>&1 # ConfigMap 变更不会自动滚动
$K rollout status deploy/claude-go-monolith --timeout=240s >/dev/null || { bad "monolith rollout 超时"; exit 1; }
POD=$(newest_pod claude-go-monolith) || { bad "找不到本轮镜像的单体 Pod"; exit 1; }
ok "monolith pod=$POD (本轮镜像)"
$K exec "$POD" -- sh -c 'curl -s --max-time 8 http://claude-go-ollama:11434/api/tags | head -c 200' | grep -q 'models' \
  && ok "monolith → 集群内 ollama 连通" || { bad "monolith 连不上集群内 ollama"; exit 1; }

# ━━━ 4. 任务电池 (A/B: CLAUDE_GO_WEAK_MODEL=0|1) ━━━
step "4/6 运行任务电池 (结果落 $RESULTS_FILE)"

run_case() { # $1=model_alias $2=task_id $3=wml(0|1) $4=prompt $5=checker_cmd $6=max_turns
  local alias=$1 tid=$2 wml=$3 prompt=$4 checker=$5 turns=$6
  local dir=/data/wml/$tid-$wml
  $K exec "$POD" -- sh -c "rm -rf $dir && mkdir -p $dir" >/dev/null 2>&1
  case "$tid" in
    t2) $K exec "$POD" -- sh -c "printf 'def add(a, b):\n    return a - b  # BUG: should be plus\n' > $dir/calc.py && printf 'import calc\nassert calc.add(2,3)==5\nassert calc.add(-1,1)==0\nprint(\"TESTS_OK\")\n' > $dir/calc_test.py" ;;
    t3) $K exec "$POD" -- sh -c "echo 17 > $dir/a.txt; echo 23 > $dir/b.txt" ;;
  esac
  local t0 t1 out rc wall
  t0=$(date +%s)
  out=$($K exec "$POD" -- sh -c "CLAUDE_GO_WEAK_MODEL=$wml timeout 1500 claude-go run '$prompt' --final-only --model '$alias' --max-turns $turns --cwd $dir" 2>/tmp/wml-stderr-$tid-$wml.log)
  rc=$?
  t1=$(date +%s); wall=$((t1-t0))
  echo "$out" > /tmp/wml-out-$tid-$wml.log # 留存最终文本供失败模式分析
  local pass=0
  if [ $rc -eq 0 ] && $K exec "$POD" -- sh -c "cd $dir && $checker" >/dev/null 2>&1; then pass=1; fi
  local sv lp jr banner
  sv=$(grep -c 'schema_validate' /tmp/wml-stderr-$tid-$wml.log 2>/dev/null || true)
  lp=$(grep -ci 'loop' /tmp/wml-stderr-$tid-$wml.log 2>/dev/null || true)
  jr=$(grep -c 'JSONRepair' /tmp/wml-stderr-$tid-$wml.log 2>/dev/null || true)
  banner=$(grep -c 'weakmodel' /tmp/wml-stderr-$tid-$wml.log 2>/dev/null || true)
  record "{\"model\":\"$alias\",\"task\":\"$tid\",\"wml\":$wml,\"rc\":$rc,\"pass\":$pass,\"wall_s\":$wall,\"schema_validate_logs\":$sv,\"loop_logs\":$lp,\"jsonrepair_logs\":$jr,\"wml_banner\":$banner}"
  echo "  [$alias][$tid][wml=$wml] rc=$rc pass=$pass wall=${wall}s sv=$sv loop=$lp jr=$jr"
}

# 任务定义: 全部确定性 checker, 零模型自评 (手册 13.3.1 纪律)
T1_P='在当前目录创建文件 hello.txt，内容恰好为一行: hello-weak-model。创建后用 Shell 运行 cat hello.txt 确认，然后简要结束。'
T1_C='grep -qx "hello-weak-model" hello.txt'
# prompt 内绝不嵌套引号 (嵌套引号经 sh -c 两层展开变形, 曾把 t2 带偏:
# 模型反复理解坏命令而整轮不编辑 —— 提示词缺陷, 非被测物缺陷)。
T2_P='当前目录的 calc.py 里 add 函数有 bug（写成了减法）。请先用 Read 查看 calc.py，用 StrReplace 把减法修复为加法，然后用 Shell 运行 python3 calc_test.py，看到输出 TESTS_OK 后结束。'
T2_C='python3 calc_test.py'
T3_P='当前目录有 a.txt 和 b.txt 各含一个整数。读取两个文件，把两数乘积（只要数字）写入 result.txt，然后结束。'
T3_C='grep -qx "391" result.txt'
T4_P='找出当前目录下所有扩展名为 .nonexistent-xyz 的文件并列出；一个都没有的话，明确回答"没有"。不要重复运行相同的命令。'
T4_C='true'

battery() { # $1=alias $2=任务id列表(空格分隔)
  local alias=$1 tasks=$2
  for wml in 0 1; do
    for t in $tasks; do
      local p c mt
      case "$t" in
        t1) p="$T1_P"; c="$T1_C"; mt=8 ;;
        t2) p="$T2_P"; c="$T2_C"; mt=10 ;;
        t3) p="$T3_P"; c="$T3_C"; mt=10 ;;
        t4) p="$T4_P"; c="$T4_C"; mt=6 ;;
      esac
      run_case "$alias" "$t" $wml "$p" "$c" "$mt"
    done
  done
}

if [ "$MODELS" = "lfm" ] || [ "$MODELS" = "all" ]; then battery "ollama:lfm2.5:2.6b-q4_k_m" "t1 t2 t3 t4"; fi
if [ "$MODELS" = "gemma" ] || [ "$MODELS" = "all" ]; then battery "ollama:gemma4-weak:q8_0" "${GEMMA_TASKS:-t1 t2}"; fi

# ━━━ 5. 汇总 ━━━
step "5/6 A/B 汇总"
python3 - "$RESULTS_FILE" <<'EOF'
import json, sys, collections
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
agg = collections.defaultdict(lambda: {"n":0,"pass":0,"wall":0,"sv":0})
for r in rows:
    k = (r["model"].split(":")[1], "plan3" if r["wml"]==1 else "baseline")
    a = agg[k]; a["n"]+=1; a["pass"]+=r["pass"]; a["wall"]+=r["wall_s"]; a["sv"]+=r["schema_validate_logs"]
print(f"{'model':<22}{'mode':<10}{'runs':>5}{'pass':>6}{'pass%':>7}{'avg_wall_s':>11}{'sv_events':>10}")
for (m, mode), a in sorted(agg.items()):
    print(f"{m:<22}{mode:<10}{a['n']:>5}{a['pass']:>6}{100*a['pass']/a['n']:>6.0f}%{a['wall']/a['n']:>10.0f}{a['sv']:>10}")
EOF

step "6/6 结论"
echo "  明细: $RESULTS_FILE"
echo "  通过 $PASSED 项 / 未通过 $FAILED 项"
[ $FAILED -eq 0 ]
