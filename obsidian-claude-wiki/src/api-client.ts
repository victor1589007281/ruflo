import type { LintResult } from "./lint-runner";

/** Claude-Go 服务端返回的百科状态（字段按需扩展） */
export interface WikiStatus {
  rawCount?: number;
  wikiCount?: number;
  lastSyncAt?: string;
  healthy?: boolean;
  message?: string;
}

type ApiHeaders = Record<string, string>;

/**
 * 与 claude-go HTTP API 通讯的轻量客户端。
 * 约定：ingest POST /wiki/ingest，lint POST /wiki/lint，status GET /wiki/status（均相对 baseUrl）
 */
export class ApiClient {
  constructor(
    readonly baseUrl: string,
    readonly apiKey: string
  ) {}

  private headers(json = true): ApiHeaders {
    const h: ApiHeaders = {};
    if (json) h["Content-Type"] = "application/json";
    if (this.apiKey) h["Authorization"] = `Bearer ${this.apiKey}`;
    return h;
  }

  private async parseJson<T>(res: Response): Promise<T> {
    const text = await res.text();
    if (!res.ok) {
      throw new Error(`API ${res.status}: ${text || res.statusText}`);
    }
    if (!text) return {} as T;
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new Error("Invalid JSON from API");
    }
  }

  /** 通知服务端抓取并处理某 URL */
  async triggerIngest(url: string): Promise<void> {
    const u = `${this.baseUrl}/wiki/ingest`;
    const res = await fetch(u, {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify({ url }),
    });
    await this.parseJson<unknown>(res);
  }

  /** 请求服务端执行 lint（若服务端未实现则抛错，由调用方降级到本地 LintRunner） */
  async triggerLint(): Promise<LintResult> {
    const u = `${this.baseUrl}/wiki/lint`;
    const res = await fetch(u, {
      method: "POST",
      headers: this.headers(true),
      body: "{}",
    });
    return this.parseJson<LintResult>(res);
  }

  async getStatus(): Promise<WikiStatus> {
    const u = `${this.baseUrl}/wiki/status`;
    const res = await fetch(u, {
      method: "GET",
      headers: this.headers(false),
    });
    return this.parseJson<WikiStatus>(res);
  }

  /** 查询知识库 */
  async query(question: string, archive = false): Promise<{ answer: string; archived?: boolean }> {
    const u = `${this.baseUrl}/wiki/query`;
    const res = await fetch(u, {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify({ question, archive }),
    });
    return this.parseJson<{ answer: string; archived?: boolean }>(res);
  }

  /** 触发 wiki 全量/增量整理 */
  async organize(mode: "full" | "incremental" = "full"): Promise<{ updated_pages: number; log: string }> {
    const u = `${this.baseUrl}/wiki/organize`;
    const res = await fetch(u, {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify({ mode }),
    });
    return this.parseJson<{ updated_pages: number; log: string }>(res);
  }

  /** 触发 LLM 健康检查 */
  async healthCheck(): Promise<HealthCheckResult> {
    const u = `${this.baseUrl}/wiki/health-check`;
    const res = await fetch(u, {
      method: "POST",
      headers: this.headers(true),
      body: "{}",
    });
    return this.parseJson<HealthCheckResult>(res);
  }

  /** 摄取纯文本到 wiki */
  async ingestText(title: string, text: string): Promise<void> {
    const u = `${this.baseUrl}/wiki/ingest`;
    const res = await fetch(u, {
      method: "POST",
      headers: this.headers(true),
      body: JSON.stringify({ title, text }),
    });
    await this.parseJson<unknown>(res);
  }
}

export interface HealthCheckResult {
  contradictions?: { page1: string; page2: string; issue: string }[];
  outdated?: { page: string; issue: string }[];
  missing_concepts?: string[];
  orphaned_pages?: string[];
  missing_refs?: { source: string; should_link_to: string }[];
  research_suggestions?: string[];
  summary: string;
}
