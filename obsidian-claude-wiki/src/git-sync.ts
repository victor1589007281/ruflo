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
 * 将 SSH Git URL 转换为 HTTPS URL。
 * isomorphic-git 不支持 SSH 协议，必须转换。
 *
 * 例: git@github.com:user/repo.git → https://github.com/user/repo.git
 * 例: ssh://git@github.com/user/repo.git → https://github.com/user/repo.git
 */
function sshToHttps(url: string): string {
  // ssh://git@github.com/user/repo.git
  const sshProtoRe = /^ssh:\/\/(?:[^@]+@)?([^/]+)\/(.*)/;
  const sshProtoMatch = url.match(sshProtoRe);
  if (sshProtoMatch) {
    return `https://${sshProtoMatch[1]}/${sshProtoMatch[2]}`;
  }
  // git@github.com:user/repo.git
  const scpRe = /^(?:[^@]+@)?([^:]+):(.+)/;
  const scpMatch = url.match(scpRe);
  if (scpMatch && !url.includes("://")) {
    return `https://${scpMatch[1]}/${scpMatch[2]}`;
  }
  return url;
}

/**
 * 判断是否为 SSH 格式的 URL。
 */
function isSshUrl(url: string): boolean {
  return /^(ssh:\/\/|git@)/.test(url) || (/^[^:/]+:/.test(url) && !url.includes("://"));
}

/**
 * 使用 isomorphic-git + Node http 在桌面端同步任意路径仓库。
 *
 * SSH URL 自动转换为 HTTPS（isomorphic-git 不支持 SSH 协议）。
 * 移动端无 Node `fs`：使用 LightningFS 内存文件系统作为 fallback。
 */
export class GitSync {
  private intervalId: ReturnType<typeof setInterval> | null = null;
  private readonly repoPath: string;
  private readonly settings: ClaudeWikiSettings;
  private lightFs: unknown | null = null;

  constructor(repoPath: string, settings: ClaudeWikiSettings) {
    this.repoPath = repoPath.replace(/\/+$/, "");
    this.settings = settings;
  }

  /**
   * 获取文件系统 — 桌面端用 Node fs，移动端用 LightningFS。
   * LightningFS 是 isomorphic-git 官方推荐的浏览器端 fs 兼容层。
   */
  async getFS(): Promise<NodeFS> {
    const nodeFs = GitSync.tryGetNodeFS();
    if (nodeFs) return nodeFs;

    // 移动端 fallback: 使用 @nicolo-ribaudo/chokidar-2 自带的 fs 兼容层
    // 或使用 LightningFS (已作为 isomorphic-git 推荐方案)
    if (this.lightFs) return this.lightFs as NodeFS;

    try {
      // 尝试加载 LightningFS (@isomorphic-git/lightning-fs)
      const LightningFS = (await import("@isomorphic-git/lightning-fs" as string)).default;
      this.lightFs = new LightningFS("claude-wiki");
      return this.lightFs as unknown as NodeFS;
    } catch {
      throw new Error(
        "当前环境无 Node fs 且 LightningFS 不可用。" +
        "桌面端 Obsidian 会自动使用 Node fs；" +
        "移动端请确保 @isomorphic-git/lightning-fs 已安装。"
      );
    }
  }

  /** 桌面端返回 Node fs；移动端返回 null */
  static tryGetNodeFS(): NodeFS | null {
    try {
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

  private async loadHttpClient(): Promise<HttpClient> {
    try {
      // 桌面端: isomorphic-git/http/node
      const m = await import("isomorphic-git/http/node");
      const rec = m as { request?: unknown; default?: { request?: unknown } };
      const reqFn =
        (typeof rec.request === "function" ? rec.request : null) ??
        (rec.default && typeof rec.default.request === "function" ? rec.default.request : null);
      if (reqFn) return { request: reqFn as HttpClient["request"] };
    } catch {
      /* 移动端可能不支持 node http */
    }

    // Fallback: 使用 isomorphic-git/http/web (浏览器端)
    try {
      const m = await import("isomorphic-git/http/web" as string);
      const rec = m as { request?: unknown; default?: { request?: unknown } };
      const reqFn =
        (typeof rec.request === "function" ? rec.request : null) ??
        (rec.default && typeof rec.default.request === "function" ? rec.default.request : null);
      if (reqFn) return { request: reqFn as HttpClient["request"] };
    } catch {
      /* fallback also failed */
    }

    throw new Error("无法加载 HTTP 客户端（isomorphic-git/http/node 或 web）");
  }

  private author() {
    return {
      name: this.settings.commitAuthorName,
      email: this.settings.commitAuthorEmail,
    };
  }

  /**
   * 解析 remote URL，自动将 SSH 转换为 HTTPS。
   */
  private resolveUrl(url: string): string {
    if (isSshUrl(url)) {
      console.warn(
        `[Claude Wiki] SSH URL 不被 isomorphic-git 支持，自动转换为 HTTPS: ${url} → ${sshToHttps(url)}`
      );
      return sshToHttps(url);
    }
    return url;
  }

  /** 克隆远程仓库到本地路径 */
  async clone(remoteUrl: string): Promise<void> {
    const fs = await this.getFS();
    const git = await this.loadGit();
    const http = await this.loadHttpClient();
    const resolvedUrl = this.resolveUrl(remoteUrl);
    await git.clone({
      fs,
      http,
      dir: this.repoPath,
      url: resolvedUrl,
      singleBranch: true,
      depth: 1,
    });
  }

  async pull(): Promise<void> {
    const fs = await this.getFS();
    const git = await this.loadGit();
    const http = await this.loadHttpClient();
    const dir = this.repoPath;
    const ref = this.settings.defaultBranch || "main";

    // 读取并转换 remote URL
    let remoteUrl: string | undefined;
    try {
      const remotes = await git.listRemotes({ fs, dir });
      const origin = remotes.find(r => r.remote === "origin");
      if (origin && isSshUrl(origin.url)) {
        remoteUrl = sshToHttps(origin.url);
      }
    } catch { /* 忽略 */ }

    const fetchOpts: Parameters<GitModule["fetch"]>[0] = { fs, http, dir, singleBranch: true };
    if (remoteUrl) fetchOpts.url = remoteUrl;
    await git.fetch(fetchOpts);

    const pullOpts: Parameters<GitModule["pull"]>[0] = {
      fs, http, dir, ref,
      author: this.author(),
      fastForwardOnly: false,
    };
    if (remoteUrl) pullOpts.url = remoteUrl;
    await git.pull(pullOpts);
  }

  async push(): Promise<void> {
    const fs = await this.getFS();
    const git = await this.loadGit();
    const http = await this.loadHttpClient();
    const dir = this.repoPath;
    const ref = this.settings.defaultBranch || "main";

    await git.add({ fs, dir, filepath: "." });

    try {
      await git.commit({
        fs, dir,
        message: `chore(wiki): sync from Obsidian ${new Date().toISOString()}`,
        author: this.author(),
      });
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      if (!/nothing to commit|No changes/i.test(msg)) throw e;
    }

    // 读取并转换 remote URL
    let remoteUrl: string | undefined;
    try {
      const remotes = await git.listRemotes({ fs, dir });
      const origin = remotes.find(r => r.remote === "origin");
      if (origin && isSshUrl(origin.url)) {
        remoteUrl = sshToHttps(origin.url);
      }
    } catch { /* 忽略 */ }

    const pushOpts: Parameters<GitModule["push"]>[0] = {
      fs, http, dir, remote: "origin", ref,
    };
    if (remoteUrl) pushOpts.url = remoteUrl;
    await git.push(pushOpts);
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

  async getStatus(): Promise<{ ahead: number; behind: number }> {
    let fs: NodeFS;
    try {
      fs = await this.getFS();
    } catch {
      return { ahead: 0, behind: 0 };
    }
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
      const commits = await git.log({ fs, dir, ref: tip });
      const exclude = new Set<string>();
      try {
        const ex = await git.log({ fs, dir, ref: notReachableFrom });
        for (const c of ex) exclude.add(c.oid);
      } catch { /* ignore */ }
      return commits.filter((c) => !exclude.has(c.oid)).length;
    };

    const ahead = await countReachable(localOid, remoteOid);
    const behind = await countReachable(remoteOid, localOid);
    return { ahead, behind };
  }
}
