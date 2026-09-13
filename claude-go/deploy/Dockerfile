# claude-go 运行镜像 (design/02 §四 部署形态 T0/T2/T3 共用)。
# 约定: 二进制在宿主机构建, 镜像只做打包 —— 避免容器内拉依赖受代理影响,
# 与本机既有构建纪律一致:
#   go build -o deploy/claude-go-linux          ./cmd/claude-go
#   go build -o deploy/claude-go-worker-linux   ./cmd/claude-go-worker
#   go build -o deploy/anthropic-gateway-linux  ./cmd/anthropic-gateway
FROM debian:bookworm-slim

# git 是 cwd 的 git 档位 (design/02 §3.3) 的硬依赖: worker 每阶段 fetch/commit/push,
# 控制面 ff-only 同步。不装它, git 档位会在第一条 git 命令上失败。
# python3: if-jd-analysis 等团队工作流明确要求用 `python3 json.dump` 写盘产物,
# 不装它会导致写盘步骤在容器内失败。
# golang-go: 引擎级 AutoMode 让模型在容器内"写代码 → go test 验证"闭环;
# 不装 go, 模型写完 .go 文件后无法自测 (实测 exit 127)。
# ripgrep: Grep 工具是 exec rg 的实现, 镜像里没有就直接 "exec: rg: executable not
# found in $PATH" —— 模型每次想搜索都白烧一个 turn (实测 if-question-expand 的
# 一次 run 里就这么废掉一轮)。apt 的包名是 ripgrep, 二进制叫 rg。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl python3 golang-go nodejs openssh-client ripgrep \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -u 1000 -M -s /usr/sbin/nologin claude
# nodejs: IMA 同步适配器 exec node ima_api.cjs (pkg/sync/ima.go)。
# openssh-client: 同步后知识库 git push (ssh remote)。
# useradd: pod 以 runAsUser 1000 运行时, openssh 启动即要 getpwuid(1000) 成功,
# 没有 passwd 条目会直接 "No user exists for uid 1000" 退出, git push 全灭。

# go test 需要可写缓存目录; stateDir 挂载的 /data 够用, 显式指到 /tmp 避免
# 与部署卷权限纠缠。
ENV GOCACHE=/tmp/go-build GOTMPDIR=/tmp

COPY claude-go-linux /usr/local/bin/claude-go
# 真执行体的 worker 进程 (cmd/claude-go/main.go 的 worker 子命令仍是回显桩,
# 分布式部署必须用这个二进制, 见 cmd/claude-go-worker/main.go 文件头)。
COPY claude-go-worker-linux /usr/local/bin/claude-go-worker
# anthropic-gateway: Anthropic 协议翻译/透传网关, 供 gateway 部署使用。
# 打进同一镜像 (而不是再叠一层 FROM claude-go:agentsN-xxx 的私有 tag) ——
# agents 命名空间里 5 个 Deployment 就能共用一个 tag, 重建/回滚只需切一个 tag。
COPY anthropic-gateway-linux /usr/local/bin/anthropic-gateway
# pathfix.py: 老客户端裸路径 /messages → /v1/messages 的改写代理。
# gateway 的启动命令里后台拉起它 (18081 对外 → 127.0.0.1:18085 真网关),
# 见 deploy/k8s/hakit/distributed-agents.yaml 里 gateway 那段的 args。
COPY pathfix.py /usr/local/bin/pathfix.py

# 状态目录: 单体模式挂 PVC/emptyDir; 分布式模式 worker 无状态 (design/02 §3.3)
ENV CLAUDE_GO_STATE_DIR=/data/.claude-go
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/claude-go"]
# 子命令由部署清单指定: serve | worker | llm-gateway | feishu
