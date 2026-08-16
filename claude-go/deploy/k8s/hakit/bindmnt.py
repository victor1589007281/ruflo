#!/usr/bin/env python3
# bindmnt.py —— 把宿主目录挂载进运行中的容器 mount ns。
# 用 open_tree(2) 克隆宿主挂载 fd → setns 进容器 → move_mount(2) 按 fd 附着。
# 全程 fd 驱动, 不经路径解析 (util-linux mount 会把 /proc/<pid>/root 魔法链接
# realpath 成容器内同名路径, 静默自绑定 —— 本脚本就是为绕开它)。
# 必须单线程: setns 进 mount ns 要求调用进程单线程, 故勿在任何线程中调用。
#
# 用法: sudo python3 bindmnt.py <容器init PID> <宿主源目录> <容器内目标路径>
import ctypes
import os
import sys

SYS_open_tree = 428   # amd64
SYS_move_mount = 429
SYS_setns = 308
AT_FDCWD = -100
OPEN_TREE_CLONE = 0x000001
OPEN_TREE_CLOEXEC = 0x080000
MOVE_MOUNT_F_EMPTY_PATH = 0x000004

libc = ctypes.CDLL(None, use_errno=True)


def main():
    if len(sys.argv) != 4:
        sys.exit(f"用法: {sys.argv[0]} <容器init PID> <宿主源目录> <容器内目标路径>")
    pid, src, dst = sys.argv[1], sys.argv[2].encode(), sys.argv[3].encode()

    # ① 宿主侧克隆源挂载 (此时仍在宿主 mount ns)
    tree_fd = libc.syscall(SYS_open_tree, AT_FDCWD, src,
                           OPEN_TREE_CLONE | OPEN_TREE_CLOEXEC)
    if tree_fd < 0:
        raise OSError(ctypes.get_errno(), f"open_tree({src!r}) 失败")

    # ② 切入容器 mount ns (单线程, 合法)
    ns_fd = os.open(f"/proc/{pid}/ns/mnt", os.O_RDONLY)
    if libc.syscall(SYS_setns, ns_fd, 0) != 0:
        raise OSError(ctypes.get_errno(), "setns 失败")

    # ③ 容器 ns 内按 fd 附着
    if libc.syscall(SYS_move_mount, tree_fd, b"", AT_FDCWD, dst,
                    MOVE_MOUNT_F_EMPTY_PATH) != 0:
        raise OSError(ctypes.get_errno(), f"move_mount({dst!r}) 失败")
    print(f"✅ {src.decode()} 已挂载进容器(ns of pid {pid}) 的 {dst.decode()}")


if __name__ == "__main__":
    main()
