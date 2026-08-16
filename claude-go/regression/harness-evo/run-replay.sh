#!/usr/bin/env bash
# run-replay.sh —— 轨 B: 单发 replay 评测 + 台账记录 (手册 7.5)。
#
# 跑 evo replay (judge=nil, 只认 Expect/Gate 确定性判分), 逐条结果入台账。
#
# 用法:
#   bash regression/harness-evo/run-replay.sh <模型别名> <prompt正文文件> \
#        [baseline|plan3|candidate] ["变更说明"] [进化对象]
#
# 前置:
#   - claude-go 二进制可用 (CLAUDE_GO_BIN 覆盖, 缺省 PATH 里的 claude-go)
#   - 模型别名已在 providers 注册 (裸模型名的冒号前缀会被剥, 必须走别名, 手册 13.3.9)
#
# 例:
#   bash regression/harness-evo/run-replay.sh ollama:lfm2.5:2.6b-q4_k_m \
#        /tmp/prompt-dev-implement.txt baseline "L5 基线复测"
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
LEDGER=${HARNESS_EVO_LEDGER:-$HERE/ledger.jsonl}
BIN=${CLAUDE_GO_BIN:-claude-go}

ALIAS=${1:?缺模型别名}
BODY=${2:?缺 prompt 正文文件}
MODE=${3:-baseline}
CHANGE=${4:-}
TARGET=${5:-dev/implement}
TASKS=${HARNESS_EVO_TASKS:-$HERE/../evalenv/coding-basic.jsonl}

OUT=/tmp/harness-evo-replay-$(date +%s).jsonl
"$BIN" evo replay --tasks "$TASKS" --target "$TARGET" \
  --body-file "$BODY" --model "$ALIAS" --out "$OUT" || exit 1

python3 "$HERE/ledger.py" append-replay "$OUT" --model "$ALIAS" --mode "$MODE" \
  --change "$CHANGE" --ledger "$LEDGER"
echo
python3 "$HERE/ledger.py" report --ledger "$LEDGER"
