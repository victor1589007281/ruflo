#!/usr/bin/env bash
# 保存 GitHub Token, 供 claude-go 搜索源提升 GitHub API 限额。
#
# 写两处:
#   1) 宿主 ~/.claude-go/settings.json 的 env.GITHUB_TOKEN (0600)
#      claude-go 每次 buildEngine 都会读这份 settings 并 ApplyEnv, 所以
#      webapp 调试台/CLI 的下一次运行立即生效, 不需要重启 webapp。
#   2) 集群 agents/claude-go-search-github Secret
#      control/worker/search-mcp 通过 secretKeyRef 注入; 控制面只在启动时
#      连一次 MCP, 所以先滚 mcp 等就绪, 再滚 control/worker。
#
# Token 只从 $GITHUB_TOKEN 或 stdin 读取; 不回显、不写临时文件、不进日志。
# 用法:
#   GITHUB_TOKEN=ghp_xxx deploy/set-github-token.sh
#   gh auth token | deploy/set-github-token.sh
set -euo pipefail

CLUSTER=${CLUSTER:-hakit}
NS=${NS:-agents}
SECRET=${SECRET:-claude-go-search-github}

TOKEN=${GITHUB_TOKEN:-}
if [ -z "$TOKEN" ] && [ ! -t 0 ]; then
  TOKEN=$(cat)
fi
TOKEN=${TOKEN%$'\r'}
if [ -z "$TOKEN" ]; then
  echo "用法: GITHUB_TOKEN=... $0  或  gh auth token | $0" >&2
  exit 2
fi
case "$TOKEN" in
  *[[:space:]]*)
    echo "token 含空白字符, 拒绝写入" >&2
    exit 2
    ;;
esac

echo "==> 写入宿主 ~/.claude-go/settings.json"
printf '%s' "$TOKEN" | python3 -I - <<'PY'
import json
import os
import sys
import tempfile

token = sys.stdin.read().strip()
path = os.path.expanduser("~/.claude-go/settings.json")
data = {}
if os.path.exists(path):
    with open(path) as f:
        data = json.load(f)
env = data.setdefault("env", {})
env["GITHUB_TOKEN"] = token

# 原子写 + 0600: 先写同目录临时文件, 再 rename, 避免半个文件被读到。
d = os.path.dirname(path)
os.makedirs(d, exist_ok=True)
fd, tmp = tempfile.mkstemp(dir=d, prefix=".settings.", suffix=".json")
try:
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
        f.write("\n")
    os.replace(tmp, path)
except Exception:
    try:
        os.unlink(tmp)
    except FileNotFoundError:
        pass
    raise
print("    已写入", path, "env.GITHUB_TOKEN")
PY

echo "==> 写入 Secret $NS/$SECRET"
printf '%s' "$TOKEN" | kubectl --context "$CLUSTER" -n "$NS" create secret generic "$SECRET" \
  --from-file=GITHUB_TOKEN=/dev/stdin \
  --dry-run=client -o yaml | kubectl --context "$CLUSTER" -n "$NS" apply -f - >/dev/null
echo "    已写入 Secret"

# 注入顺序: mcp 先就绪, 控制面再启动。
# 控制面启动时只连一次 MCP, 失败不重试; 同时滚会连到 Terminating 的旧 Pod。
echo "==> 注入 GITHUB_TOKEN"
kubectl --context "$CLUSTER" -n "$NS" set env deploy/claude-go-mcp \
  --from=secret/"$SECRET" --keys=GITHUB_TOKEN >/dev/null
kubectl --context "$CLUSTER" -n "$NS" rollout status deploy/claude-go-mcp --timeout=180s
for d in claude-go-control claude-go-worker claude-go-dashboard; do
  if kubectl --context "$CLUSTER" -n "$NS" get deploy "$d" >/dev/null 2>&1; then
    kubectl --context "$CLUSTER" -n "$NS" set env "deploy/$d" \
      --from=secret/"$SECRET" --keys=GITHUB_TOKEN >/dev/null
  fi
done
for d in claude-go-control claude-go-worker claude-go-dashboard; do
  kubectl --context "$CLUSTER" -n "$NS" get deploy "$d" >/dev/null 2>&1 && \
    kubectl --context "$CLUSTER" -n "$NS" rollout status "deploy/$d" --timeout=180s
done

echo "==> 完成 (token 未回显)"
