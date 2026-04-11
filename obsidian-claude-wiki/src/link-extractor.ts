import { normalizePath, requestUrl, type TFile, type Vault } from "obsidian";

/**
 * 从网页拉取正文并落盘到 Vault `raw/` 目录，供后续管线或 Claude-Go 处理。
 */
export class LinkExtractor {
  /**
   * 抓取 URL，解析 HTML 并抽取可读正文（去 script/style，取 body 文本）。
   * 使用 Obsidian `requestUrl` 以适配桌面与移动端网络栈。
   */
  async extractFromUrl(url: string): Promise<{ title: string; content: string }> {
    const res = await requestUrl({ url, method: "GET" });
    const html =
      typeof res.text === "string"
        ? res.text
        : res.arrayBuffer
          ? new TextDecoder().decode(res.arrayBuffer)
          : "";
    if (!html) {
      throw new Error("Empty response body");
    }

    const doc = new DOMParser().parseFromString(html, "text/html");
    const titleEl = doc.querySelector("title");
    let title = titleEl?.textContent?.trim() || new URL(url).hostname;

    const article =
      doc.querySelector("article") ||
      doc.querySelector("main") ||
      doc.querySelector('[role="main"]') ||
      doc.body;
    if (!article) {
      throw new Error("No parseable content root");
    }

    article.querySelectorAll("script, style, noscript, template").forEach((n) => n.remove());
    const content = article.textContent?.replace(/\n{3,}/g, "\n\n").trim() || "";

    const h1 = article.querySelector("h1");
    if (h1?.textContent?.trim()) title = h1.textContent.trim();

    return { title, content };
  }

  private slugify(title: string): string {
    const base = title
      .trim()
      .replace(/[<>:"/\\|?*]/g, "")
      .replace(/\s+/g, "-")
      .slice(0, 120);
    return base || "untitled";
  }

  /**
   * 写入 `raw/<slug>.md`，带 YAML frontmatter（title、source_url、ingested_at）。
   */
  async saveToRaw(vault: Vault, title: string, content: string, sourceUrl: string): Promise<TFile> {
    const folder = "raw";
    const dir = vault.getAbstractFileByPath(folder);
    if (!dir) {
      await vault.createFolder(folder);
    }

    const slug = this.slugify(title);
    const stamp = new Date().toISOString();
    const body =
      "---\n" +
      `title: ${JSON.stringify(title)}\n` +
      `source_url: ${JSON.stringify(sourceUrl)}\n` +
      `ingested_at: ${JSON.stringify(stamp)}\n` +
      "---\n\n" +
      `# ${title}\n\n` +
      content +
      "\n";

    const path = normalizePath(`${folder}/${slug}.md`);
    const existing = vault.getAbstractFileByPath(path);
    if (existing && "extension" in existing) {
      await vault.modify(existing as TFile, body);
      return existing as TFile;
    }
    return vault.create(path, body);
  }
}
