import { App, Modal, Notice, Plugin, Setting, normalizePath, type TextComponent } from "obsidian";
import { ApiClient } from "./api-client";
import {
  ClaudeWikiSettingTab,
  DEFAULT_SETTINGS,
  type ClaudeWikiPluginApi,
  type ClaudeWikiSettings,
  type WikiIngestRecord,
} from "./settings";
import { GitSync } from "./git-sync";
import { LinkExtractor } from "./link-extractor";
import { LintRunner, type LintResult } from "./lint-runner";
import { WIKI_DASHBOARD_VIEW_TYPE, WikiDashboardView } from "./wiki-dashboard";

/** 与 Obsidian saveData/loadData 对齐的持久化根对象 */
interface ClaudeWikiStoredData {
  settings: ClaudeWikiSettings;
  lastSyncTime: number | null;
  recentIngests: WikiIngestRecord[];
}

/** 简单 URL 输入弹窗，用于「抓取链接到 raw」 */
class IngestUrlModal extends Modal {
  private urlInput: TextComponent | null = null;

  constructor(app: App, private readonly onSubmit: (url: string) => void) {
    super(app);
  }

  onOpen(): void {
    const { contentEl } = this;
    contentEl.empty();
    contentEl.createEl("h2", { text: "抓取网页到 raw/" });
    new Setting(contentEl)
      .setName("页面 URL")
      .addText((t) => {
        this.urlInput = t;
        t.setPlaceholder("https://");
      });
    new Setting(contentEl).addButton((b) =>
      b.setButtonText("开始抓取").onClick(() => {
        const url = this.urlInput?.getValue().trim() ?? "";
        this.onSubmit(url);
        this.close();
      })
    );
  }
}

export default class ClaudeWikiPlugin extends Plugin implements ClaudeWikiPluginApi {
  settings: ClaudeWikiSettings = { ...DEFAULT_SETTINGS };
  lastSyncTime: number | null = null;
  recentIngests: WikiIngestRecord[] = [];
  dashboardViewRef: { render(): Promise<void> } | null = null;

  private gitRunner: GitSync | null = null;
  private readonly linkExtractor = new LinkExtractor();
  private readonly lintRunner = new LintRunner();

  async onload(): Promise<void> {
    const stored = (await this.loadData()) as Partial<ClaudeWikiStoredData> | null;
    this.settings = { ...DEFAULT_SETTINGS, ...(stored?.settings ?? {}) };
    this.lastSyncTime = stored?.lastSyncTime ?? null;
    this.recentIngests = Array.isArray(stored?.recentIngests) ? stored.recentIngests : [];

    this.addSettingTab(new ClaudeWikiSettingTab(this.app, this));

    this.registerView(WIKI_DASHBOARD_VIEW_TYPE, (leaf) => new WikiDashboardView(leaf, this));

    this.addRibbonIcon("book-open", "打开 Claude Wiki 仪表盘", () => {
      void this.openDashboard();
    });

    this.addCommand({
      id: "extract-link-to-raw",
      name: "Extract Link to Raw",
      callback: () => {
        const modal = new IngestUrlModal(this.app, (url) => {
          void this.runExtractToRaw(url);
        });
        modal.open();
      },
    });

    this.addCommand({
      id: "sync-wiki",
      name: "Sync Wiki",
      callback: () => {
        void this.syncWikiRepo().then(
          () => new Notice("Wiki 同步完成"),
          (e) => new Notice(`同步失败：${e instanceof Error ? e.message : String(e)}`)
        );
      },
    });

    this.addCommand({
      id: "open-dashboard",
      name: "Open Dashboard",
      callback: () => void this.openDashboard(),
    });

    this.addCommand({
      id: "run-lint",
      name: "Run Lint",
      callback: () => {
        void this.runVaultLint().then(
          (r) =>
            new Notice(
              `Lint：坏链 ${r.brokenLinks.length}，孤立页 ${r.orphanedPages.length}，Wiki ${r.totalPages}，Raw ${r.totalRaw}`
            ),
          (e) => new Notice(`Lint 失败：${e instanceof Error ? e.message : String(e)}`)
        );
      },
    });

    this.addCommand({
      id: "trigger-wiki-maintenance",
      name: "Trigger Wiki Maintenance",
      callback: () => void this.triggerWikiMaintenance(),
    });

    await this.applyAutoSyncFromSettings();
  }

  onunload(): void {
    this.gitRunner?.stopAutoSync();
    this.gitRunner = null;
    void this.app.workspace.detachLeavesOfType(WIKI_DASHBOARD_VIEW_TYPE);
  }

  async saveSettings(): Promise<void> {
    await this.persistAll();
  }

  private async persistAll(): Promise<void> {
    const payload: ClaudeWikiStoredData = {
      settings: this.settings,
      lastSyncTime: this.lastSyncTime,
      recentIngests: this.recentIngests,
    };
    await this.saveData(payload);
  }

  refreshDashboardView(): void {
    void this.dashboardViewRef?.render();
  }

  async runVaultLint(): Promise<LintResult> {
    const r = await this.lintRunner.run(this.app.vault);
    this.refreshDashboardView();
    return r;
  }

  async runApiLint(): Promise<LintResult | null> {
    const base = this.settings.claudeGoApiUrl?.trim();
    if (!base) return null;
    const client = new ApiClient(base, this.settings.claudeGoApiKey);
    try {
      return await client.triggerLint();
    } catch {
      return null;
    }
  }

  async syncWikiRepo(): Promise<void> {
    const dir = this.settings.gitRepoPath?.trim();
    if (!dir) {
      throw new Error("请先在设置中填写 Git 仓库路径");
    }
    if (!GitSync.tryGetNodeFS()) {
      throw new Error("当前环境不支持 Node fs，无法执行 Git 同步");
    }
    this.gitRunner = new GitSync(dir, this.settings);
    await this.gitRunner.pull();
    await this.gitRunner.push();
    await this.recordSyncSuccess();
    this.refreshDashboardView();
  }

  async recordSyncSuccess(): Promise<void> {
    this.lastSyncTime = Date.now();
    await this.persistAll();
  }

  async recordIngest(entry: WikiIngestRecord): Promise<void> {
    this.recentIngests = [entry, ...this.recentIngests].slice(0, 50);
    await this.persistAll();
    this.refreshDashboardView();
  }

  async openDashboard(): Promise<void> {
    const leaf = this.app.workspace.getLeavesOfType(WIKI_DASHBOARD_VIEW_TYPE)[0];
    if (leaf && leaf.view instanceof WikiDashboardView) {
      this.app.workspace.revealLeaf(leaf);
      await leaf.view.render();
      return;
    }
    const right = this.app.workspace.getRightLeaf(false);
    if (right) {
      await right.setViewState({
        type: WIKI_DASHBOARD_VIEW_TYPE,
        active: true,
      });
      return;
    }
    const leafNew = this.app.workspace.getLeaf("tab");
    await leafNew.setViewState({
      type: WIKI_DASHBOARD_VIEW_TYPE,
      active: true,
    });
  }

  async triggerWikiMaintenance(): Promise<void> {
    const base = this.settings.claudeGoApiUrl?.trim();
    if (!base) {
      new Notice("请配置 Claude-Go API 地址");
      return;
    }
    const client = new ApiClient(base, this.settings.claudeGoApiKey);
    try {
      const status = await client.getStatus();
      const parts: string[] = [];
      if (status.rawCount != null) parts.push(`Raw ${status.rawCount}`);
      if (status.wikiCount != null) parts.push(`Wiki ${status.wikiCount}`);
      if (status.lastSyncAt) parts.push(`服务端上次同步 ${status.lastSyncAt}`);
      if (status.message) parts.push(status.message);
      let lintNote = "";
      try {
        const remoteLint = await client.triggerLint();
        lintNote = `远程 Lint：坏链 ${remoteLint.brokenLinks.length}，孤立 ${remoteLint.orphanedPages.length}`;
      } catch {
        lintNote = "远程 Lint 不可用";
      }
      new Notice([parts.join(" · ") || "已联系服务端", lintNote].join(" | "));
    } catch (e) {
      new Notice(`维护失败：${e instanceof Error ? e.message : String(e)}`);
    }
    this.refreshDashboardView();
  }

  async applyAutoSyncFromSettings(): Promise<void> {
    this.gitRunner?.stopAutoSync();
    this.gitRunner = null;
    if (!this.settings.autoSync) return;
    const dir = this.settings.gitRepoPath?.trim();
    if (!dir || !GitSync.tryGetNodeFS()) {
      return;
    }
    const ms = Math.max(1, this.settings.syncIntervalMinutes) * 60_000;
    this.gitRunner = new GitSync(dir, this.settings);
    this.gitRunner.autoSync(ms);
  }

  private async runExtractToRaw(url: string): Promise<void> {
    if (!/^https?:\/\//i.test(url)) {
      new Notice("请输入以 http(s):// 开头的 URL");
      return;
    }
    try {
      const { title, content } = await this.linkExtractor.extractFromUrl(url);
      const file = await this.linkExtractor.saveToRaw(this.app.vault, title, content, url);
      await this.recordIngest({ url, title, at: Date.now() });
      new Notice(`已保存到 ${normalizePath(file.path)}`);

      const base = this.settings.claudeGoApiUrl?.trim();
      if (base) {
        const client = new ApiClient(base, this.settings.claudeGoApiKey);
        try {
          await client.triggerIngest(url);
        } catch {
          /* 可选：服务端未启动时不阻断本地抓取 */
        }
      }
    } catch (e) {
      new Notice(`抓取失败：${e instanceof Error ? e.message : String(e)}`);
    }
  }
}
