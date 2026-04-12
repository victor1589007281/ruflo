import { ItemView, Notice, WorkspaceLeaf, normalizePath, type TFile } from "obsidian";
import type { ClaudeWikiPluginInstance } from "./settings";

export const WIKI_DASHBOARD_VIEW_TYPE = "claude-wiki-dashboard";

/**
 * 百科仪表盘：统计 raw/wiki 数量、上次同步、最近抓取，并提供快捷跳转与 lint 入口。
 */
export class WikiDashboardView extends ItemView {
  constructor(leaf: WorkspaceLeaf, private readonly plugin: ClaudeWikiPluginInstance) {
    super(leaf);
  }

  getViewType(): string {
    return WIKI_DASHBOARD_VIEW_TYPE;
  }

  getDisplayText(): string {
    return "Claude Wiki 仪表盘";
  }

  getIcon(): string {
    return "book-open";
  }

  async onOpen(): Promise<void> {
    this.plugin.dashboardViewRef = this;
    await this.render();
  }

  async onClose(): Promise<void> {
    if (this.plugin.dashboardViewRef === this) {
      this.plugin.dashboardViewRef = null;
    }
  }

  /** 外部触发重绘（同步、抓取、lint 后调用） */
  async render(): Promise<void> {
    const container = this.contentEl;
    const root = container;
    root.empty();
    root.addClass("claude-wiki-dashboard");

    const h = root.createEl("h2", { text: "Claude Wiki" });
    h.style.marginTop = "0";

    const stats = root.createDiv({ cls: "claude-wiki-dashboard__stats" });
    const vault = this.plugin.app.vault;
    const rawCount = vault.getMarkdownFiles().filter((f) => f.path === "raw" || f.path.startsWith("raw/")).length;
    const wikiCount = vault.getMarkdownFiles().filter((f) => f.path === "wiki" || f.path.startsWith("wiki/")).length;

    const lastSync =
      this.plugin.lastSyncTime != null
        ? new Date(this.plugin.lastSyncTime).toLocaleString()
        : "尚未同步";

    stats.createEl("p", { text: `Raw 文章：${rawCount}` });
    stats.createEl("p", { text: `Wiki 页面：${wikiCount}` });
    stats.createEl("p", { text: `上次同步：${lastSync}` });

    const recent = this.plugin.recentIngests.slice(0, 8);
    const recentEl = root.createDiv({ cls: "claude-wiki-dashboard__recent" });
    recentEl.createEl("h3", { text: "最近抓取" });
    if (recent.length === 0) {
      recentEl.createEl("p", { text: "暂无记录", attr: { style: "opacity:0.7" } });
    } else {
      const ul = recentEl.createEl("ul");
      for (const r of recent) {
        const li = ul.createEl("li");
        li.createEl("span", { text: `${new Date(r.at).toLocaleString()} — ${r.title}` });
        li.createEl("br");
        li.createEl("a", { text: r.url, href: r.url });
      }
    }

    const nav = root.createDiv({ cls: "claude-wiki-dashboard__nav" });
    nav.createEl("h3", { text: "快捷跳转" });
    const navRow = nav.createDiv({ attr: { style: "display:flex;gap:8px;flex-wrap:wrap" } });
    const openFolderBtn = (label: string, folder: string) => {
      navRow.createEl("button", { text: label }).addEventListener("click", () => {
        void this.openFirstInFolder(folder);
      });
    };
    openFolderBtn("打开 raw/", "raw");
    openFolderBtn("打开 wiki/", "wiki");

    const actions = root.createDiv({ cls: "claude-wiki-dashboard__actions" });
    actions.createEl("h3", { text: "操作" });
    const row = actions.createDiv({ attr: { style: "display:flex;gap:8px;flex-wrap:wrap" } });

    row.createEl("button", { text: "运行 Lint" }).addEventListener("click", () => {
      void this.plugin.runVaultLint().then(
        (r) => {
          new Notice(
            `Lint：坏链 ${r.brokenLinks.length}，孤立页 ${r.orphanedPages.length}，Wiki ${r.totalPages}，Raw ${r.totalRaw}`
          );
          void this.render();
        },
        (e) => new Notice(`Lint 失败：${e instanceof Error ? e.message : String(e)}`)
      );
    });

    row.createEl("button", { text: "抓取链接" }).addEventListener("click", () => {
      this.plugin.openIngestUrlModal();
    });

    row.createEl("button", { text: "同步 Wiki（Git）" }).addEventListener("click", () => {
      void this.plugin.syncWikiRepo().then(
        () => {
          new Notice("同步完成");
          void this.render();
        },
        (e) => new Notice(`同步失败：${e instanceof Error ? e.message : String(e)}`)
      );
    });

    row.createEl("button", { text: "整理 Wiki（全量）" }).addEventListener("click", () => {
      void this.plugin.organizeWiki("full").then(
        (r) => {
          new Notice(`全量整理完成：更新 ${r.updated_pages} 个页面`);
          void this.render();
        },
        (e) => new Notice(`整理失败：${e instanceof Error ? e.message : String(e)}`)
      );
    });

    row.createEl("button", { text: "整理 Wiki（增量）" }).addEventListener("click", () => {
      void this.plugin.organizeWiki("incremental").then(
        (r) => {
          new Notice(`增量整理完成：更新 ${r.updated_pages} 个页面`);
          void this.render();
        },
        (e) => new Notice(`整理失败：${e instanceof Error ? e.message : String(e)}`)
      );
    });
  }

  /**
   * 在编辑器中打开某目录下的首个 Markdown；若无则创建占位文件便于定位目录。
   * 业务上：引导用户进入 raw/ 或 wiki/ 工作区。
   */
  private async openFirstInFolder(folder: string): Promise<void> {
    const vault = this.plugin.app.vault;
    const prefix = normalizePath(folder.replace(/^\/+|\/+$/g, ""));
    const mds = vault.getMarkdownFiles().filter((f) => f.path === prefix || f.path.startsWith(prefix + "/"));
    if (mds.length > 0) {
      const leaf = this.plugin.app.workspace.getLeaf(false);
      await leaf.openFile(mds[0] as TFile);
      return;
    }

    const abstract = vault.getAbstractFileByPath(prefix);
    if (!abstract) {
      await vault.createFolder(prefix).catch(() => undefined);
    }

    const placeholder = normalizePath(`${prefix}/.claude-wiki-placeholder.md`);
    let target = vault.getAbstractFileByPath(placeholder) as TFile | null;
    if (!target) {
      target = await vault.create(placeholder, `<!-- 占位：可删除。用于在导航中定位 ${prefix}/ 目录。 -->\n`);
    }
    const leaf = this.plugin.app.workspace.getLeaf(false);
    await leaf.openFile(target);
    new Notice(`${prefix}/ 下暂无其他笔记，已打开占位文件`);
  }
}
