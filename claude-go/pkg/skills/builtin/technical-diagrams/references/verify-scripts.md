# 图表验收脚本（technical-diagrams 配套）

三个 Node 脚本，依赖全局 puppeteer（本机在 `/home/victor/base/node/lib/node_modules`）与 Chrome。
运行前确保文档站已启动（如 docforge `http://127.0.0.1:4500`）。

> 本目录不参与 Go 构建：`pkg/skills/autocreate.go` 的 `//go:embed builtin/*/SKILL.md`
> 只收录 SKILL.md，references/ 仅供人与 Claude Code 侧使用。

## measure.mjs —— 量每张图的缩放比

`scale = 渲染宽 / 自然宽(viewBox 宽)`。**<0.6 偏小、<0.35 基本不可读**，目标全部 ≥0.6。

```js
import { createRequire } from 'module';
const require = createRequire('/home/victor/base/node/lib/node_modules/');
const puppeteer = require('puppeteer');
const b = await puppeteer.launch({ executablePath: '/usr/bin/google-chrome', headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'] });
const pages = process.argv.slice(2);
for (const slug of pages) {
  const p = await b.newPage();
  await p.setViewport({ width: 1400, height: 1200 });
  await p.goto(`http://127.0.0.1:4500/p/claude-go/${slug}`, { waitUntil: 'networkidle2', timeout: 45000 });
  await new Promise(r => setTimeout(r, 3000));           // 等 mermaid 异步渲染
  const info = await p.evaluate(() => Array.from(document.querySelectorAll('.mermaid svg')).map((s, i) => {
    const vb = (s.getAttribute('viewBox') || '').split(/\s+/).map(Number);
    const r = s.getBoundingClientRect();
    const scale = vb.length === 4 && vb[2] > 0 ? +(r.width / vb[2]).toFixed(2) : null;
    return { i, natW: vb[2] || null, renderedW: Math.round(r.width), scale };
  }));
  console.log(slug.padEnd(20), JSON.stringify(info));
  await p.close();
}
await b.close();
```

## shot.mjs —— 截图 + 渲染诊断

输出 `{mermaidBlocks, mermaidSvgs, inlineSvgs, mermaidErrors, h2}`。
**验收条件：`mermaidSvgs === mermaidBlocks` 且 `mermaidErrors === 0`。**

```js
import { createRequire } from 'module';
const require = createRequire('/home/victor/base/node/lib/node_modules/');
const puppeteer = require('puppeteer');
const url = process.argv[2], out = process.argv[3];
const b = await puppeteer.launch({ executablePath: '/usr/bin/google-chrome', headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--window-size=1400,2400'] });
const p = await b.newPage();
await p.setViewport({ width: 1400, height: 2400, deviceScaleFactor: 1 });
await p.goto(url, { waitUntil: 'networkidle2', timeout: 45000 });
await new Promise(r => setTimeout(r, 3500));
console.log(JSON.stringify(await p.evaluate(() => ({
  mermaidBlocks: document.querySelectorAll('.mermaid').length,
  mermaidSvgs: document.querySelectorAll('.mermaid svg').length,
  inlineSvgs: Array.from(document.querySelectorAll('svg')).filter(s => !s.closest('.mermaid')).length,
  mermaidErrors: Array.from(document.querySelectorAll('.mermaid'))
    .filter(m => /Syntax error|渲染失败|error in text/i.test(m.textContent || '')).length,
  h2: document.querySelectorAll('.doc-prose h2').length,
}))));
await p.screenshot({ path: out, fullPage: true });
await b.close();
```

## sweep.mjs —— 全站扫描（改完一批后跑）

遍历全部页面，汇总渲染异常与仍偏窄的图。slug 列表写进 `slugs.txt`（空格或换行分隔）。
**47 页约需 3-4 分钟**，注意调用方的超时设置（曾因 2 分钟超时被杀）。

```js
import { createRequire } from 'module';
import { readFileSync } from 'fs';
const require = createRequire('/home/victor/base/node/lib/node_modules/');
const puppeteer = require('puppeteer');
const slugs = readFileSync('slugs.txt', 'utf8').trim().split(/\s+/);
const b = await puppeteer.launch({ executablePath: '/usr/bin/google-chrome', headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'] });
const p = await b.newPage();
await p.setViewport({ width: 1400, height: 1000 });
let bad = [], total = 0, wide = [];
for (const s of slugs) {
  await p.goto(`http://127.0.0.1:4500/p/claude-go/${s}`, { waitUntil: 'networkidle2', timeout: 45000 });
  await new Promise(r => setTimeout(r, 2200));
  const d = await p.evaluate(() => {
    const blocks = document.querySelectorAll('.mermaid'), svgs = document.querySelectorAll('.mermaid svg');
    return {
      b: blocks.length, s: svgs.length,
      errs: Array.from(blocks).filter(m => /Syntax error|渲染失败/i.test(m.textContent || '')).length,
      scales: Array.from(svgs).map(sv => {
        const vb = (sv.getAttribute('viewBox') || '').split(/\s+/).map(Number);
        return vb[2] > 0 ? +(sv.getBoundingClientRect().width / vb[2]).toFixed(2) : 1;
      }),
    };
  });
  total += d.b;
  if (d.errs > 0 || d.b !== d.s) bad.push(`${s}(blocks=${d.b} svgs=${d.s} errs=${d.errs})`);
  const w = d.scales.filter(x => x < 0.6);
  if (w.length) wide.push(`${s}:${w.join(',')}`);
}
console.log(`页数=${slugs.length} mermaid总数=${total}`);
console.log(`渲染异常: ${bad.length ? bad.join(' | ') : '无'}`);
console.log(`仍偏窄(<0.6): ${wide.length ? wide.join(' | ') : '无'}`);
await b.close();
```

## 目视抽检（第三步，不可省）

数字全绿不等于好看。至少抽 2–3 张裁图真的用眼睛看：

```bash
python3 -c "
from PIL import Image
im = Image.open('out.png'); print('size', im.size)
im.crop((240, 200, 1180, 900)).save('crop.png')"
```

然后读 `crop.png`。要确认：文字清晰不重叠、配色语义正确、箭头没有穿过节点、
占位符（`<name>` 这类）没被吞掉。

## 结构一致性自检（改目录/编号后必跑）

图表之外，凡改动过章节编号，必须机械核对"标题编号 ↔ 目录编号"不漂移：

```python
# 每页 h1 必须等于 manifest 标题；页内 h2/h3 编号必须以本页编号为前缀；
# 子页章号必须等于父页章号
import json, re
man = json.load(open('manifest.json'))
for p in man['pages']:
    t = open(p['slug'] + '.html').read()
    h1 = re.sub(r'<[^>]+>', '', re.search(r'<h1[^>]*>(.*?)</h1>', t, re.S).group(1))
    assert h1.replace('　', ' ').strip() == p['title'], p['slug']
```

## 常见排错

| 现象 | 排查 |
|---|---|
| `mermaidSvgs` 少于 `mermaidBlocks` | 语法错。看该块的 `textContent` 有没有 `Syntax error`；常见是 subgraph 未闭合、`classDef` 引用了不存在的节点名 |
| 图渲染但 scale 极低 | 图太宽。查 `natW`；>2000px 必须结构性重排，CSS 救不了 |
| 节点标签缺字 | 占位符或 `<br/>` 被吞（见 SKILL.md §5） |
| `direction LR` 在 subgraph 内无效 | mermaid 已知限制，别依赖它 |
| 脚本跑一半被杀 | 全站扫描耗时数分钟，调用方超时要给足（≥400s） |
