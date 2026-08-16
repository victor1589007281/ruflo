#!/usr/bin/env bash
# run-k8s-ab.sh —— 轨 A: 引擎级 A/B 电池 + 台账记录 (手册 7.5)。
#
# 包裹 deploy/k8s-e2e-weakmodel.sh (真集群 kind-dbk8s-e2e, 集群内 Ollama,
# 逐 run CLAUDE_GO_WEAK_MODEL=0|1 切换), 跑完把结果正规化追加进台账。
#
# 用法:
#   bash regression/harness-evo/run-k8s-ab.sh [lfm|gemma|all] [smoke|full] ["变更说明"]
#
# 例: 改了 L2 schema 校验后
#   bash regression/harness-evo/run-k8s-ab.sh all full "L2 schema 修复环重试上限 2→3"
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/../.." && pwd)
LEDGER=${HARNESS_EVO_LEDGER:-$HERE/ledger.jsonl}
MARK=$(date +%s)

bash "$REPO/deploy/k8s-e2e-weakmodel.sh" "${1:-all}" "${2:-full}"
BATTERY_RC=$?

# 找本轮产出的结果文件 (必须比 MARK 新, 防捡到上一轮)
RESULT=""
for f in $(ls -t /tmp/wml-e2e-results-*.jsonl 2>/dev/null); do
  if [ "$(stat -c %Y "$f")" -ge "$MARK" ]; then RESULT=$f; break; fi
done
if [ -z "$RESULT" ]; then
  echo "⚠️ 未找到本轮电池结果文件 (电池 rc=$BATTERY_RC), 台账未追加" >&2
  exit ${BATTERY_RC:-1}
fi

python3 "$HERE/ledger.py" append-k8s "$RESULT" --change "${3:-}" --ledger "$LEDGER"
echo
python3 "$HERE/ledger.py" report --ledger "$LEDGER"
exit $BATTERY_RC
