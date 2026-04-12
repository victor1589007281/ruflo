import { App, type Plugin, PluginSettingTab, Setting } from "obsidian";
import type { LintResult } from "./lint-runner";

/** 插件持久化配置：Git 路径、同步策略、Claude-Go API、提交身份 */
export interface ClaudeWikiSettings {
  gitRepoPath: string;
  gitUsername: string;
  gitToken: string;
  autoSync: boolean;
  syncIntervalMinutes: number;
  claudeGoApiUrl: string;
  claudeGoApiKey: string;
  defaultBranch: string;
  commitAuthorName: string;
  commitAuthorEmail: string;
}

export const DEFAULT_SETTINGS: ClaudeWikiSettings = {
  gitRepoPath: "",
  gitUsername: "",
  gitToken: "",
  autoSync: false,
  syncIntervalMinutes: 30,
  claudeGoApiUrl: "http://127.0.0.1:8080",
  claudeGoApiKey: "",
  defaultBranch: "main",
  commitAuthorName: "Obsidian Claude Wiki",
  commitAuthorEmail: "wiki@local",
};

/** 最近抓取记录，供仪表盘与维护流程展示 */
export interface WikiIngestRecord {
  url: string;
  title: string;
  at: number;
}

/**
 * 主插件对外能力（供设置页、仪表盘等引用，避免与 main 循环依赖）。
 */
export interface ClaudeWikiPluginApi {
  settings: ClaudeWikiSettings;
  lastSyncTime: number | null;
  recentIngests: WikiIngestRecord[];
  /** 当前打开的仪表盘视图，用于增量刷新 UI（避免对 wiki-dashboard 的反向类型依赖） */
  dashboardViewRef: { render(): Promise<void> } | null;
  saveSettings(): Promise<void>;
  applyAutoSyncFromSettings(): Promise<void>;
  refreshDashboardView(): void;
  runVaultLint(): Promise<LintResult>;
  runApiLint(): Promise<LintResult | null>;
  syncWikiRepo(): Promise<void>;
  organizeWiki(mode: "full" | "incremental"): Promise<{ updated_pages: number; log: string }>;
  openIngestUrlModal(): void;
  recordSyncSuccess(): Promise<void>;
  recordIngest(entry: WikiIngestRecord): Promise<void>;
  openDashboard(): Promise<void>;
  triggerWikiMaintenance(): Promise<void>;
}

export type ClaudeWikiPluginInstance = Plugin & ClaudeWikiPluginApi;

export class ClaudeWikiSettingTab extends PluginSettingTab {
  plugin: ClaudeWikiPluginInstance;

  constructor(app: App, plugin: ClaudeWikiPluginInstance) {
    super(app, plugin);
    this.plugin = plugin;
  }

  display(): void {
    const { containerEl } = this;
    containerEl.empty();
    containerEl.createEl("h2", { text: "Claude Wiki" });

    new Setting(containerEl)
      .setName("Git 仓库路径")
      .setDesc("桌面端为本地目录绝对路径；移动端无 Node fs 时 Git 同步不可用。")
      .addText((text) =>
        text
          .setPlaceholder("/path/to/wiki-repo")
          .setValue(this.plugin.settings.gitRepoPath)
          .onChange(async (v) => {
            this.plugin.settings.gitRepoPath = v.trim();
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Git 用户名")
      .setDesc("Gitee/GitHub 用户名（用于 HTTPS 认证）")
      .addText((text) =>
        text
          .setPlaceholder("your-username")
          .setValue(this.plugin.settings.gitUsername)
          .onChange(async (v) => {
            this.plugin.settings.gitUsername = v.trim();
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Git Token/密码")
      .setDesc("Gitee 私人令牌 或 GitHub Personal Access Token")
      .addText((text) => {
        text.inputEl.type = "password";
        text.setValue(this.plugin.settings.gitToken);
        return text.onChange(async (v) => {
          this.plugin.settings.gitToken = v;
          await this.plugin.saveSettings();
        });
      });

    new Setting(containerEl)
      .setName("自动同步")
      .setDesc("按下方间隔定时 pull + push（需桌面端可用 fs）。")
      .addToggle((toggle) =>
        toggle.setValue(this.plugin.settings.autoSync).onChange(async (v) => {
          this.plugin.settings.autoSync = v;
          await this.plugin.saveSettings();
          await this.plugin.applyAutoSyncFromSettings();
        })
      );

    new Setting(containerEl)
      .setName("同步间隔（分钟）")
      .setDesc("仅当开启自动同步时生效。")
      .addText((text) => {
        text.inputEl.type = "number";
        text.setPlaceholder("30");
        text.setValue(String(this.plugin.settings.syncIntervalMinutes));
        return text.onChange(async (v) => {
          const n = Math.max(1, parseInt(v, 10) || 30);
          this.plugin.settings.syncIntervalMinutes = n;
          await this.plugin.saveSettings();
          await this.plugin.applyAutoSyncFromSettings();
        });
      });

    new Setting(containerEl)
      .setName("Claude-Go API 地址")
      .setDesc("不含末尾斜杠，例如 http://127.0.0.1:8080")
      .addText((text) =>
        text
          .setPlaceholder("http://127.0.0.1:8080")
          .setValue(this.plugin.settings.claudeGoApiUrl)
          .onChange(async (v) => {
            this.plugin.settings.claudeGoApiUrl = v.trim().replace(/\/+$/, "");
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("Claude-Go API Key")
      .setDesc("可选；将放在 Authorization: Bearer 头中。")
      .addText((text) => {
        text.inputEl.type = "password";
        text.setValue(this.plugin.settings.claudeGoApiKey);
        return text.onChange(async (v) => {
          this.plugin.settings.claudeGoApiKey = v;
          await this.plugin.saveSettings();
        });
      });

    new Setting(containerEl)
      .setName("默认分支")
      .setDesc("pull / push / 状态比较使用的分支名。")
      .addText((text) =>
        text
          .setPlaceholder("main")
          .setValue(this.plugin.settings.defaultBranch)
          .onChange(async (v) => {
            this.plugin.settings.defaultBranch = v.trim() || "main";
            await this.plugin.saveSettings();
          })
      );

    new Setting(containerEl)
      .setName("提交作者名称")
      .addText((text) =>
        text.setValue(this.plugin.settings.commitAuthorName).onChange(async (v) => {
          this.plugin.settings.commitAuthorName = v || DEFAULT_SETTINGS.commitAuthorName;
          await this.plugin.saveSettings();
        })
      );

    new Setting(containerEl)
      .setName("提交作者邮箱")
      .addText((text) =>
        text.setValue(this.plugin.settings.commitAuthorEmail).onChange(async (v) => {
          this.plugin.settings.commitAuthorEmail = v || DEFAULT_SETTINGS.commitAuthorEmail;
          await this.plugin.saveSettings();
        })
      );
  }
}
