import type { Vault } from "obsidian";

/** 单条坏链：源文件路径与无法解析的 wikilink 锚文本 */
export interface BrokenLink {
  sourcePath: string;
  linkText: string;
}

/** 本地 Vault 维基的 lint 结果（与 API 返回结构对齐便于合并展示） */
export interface LintResult {
  brokenLinks: BrokenLink[];
  orphanedPages: string[];
  totalPages: number;
  totalRaw: number;
}

const WIKI_LINK_RE = /\[\[([^\]|#]+)(?:#[^\]|]+)?(?:\|[^\]]+)?\]\]/g;

function normalizeWikiTarget(name: string): string {
  return name.trim().replace(/\\/g, "/");
}

/** 将 wikilink 解析为「无扩展名的 basename」用于与 vault 内 md 匹配 */
function linkToBasename(linkText: string): string {
  const n = normalizeWikiTarget(linkText);
  const base = n.split("/").pop() ?? n;
  return base.replace(/\.md$/i, "");
}

async function collectMarkdownPaths(vault: Vault, rootPrefix: string): Promise<string[]> {
  const files = vault.getMarkdownFiles();
  const prefix = rootPrefix.replace(/^\/+|\/+$/g, "");
  if (!prefix) return files.map((f) => f.path);
  return files.filter((f) => f.path === prefix || f.path.startsWith(prefix + "/")).map((f) => f.path);
}

/**
 * 扫描 wiki/ 与 raw/ 下的 Markdown：
 * - brokenLinks：[[...]] 在 wiki 命名空间中找不到对应页面
 * - orphanedPages：wiki/ 中从未被其他 wiki 页面 wikilink 指向的条目
 */
export class LintRunner {
  async run(vault: Vault): Promise<LintResult> {
    const wikiPaths = await collectMarkdownPaths(vault, "wiki");
    const rawPaths = await collectMarkdownPaths(vault, "raw");

    const pathByBasenameLower = new Map<string, string>();
    for (const p of wikiPaths) {
      const seg = p.split("/").pop() ?? p;
      const base = seg.replace(/\.md$/i, "");
      pathByBasenameLower.set(base.toLowerCase(), p);
    }

    const brokenLinks: BrokenLink[] = [];
    const incomingToWiki = new Map<string, Set<string>>();
    for (const p of wikiPaths) {
      incomingToWiki.set(p, new Set());
    }

    for (const filePath of wikiPaths) {
      const file = vault.getAbstractFileByPath(filePath);
      if (!file || !("extension" in file) || file.extension !== "md") continue;
      const content = await vault.read(file as import("obsidian").TFile);
      let m: RegExpExecArray | null;
      WIKI_LINK_RE.lastIndex = 0;
      while ((m = WIKI_LINK_RE.exec(content)) !== null) {
        const rawLink = m[1];
        if (!rawLink) continue;
        const key = linkToBasename(rawLink).toLowerCase();
        const resolved = pathByBasenameLower.get(key);
        if (!resolved) {
          brokenLinks.push({ sourcePath: filePath, linkText: rawLink });
          continue;
        }
        const set = incomingToWiki.get(resolved);
        if (set && resolved !== filePath) {
          set.add(filePath);
        }
      }
    }

    const orphanedPages: string[] = [];
    for (const p of wikiPaths) {
      const inc = incomingToWiki.get(p);
      if (inc && inc.size === 0) orphanedPages.push(p);
    }

    return {
      brokenLinks,
      orphanedPages,
      totalPages: wikiPaths.length,
      totalRaw: rawPaths.length,
    };
  }
}
