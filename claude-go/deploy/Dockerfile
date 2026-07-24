# claude-go 运行镜像 (design/02 §四 部署形态 T0/T2/T3 共用)。
# 约定: 二进制在宿主机构建 (go build -o deploy/claude-go-linux ./cmd/claude-go),
# 镜像只做打包 —— 避免容器内拉依赖受代理影响, 与本机既有构建纪律一致。
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git curl \
    && rm -rf /var/lib/apt/lists/*

COPY claude-go-linux /usr/local/bin/claude-go

# 状态目录: 单体模式挂 PVC/emptyDir; 分布式模式 worker 无状态 (design/02 §3.3)
ENV CLAUDE_GO_STATE_DIR=/data/.claude-go
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/claude-go"]
# 子命令由部署清单指定: serve | worker | llm-gateway | feishu
