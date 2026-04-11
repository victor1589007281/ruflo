import type { ClaudeWikiSettings } from "./settings";
import type { HttpClient } from "isomorphic-git/http/node";

type NodeFS = typeof import("fs");
type GitModule = typeof import("isomorphic-git");

function unwrapGitMod(m: unknown): GitModule {
  const x = m as Record<string, unknown> & { default?: unknown };
  if (x && typeof x.pull === "function") return x as unknown as GitModule;
  const d = x?.default;
  if (d && typeof (d as GitModule).pull === "function") return d as GitModule;
  throw new Error("无法加载 isomorphic-git");
}

/**
 * 使用 isomorphic-git + Node http 在桌面端同步任意路径仓库。
 * 移动端无 Node `fs`：调用方应检测并提示用户，而非假定 Git 可用。
 */
export class GitSync {
  private intervalId: ReturnType<typeof setInterval> | null = null;
  private readonly repoPath: string;
  private readonly settings: ClaudeWikiSettings;

  constructor(repoPath: string, settings: ClaudeWikiSettings) {
    this.repoPath = repoPath.replace(/\/+$/, "");
    this.settings = settings;
  }

  /** 桌面端返回 Node fs；移动端返回 null */
  static tryGetNodeFS(): NodeFS | null {
    try {
      // Obsidian 桌面端为 Electron，可通过 require 拿到 Node fs
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const req = typeof (globalThis as any).require === "function" ? (globalThis as any).require : null;
      if (!req) return null;
      return req("fs") as NodeFS;
    } catch {
      return null;
    }
  }

  private async loadGit(): Promise<GitModule> {
    const m = await import("isomorphic-git");
    return unwrapGitMod(m as GitModule | { default: GitModule });
  }

  /** isomorphic-git 需要带 `request` 方法的 HttpClient；兼容 default / 命名导出 */
  private async loadHttpClient(): Promise<HttpClient> {
    const m = await import("isomorphic-git/http/node");
    const rec = m as { request?: unknown; default?: { request?: unknown } };
    const reqFn =
      (typeof rec.request === "function" ? rec.request : null) ??
      (rec.default && typeof rec.default.request === "function" ? rec.default.request : null);
    if (!reqFn) throw new Error("无法加载 isomorphic-git/http/node");
    return { request: reqFn as HttpClient["request"] };
  }

  private assertFs(fs: NodeFS | null): asserts fs is NodeFS {
    if (!fs) {
      throw new Error("当前环境无 Node fs，无法在 Vault 外执行 Git（请使用桌面端并配置仓库路径）。");
    }
  }

  private author() {
    return {
      name: this.settings.commitAuthorName,
      email: this.settings.commitAuthorEmail,
    };
  }

  /** 克隆远程仓库到本地路径 */
  async clone(remoteUrl: string): Promise<void> {
    const fs = GitSync.tryGetNodeFS();
    this.assertFs(fs);
    const git = await this.loadGit();
    const http = await this.loadHttpClient();
    await git.clone({
      fs,
      http,
      dir: this.repoPath,
      url: remoteUrl,
      singleBranch: true,
      depth: 1,
    });
  }

  async pull(): Promise<void> {
    const fs = GitSync.tryGetNodeFS();
    this.assertFs(fs);
    const git = await this.loadGit();
    const http = await this.loadHttpClient();
    const dir = this.repoPath;
    const ref = this.settings.defaultBranch || "main";
    await git.fetch({ fs, http, dir, singleBranch: true });
    await git.pull({
      fs,
      http,
      dir,
      ref,
      author: this.author(),
      fastForwardOnly: false,
    });
  }

  /**
   * 暂存全部变更、提交并 push。
   * 若无变更则跳过提交，仍尝试 push。
   */
  async push(): Promise<void> {
    const fs = GitSync.tryGetNodeFS();
    this.assertFs(fs);
    const git = await this.loadGit();
    const http = await this.loadHttpClient();
    const dir = this.repoPath;
    const ref = this.settings.defaultBranch || "main";

    await git.add({ fs, dir, filepath: "." });

    try {
      await git.commit({
        fs,
        dir,
        message: `chore(wiki): sync from Obsidian ${new Date().toISOString()}`,
        author: this.author(),
      });
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      // 无变更时 isomorphic-git 会拒绝提交
      if (!/nothing to commit|No changes/i.test(msg)) throw e;
    }

    await git.push({
      fs,
      http,
      dir,
      remote: "origin",
      ref,
    });
  }

  autoSync(intervalMs: number): void {
    this.stopAutoSync();
    if (intervalMs < 5_000) return;
    this.intervalId = setInterval(() => {
      void this.safePullPush();
    }, intervalMs);
  }

  stopAutoSync(): void {
    if (this.intervalId !== null) {
      clearInterval(this.intervalId);
      this.intervalId = null;
    }
  }

  private async safePullPush(): Promise<void> {
    try {
      await this.pull();
      await this.push();
    } catch {
      /* 定时同步失败静默；用户可通过「同步」命令查看错误 */
    }
  }

  /**
   * 比较本地分支与 origin/<branch> 的可达提交数，估算 ahead / behind。
   * 需已存在远程跟踪引用；否则返回 0,0。
   */
  async getStatus(): Promise<{ ahead: number; behind: number }> {
    const fs = GitSync.tryGetNodeFS();
    if (!fs) return { ahead: 0, behind: 0 };
    const git = await this.loadGit();
    const dir = this.repoPath;
    const branch = this.settings.defaultBranch || "main";
    const remoteRef = `refs/remotes/origin/${branch}`;

    let localOid: string;
    let remoteOid: string;
    try {
      localOid = await git.resolveRef({ fs, dir, ref: branch });
      remoteOid = await git.resolveRef({ fs, dir, ref: remoteRef });
    } catch {
      return { ahead: 0, behind: 0 };
    }

    if (localOid === remoteOid) return { ahead: 0, behind: 0 };

    const countReachable = async (tip: string, notReachableFrom: string): Promise<number> => {
      const commits = await git.log({
        fs,
        dir,
        ref: tip,
      });
      const exclude = new Set<string>();
      try {
        const ex = await git.log({ fs, dir, ref: notReachableFrom });
        for (const c of ex) exclude.add(c.oid);
      } catch {
        /* 忽略 */
      }
      return commits.filter((c) => !exclude.has(c.oid)).length;
    };

    const ahead = await countReachable(localOid, remoteOid);
    const behind = await countReachable(remoteOid, localOid);
    return { ahead, behind };
  }
}
