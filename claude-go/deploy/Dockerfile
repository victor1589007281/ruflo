# claude-go 运行镜像 (design/02 §四 部署形态 T0/T2/T3 共用)。
# 约定: 二进制在宿主机构建, 镜像只做打包 —— 避免容器内拉依赖受代理影响,
# 与本机既有构建纪律一致:
#   go build -o deploy/claude-go-linux        ./cmd/claude-go
#   go build -o deploy/claude-go-worker-linux ./cmd/claude-go-worker
FROM debian:bookworm-slim

# git 是 cwd 的 git 档位 (design/02 §3.3) 的硬依赖: worker 每阶段 fetch/commit/push,
# 控制面 ff-only 同步。不装它, git 档位会在第一条 git 命令上失败。
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl \
    && rm -rf /var/lib/apt/lists/*

COPY claude-go-linux /usr/local/bin/claude-go
# 真执行体的 worker 进程 (cmd/claude-go/main.go 的 worker 子命令仍是回显桩,
# 分布式部署必须用这个二进制, 见 cmd/claude-go-worker/main.go 文件头)。
COPY claude-go-worker-linux /usr/local/bin/claude-go-worker

# 状态目录: 单体模式挂 PVC/emptyDir; 分布式模式 worker 无状态 (design/02 §3.3)
ENV CLAUDE_GO_STATE_DIR=/data/.claude-go
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/claude-go"]
# 子命令由部署清单指定: serve | worker | llm-gateway | feishu
