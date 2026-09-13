#!/bin/bash
# claude-go-hakit-net —— hakit kind 节点的宿主侧网络/挂载接线 (幂等)。
# 1) 把宿主 /mnt/data bind 进节点 mount ns (bindmnt, open_tree/move_mount)
# 2) iptables DNAT: 宿主 18080/7777/9234 → 节点 NodePort (serve/dashboard/codeintel)
set -u
NODE=hakit-control-plane
PID=$(docker inspect -f '{{.State.Pid}}' "$NODE" 2>/dev/null) || exit 0
[ -z "$PID" ] || [ "$PID" = "0" ] && exit 0
IP=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$v.IPAddress}}{{end}}' "$NODE")
# 1) bind-mount (节点挂载表已有宿主 /mnt/data 则跳过)
grep -q 'nvme1n1p1 /mnt/data' /proc/$PID/mounts 2>/dev/null || \
  python3 /usr/local/bin/bindmnt.py "$PID" /mnt/data /mnt/data
  nsenter -t "$PID" -m -- mkdir -p /home/victor/base/git/gitee/knowledge
  grep -q "gitee/knowledge" /proc/$PID/mounts 2>/dev/null || python3 /usr/local/bin/bindmnt.py "$PID" /home/victor/base/git/gitee/knowledge /home/victor/base/git/gitee/knowledge
  # claude-go 的团队产物目录: 模型按 workflow 指令把 questions.json 等写这里,
  # 宿主侧 interviewforge 再读回去。不 bind 进节点的话, hostPath 落的是节点容器
  # 自己的空目录, 写入静默丢失(实测 ~/.claude-go/teams 下只有 claude-go 自己写的
  # REPORT.md/blackboard.json, 没有模型脚本写的 questions.json)。
  nsenter -t "$PID" -m -- mkdir -p /home/victor/.claude-go/teams
  grep -q "claude-go/teams" /proc/$PID/mounts 2>/dev/null || python3 /usr/local/bin/bindmnt.py "$PID" /home/victor/.claude-go/teams /home/victor/.claude-go/teams
  nsenter -t "$PID" -m -- mkdir -p /home/victor/.ssh
  grep -q "victor/.ssh" /proc/$PID/mounts 2>/dev/null || python3 /usr/local/bin/bindmnt.py "$PID" /home/victor/.ssh /home/victor/.ssh
sysctl -w net.ipv4.conf.all.route_localnet=1 >/dev/null
iptables -C INPUT -p tcp --dport 11434 -j ACCEPT 2>/dev/null || iptables -A INPUT -p tcp --dport 11434 -j ACCEPT
# 2) 端口转发 (宿主 -> NodePort)
for pair in 18080:31080 7777:31777 9234:31234; do
  hp=${pair%%:*}; np=${pair##*:}
  iptables -t nat -C PREROUTING -p tcp --dport "$hp" -j DNAT --to-destination "$IP:$np" 2>/dev/null || \
    iptables -t nat -A PREROUTING -p tcp --dport "$hp" -j DNAT --to-destination "$IP:$np"
  iptables -t nat -C OUTPUT -p tcp --dport "$hp" -j DNAT --to-destination "$IP:$np" 2>/dev/null || \
    iptables -t nat -A OUTPUT -p tcp --dport "$hp" -j DNAT --to-destination "$IP:$np"
done
iptables -t nat -C POSTROUTING -d "$IP" -j MASQUERADE 2>/dev/null || \
  iptables -t nat -A POSTROUTING -d "$IP" -j MASQUERADE
