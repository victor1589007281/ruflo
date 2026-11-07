# gf-voice 宿主机转发器: 127.0.0.1:38795 / 100.111.70.76:38795 (tailnet) → kind 节点上的 voice-service
#
# 为什么需要它: voice-service 是 hostNetwork Pod, 端口绑在 kind 节点容器自己的网络命名空间
# (172.18.0.9:38795), 宿主机回环和 tailscale 网卡都够不着 —— 手机走 tailnet 直连就必须
# 有这一跳。转发器顺带解决 kind 重建后节点 IP 漂移: 每 5s 重新解析节点 IP, 漂移自动跟。
#
# 部署: systemd 用户单元 gf-voice-fwd.service (Restart=always, 独立 cgroup,
# 不受 webapp-service 重启连坐 —— 见 gameforge cgroup kill-zone 教训)。
# 鉴权不在这里做: 透传字节流, token 校验仍在 voice-service 内。
#
# 只解析, 不缓存连接 —— 后端是单副本, 每条连接现场建, 后端重启当下连接自然断,
# 客户端重试即可。
import asyncio
import subprocess

LISTEN_HOST = "0.0.0.0"
LISTEN_PORT = 38795
NODE_CONTAINER = "hakit-control-plane"   # kind 节点容器名
BACKEND_PORT = 38795
RESOLVE_INTERVAL_S = 5


async def resolve_backend() -> str | None:
    """docker inspect 拿 kind 节点容器 IP。集群重建后容器重建, IP 可能变, 所以每次重新问。"""
    try:
        out = await asyncio.to_thread(
            subprocess.run,
            # kind 节点在名为 kind 的自定义网络上, .NetworkSettings.IPAddress 为空,
            # IP 在 Networks.*.IPAddress 下; range 取第一个网络, 与网络名解耦
            ["docker", "inspect", NODE_CONTAINER, "--format",
             "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"],
            capture_output=True, text=True, timeout=10,
        )
        ip = out.stdout.strip()
        return f"{ip}:{BACKEND_PORT}" if out.returncode == 0 and ip else None
    except Exception:
        return None


async def pipe(reader: asyncio.StreamReader, writer: asyncio.StreamWriter):
    try:
        while True:
            chunk = await reader.read(65536)
            if not chunk:
                break
            writer.write(chunk)
            await writer.drain()
    except Exception:
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def handle(client_reader: asyncio.StreamReader, client_writer: asyncio.StreamWriter, backend_addr):
    backend = backend_addr[0]
    if backend is None:
        client_writer.close()
        return
    try:
        backend_reader, backend_writer = await asyncio.open_connection(*backend.rsplit(":", 1))
    except Exception:
        client_writer.close()
        return
    await asyncio.gather(
        pipe(client_reader, backend_writer),
        pipe(backend_reader, client_writer),
    )


async def main():
    backend_addr = [await resolve_backend()]
    print(f"[gf-voice-fwd] listen {LISTEN_HOST}:{LISTEN_PORT} → backend {backend_addr[0]}", flush=True)

    async def refresh():
        while True:
            await asyncio.sleep(RESOLVE_INTERVAL_S)
            backend_addr[0] = await resolve_backend()

    asyncio.create_task(refresh())
    server = await asyncio.start_server(
        lambda r, w: handle(r, w, backend_addr), LISTEN_HOST, LISTEN_PORT,
    )
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    asyncio.run(main())
