#!/usr/bin/env bash
# 保存 GitHub Token 到 claude-go 配置文件 (不使用环境变量)。
#
# 写两处:
#   1) 宿主 ~/.claude-go/config/config.json 的 search.githubToken (0600)
#      claude-go 的 run/chat/feishu 与 search-mcp-server 都从这份配置读取,
#      下一进程/下一次运行生效, 不需要重启 webapp。
#   2) 集群 agents-config / agents-config-worker 的同名字段
#      Pod 把 ConfigMap 挂在 /etc/claude-go/config.json; control/worker 热加载
#      约 60s, search-mcp 只在启动时读一次, 所以脚本最后按顺序滚一遍。
#
# Token 只从 stdin 读取; 不回显、不进命令行参数 (ps 可见)、不写日志。
# 用法:
#   gh auth token | deploy/set-github-token.sh
#   deploy/set-github-token.sh < token.txt
set -euo pipefail

CLUSTER=${CLUSTER:-hakit}
NS=${NS:-agents}
MAPS=${MAPS:-"agents-config agents-config-worker"}

if [ -t 0 ]; then
  echo "用法: gh auth token | $0   (token 只从 stdin 读, 不接受环境变量)" >&2
  exit 2
fi

TOKEN=$(cat)
TOKEN=${TOKEN%$'\r'}
if [ -z "$TOKEN" ]; then
  echo "token 为空, 拒绝写入" >&2
  exit 2
fi

echo "==> 写入宿主 config.json 的 search.githubToken"
# 用 -c 传脚本、管道传 token: 如果写成 `python3 - <<'PY'`, stdin 会被 heredoc
# 占去当脚本, 脚本里的 sys.stdin.read() 只能读到 EOF, token 永远写不进去。
printf '%s' "$TOKEN" | python3 -I -c '
import json
import os
import sys
import tempfile

token = sys.stdin.read().strip()
if not token:
    raise SystemExit("token 为空, 拒绝写入")
if any(c.isspace() for c in token):
    raise SystemExit("token 含空白字符, 拒绝写入")

path = os.path.expanduser("~/.claude-go/config/config.json")
data = {}
if os.path.exists(path):
    with open(path) as f:
        data = json.load(f)
search = data.setdefault("search", {})
search["githubToken"] = token

# 原子写 + 0600: 先写同目录临时文件, 再 rename, 避免半个文件被读到。
d = os.path.dirname(path)
os.makedirs(d, exist_ok=True)
fd, tmp = tempfile.mkstemp(dir=d, prefix=".config.", suffix=".json")
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
print("    已写入", path, "search.githubToken")
'
unset TOKEN

echo "==> 同步到 ConfigMap: $MAPS"
# 在 Python 里读刚写好的配置并通过 --patch-file 下发: token 不经过 shell 变量,
# 也不出现在 kubectl 的命令行参数里。patch 文件 0600, 用完立即删除。
python3 -I - "$CLUSTER" "$NS" $MAPS <<'PY'
import json
import os
import subprocess
import sys
import tempfile

cluster, ns, maps = sys.argv[1], sys.argv[2], sys.argv[3:]
with open(os.path.expanduser("~/.claude-go/config/config.json")) as f:
    token = (json.load(f).get("search") or {}).get("githubToken", "")
if not token:
    raise SystemExit("本地配置里没有 search.githubToken")

for name in maps:
    got = subprocess.run(
        ["kubectl", "--context", cluster, "-n", ns, "get", "cm", name, "-o", "json"],
        check=True, capture_output=True, text=True,
    )
    cm = json.loads(got.stdout)
    data = cm.get("data") or {}
    raw = data.get("config.json")
    if raw is None:
        raise SystemExit(f"ConfigMap {name} 没有 config.json")
    inner = json.loads(raw)
    inner.setdefault("search", {})["githubToken"] = token

    patch = {"data": {**data, "config.json": json.dumps(inner, ensure_ascii=False, indent=2)}}
    fd, tmp = tempfile.mkstemp(prefix="cm-patch-", suffix=".json")
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w") as f:
            json.dump(patch, f, ensure_ascii=False)
        subprocess.run(
            ["kubectl", "--context", cluster, "-n", ns, "patch", "cm", name,
             "--type=merge", "--patch-file", tmp],
            check=True, stdout=subprocess.DEVNULL,
        )
    finally:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
    print("    已更新", name, "search.githubToken")
PY

# search-mcp 先就绪, 控制面再启动: 控制面启动时只连一次 MCP, 失败不重试,
# 同时滚会连到 Terminating 的旧 Pod。
echo "==> 滚动重启以加载新配置"
if kubectl --context "$CLUSTER" -n "$NS" get deploy claude-go-mcp >/dev/null 2>&1; then
  kubectl --context "$CLUSTER" -n "$NS" rollout restart deploy/claude-go-mcp >/dev/null
  kubectl --context "$CLUSTER" -n "$NS" rollout status deploy/claude-go-mcp --timeout=180s
fi
for d in claude-go-control claude-go-worker claude-go-dashboard; do
  if kubectl --context "$CLUSTER" -n "$NS" get deploy "$d" >/dev/null 2>&1; then
    kubectl --context "$CLUSTER" -n "$NS" rollout restart "deploy/$d" >/dev/null
  fi
done
for d in claude-go-control claude-go-worker claude-go-dashboard; do
  kubectl --context "$CLUSTER" -n "$NS" get deploy "$d" >/dev/null 2>&1 && \
    kubectl --context "$CLUSTER" -n "$NS" rollout status "deploy/$d" --timeout=180s
done

echo "==> 完成 (token 未回显)"
