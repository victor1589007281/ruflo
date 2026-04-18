// Claude-Go Dashboard — Vanilla SPA
// -----------------------------------
// Router: hash-based (#/teams, #/metrics, ...)
// Data: fetch /api/... JSON
// Charts: Chart.js (umd) loaded via <script>

(function () {
  'use strict';

  // ========== utils ==========
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
  const h = (tag, props = {}, children = []) => {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (k === 'class') el.className = v;
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === 'html') el.innerHTML = v;
      else el.setAttribute(k, v);
    }
    const arr = Array.isArray(children) ? children : [children];
    for (const c of arr) {
      if (c == null || c === false) continue;
      if (typeof c === 'string') el.appendChild(document.createTextNode(c));
      else el.appendChild(c);
    }
    return el;
  };
  const fmtTime = (t) => {
    if (!t) return '—';
    const d = new Date(t);
    if (isNaN(d)) return '—';
    return d.toLocaleString('zh-CN', { hour12: false });
  };
  const fmtRel = (t) => {
    if (!t) return '—';
    const d = new Date(t);
    if (isNaN(d)) return '—';
    const diff = (Date.now() - d.getTime()) / 1000;
    if (diff < 60) return Math.max(0, Math.floor(diff)) + ' 秒前';
    if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
    if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
    return Math.floor(diff / 86400) + ' 天前';
  };
  const fmtDur = (sec) => {
    if (sec == null || isNaN(sec) || sec <= 0) return '—';
    if (sec < 60) return sec.toFixed(1) + 's';
    if (sec < 3600) return (sec / 60).toFixed(1) + 'm';
    return (sec / 3600).toFixed(1) + 'h';
  };
  const fmtNum = (n, digits = 2) => {
    if (n == null || isNaN(n)) return '—';
    if (Math.abs(n) >= 1000) return n.toFixed(0);
    if (Math.abs(n) >= 1) return n.toFixed(digits);
    return n.toFixed(digits);
  };
  const statusBadgeClass = (s) => {
    switch ((s || '').toLowerCase()) {
      case 'completed': case 'success': return 'ok';
      case 'failed': case 'error':      return 'err';
      case 'running':                   return 'run';
      case 'stopped': case 'cancelled': return 'warn';
      default: return '';
    }
  };

  const api = async (path) => {
    const res = await fetch(path, { headers: { Accept: 'application/json' } });
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = (await res.json()).error || msg; } catch (_) {}
      throw new Error(`[${res.status}] ${msg}`);
    }
    return res.json();
  };

  // ========== state ==========
  const state = {
    route: null,
    currentTeam: null,
    activeTab: 'timeline',
    activeMetricModule: null,
    pollTimer: null,
    charts: {},
    sse: null,
    sseEnabled: false,
    projects: null,
  };

  function destroyCharts() {
    for (const k of Object.keys(state.charts)) {
      try { state.charts[k] && state.charts[k].destroy(); } catch (_) {}
    }
    state.charts = {};
  }

  // ========== router ==========
  const routes = [
    { path: /^\/?$/,                          render: renderOverview,  title: '系统概览',    poll: 10000 },
    { path: /^\/teams\/?$/,                   render: renderTeams,     title: '团队运行',    poll: 5000  },
    { path: /^\/teams\/(.+)$/,                render: renderTeamDetail,title: '团队详情',    poll: 3000  },
    { path: /^\/metrics\/?$/,                 render: renderMetrics,   title: '观测指标',    poll: 0     },
    { path: /^\/metrics\/(.+)$/,              render: renderMetricsDetail, title: '指标详情', poll: 0     },
    { path: /^\/cron\/?$/,                    render: renderCron,      title: 'Cron 任务',   poll: 30000 },
    { path: /^\/dreaming\/?$/,                render: renderDreaming,  title: 'Dreaming 机制', poll: 30000 },
    { path: /^\/evolution\/?$/,               render: renderEvolution, title: 'Evolution 机制', poll: 30000 },
    { path: /^\/tasks\/?$/,                   render: renderTasks,     title: '任务',       poll: 15000 },
  ];

  function parseRoute() {
    const hash = location.hash.replace(/^#/, '') || '/';
    for (const r of routes) {
      const m = r.path.exec(hash);
      if (m) return { handler: r, params: m.slice(1), title: r.title, poll: r.poll, hash };
    }
    return null;
  }

  async function navigate() {
    if (state.pollTimer) { clearInterval(state.pollTimer); state.pollTimer = null; }
    destroyCharts();
    const r = parseRoute();
    if (!r) {
      $('#view').innerHTML = '';
      $('#view').appendChild(h('div', { class: 'empty' }, '未知路由'));
      return;
    }
    state.route = r;
    $('#page-title').textContent = r.title;
    updateNavActive(r.hash);
    try {
      await r.handler.render(...r.params);
    } catch (err) {
      $('#view').innerHTML = '';
      $('#view').appendChild(h('div', { class: 'empty' }, '加载失败: ' + err.message));
    }
    if (r.poll > 0) {
      state.pollTimer = setInterval(async () => {
        try { await r.handler.render(...r.params, { quiet: true }); } catch (_) {}
      }, r.poll);
    }
  }

  function updateNavActive(hash) {
    $$('.nav-item').forEach(el => {
      const route = el.getAttribute('data-route');
      let normalized = hash;
      if (route === '/' && (hash === '' || hash === '/')) {
        el.classList.add('active');
      } else if (route !== '/' && hash.startsWith(route)) {
        el.classList.add('active');
      } else {
        el.classList.remove('active');
      }
    });
  }

  // ========== pages ==========
  async function renderOverview(opts = {}) {
    const data = await api('/api/overview');
    const v = $('#view');
    v.innerHTML = '';
    if (!data.ready) {
      v.appendChild(h('div', { class: 'empty' }, `数据目录 ${data.stateDir} 不存在或不可读, 请先在项目中运行过 claude-go。`));
      return;
    }

    const statCards = h('div', { class: 'grid grid-4' }, [
      statCard('团队总数',  data.totalTeams,    `运行中 ${data.runningTeams} · 失败 ${data.failedTeams}`, 'accent'),
      statCard('活跃 Cron', data.activeCrons,   data.activeCrons === 0 ? '无启用任务' : '已启用', 'purple'),
      statCard('经验库',   data.totalExperiences, '平均质量 ' + fmtNum(data.avgQuality, 2), 'ok'),
      statCard('Dreaming', data.dreamCount,    data.lastDreamAt ? ('上次 ' + fmtRel(data.lastDreamAt)) : '未启用/未运行', 'purple'),
    ]);
    v.appendChild(statCards);

    // daily chart
    const dailyCard = h('div', { class: 'card mt-16' }, [
      h('h3', {}, '最近 7 天团队运行'),
      h('div', { class: 'chart-wrap' }, h('canvas', { id: 'chart-daily' })),
    ]);
    v.appendChild(dailyCard);

    // 三栏: 运行中 / 最近完成 / 告警
    const running = (data.recentRuns || []).filter(t => t.status === 'running');
    const finished = (data.recentRuns || []).filter(t => t.status !== 'running');
    const triCols = h('div', { class: 'grid grid-3 mt-16' }, [
      makeRunList('运行中', running, running.length === 0 ? '暂无运行中团队' : '', 'accent'),
      makeRunList('最近完成', finished.slice(0, 8), finished.length === 0 ? '暂无完成团队' : '', 'ok'),
      makeAlertList(data.alerts || []),
    ]);
    v.appendChild(triCols);

    // 智能诊断面板
    const insightsCard = h('div', { class: 'card mt-16' }, [
      h('div', { class: 'row' }, [
        h('h3', { style: { margin: 0, flex: 1 } }, '🧠 智能诊断 (本地规则, 不经 LLM)'),
        h('span', { id: 'insights-meta', class: 'muted' }, ''),
      ]),
      h('div', { id: 'insights-wrap', class: 'mt-16' }, h('div', { class: 'loading' }, '分析中…')),
    ]);
    v.appendChild(insightsCard);

    // draw daily chart
    const ctx = $('#chart-daily');
    if (ctx && window.Chart) {
      state.charts.daily = new Chart(ctx.getContext('2d'), {
        type: 'bar',
        data: {
          labels: data.dailyRuns.map(d => d.date),
          datasets: [
            { label: '成功', data: data.dailyRuns.map(d => d.successes), backgroundColor: '#3fb950' },
            { label: '失败', data: data.dailyRuns.map(d => d.failures), backgroundColor: '#f85149' },
            { label: '平均耗时(s)', data: data.dailyRuns.map(d => d.avgDuration), type: 'line', borderColor: '#a78bfa', backgroundColor: 'transparent', tension: 0.3, yAxisID: 'y2' },
          ],
        },
        options: baseChartOpts({ stacked: true, dualAxis: true }),
      });
    }

    // 异步加载 insights
    api('/api/insights').then(resp => {
      const wrap = $('#insights-wrap');
      const meta = $('#insights-meta');
      if (!wrap) return;
      if (meta) meta.textContent = (resp.total || 0) + ' 条 · ' + fmtRel(resp.generatedAt);
      if (!resp.insights || resp.insights.length === 0) {
        wrap.innerHTML = '';
        wrap.appendChild(h('div', { class: 'empty' }, '✓ 无异常, 系统运行正常'));
        return;
      }
      wrap.innerHTML = '';
      const list = h('div', { class: 'insight-list' });
      for (const ins of resp.insights) {
        list.appendChild(h('div', { class: 'insight ' + (ins.severity || 'info') }, [
          h('div', { class: 'sev' }, (ins.severity || 'info').toUpperCase()),
          h('div', {}, [
            h('div', { class: 'title' }, ins.title),
            ins.suggestion ? h('div', { class: 'sug' }, '💡 ' + ins.suggestion) : null,
            ins.module ? h('div', { class: 'muted', style: { marginTop: '4px', fontSize: '11px' } }, 'module: ' + ins.module + (ins.target ? ' · target: ' + ins.target : '')) : null,
          ]),
        ]));
      }
      wrap.appendChild(list);
    }).catch(() => {
      const wrap = $('#insights-wrap');
      if (wrap) wrap.innerHTML = '<div class="empty">诊断服务暂不可用</div>';
    });
  }

  function makeRunList(title, items, emptyTip, klass) {
    const card = h('div', { class: 'card' }, [h('h3', {}, title)]);
    if (!items || items.length === 0) {
      card.appendChild(h('div', { class: 'empty' }, emptyTip || '—'));
      return card;
    }
    const list = h('div', { class: 'col' });
    for (const t of items) {
      list.appendChild(h('div', {
        class: 'team-card',
        style: { padding: '10px 12px', borderRadius: '6px', border: '1px solid var(--border)' },
        onClick: () => { location.hash = '#/teams/' + encodeURIComponent(t.name); },
      }, [
        h('div', { class: 'team-name' }, t.name),
        h('div', { class: 'team-meta' }, [
          h('span', { class: 'badge ' + statusBadgeClass(t.status) }, t.status || '—'),
          h('span', { class: 'badge' }, t.workflow || '—'),
          h('span', {}, fmtRel(t.createdAt)),
          h('span', {}, fmtDur(t.durationSec)),
        ]),
      ]));
    }
    card.appendChild(list);
    return card;
  }
  function makeAlertList(alerts) {
    const card = h('div', { class: 'card' }, [h('h3', {}, '告警 / 降级')]);
    if (!alerts || alerts.length === 0) {
      card.appendChild(h('div', { class: 'empty' }, '✓ 暂无告警'));
    } else {
      const wrap = h('div', { class: 'alerts' });
      for (const a of alerts.slice(0, 20)) wrap.appendChild(h('div', { class: 'alert' }, a));
      card.appendChild(wrap);
    }
    return card;
  }

  function statCard(label, value, hint, klass = '') {
    return h('div', { class: 'card stat ' + klass }, [
      h('div', { class: 'value' }, String(value ?? '—')),
      h('div', { class: 'label' }, label),
      h('div', { class: 'hint' }, hint || ''),
    ]);
  }

  async function renderTeams(opts = {}) {
    const list = await api('/api/teams');
    const v = $('#view');
    v.innerHTML = '';

    if (!state.cmp) state.cmp = { enabled: false, picked: new Set() };

    const toolbar = h('div', { class: 'card mb-16', style: { display: 'flex', justifyContent: 'space-between', alignItems: 'center' } }, [
      h('div', {}, [
        h('strong', {}, `团队总数: ${list.length}`),
        h('span', { class: 'muted', style: { marginLeft: '12px' } },
          `运行中 ${list.filter(t => t.status === 'running').length} · ` +
          `已完成 ${list.filter(t => t.status === 'completed').length} · ` +
          `失败 ${list.filter(t => t.status === 'failed').length}`),
      ]),
      h('div', {}, [
        h('button', {
          class: 'btn ' + (state.cmp.enabled ? '' : 'ghost'),
          onClick: () => {
            state.cmp.enabled = !state.cmp.enabled;
            if (!state.cmp.enabled) state.cmp.picked.clear();
            navigate();
          },
        }, state.cmp.enabled ? '✓ 对比模式 (已选 ' + state.cmp.picked.size + ')' : '开启对比模式'),
      ]),
    ]);
    v.appendChild(toolbar);

    const content = h('div', { class: 'two-col' });

    const leftBody = h('div', { class: 'team-list-body' });
    if (!list.length) {
      leftBody.appendChild(h('div', { class: 'empty' }, '没有团队数据'));
    } else {
      for (const t of list) {
        const picked = state.cmp.picked.has(t.name);
        const card = h('div', {
          class: 'team-card' + (picked ? ' active' : ''),
          onClick: () => {
            if (state.cmp.enabled) {
              if (picked) state.cmp.picked.delete(t.name);
              else {
                if (state.cmp.picked.size >= 5) { alert('对比模式下最多选择 5 个团队'); return; }
                state.cmp.picked.add(t.name);
              }
              navigate();
            } else {
              location.hash = '#/teams/' + encodeURIComponent(t.name);
            }
          },
        }, [
          h('div', { class: 'team-name' }, [
            state.cmp.enabled ? h('span', { style: { marginRight: '6px' } }, picked ? '☑' : '☐') : null,
            t.name,
          ]),
          h('div', { class: 'team-meta' }, [
            h('span', { class: 'badge ' + statusBadgeClass(t.status) }, t.status || '—'),
            h('span', { class: 'badge' }, t.workflow || '—'),
            h('span', {}, fmtRel(t.createdAt)),
          ]),
          t.objective ? h('div', { class: 'muted', style: { marginTop: '6px', fontSize: '12px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } }, t.objective) : null,
        ]);
        leftBody.appendChild(card);
      }
    }
    const left = h('div', { class: 'team-list' }, [
      h('div', { class: 'team-list-header' }, state.cmp.enabled ? '选择 2-5 个进行对比' : `共 ${list.length} 个团队`),
      leftBody,
    ]);

    const right = h('div', { class: 'team-detail' });
    if (state.cmp.enabled) {
      await renderTeamsCompare(right, list);
    } else {
      right.appendChild(h('div', { class: 'empty' }, '← 从左侧选择一个团队查看详情'));
    }
    content.appendChild(left);
    content.appendChild(right);
    v.appendChild(content);
  }

  async function renderTeamsCompare(container, list) {
    const picks = [...state.cmp.picked];
    if (picks.length < 2) {
      container.appendChild(h('div', { class: 'empty' }, '请至少选择 2 个团队以进行多维对比 (parallel coordinates on radar)'));
      return;
    }
    container.appendChild(h('h3', {}, '多维对比 · Parallel Coordinates (雷达形式)'));
    const loading = h('div', { class: 'muted' }, '加载中…');
    container.appendChild(loading);

    const details = await Promise.all(picks.map(n =>
      api('/api/teams/' + encodeURIComponent(n)).catch(() => null)));
    loading.remove();

    const axes = ['耗时', '完成率', '失败率', '对抗avg', 'Agent数'];
    const rows = [];
    for (const d of details) {
      if (!d) continue;
      const stagesTotal = Math.max(1, (d.stagesTotal || 0));
      const compRate = (d.stagesDone || 0) / stagesTotal;
      const failRate = (d.stagesFail || 0) / stagesTotal;
      let advAvg = 0;
      const ar = d.adversaryRounds || [];
      if (ar.length) {
        advAvg = ar.reduce((s, r) => s + (r.avgScore || 0), 0) / ar.length / 10; // normalize 0-10 -> 0-1
      }
      rows.push({
        name: d.name,
        durationSec: d.durationSec || 0,
        compRate, failRate, advAvg,
        agentsTotal: d.agentsTotal || 0,
      });
    }
    // Normalize duration & agents by max
    const maxDur = Math.max(...rows.map(r => r.durationSec), 1);
    const maxAg = Math.max(...rows.map(r => r.agentsTotal), 1);
    const datasets = rows.map((r, i) => {
      const hue = (i * 67) % 360;
      const color = `hsl(${hue},70%,60%)`;
      return {
        label: r.name,
        data: [
          Math.min(1, r.durationSec / maxDur),
          r.compRate,
          r.failRate,
          r.advAvg,
          r.agentsTotal / maxAg,
        ],
        borderColor: color,
        backgroundColor: `hsla(${hue},70%,60%,0.15)`,
        pointBackgroundColor: color,
      };
    });
    const wrap = h('div', { class: 'chart-wrap radar' });
    const cv = h('canvas');
    wrap.appendChild(cv);
    container.appendChild(wrap);
    new Chart(cv, {
      type: 'radar',
      data: { labels: axes, datasets },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend: { position: 'bottom', labels: { color: '#e5e9f2' } } },
        scales: {
          r: {
            min: 0, max: 1,
            ticks: { color: '#6b7689', backdropColor: 'transparent', stepSize: 0.25 },
            grid: { color: 'rgba(255,255,255,0.08)' },
            angleLines: { color: 'rgba(255,255,255,0.1)' },
            pointLabels: { color: '#c6cbd4', font: { size: 12 } },
          },
        },
      },
    });

    // Table
    const tbl = h('table', { class: 'tbl', style: { marginTop: '12px' } });
    tbl.appendChild(h('thead', {}, h('tr', {}, [
      h('th', {}, '团队'),
      h('th', {}, '耗时(s)'),
      h('th', {}, '完成率'),
      h('th', {}, '失败率'),
      h('th', {}, '对抗avg/10'),
      h('th', {}, 'Agents'),
    ])));
    const tb = h('tbody');
    for (const r of rows) {
      tb.appendChild(h('tr', {}, [
        h('td', {}, r.name),
        h('td', {}, fmtNum(r.durationSec, 1)),
        h('td', {}, (r.compRate * 100).toFixed(1) + '%'),
        h('td', {}, (r.failRate * 100).toFixed(1) + '%'),
        h('td', {}, (r.advAvg * 10).toFixed(2)),
        h('td', {}, r.agentsTotal),
      ]));
    }
    tbl.appendChild(tb);
    container.appendChild(tbl);

    container.appendChild(h('div', { class: 'muted', style: { marginTop: '8px', fontSize: '12px' } },
      '提示: 耗时与 Agents 数按当前选中团队内 max 归一化; 完成率 = stagesDone/stagesTotal; 对抗avg 取各轮 avgScore 均值(0-10 归一到 0-1)。'));
  }

  async function renderTeamDetail(name, opts = {}) {
    const quiet = opts && opts.quiet;
    const list = await api('/api/teams');
    const detail = await api('/api/teams/' + encodeURIComponent(name));
    const v = $('#view');
    if (!state.activeTab) state.activeTab = 'timeline';

    const content = h('div', { class: 'two-col' });
    const leftBody = h('div', { class: 'team-list-body' });
    for (const t of list) {
      const card = h('div', {
        class: 'team-card' + (t.name === name ? ' active' : ''),
        onClick: () => { location.hash = '#/teams/' + encodeURIComponent(t.name); },
      }, [
        h('div', { class: 'team-name' }, t.name),
        h('div', { class: 'team-meta' }, [
          h('span', { class: 'badge ' + statusBadgeClass(t.status) }, t.status || '—'),
          h('span', { class: 'badge' }, t.workflow || '—'),
        ]),
      ]);
      leftBody.appendChild(card);
    }
    const left = h('div', { class: 'team-list' }, [
      h('div', { class: 'team-list-header' }, `共 ${list.length} 个团队`),
      leftBody,
    ]);

    // right detail
    const tabs = ['timeline', 'stages', 'eval', 'metrics', 'agents', 'blackboard', 'report'];
    const tabNames = { timeline: 'Timeline', stages: 'Stages', eval: 'Eval', metrics: 'Metrics', agents: 'Agents', blackboard: 'Blackboard', report: 'Report' };
    const tabBar = h('div', { class: 'tabs' });
    for (const t of tabs) {
      tabBar.appendChild(h('div', {
        class: 'tab' + (state.activeTab === t ? ' active' : ''),
        onClick: () => { state.activeTab = t; navigate(); },
      }, tabNames[t]));
    }

    const panel = h('div', { class: 'tab-panel' });
    const right = h('div', { class: 'team-detail' }, [
      h('h2', {}, detail.name + '  ', h('span', { class: 'badge ' + statusBadgeClass(detail.status) }, detail.status || '—')),
      h('div', { class: 'team-sub' }, [
        h('span', {}, 'workflow: ' + (detail.workflow || '—')),
        h('span', { style: { marginLeft: '18px' } }, 'created: ' + fmtTime(detail.createdAt)),
        h('span', { style: { marginLeft: '18px' } }, '耗时: ' + fmtDur(detail.durationSec)),
      ]),
      detail.objective ? h('div', { class: 'kv mb-16' }, [
        h('div', { class: 'k' }, '目标'),
        h('div', { class: 'v' }, detail.objective),
      ]) : null,
      tabBar, panel,
    ]);
    content.appendChild(left);
    content.appendChild(right);
    v.innerHTML = '';
    v.appendChild(content);

    // render active tab
    await renderTeamTab(state.activeTab, detail, panel);
  }

  async function renderTeamTab(tab, detail, panel) {
    panel.innerHTML = '';
    if (tab === 'timeline') {
      if (!detail.stages || !detail.stages.length) {
        panel.appendChild(h('div', { class: 'empty' }, '无阶段数据'));
        return;
      }
      const maxDur = Math.max(1, ...detail.stages.map(s => s.durationSec || 0));
      const gantt = h('div', { class: 'gantt' });
      for (const s of detail.stages) {
        const pct = Math.max(2, ((s.durationSec || 0) / maxDur) * 100);
        const cls = s.status === 'completed' ? 'ok' : s.status === 'failed' ? 'err' : 'run';
        gantt.appendChild(h('div', { class: 'gantt-row' }, [
          h('div', { class: 'gantt-label', title: s.name }, (s.role ? s.role + ' · ' : '') + s.name),
          h('div', { class: 'gantt-track' }, h('div', { class: 'gantt-bar ' + cls, style: { width: pct + '%' } })),
          h('div', { class: 'gantt-dur' }, s.duration || '—'),
        ]));
      }
      panel.appendChild(gantt);
    } else if (tab === 'stages') {
      if (!detail.stages || !detail.stages.length) {
        panel.appendChild(h('div', { class: 'empty' }, '无阶段数据'));
        return;
      }
      for (const s of detail.stages) {
        const card = h('div', { class: 'card mb-16' }, [
          h('div', { class: 'row' }, [
            h('div', { style: { flex: '1', fontWeight: '600' } }, (s.role ? s.role + ' · ' : '') + s.name),
            h('span', { class: 'badge ' + statusBadgeClass(s.status) }, s.status || '—'),
            h('span', { class: 'badge' }, s.duration || '—'),
          ]),
          s.input ? h('div', { class: 'mt-16' }, [h('h4', {}, 'Input'), h('pre', { class: 'pre' }, s.input)]) : null,
          s.output ? h('div', { class: 'mt-16' }, [h('h4', {}, 'Output'), h('pre', { class: 'pre' }, s.output)]) : null,
          s.error ? h('div', { class: 'mt-16' }, [h('h4', {}, 'Error'), h('pre', { class: 'pre', style: { color: 'var(--err)' } }, s.error)]) : null,
        ]);
        panel.appendChild(card);
      }
    } else if (tab === 'agents') {
      if (!detail.agents || !detail.agents.length) {
        panel.appendChild(h('div', { class: 'empty' }, '无 Agent 数据'));
        return;
      }
      const grid = h('div', { class: 'grid grid-3' });
      for (const a of detail.agents) {
        grid.appendChild(h('div', { class: 'card' }, [
          h('div', { class: 'row' }, [
            h('div', { style: { flex: '1', fontWeight: '600' } }, a.name),
            h('span', { class: 'badge ' + statusBadgeClass(a.status) }, a.status || '—'),
          ]),
          h('div', { class: 'muted mt-16' }, '角色: ' + (a.role || '—')),
          a.result ? h('pre', { class: 'pre mt-16' }, a.result) : null,
          a.error ? h('pre', { class: 'pre mt-16', style: { color: 'var(--err)' } }, a.error) : null,
        ]));
      }
      panel.appendChild(grid);
    } else if (tab === 'blackboard') {
      try {
        const bb = await api('/api/teams/' + encodeURIComponent(detail.name) + '/blackboard');
        if (!bb || Object.keys(bb).length === 0) {
          panel.appendChild(h('div', { class: 'empty' }, '黑板为空'));
          return;
        }
        panel.appendChild(h('pre', { class: 'pre' }, JSON.stringify(bb, null, 2)));
      } catch (_) {
        panel.appendChild(h('div', { class: 'empty' }, '无黑板数据'));
      }
    } else if (tab === 'report') {
      if (!detail.report) {
        panel.appendChild(h('div', { class: 'empty' }, '没有 REPORT.md'));
        return;
      }
      const mdBox = h('div', { class: 'md' });
      mdBox.innerHTML = renderMarkdown(detail.report);
      panel.appendChild(mdBox);
    } else if (tab === 'eval') {
      const rounds = detail.adversaryRounds || [];
      if (!rounds.length) {
        panel.appendChild(h('div', { class: 'empty' }, '该团队没有对抗循环评分数据 (仅 dev 工作流多轮评估时产生)'));
        return;
      }
      const last = rounds[rounds.length - 1];
      panel.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
        statCard('轮次', rounds.length, '最后一轮: round ' + last.round, 'accent'),
        statCard('综合分', fmtNum(last.avgScore, 2), '对应 5 维平均', last.passed ? 'ok' : 'warn'),
        statCard('通过', last.passed ? 'true' : 'false', '阈值由工作流决定', last.passed ? 'ok' : 'err'),
        statCard('维度', '5', '正确/完整/安全/质量/对齐', 'purple'),
      ]));

      const radarCard = h('div', { class: 'card mb-16' }, [
        h('h3', {}, '最近一轮 5 维雷达'),
        h('div', { class: 'chart-wrap radar' }, h('canvas', { id: 'chart-eval-radar' })),
      ]);
      panel.appendChild(radarCard);

      const lineCard = h('div', { class: 'card mb-16' }, [
        h('h3', {}, '逐轮质量演化'),
        h('div', { class: 'chart-wrap' }, h('canvas', { id: 'chart-eval-line' })),
      ]);
      panel.appendChild(lineCard);

      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '轮'), h('th', {}, '正确'), h('th', {}, '完整'), h('th', {}, '安全'), h('th', {}, '质量'), h('th', {}, '对齐'), h('th', {}, '均分'), h('th', {}, '通过'),
      ])));
      const tb = h('tbody');
      for (const r of rounds) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, String(r.round)),
          h('td', {}, fmtNum(r.correctness, 1)),
          h('td', {}, fmtNum(r.completeness, 1)),
          h('td', {}, fmtNum(r.security, 1)),
          h('td', {}, fmtNum(r.codeQuality, 1)),
          h('td', {}, fmtNum(r.designAlignment, 1)),
          h('td', {}, fmtNum(r.avgScore, 2)),
          h('td', {}, h('span', { class: 'badge ' + (r.passed ? 'ok' : 'err') }, r.passed ? '通过' : '未通过')),
        ]));
      }
      tbl.appendChild(tb);
      panel.appendChild(h('div', { class: 'card' }, [h('h3', {}, '全部轮次'), tbl]));

      requestAnimationFrame(() => {
        const radarEl = document.getElementById('chart-eval-radar');
        if (radarEl && window.Chart) {
          state.charts.evalRadar = new Chart(radarEl.getContext('2d'), {
            type: 'radar',
            data: {
              labels: ['正确', '完整', '安全', '质量', '对齐'],
              datasets: [{
                label: 'round ' + last.round,
                data: [last.correctness, last.completeness, last.security, last.codeQuality, last.designAlignment],
                borderColor: '#58a6ff',
                backgroundColor: 'rgba(88,166,255,0.2)',
                pointBackgroundColor: '#a78bfa',
              }],
            },
            options: {
              responsive: true, maintainAspectRatio: false,
              plugins: { legend: { labels: { color: '#8b949e' } } },
              scales: {
                r: {
                  min: 0, max: 10,
                  grid: { color: '#21262d' },
                  angleLines: { color: '#21262d' },
                  pointLabels: { color: '#e6edf3', font: { size: 12 } },
                  ticks: { color: '#8b949e', backdropColor: 'transparent' },
                },
              },
            },
          });
        }
        const lineEl = document.getElementById('chart-eval-line');
        if (lineEl && window.Chart) {
          state.charts.evalLine = new Chart(lineEl.getContext('2d'), {
            type: 'line',
            data: {
              labels: rounds.map(r => 'R' + r.round),
              datasets: [
                { label: '正确', data: rounds.map(r => r.correctness), borderColor: '#58a6ff', tension: 0.2 },
                { label: '完整', data: rounds.map(r => r.completeness), borderColor: '#3fb950', tension: 0.2 },
                { label: '安全', data: rounds.map(r => r.security), borderColor: '#f85149', tension: 0.2 },
                { label: '质量', data: rounds.map(r => r.codeQuality), borderColor: '#a78bfa', tension: 0.2 },
                { label: '对齐', data: rounds.map(r => r.designAlignment), borderColor: '#d29922', tension: 0.2 },
                { label: '均分', data: rounds.map(r => r.avgScore), borderColor: '#ffffff', borderDash: [5, 3], tension: 0.2 },
              ],
            },
            options: baseChartOpts({}),
          });
        }
      });
    } else if (tab === 'metrics') {
      const series = detail.runMetrics || [];
      if (!series.length) {
        panel.appendChild(h('div', { class: 'empty' }, '该团队没有关联的 per-run 指标 (metrics 事件需通过 labels.team 标注)。'));
        return;
      }
      for (const s of series) {
        const card = h('div', { class: 'card mb-16' }, [
          h('div', { class: 'row' }, [
            h('div', { style: { flex: '1', fontWeight: '600' } }, s.name),
            h('span', { class: 'badge' }, s.points.length + ' points'),
          ]),
          h('div', { class: 'chart-wrap small' }, h('canvas', { id: 'chart-tm-' + s.name.replace(/[^a-z0-9]/gi, '_') })),
        ]);
        panel.appendChild(card);
      }
      requestAnimationFrame(() => {
        for (const s of series) {
          const cid = 'chart-tm-' + s.name.replace(/[^a-z0-9]/gi, '_');
          const cv = document.getElementById(cid);
          if (!cv || !window.Chart) continue;
          state.charts[cid] = new Chart(cv.getContext('2d'), {
            type: 'line',
            data: {
              labels: s.points.map(p => fmtTsShort(p.ts)),
              datasets: [{
                label: s.name,
                data: s.points.map(p => p.value),
                borderColor: '#58a6ff',
                backgroundColor: 'rgba(88,166,255,0.15)',
                tension: 0.25, fill: true, pointRadius: 2,
              }],
            },
            options: baseChartOpts({}),
          });
        }
      });
    }
  }

  async function renderMetrics(opts = {}) {
    const sums = await api('/api/metrics');
    const v = $('#view');
    v.innerHTML = '';
    if (!sums.length) {
      v.appendChild(h('div', { class: 'empty' }, '暂无指标数据, 触发团队/dreaming/cron 后会自动产生。'));
      return;
    }
    for (const s of sums) {
      const card = h('div', { class: 'card mb-16' }, [
        h('div', { class: 'row' }, [
          h('h3', { style: { margin: 0 } }, s.module.toUpperCase()),
          h('span', { class: 'badge' }, `${s.eventCount} events`),
          (s.trendAlerts && s.trendAlerts.length) ? h('span', { class: 'badge warn' }, `${s.trendAlerts.length} alerts`) : null,
        ]),
      ]);
      const grid = h('div', { class: 'grid grid-4 mt-16' });
      const names = Object.keys(s.metrics || {}).sort();
      for (const name of names) {
        const m = s.metrics[name];
        grid.appendChild(h('div', { class: 'metric-card', onClick: () => { location.hash = '#/metrics/' + encodeURIComponent(s.module) + '?name=' + encodeURIComponent(name); } }, [
          h('div', { class: 'metric-name' }, name),
          h('div', { class: 'metric-value' }, fmtNum(m.last, 2)),
          h('div', { class: 'metric-meta' }, [
            h('span', {}, `x̄=${fmtNum(m.avg, 2)}`),
            h('span', {}, `min=${fmtNum(m.min, 2)}`),
            h('span', {}, `max=${fmtNum(m.max, 2)}`),
            h('span', {}, `n=${m.count}`),
            h('span', { class: 'metric-trend ' + (m.trend || 'stable') }, m.trend || 'stable'),
          ]),
        ]));
      }
      card.appendChild(grid);
      if (s.trendAlerts && s.trendAlerts.length) {
        const wrap = h('div', { class: 'alerts mt-16' });
        for (const a of s.trendAlerts) wrap.appendChild(h('div', { class: 'alert' }, a));
        card.appendChild(wrap);
      }
      v.appendChild(card);
    }
  }

  async function renderMetricsDetail(module, opts = {}) {
    // 解析 query 拿到高亮指标名 + 时间范围
    const qs = location.hash.split('?')[1] || '';
    const params = new URLSearchParams(qs);
    const focus = params.get('name') || null;
    const since = params.get('since') || '';
    const apiURL = '/api/metrics/' + encodeURIComponent(module) + (since ? ('?since=' + encodeURIComponent(since)) : '');
    const data = await api(apiURL);
    const v = $('#view');
    v.innerHTML = '';

    const ranges = [
      { k: '', label: '全部' },
      { k: '1h', label: '1h' },
      { k: '6h', label: '6h' },
      { k: '24h', label: '24h' },
      { k: '168h', label: '7天' },
    ];
    const rangeBtns = h('div', { class: 'row', style: { gap: '4px' } });
    for (const r of ranges) {
      rangeBtns.appendChild(h('button', {
        class: 'btn small ghost' + (since === r.k ? ' active' : ''),
        onClick: () => {
          const np = new URLSearchParams(qs);
          if (r.k) np.set('since', r.k); else np.delete('since');
          location.hash = '#/metrics/' + encodeURIComponent(module) + (np.toString() ? '?' + np.toString() : '');
        },
      }, r.label));
    }
    const csvURL = '/api/metrics/' + encodeURIComponent(module) + '?format=csv' + (since ? '&since=' + encodeURIComponent(since) : '');

    const header = h('div', { class: 'card mb-16' }, [
      h('div', { class: 'row' }, [
        h('h3', { style: { margin: 0 } }, module.toUpperCase()),
        h('span', { class: 'badge' }, `${(data.events || []).length} events`),
        rangeBtns,
        h('a', { href: csvURL, class: 'btn small', download: `metrics-${module}.csv` }, '⬇ CSV'),
        h('button', { class: 'btn small', onClick: () => { location.hash = '#/metrics'; } }, '← 返回'),
      ]),
    ]);
    v.appendChild(header);

    if (!data.events || !data.events.length) {
      v.appendChild(h('div', { class: 'empty' }, '无事件数据'));
      return;
    }

    // group by name
    const groups = {};
    for (const e of data.events) {
      (groups[e.name] = groups[e.name] || []).push(e);
    }
    const names = Object.keys(groups).sort((a, b) => {
      if (a === focus) return -1;
      if (b === focus) return 1;
      return a.localeCompare(b);
    });

    for (const name of names) {
      const evs = groups[name];
      const card = h('div', { class: 'card mb-16' }, [
        h('div', { class: 'row' }, [
          h('div', { style: { flex: '1', fontWeight: '600' } }, name),
          h('span', { class: 'badge' }, `${evs.length} 个点`),
          h('span', { class: 'badge' }, '末值: ' + fmtNum(evs[evs.length - 1].value, 3)),
        ]),
        h('div', { class: 'chart-wrap small mt-16' }, h('canvas', { id: `chart-m-${name.replace(/[^a-z0-9]/gi, '_')}` })),
      ]);
      v.appendChild(card);
    }
    // Draw after appending (so canvas has dimensions)
    requestAnimationFrame(() => {
      for (const name of names) {
        const evs = groups[name];
        const id = `chart-m-${name.replace(/[^a-z0-9]/gi, '_')}`;
        const cv = document.getElementById(id);
        if (!cv || !window.Chart) continue;
        state.charts[id] = new Chart(cv.getContext('2d'), {
          type: 'line',
          data: {
            labels: evs.map(e => fmtTsShort(e.ts)),
            datasets: [{
              label: name,
              data: evs.map(e => e.value),
              borderColor: '#58a6ff',
              backgroundColor: 'rgba(88,166,255,0.15)',
              tension: 0.25,
              pointRadius: 2,
              fill: true,
            }],
          },
          options: baseChartOpts({}),
        });
      }
    });
  }

  async function renderCron(opts = {}) {
    const list = await api('/api/cron');
    const v = $('#view');
    v.innerHTML = '';
    if (!list.length) {
      v.appendChild(h('div', { class: 'empty' }, '未定义任何 Cron 任务'));
      return;
    }
    const stats = { total: list.length, enabled: 0, runs: 0, fails: 0 };
    for (const j of list) {
      if (j.enabled) stats.enabled++;
      stats.runs += j.runCount || 0;
      stats.fails += j.failCount || 0;
    }
    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('任务数', stats.total, `启用 ${stats.enabled}`, 'accent'),
      statCard('总执行', stats.runs, `失败 ${stats.fails}`, stats.fails > 0 ? 'warn' : 'ok'),
      statCard('成功率', (stats.runs > 0 ? ((stats.runs - stats.fails) / stats.runs * 100).toFixed(1) : '—') + '%', 'runs-fails / runs', 'purple'),
      statCard('类型', (new Set(list.map(j => j.jobType))).size, list.map(j => j.jobType).filter(Boolean).join(', '), ''),
    ]));

    const card = h('div', { class: 'card' }, [h('h3', {}, '任务列表')]);
    const tbl = h('table', { class: 'tbl' });
    tbl.appendChild(h('thead', {}, h('tr', {}, [
      h('th', {}, '名称'),
      h('th', {}, '表达式'),
      h('th', {}, '类型'),
      h('th', {}, '启用'),
      h('th', {}, '执行'),
      h('th', {}, '上次运行'),
      h('th', {}, '结果摘要'),
    ])));
    const tb = h('tbody');
    for (const j of list) {
      tb.appendChild(h('tr', {}, [
        h('td', {}, j.name || j.id),
        h('td', {}, h('code', { style: { fontSize: '12px' } }, j.schedule || '—')),
        h('td', {}, j.jobType + (j.workflow ? ' / ' + j.workflow : '')),
        h('td', {}, h('span', { class: 'badge ' + (j.enabled ? 'ok' : 'warn') }, j.enabled ? '启用' : '暂停')),
        h('td', {}, `${j.runCount || 0} (err ${j.failCount || 0})`),
        h('td', {}, fmtRel(j.lastRunAt)),
        h('td', { style: { maxWidth: '340px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } }, j.lastResult || '—'),
      ]));
    }
    tbl.appendChild(tb);
    card.appendChild(tbl);
    v.appendChild(card);
  }

  async function renderDreaming(opts = {}) {
    const data = await api('/api/dreaming');
    const v = $('#view');
    v.innerHTML = '';
    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('状态', data.enabled ? '启用' : '关闭', data.memoryDir, data.enabled ? 'ok' : 'warn'),
      statCard('Dream 次数', data.dreamCount, data.lastDreamAt ? ('上次 ' + fmtRel(data.lastDreamAt)) : '—', 'purple'),
      statCard('Memory 文件', (data.memoryFiles || []).length, '', 'accent'),
      statCard('压缩率(avg)',
        data.metricSummary && data.metricSummary.metrics && data.metricSummary.metrics.dream_compression_ratio
          ? fmtNum(data.metricSummary.metrics.dream_compression_ratio.avg, 2) : '—',
        '最近 100 次', 'ok'),
    ]));

    // Dream 指标趋势图 (若有) - 双轴: compression_ratio vs duration
    if (data.metricSummary && data.metricSummary.metrics && Object.keys(data.metricSummary.metrics).length) {
      const metricCard = h('div', { class: 'card mb-16' }, [
        h('h3', {}, 'Dream 指标摘要'),
      ]);
      const grid = h('div', { class: 'grid grid-4' });
      for (const [name, m] of Object.entries(data.metricSummary.metrics)) {
        grid.appendChild(h('div', { class: 'metric-card' }, [
          h('div', { class: 'metric-name' }, name),
          h('div', { class: 'metric-value' }, fmtNum(m.last, 3)),
          h('div', { class: 'metric-meta' }, [
            h('span', {}, `x̄=${fmtNum(m.avg, 3)}`),
            h('span', {}, `n=${m.count}`),
            h('span', { class: 'metric-trend ' + (m.trend || 'stable') }, m.trend || 'stable'),
          ]),
        ]));
      }
      metricCard.appendChild(grid);
      v.appendChild(metricCard);
    }

    // Topic treemap: 从 memoryFiles 聚合 topics
    const topicMap = {};
    for (const f of (data.memoryFiles || [])) {
      for (const t of (f.topics || [])) {
        topicMap[t] = (topicMap[t] || 0) + 1;
      }
    }
    const topicEntries = Object.entries(topicMap).sort((a, b) => b[1] - a[1]).slice(0, 30);
    if (topicEntries.length) {
      const tmCard = h('div', { class: 'card mb-16' }, [h('h3', {}, '记忆话题分布 (Topic Treemap)')]);
      const tm = h('div', { class: 'treemap' });
      const maxV = topicEntries[0][1];
      for (const [name, n] of topicEntries) {
        const weight = 0.2 + 0.8 * (n / maxV);
        tm.appendChild(h('div', {
          class: 'tile',
          style: { flexGrow: String(n), minWidth: '80px', background: `rgba(88,166,255,${0.1 + weight * 0.4})`, borderColor: `rgba(88,166,255,${weight})` },
          title: name + ' · ' + n,
        }, name + ' (' + n + ')'));
      }
      tmCard.appendChild(tm);
      v.appendChild(tmCard);
    }

    // Dream logs
    const logsCard = h('div', { class: 'card mb-16' }, [h('h3', {}, 'Dream 日志')]);
    if (!data.logs || !data.logs.length) {
      logsCard.appendChild(h('div', { class: 'empty' }, '暂无 dream-*.md 日志文件'));
    } else {
      for (const l of data.logs.slice(0, 10)) {
        logsCard.appendChild(h('div', { class: 'mb-16' }, [
          h('div', { class: 'row' }, [
            h('div', { style: { flex: '1', fontWeight: '600' } }, l.filename),
            h('span', { class: 'muted' }, fmtRel(l.modTime) + ' · ' + (l.size / 1024).toFixed(1) + ' KB'),
          ]),
          l.preview ? h('pre', { class: 'pre', style: { marginTop: '8px' } }, l.preview) : null,
        ]));
      }
    }
    v.appendChild(logsCard);

    if (data.memoryFiles && data.memoryFiles.length) {
      const mfCard = h('div', { class: 'card' }, [h('h3', {}, 'Memory 文件')]);
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [h('th', {}, '文件名'), h('th', {}, '大小'), h('th', {}, '修改时间')])));
      const tb = h('tbody');
      for (const f of data.memoryFiles.slice(0, 30)) {
        tb.appendChild(h('tr', {}, [h('td', {}, f.filename), h('td', {}, (f.size / 1024).toFixed(1) + ' KB'), h('td', {}, fmtRel(f.modTime))]));
      }
      tbl.appendChild(tb);
      mfCard.appendChild(tbl);
      v.appendChild(mfCard);
    }
  }

  async function renderEvolution(opts = {}) {
    const data = await api('/api/evolution');
    const v = $('#view');
    v.innerHTML = '';
    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('经验总数', data.totalExperiences, '角色/错误/通用', 'purple'),
      statCard('轨迹', data.totalTrajectories, '最近执行记录', 'accent'),
      statCard('平均质量', fmtNum(data.avgQuality, 3), '范围 0-1', 'ok'),
      statCard('成功率', (data.successRate * 100).toFixed(1) + '%', `利用次数 ${data.totalUsageCount}`, data.successRate >= 0.8 ? 'ok' : 'warn'),
    ]));

    // 质量直方图
    const histCard = h('div', { class: 'card mb-16' }, [
      h('h3', {}, '经验质量分布'),
      (() => {
        const hist = data.qualityHistogram || [];
        const max = Math.max(1, ...hist.map(b => b.count));
        const wrap = h('div', {});
        const bars = h('div', { class: 'histogram' });
        for (const b of hist) {
          const height = (b.count / max) * 100;
          bars.appendChild(h('div', { class: 'bar', style: { height: height + '%' } }, h('span', { class: 'v' }, String(b.count))));
        }
        const labels = h('div', { class: 'histogram-labels' });
        for (const b of hist) labels.appendChild(h('span', {}, b.from.toFixed(1) + '-' + b.to.toFixed(1)));
        wrap.appendChild(bars);
        wrap.appendChild(labels);
        return wrap;
      })(),
    ]);
    v.appendChild(histCard);

    // 热力图: category × role
    if (data.heatmap && data.heatmap.length) {
      const hmCard = h('div', { class: 'card mb-16' }, [h('h3', {}, 'category × role 热力图')]);
      hmCard.appendChild(renderHeatmapEl(data.heatmap));
      v.appendChild(hmCard);
    }

    // 分类计数 & 角色计数
    v.appendChild(h('div', { class: 'grid grid-2 mb-16' }, [
      (() => {
        const c = h('div', { class: 'card' }, [h('h3', {}, '类别分布')]);
        const entries = Object.entries(data.categoryCounts || {}).sort((a, b) => b[1] - a[1]);
        if (!entries.length) c.appendChild(h('div', { class: 'empty' }, '无数据'));
        else {
          const tbl = h('table', { class: 'tbl' });
          tbl.appendChild(h('thead', {}, h('tr', {}, [h('th', {}, '类别'), h('th', {}, '数量')])));
          const tb = h('tbody');
          for (const [k, v2] of entries) tb.appendChild(h('tr', {}, [h('td', {}, k), h('td', {}, String(v2))]));
          tbl.appendChild(tb);
          c.appendChild(tbl);
        }
        return c;
      })(),
      (() => {
        const c = h('div', { class: 'card' }, [h('h3', {}, '角色分布')]);
        const entries = Object.entries(data.roleCounts || {}).sort((a, b) => b[1] - a[1]);
        if (!entries.length) c.appendChild(h('div', { class: 'empty' }, '无角色经验'));
        else {
          const tbl = h('table', { class: 'tbl' });
          tbl.appendChild(h('thead', {}, h('tr', {}, [h('th', {}, '角色'), h('th', {}, '数量')])));
          const tb = h('tbody');
          for (const [k, v2] of entries) tb.appendChild(h('tr', {}, [h('td', {}, k), h('td', {}, String(v2))]));
          tbl.appendChild(tb);
          c.appendChild(tbl);
        }
        return c;
      })(),
    ]));

    // Top 经验
    if (data.topExperiences && data.topExperiences.length) {
      const card = h('div', { class: 'card mb-16' }, [h('h3', {}, 'Top 经验 (按使用率 × 质量)')]);
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, 'ID'), h('th', {}, '类别'), h('th', {}, '角色'),
        h('th', {}, '质量'), h('th', {}, '使用'), h('th', {}, '成功率'), h('th', {}, '内容'),
      ])));
      const tb = h('tbody');
      for (const e of data.topExperiences) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, e.id || '—'),
          h('td', {}, h('span', { class: 'badge' }, e.category || '—')),
          h('td', {}, e.role || '—'),
          h('td', {}, fmtNum(e.quality, 2)),
          h('td', {}, String(e.usageCount)),
          h('td', {}, (e.successRate * 100).toFixed(0) + '%'),
          h('td', { style: { maxWidth: '440px' } }, h('div', { style: { whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: '12px', color: 'var(--text-dim)' } }, (e.content || '').slice(0, 200))),
        ]));
      }
      tbl.appendChild(tb);
      card.appendChild(tbl);
      v.appendChild(card);
    }

    // 最近轨迹
    if (data.recentTrajectories && data.recentTrajectories.length) {
      const card = h('div', { class: 'card' }, [h('h3', {}, '最近轨迹')]);
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '时间'), h('th', {}, '团队'), h('th', {}, '阶段'), h('th', {}, '角色'), h('th', {}, '状态'), h('th', {}, '耗时'),
      ])));
      const tb = h('tbody');
      for (const t of data.recentTrajectories) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, fmtRel(t.timestamp)),
          h('td', {}, t.teamName || '—'),
          h('td', {}, t.stageName || '—'),
          h('td', {}, t.role || '—'),
          h('td', {}, h('span', { class: 'badge ' + (t.success ? 'ok' : 'err') }, t.success ? '成功' : '失败')),
          h('td', {}, t.duration || '—'),
        ]));
      }
      tbl.appendChild(tb);
      card.appendChild(tbl);
      v.appendChild(card);
    }
  }

  async function renderTasks(opts = {}) {
    const list = await api('/api/tasks');
    const v = $('#view');
    v.innerHTML = '';
    if (!list.length) {
      v.appendChild(h('div', { class: 'empty' }, '无 V2 任务数据'));
      return;
    }
    const counts = { pending: 0, running: 0, completed: 0, failed: 0, blocked: 0, other: 0 };
    for (const t of list) {
      if (counts[t.status] != null) counts[t.status]++;
      else counts.other++;
    }
    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('任务总数', list.length, `运行中 ${counts.running}`, 'accent'),
      statCard('已完成', counts.completed, '', 'ok'),
      statCard('失败', counts.failed, '', counts.failed > 0 ? 'err' : ''),
      statCard('待处理', counts.pending + counts.blocked, `pending ${counts.pending} / blocked ${counts.blocked}`, 'warn'),
    ]));
    const card = h('div', { class: 'card' }, [h('h3', {}, '任务列表')]);
    const tbl = h('table', { class: 'tbl' });
    tbl.appendChild(h('thead', {}, h('tr', {}, [
      h('th', {}, 'ID'), h('th', {}, '主题'), h('th', {}, '状态'), h('th', {}, '负责人'), h('th', {}, '优先级'), h('th', {}, '依赖'),
    ])));
    const tb = h('tbody');
    for (const t of list) {
      tb.appendChild(h('tr', {}, [
        h('td', {}, t.id),
        h('td', {}, t.subject || '—'),
        h('td', {}, h('span', { class: 'badge ' + statusBadgeClass(t.status) }, t.status || '—')),
        h('td', {}, t.owner || '—'),
        h('td', {}, String(t.priority || 0)),
        h('td', {}, (t.dependsOn || []).join(', ') || '—'),
      ]));
    }
    tbl.appendChild(tb);
    card.appendChild(tbl);
    v.appendChild(card);
  }

  // ========== Chart.js options ==========
  function baseChartOpts({ stacked = false, dualAxis = false } = {}) {
    const gridColor = '#21262d';
    const tickColor = '#8b949e';
    const opts = {
      responsive: true,
      maintainAspectRatio: false,
      interaction: { mode: 'nearest', intersect: false },
      plugins: {
        legend: { labels: { color: tickColor, boxWidth: 12 } },
        tooltip: { backgroundColor: '#0e1117', borderColor: '#30363d', borderWidth: 1 },
      },
      scales: {
        x: {
          stacked,
          grid: { color: gridColor, drawOnChartArea: true },
          ticks: { color: tickColor, maxTicksLimit: 10, autoSkip: true },
          type: 'category',
        },
        y: {
          stacked,
          beginAtZero: true,
          grid: { color: gridColor },
          ticks: { color: tickColor },
        },
      },
    };
    if (dualAxis) {
      opts.scales.y2 = {
        beginAtZero: true, position: 'right',
        grid: { drawOnChartArea: false }, ticks: { color: tickColor },
      };
    }
    return opts;
  }

  function fmtTsShort(t) {
    if (!t) return '';
    const d = new Date(t);
    if (isNaN(d)) return '';
    const pad = (n) => (n < 10 ? '0' + n : '' + n);
    return `${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }

  // ========== Markdown (tiny, safe-ish) ==========
  function renderMarkdown(src) {
    if (!src) return '';
    const escapeHTML = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    const lines = src.split(/\r?\n/);
    const out = [];
    let inCode = false;
    let codeLang = '';
    let codeBuf = [];
    let listBuf = null;
    let listType = null;
    const flushList = () => {
      if (listBuf && listBuf.length) {
        out.push('<' + listType + '>' + listBuf.join('') + '</' + listType + '>');
      }
      listBuf = null; listType = null;
    };
    for (let raw of lines) {
      if (inCode) {
        if (raw.startsWith('```')) {
          out.push('<pre><code data-lang="' + escapeHTML(codeLang) + '">' + escapeHTML(codeBuf.join('\n')) + '</code></pre>');
          inCode = false; codeLang = ''; codeBuf = [];
        } else {
          codeBuf.push(raw);
        }
        continue;
      }
      if (raw.startsWith('```')) {
        flushList();
        inCode = true; codeLang = raw.slice(3).trim(); codeBuf = []; continue;
      }
      let m;
      if ((m = raw.match(/^(#{1,6})\s+(.*)$/))) {
        flushList();
        const level = m[1].length;
        out.push('<h' + level + '>' + inline(escapeHTML(m[2])) + '</h' + level + '>');
        continue;
      }
      if ((m = raw.match(/^\s*[-*]\s+(.*)$/))) {
        if (listType !== 'ul') { flushList(); listBuf = []; listType = 'ul'; }
        listBuf.push('<li>' + inline(escapeHTML(m[1])) + '</li>');
        continue;
      }
      if ((m = raw.match(/^\s*\d+\.\s+(.*)$/))) {
        if (listType !== 'ol') { flushList(); listBuf = []; listType = 'ol'; }
        listBuf.push('<li>' + inline(escapeHTML(m[1])) + '</li>');
        continue;
      }
      if ((m = raw.match(/^>\s?(.*)$/))) {
        flushList();
        out.push('<blockquote>' + inline(escapeHTML(m[1])) + '</blockquote>');
        continue;
      }
      if (raw.trim() === '') {
        flushList();
        continue;
      }
      flushList();
      out.push('<p>' + inline(escapeHTML(raw)) + '</p>');
    }
    flushList();
    if (inCode) out.push('<pre><code>' + escapeHTML(codeBuf.join('\n')) + '</code></pre>');
    return out.join('\n');
  }
  function inline(s) {
    // 顺序: inline code -> bold -> italic -> link
    s = s.replace(/`([^`]+)`/g, '<code>$1</code>');
    s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/\*([^*]+)\*/g, '<em>$1</em>');
    s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
    return s;
  }

  // ========== Heatmap ==========
  function renderHeatmapEl(cells) {
    const cats = Array.from(new Set(cells.map(c => c.category)));
    const roles = Array.from(new Set(cells.map(c => c.role)));
    const idx = {};
    for (const c of cells) idx[c.category + '|' + c.role] = c.count;
    const maxV = Math.max(1, ...cells.map(c => c.count));
    const el = h('div', { class: 'heatmap', style: { gridTemplateColumns: `minmax(120px,auto) repeat(${roles.length},minmax(60px,1fr))` } });
    el.appendChild(h('div', { class: 'label' }, 'category \\ role'));
    for (const r of roles) el.appendChild(h('div', { class: 'label', style: { fontWeight: '600', color: 'var(--text)' } }, r));
    for (const cat of cats) {
      el.appendChild(h('div', { class: 'label', style: { fontWeight: '600', color: 'var(--text)' } }, cat));
      for (const r of roles) {
        const n = idx[cat + '|' + r] || 0;
        const ratio = n / maxV;
        el.appendChild(h('div', {
          class: 'cell',
          title: `${cat} × ${r} = ${n}`,
          style: { background: `rgba(167,139,250,${0.08 + ratio * 0.7})` },
        }, n === 0 ? '' : String(n)));
      }
    }
    return el;
  }

  // ========== SSE overview streaming ==========
  function toggleSSE() {
    if (state.sseEnabled) {
      if (state.sse) { try { state.sse.close(); } catch (_) {} state.sse = null; }
      state.sseEnabled = false;
      $('#stream-btn').classList.remove('active');
      $('#refresh-status').textContent = '';
      return;
    }
    state.sse = new EventSource('/api/stream/overview?interval=3s');
    state.sse.addEventListener('overview', () => {
      $('#refresh-status').textContent = 'SSE · ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
      // 仅在 overview 页面触发重渲染 (复用当前 route)
      if (state.route && /^\/?$/.test(state.route.hash)) {
        try { state.route.handler.render({ quiet: true }); } catch (_) {}
      }
    });
    state.sse.onerror = () => {
      $('#refresh-status').textContent = 'SSE 已断开';
    };
    state.sseEnabled = true;
    $('#stream-btn').classList.add('active');
    $('#refresh-status').textContent = 'SSE · 已连接';
  }

  // ========== Project switcher ==========
  async function refreshProjects() {
    try {
      const resp = await api('/api/projects');
      state.projects = resp;
      const sel = $('#project-switcher');
      if (!sel) return;
      sel.innerHTML = '';
      if (!resp.projects || resp.projects.length <= 1) {
        sel.style.display = 'none';
        return;
      }
      sel.style.display = '';
      for (const p of resp.projects) {
        const opt = document.createElement('option');
        opt.value = p.path;
        opt.textContent = (p.current ? '● ' : '') + (p.name || p.path) + ` (${p.teamsCount})`;
        if (p.current) opt.selected = true;
        sel.appendChild(opt);
      }
      sel.onchange = () => {
        alert('切换项目需要重启 Dashboard:\n  claude-go dashboard stop && claude-go dashboard start --state-dir ' + sel.value);
      };
    } catch (_) {}
  }

  // ========== Health polling ==========
  async function updateHealth() {
    const el = $('#health-indicator');
    try {
      const d = await api('/api/health');
      el.classList.remove('err');
      el.classList.add('ok');
      $('.health-text', el).textContent = d.exists ? '在线 · 数据已连接' : '在线 · 数据目录为空';
    } catch (_) {
      el.classList.remove('ok');
      el.classList.add('err');
      $('.health-text', el).textContent = '离线';
    }
  }

  // ========== init ==========
  window.addEventListener('DOMContentLoaded', () => {
    // attach nav clicks
    $$('.nav-item').forEach(el => {
      el.addEventListener('click', (e) => {
        e.preventDefault();
        location.hash = '#' + el.getAttribute('data-route');
      });
    });
    $('#refresh-btn').addEventListener('click', () => navigate());
    const sBtn = $('#stream-btn');
    if (sBtn) sBtn.addEventListener('click', toggleSSE);

    window.addEventListener('hashchange', navigate);
    navigate();
    updateHealth();
    refreshProjects();
    setInterval(updateHealth, 15000);
  });
})();
