// Claude-Go · Cosmic Dashboard SPA
// Vanilla JS · Router (hash) · Chart.js
// v1.1 — 宇宙风 / 全局搜索 / 列表-卡片切换 / DAG / 日志 SSE / 时序 / 动作 / 群体智能 / LLM 诊断
(function () {
  'use strict';

  // ==================================================================
  // 0. utils
  // ==================================================================
  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

  const h = (tag, props = {}, children = []) => {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (k === 'html') el.innerHTML = v;
      else el.setAttribute(k, v);
    }
    const arr = Array.isArray(children) ? children : [children];
    for (const c of arr) {
      if (c == null || c === false) continue;
      if (typeof c === 'string' || typeof c === 'number') el.appendChild(document.createTextNode(String(c)));
      else el.appendChild(c);
    }
    return el;
  };

  const fmtTime = t => {
    if (!t) return '—';
    const d = new Date(t);
    return isNaN(d) ? '—' : d.toLocaleString('zh-CN', { hour12: false });
  };
  const fmtRel = t => {
    if (!t) return '—';
    const d = new Date(t);
    if (isNaN(d)) return '—';
    const diff = (Date.now() - d.getTime()) / 1000;
    if (diff < 60) return Math.max(0, Math.floor(diff)) + ' 秒前';
    if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
    if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
    return Math.floor(diff / 86400) + ' 天前';
  };
  const fmtDur = sec => {
    if (sec == null || isNaN(sec) || sec <= 0) return '—';
    if (sec < 60) return sec.toFixed(1) + 's';
    if (sec < 3600) return (sec / 60).toFixed(1) + 'm';
    return (sec / 3600).toFixed(1) + 'h';
  };
  const fmtNum = (n, digits = 2) => {
    if (n == null || isNaN(n)) return '—';
    if (Math.abs(n) >= 1000) return n.toFixed(0);
    return n.toFixed(digits);
  };
  const fmtTsShort = t => {
    if (!t) return '';
    const d = new Date(t);
    if (isNaN(d)) return '';
    const pad = n => n < 10 ? '0' + n : '' + n;
    return `${pad(d.getMonth() + 1)}/${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  };
  const statusBadgeClass = s => {
    switch ((s || '').toLowerCase()) {
      case 'completed': case 'success': case 'ok': return 'ok';
      case 'failed': case 'error': return 'err';
      case 'running': return 'run';
      case 'stopped': case 'cancelled': case 'paused': return 'warn';
      default: return '';
    }
  };

  const api = async (path, opts = {}) => {
    const res = await fetch(path, {
      method: opts.method || 'GET',
      headers: { Accept: 'application/json', ...(opts.headers || {}) },
      body: opts.body,
    });
    if (!res.ok) {
      let msg = res.statusText;
      try { msg = (await res.json()).error || msg; } catch (_) {}
      throw new Error(`[${res.status}] ${msg}`);
    }
    if ((res.headers.get('content-type') || '').includes('application/json')) return res.json();
    return res.text();
  };

  const toast = (msg, kind = '', ms = 2800) => {
    const el = $('#toast');
    if (!el) return;
    el.textContent = msg;
    el.className = 'toast show ' + (kind || '');
    setTimeout(() => el.classList.remove('show'), ms);
  };

  const safeStr = s => (s == null ? '' : String(s));

  // ==================================================================
  // 1. state & router
  // ==================================================================
  const state = {
    route: null,
    currentTeam: null,
    activeTab: 'dag',
    activeMetricModule: null,
    pollTimer: null,
    charts: {},
    sse: null,
    sseEnabled: false,
    projects: null,
    cmp: { enabled: false, picked: new Set() },
    teamsView: localStorage.getItem('teamsView') || 'list', // list | cards
    cronView: localStorage.getItem('cronView') || 'list',
    logs: { sse: null, paused: false, filter: '' },
    teamFilter: '',
    cronFilter: '',
    tasksFilter: '',
    searchTimer: null,
  };

  function destroyCharts() {
    for (const k of Object.keys(state.charts)) {
      try { state.charts[k] && state.charts[k].destroy(); } catch (_) {}
    }
    state.charts = {};
  }

  const routes = [
    { path: /^\/?$/,              render: renderOverview,      title: '系统概览',      poll: 10000 },
    { path: /^\/teams\/?$/,       render: renderTeams,         title: '团队运行',      poll: 5000 },
    { path: /^\/teams\/(.+)$/,    render: renderTeamDetail,    title: '团队详情',      poll: 3000 },
    { path: /^\/metrics\/?$/,     render: renderMetrics,       title: '观测指标',      poll: 0 },
    { path: /^\/metrics\/(.+)$/,  render: renderMetricsDetail, title: '指标详情',      poll: 0 },
    { path: /^\/cron\/?$/,        render: renderCron,          title: 'Cron 任务',     poll: 30000 },
    { path: /^\/dreaming\/?$/,    render: renderDreaming,      title: 'Dreaming 机制', poll: 30000 },
    { path: /^\/evolution\/?$/,   render: renderEvolution,     title: 'Evolution 机制',poll: 30000 },
    { path: /^\/swarm\/?$/,       render: renderSwarm,         title: '群体智能',      poll: 20000 },
    { path: /^\/tasks\/?$/,       render: renderTasks,         title: '任务',          poll: 15000 },
    { path: /^\/logs\/?$/,        render: renderLogs,          title: '实时日志',      poll: 0 },
  ];

  async function navigate() {
    if (state.pollTimer) { clearInterval(state.pollTimer); state.pollTimer = null; }
    destroyCharts();
    const hash = (location.hash || '#/').slice(1) || '/';
    const matched = routes.find(r => r.path.test(hash));
    if (!matched) {
      $('#view').innerHTML = '<div class="empty">未知路由: ' + hash + '</div>';
      return;
    }
    const m = matched.path.exec(hash);
    const arg = (m && m[1]) ? decodeURIComponent(m[1]) : null;
    state.route = { hash, handler: matched, arg };
    $('#page-title').textContent = matched.title;
    $$('.nav-item').forEach(el => {
      const r = el.getAttribute('data-route');
      el.classList.toggle('active', r && hash.startsWith(r) && (r !== '/' || hash === '/'));
    });
    try {
      await matched.render(arg);
    } catch (e) {
      $('#view').innerHTML = `<div class="empty" style="color:var(--red)">加载失败: ${e.message}</div>`;
    }
    if (matched.poll > 0) {
      state.pollTimer = setInterval(() => matched.render(arg, { quiet: true }).catch(() => {}), matched.poll);
    }
  }

  // ==================================================================
  // 2. shared helpers
  // ==================================================================
  function statCard(title, value, hint, kind = '') {
    return h('div', { class: 'card stat ' + kind }, [
      h('div', { class: 'value' }, String(value)),
      h('div', { class: 'label' }, title),
      hint ? h('div', { class: 'hint' }, hint) : null,
    ]);
  }
  function kvRow(k, v) { return [h('div', { class: 'k' }, k), h('div', { class: 'v' }, v == null ? '—' : v)]; }

  function sectionHeader(title, right) {
    return h('div', { class: 'toolbar-row' }, [
      h('h3', { style: { margin: 0 } }, title),
      right || null,
    ]);
  }

  function viewToggle(current, on) {
    return h('span', { class: 'view-toggle' }, [
      h('button', { class: current === 'list' ? 'active' : '', onClick: () => on('list') }, '列表'),
      h('button', { class: current === 'cards' ? 'active' : '', onClick: () => on('cards') }, '卡片'),
    ]);
  }

  // ==================================================================
  // 3. Overview
  // ==================================================================
  async function renderOverview(_arg, opts = {}) {
    const data = await api('/api/overview');
    const v = $('#view');
    v.innerHTML = '';

    // KPI row
    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('团队', data.totalTeams || 0,
        `运行中 ${data.runningTeams || 0} · 失败 ${data.failedTeams || 0}`, 'accent'),
      statCard('经验库', data.totalExperiences || 0,
        `质量均值 ${fmtNum(data.avgQuality || 0, 2)}`, 'purple'),
      statCard('激活 Cron', data.activeCrons || 0,
        `Dreaming: ${data.dreamEnabled ? 'ON' : 'OFF'} · ${data.dreamCount || 0} 次`, ''),
      statCard('数据就绪', data.ready ? '✓' : '✗',
        data.stateDir || '—', data.ready ? 'ok' : 'err'),
    ]));

    // Three columns: running / recent / alerts
    const three = h('div', { class: 'three-col mb-16' });
    const recent = data.recentRuns || [];
    const running = recent.filter(t => t.status === 'running');
    const done = recent.filter(t => t.status !== 'running');

    // Running teams
    const runCol = h('div', { class: 'card' }, [h('h3', {}, '正在运行的团队 (' + running.length + ')')]);
    if (!running.length) runCol.appendChild(h('div', { class: 'empty' }, '没有运行中的团队'));
    for (const t of running.slice(0, 8)) {
      runCol.appendChild(h('a', {
        href: '#/teams/' + encodeURIComponent(t.name), class: 'team-card',
        style: { display: 'block' },
      }, [
        h('div', { class: 'team-name' }, t.name),
        h('div', { class: 'team-meta' }, [
          h('span', { class: 'badge run' }, t.workflow || '—'),
          h('span', {}, fmtRel(t.startedAt || t.createdAt)),
        ]),
      ]));
    }
    three.appendChild(runCol);

    // Recent completed
    const recCol = h('div', { class: 'card' }, [h('h3', {}, '最近完成的团队')]);
    if (!done.length) recCol.appendChild(h('div', { class: 'empty' }, '暂无完成记录'));
    for (const t of done.slice(0, 8)) {
      recCol.appendChild(h('a', {
        href: '#/teams/' + encodeURIComponent(t.name), class: 'team-card',
        style: { display: 'block' },
      }, [
        h('div', { class: 'team-name' }, t.name),
        h('div', { class: 'team-meta' }, [
          h('span', { class: 'badge ' + statusBadgeClass(t.status) }, t.status || '—'),
          h('span', {}, fmtDur(t.durationSec)),
          h('span', {}, fmtRel(t.finishedAt || t.createdAt)),
        ]),
      ]));
    }
    three.appendChild(recCol);

    // Alerts
    const alerts = data.alerts || [];
    const alCol = h('div', { class: 'card' }, [h('h3', {}, '告警 (' + alerts.length + ')')]);
    if (!alerts.length) alCol.appendChild(h('div', { class: 'empty' }, '没有告警, 系统健康'));
    for (const a of alerts.slice(0, 10)) {
      alCol.appendChild(h('div', { class: 'insight warn' }, [
        h('div', { class: 'insight-title' }, typeof a === 'string' ? a : (a.title || a.message || '告警')),
      ]));
    }
    three.appendChild(alCol);
    v.appendChild(three);

    // Insights (rule-driven) + LLM button
    const insightsCard = h('div', { class: 'card mb-16' }, [
      sectionHeader('智能诊断 · 规则驱动', h('div', {}, [
        h('button', {
          class: 'btn small primary', id: 'llm-insights-btn',
          onClick: async () => {
            const btn = $('#llm-insights-btn');
            btn.disabled = true; btn.textContent = 'LLM 分析中…';
            try {
              const resp = await api('/api/insights?llm=1');
              renderLLMSummaryPanel(insightsCard, resp);
              toast('LLM 诊断已生成', 'ok');
            } catch (e) {
              toast('LLM 分析失败: ' + e.message, 'err');
            } finally {
              btn.disabled = false; btn.textContent = '✨ LLM 深度诊断';
            }
          },
        }, '✨ LLM 深度诊断'),
      ])),
    ]);
    v.appendChild(insightsCard);
    try {
      const ins = await api('/api/insights');
      const list = h('div', { class: 'insight-list' });
      if (!ins.insights || !ins.insights.length) {
        list.appendChild(h('div', { class: 'empty' }, '没有规则洞察, 一切正常'));
      } else {
        for (const i of ins.insights.slice(0, 8)) {
          list.appendChild(h('div', { class: 'insight ' + (i.severity || 'info') }, [
            h('div', { class: 'insight-title' }, i.title || '—'),
            h('div', { class: 'insight-meta' }, i.module || ''),
            i.suggestion ? h('div', { class: 'insight-sug' }, '▸ ' + i.suggestion) : null,
          ]));
        }
      }
      insightsCard.appendChild(list);
    } catch (_) {
      insightsCard.appendChild(h('div', { class: 'empty' }, '诊断不可用'));
    }
  }

  function renderLLMSummaryPanel(container, resp) {
    const exist = $('.llm-summary', container);
    if (exist) exist.remove();
    const panel = h('div', { class: 'card llm-summary mb-16', style: { marginTop: '10px' } });
    panel.appendChild(h('h3', {}, '✨ LLM 深度诊断 · ' + (resp.llmEnabled ? '已启用' : '未启用')));
    if (resp.llmError) {
      panel.appendChild(h('div', { class: 'insight critical' }, [
        h('div', { class: 'insight-title' }, 'LLM 错误'),
        h('div', { class: 'insight-sug' }, resp.llmError),
        h('div', { class: 'insight-sug' }, '提示: export ANTHROPIC_API_KEY=... 后重试'),
      ]));
    } else if (resp.llmSummary) {
      const md = h('div', { class: 'md' });
      md.innerHTML = renderMarkdown(resp.llmSummary);
      panel.appendChild(md);
    } else {
      panel.appendChild(h('div', { class: 'empty' }, '(无摘要)'));
    }
    container.appendChild(panel);
  }

  // ==================================================================
  // 4. Teams (list / cards + compare + search + actions)
  // ==================================================================
  async function renderTeams(_arg, opts = {}) {
    const list = await api('/api/teams');
    const v = $('#view');
    v.innerHTML = '';

    // stats header
    const running = list.filter(t => t.status === 'running').length;
    const completed = list.filter(t => t.status === 'completed').length;
    const failed = list.filter(t => t.status === 'failed').length;

    const toolbar = h('div', { class: 'toolbar-row mb-12' }, [
      h('div', {}, [
        h('strong', {}, '团队总数: ' + list.length),
        h('span', { class: 'muted', style: { marginLeft: '12px' } },
          `运行中 ${running} · 已完成 ${completed} · 失败 ${failed}`),
      ]),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        h('input', {
          class: 'input', placeholder: '过滤: 名称 / workflow / objective',
          value: state.teamFilter,
          onInput: e => { state.teamFilter = e.target.value; renderTeamsFilteredBody(); },
          style: { minWidth: '280px' },
        }),
        viewToggle(state.teamsView, v => { state.teamsView = v; localStorage.setItem('teamsView', v); navigate(); }),
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

    const content = h('div', { id: 'teams-content' });
    v.appendChild(content);
    v.__teamsList = list;
    renderTeamsFilteredBody();
  }

  function renderTeamsFilteredBody() {
    const v = $('#view');
    const list = (v && v.__teamsList) || [];
    const content = $('#teams-content');
    if (!content) return;
    content.innerHTML = '';

    const filter = (state.teamFilter || '').trim().toLowerCase();
    const filtered = !filter ? list : list.filter(t =>
      (t.name || '').toLowerCase().includes(filter)
      || (t.objective || '').toLowerCase().includes(filter)
      || (t.workflow || '').toLowerCase().includes(filter)
      || (t.status || '').toLowerCase().includes(filter));

    if (state.cmp.enabled || state.teamsView === 'list') {
      // 对比模式或列表视图 - 双列
      const layout = h('div', { class: 'two-col' });
      const leftBody = h('div', { class: 'team-list-body' });
      if (!filtered.length) leftBody.appendChild(h('div', { class: 'empty' }, '无匹配的团队'));
      for (const t of filtered) {
        const picked = state.cmp.picked.has(t.name);
        const card = h('div', {
          class: 'team-card' + (picked ? ' active' : ''),
          onClick: () => {
            if (state.cmp.enabled) {
              if (picked) state.cmp.picked.delete(t.name);
              else {
                if (state.cmp.picked.size >= 5) { toast('对比模式下最多选择 5 个团队', 'err'); return; }
                state.cmp.picked.add(t.name);
              }
              renderTeamsFilteredBody();
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
      const left = h('div', { class: 'team-list' }, [
        h('div', { class: 'team-list-header' }, [
          h('span', {}, state.cmp.enabled ? '选择 2-5 个进行对比' : `共 ${filtered.length} 个团队`),
        ]),
        leftBody,
      ]);
      const right = h('div', { class: 'team-detail' });
      if (state.cmp.enabled) {
        renderTeamsCompare(right).catch(e => { right.innerHTML = ''; right.appendChild(h('div', { class: 'empty' }, '对比失败: ' + e.message)); });
      } else {
        right.appendChild(h('div', { class: 'empty' }, '← 从左侧选择一个团队查看详情, 或打开对比模式以并排分析'));
      }
      layout.appendChild(left);
      layout.appendChild(right);
      content.appendChild(layout);
    } else {
      // cards 视图
      const grid = h('div', { class: 'teams-grid' });
      if (!filtered.length) grid.appendChild(h('div', { class: 'empty' }, '无匹配的团队'));
      for (const t of filtered) {
        const stagesTotal = t.stagesTotal || 0;
        const stagesDone = t.stagesDone || 0;
        const prog = stagesTotal > 0 ? stagesDone / stagesTotal : 0;
        grid.appendChild(h('div', { class: 'team-card' }, [
          h('div', { class: 'team-name', onClick: () => location.hash = '#/teams/' + encodeURIComponent(t.name) }, t.name),
          h('div', { class: 'team-meta' }, [
            h('span', { class: 'badge ' + statusBadgeClass(t.status) }, t.status || '—'),
            h('span', { class: 'badge' }, t.workflow || '—'),
            h('span', {}, fmtRel(t.createdAt)),
          ]),
          t.objective ? h('div', { class: 'team-obj' }, t.objective) : null,
          h('div', { class: 'team-progress' }, h('div', { style: { width: (prog * 100).toFixed(1) + '%' } })),
          h('div', { class: 'muted', style: { fontSize: '11px' } }, `${stagesDone}/${stagesTotal} stages · ${fmtDur(t.durationSec)}`),
          h('div', { class: 'team-actions' }, [
            h('button', { class: 'btn small', onClick: () => location.hash = '#/teams/' + encodeURIComponent(t.name) }, '详情'),
            t.status === 'running'
              ? h('button', { class: 'btn small danger', onClick: () => execAction('team', 'stop', t.name) }, '停止')
              : h('button', { class: 'btn small', onClick: () => execAction('team', 'restart', t.name) }, '重启'),
          ]),
        ]));
      }
      content.appendChild(grid);
    }
  }

  async function renderTeamsCompare(container) {
    container.innerHTML = '';
    const picks = [...state.cmp.picked];
    if (picks.length < 2) {
      container.appendChild(h('div', { class: 'empty' }, '请至少选择 2 个团队以进行多维对比 (耗时/完成率/失败率/对抗评分/Agent 数)'));
      return;
    }
    container.appendChild(h('h3', {}, '多维对比 · 雷达图'));
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
      if (ar.length) advAvg = ar.reduce((s, r) => s + (r.avgScore || 0), 0) / ar.length / 10;
      rows.push({
        name: d.name, durationSec: d.durationSec || 0,
        compRate, failRate, advAvg, agentsTotal: d.agentsTotal || 0,
      });
    }
    const maxDur = Math.max(...rows.map(r => r.durationSec), 1);
    const maxAg = Math.max(...rows.map(r => r.agentsTotal), 1);
    const datasets = rows.map((r, i) => {
      const hue = (i * 67) % 360;
      const color = `hsl(${hue},75%,65%)`;
      return {
        label: r.name,
        data: [
          Math.min(1, r.durationSec / maxDur),
          r.compRate, r.failRate, r.advAvg,
          r.agentsTotal / maxAg,
        ],
        borderColor: color,
        backgroundColor: `hsla(${hue},75%,65%,0.18)`,
        pointBackgroundColor: color,
      };
    });

    const wrap = h('div', { class: 'chart-wrap radar' });
    const cv = h('canvas');
    wrap.appendChild(cv);
    container.appendChild(wrap);

    // destroy previous compare chart if any
    if (state.charts.cmp) try { state.charts.cmp.destroy(); } catch (_) {}
    state.charts.cmp = new Chart(cv.getContext('2d'), {
      type: 'radar',
      data: { labels: axes, datasets },
      options: {
        responsive: true, maintainAspectRatio: false,
        plugins: { legend: { position: 'bottom', labels: { color: '#c6cbe8' } } },
        scales: {
          r: {
            min: 0, max: 1,
            ticks: { color: '#6d7aa0', backdropColor: 'transparent', stepSize: 0.25 },
            grid: { color: 'rgba(255,255,255,0.08)' },
            angleLines: { color: 'rgba(255,255,255,0.1)' },
            pointLabels: { color: '#d6dcf2', font: { size: 12 } },
          },
        },
      },
    });

    const tbl = h('table', { class: 'tbl', style: { marginTop: '14px' } });
    tbl.appendChild(h('thead', {}, h('tr', {}, [
      h('th', {}, '团队'), h('th', {}, '耗时(s)'), h('th', {}, '完成率'),
      h('th', {}, '失败率'), h('th', {}, '对抗avg/10'), h('th', {}, 'Agents'),
    ])));
    const tb = h('tbody');
    for (const r of rows) {
      tb.appendChild(h('tr', {}, [
        h('td', {}, r.name),
        h('td', {}, fmtNum(r.durationSec, 1)),
        h('td', {}, (r.compRate * 100).toFixed(1) + '%'),
        h('td', {}, (r.failRate * 100).toFixed(1) + '%'),
        h('td', {}, (r.advAvg * 10).toFixed(2)),
        h('td', {}, String(r.agentsTotal)),
      ]));
    }
    tbl.appendChild(tb);
    container.appendChild(tbl);
    container.appendChild(h('div', { class: 'muted', style: { marginTop: '8px', fontSize: '12px' } },
      '提示: 耗时/Agents 按当前选中团队内 max 归一化; 完成率 = stagesDone/stagesTotal; 对抗 avg 取各轮平均(0-10 归一到 0-1)。'));
  }

  // ==================================================================
  // 5. Team Detail (+ DAG tab)
  // ==================================================================
  async function renderTeamDetail(name, opts = {}) {
    const [list, detail] = await Promise.all([
      api('/api/teams'),
      api('/api/teams/' + encodeURIComponent(name)),
    ]);
    state.currentTeam = name;
    const v = $('#view');
    if (!state.activeTab) state.activeTab = 'dag';
    v.innerHTML = '';

    const layout = h('div', { class: 'two-col' });

    // Left: team list
    const leftBody = h('div', { class: 'team-list-body' });
    for (const t of list) {
      const active = t.name === name;
      leftBody.appendChild(h('div', {
        class: 'team-card' + (active ? ' active' : ''),
        onClick: () => location.hash = '#/teams/' + encodeURIComponent(t.name),
      }, [
        h('div', { class: 'team-name' }, t.name),
        h('div', { class: 'team-meta' }, [
          h('span', { class: 'badge ' + statusBadgeClass(t.status) }, t.status || '—'),
          h('span', { class: 'badge' }, t.workflow || '—'),
        ]),
      ]));
    }
    layout.appendChild(h('div', { class: 'team-list' }, [
      h('div', { class: 'team-list-header' }, [
        h('a', { href: '#/teams' }, '« 返回列表'),
        h('span', {}, '共 ' + list.length),
      ]),
      leftBody,
    ]));

    // Right: detail
    const right = h('div', { class: 'team-detail' });
    right.appendChild(h('h2', {}, [
      h('span', {}, detail.name || name),
      h('span', { class: 'badge ' + statusBadgeClass(detail.status) }, detail.status || '—'),
      h('span', { class: 'badge purple' }, detail.workflow || '—'),
    ]));
    right.appendChild(h('div', { class: 'team-sub' }, [
      h('span', {}, 'objective: '),
      h('span', {}, detail.objective || '—'),
    ]));

    // actions toolbar
    right.appendChild(h('div', { class: 'actions' }, [
      (detail.status === 'running')
        ? h('button', { class: 'btn danger', onClick: () => execAction('team', 'stop', name) }, '⏹ 停止')
        : h('button', { class: 'btn', onClick: () => execAction('team', 'restart', name) }, '↻ 重启'),
      h('button', { class: 'btn ghost', onClick: () => execAction('team', 'delete', name, { confirm: '删除该团队的所有记录?' }) }, '🗑 删除'),
      h('button', { class: 'btn ghost small', onClick: () => navigator.clipboard && navigator.clipboard.writeText(location.href) }, '复制链接'),
    ]));

    right.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('阶段', `${detail.stagesDone}/${detail.stagesTotal}`,
        `失败 ${detail.stagesFail}`, detail.stagesFail > 0 ? 'warn' : 'accent'),
      statCard('Agents', detail.agentsTotal || 0, '参与角色数', 'purple'),
      statCard('总耗时', fmtDur(detail.durationSec), fmtRel(detail.createdAt), 'accent'),
      statCard('对抗轮次', (detail.adversaryRounds || []).length, '多轮 Eval', 'accent'),
    ]));

    const tabs = ['dag', 'timeline', 'stages', 'agents', 'eval', 'metrics', 'blackboard', 'report'];
    const tabBar = h('div', { class: 'tabs' });
    for (const t of tabs) {
      tabBar.appendChild(h('div', {
        class: 'tab' + (t === state.activeTab ? ' active' : ''),
        onClick: () => { state.activeTab = t; renderTeamDetail(name, { quiet: true }); },
      }, labelTab(t)));
    }
    right.appendChild(tabBar);

    const panel = h('div');
    right.appendChild(panel);
    layout.appendChild(right);
    v.appendChild(layout);

    await renderTeamTab(state.activeTab, detail, panel);
  }

  function labelTab(t) {
    return ({
      dag: 'DAG',
      timeline: '时间线',
      stages: '阶段',
      agents: 'Agents',
      eval: 'Eval',
      metrics: 'Metrics',
      blackboard: '黑板',
      report: 'Report',
    })[t] || t;
  }

  async function renderTeamTab(tab, detail, panel) {
    panel.innerHTML = '';
    if (tab === 'dag') {
      await renderTeamDAG(detail, panel);
    } else if (tab === 'timeline') {
      renderTeamTimeline(detail, panel);
    } else if (tab === 'stages') {
      renderTeamStages(detail, panel);
    } else if (tab === 'agents') {
      renderTeamAgents(detail, panel);
    } else if (tab === 'blackboard') {
      try {
        const bb = await api('/api/teams/' + encodeURIComponent(detail.name) + '/blackboard');
        if (!bb || Object.keys(bb).length === 0) panel.appendChild(h('div', { class: 'empty' }, '黑板为空'));
        else panel.appendChild(h('pre', { class: 'pre' }, JSON.stringify(bb, null, 2)));
      } catch (_) { panel.appendChild(h('div', { class: 'empty' }, '无黑板数据')); }
    } else if (tab === 'report') {
      if (!detail.report) panel.appendChild(h('div', { class: 'empty' }, '没有 REPORT.md'));
      else { const mdBox = h('div', { class: 'md' }); mdBox.innerHTML = renderMarkdown(detail.report); panel.appendChild(mdBox); }
    } else if (tab === 'eval') {
      renderTeamEval(detail, panel);
    } else if (tab === 'metrics') {
      renderTeamMetrics(detail, panel);
    }
  }

  // ---- DAG ----
  async function renderTeamDAG(detail, panel) {
    const wfName = detail.workflow || 'development';
    let wf = null;
    try { wf = await api('/api/workflows/' + encodeURIComponent(wfName)); } catch (_) {}
    const stageStatus = {};
    (detail.stages || []).forEach(s => {
      // 允许多条同名 (round1/round2) 只存第一条状态
      if (!stageStatus[s.name]) stageStatus[s.name] = s.status;
      else if (s.status === 'failed') stageStatus[s.name] = 'failed';
      else if (stageStatus[s.name] === 'pending') stageStatus[s.name] = s.status;
    });

    if (!wf) {
      // fallback - 线性
      const nodes = (detail.stages || []).map((s, i) => ({ name: s.name, role: s.role, dependsOn: i === 0 ? [] : [detail.stages[i - 1].name], status: s.status }));
      panel.appendChild(renderDAGSVG(nodes, '(未知 workflow, 按顺序推断)'));
      return;
    }

    const nodes = wf.stages.map(s => ({
      name: s.name, role: s.role,
      dependsOn: s.dependsOn || [],
      parallel: s.parallel,
      status: stageStatus[s.name] || 'pending',
    }));

    const meta = h('div', { class: 'muted', style: { marginBottom: '10px', fontSize: '12px' } }, [
      h('strong', {}, 'workflow: '), wf.name,
      ' · mode=', wf.mode,
      ' · stages=', String(wf.stages.length),
      wf.rounds ? (' · rounds=' + wf.rounds) : '',
    ]);
    panel.appendChild(meta);
    panel.appendChild(renderDAGSVG(nodes, wf.description));
    panel.appendChild(h('div', { class: 'legend' }, [
      h('span', {}, [h('span', { class: 'dot ok' }), '完成']),
      h('span', {}, [h('span', { class: 'dot run' }), '运行']),
      h('span', {}, [h('span', { class: 'dot err' }), '失败']),
      h('span', {}, [h('span', { class: 'dot pending' }), '待执行']),
    ]));
  }

  function renderDAGSVG(nodes, subtitle) {
    // 拓扑排序分层
    const nameToIdx = {};
    nodes.forEach((n, i) => { nameToIdx[n.name] = i; });
    const layers = [];
    const visited = new Map();
    const depth = (n) => {
      if (visited.has(n.name)) return visited.get(n.name);
      let d = 0;
      for (const p of n.dependsOn || []) {
        const pn = nodes[nameToIdx[p]];
        if (pn) d = Math.max(d, depth(pn) + 1);
      }
      visited.set(n.name, d);
      return d;
    };
    nodes.forEach(n => depth(n));
    const byLayer = {};
    nodes.forEach(n => {
      const d = visited.get(n.name);
      (byLayer[d] ||= []).push(n);
    });
    const layerKeys = Object.keys(byLayer).map(Number).sort((a, b) => a - b);
    const cols = layerKeys.length;
    const maxRows = Math.max(...Object.values(byLayer).map(a => a.length));
    const W = Math.max(720, 220 * cols + 80);
    const nodeW = 170, nodeH = 52;
    const colGap = (W - 80 - nodeW * cols) / Math.max(1, cols - 1) + nodeW;
    const H = 80 + maxRows * 76;
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('width', W); svg.setAttribute('height', H);
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    svg.innerHTML = `
      <defs>
        <marker id="dag-arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
          <path d="M0,0 L10,5 L0,10 Z" fill="rgba(138,160,230,0.5)" />
        </marker>
      </defs>`;
    if (subtitle) {
      const t = document.createElementNS('http://www.w3.org/2000/svg', 'text');
      t.setAttribute('x', 16); t.setAttribute('y', 24);
      t.setAttribute('fill', '#8b93c2'); t.setAttribute('font-size', '12');
      t.textContent = subtitle.length > 120 ? subtitle.slice(0, 120) + '…' : subtitle;
      svg.appendChild(t);
    }
    // layout nodes
    const pos = {};
    layerKeys.forEach((d, colIdx) => {
      const arr = byLayer[d];
      arr.forEach((n, rowIdx) => {
        const x = 40 + colIdx * colGap;
        const y = 40 + rowIdx * 76;
        pos[n.name] = { x, y, ...n };
      });
    });
    // edges
    for (const n of nodes) {
      for (const p of n.dependsOn || []) {
        const from = pos[p]; const to = pos[n.name];
        if (!from || !to) continue;
        const x1 = from.x + nodeW, y1 = from.y + nodeH / 2;
        const x2 = to.x,          y2 = to.y + nodeH / 2;
        const mx = (x1 + x2) / 2;
        const d = `M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}`;
        const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
        const cls = (to.status === 'running') ? 'dag-edge run'
                  : (to.status === 'completed' ? 'dag-edge ok' : 'dag-edge');
        path.setAttribute('class', cls);
        path.setAttribute('d', d);
        svg.appendChild(path);
      }
    }
    // nodes
    for (const n of nodes) {
      const p = pos[n.name];
      const g = document.createElementNS('http://www.w3.org/2000/svg', 'g');
      const cls = 'dag-node ' + (
        n.status === 'completed' ? 'ok'
        : n.status === 'running' ? 'run'
        : n.status === 'failed' ? 'err'
        : 'pending');
      g.setAttribute('class', cls);
      g.setAttribute('transform', `translate(${p.x},${p.y})`);
      const r = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
      r.setAttribute('rx', 8); r.setAttribute('ry', 8);
      r.setAttribute('width', nodeW); r.setAttribute('height', nodeH);
      g.appendChild(r);
      const t1 = document.createElementNS('http://www.w3.org/2000/svg', 'text');
      t1.setAttribute('x', 12); t1.setAttribute('y', 20); t1.setAttribute('class', 'name');
      t1.textContent = n.name;
      g.appendChild(t1);
      const t2 = document.createElementNS('http://www.w3.org/2000/svg', 'text');
      t2.setAttribute('x', 12); t2.setAttribute('y', 38); t2.setAttribute('class', 'role');
      t2.textContent = (n.role || '—') + ' · ' + (n.status || 'pending');
      g.appendChild(t2);
      svg.appendChild(g);
    }
    const wrap = h('div', { class: 'dag-canvas' });
    wrap.appendChild(svg);
    return wrap;
  }

  // ---- timeline / stages / agents / eval / metrics ----
  function renderTeamTimeline(detail, panel) {
    const stages = detail.stages || [];
    if (!stages.length) { panel.appendChild(h('div', { class: 'empty' }, '无阶段数据')); return; }
    const tl = h('div', { class: 'timeline' });
    for (const s of stages) {
      const cls = s.status === 'completed' ? 'ok' : s.status === 'failed' ? 'err' : s.status === 'running' ? 'run' : '';
      tl.appendChild(h('div', { class: 'tl-item ' + cls }, [
        h('span', { class: 'tl-dot' }),
        h('div', { class: 'tl-head' }, (s.role ? (s.role + ' · ') : '') + s.name),
        h('div', { class: 'tl-meta' }, [
          h('span', { class: 'badge ' + statusBadgeClass(s.status) }, s.status || '—'),
          h('span', { class: 'muted' }, fmtDur(s.durationSec)),
          s.startedAt ? h('span', { class: 'muted' }, '开始: ' + fmtTime(s.startedAt)) : null,
        ]),
        s.error ? h('div', { style: { color: 'var(--red)', fontSize: '12px' } }, s.error) : null,
      ]));
    }
    panel.appendChild(tl);
  }

  function renderTeamStages(detail, panel) {
    const stages = detail.stages || [];
    if (!stages.length) { panel.appendChild(h('div', { class: 'empty' }, '无阶段数据')); return; }
    for (const s of stages) {
      panel.appendChild(h('div', { class: 'card mb-16' }, [
        h('div', { style: { display: 'flex', gap: '10px', alignItems: 'center' } }, [
          h('div', { style: { flex: '1', fontWeight: '600' } }, (s.role ? s.role + ' · ' : '') + s.name),
          h('span', { class: 'badge ' + statusBadgeClass(s.status) }, s.status || '—'),
          h('span', { class: 'badge' }, fmtDur(s.durationSec)),
        ]),
        s.input ? h('details', { class: 'mt-12' }, [h('summary', { class: 'muted' }, 'Input'), h('pre', {}, s.input)]) : null,
        s.output ? h('details', { class: 'mt-12' }, [h('summary', { class: 'muted' }, 'Output'), h('pre', {}, s.output)]) : null,
        s.error ? h('pre', { style: { color: 'var(--red)' } }, s.error) : null,
      ]));
    }
  }

  function renderTeamAgents(detail, panel) {
    const agents = detail.agents || [];
    if (!agents.length) { panel.appendChild(h('div', { class: 'empty' }, '无 Agent')); return; }
    const grid = h('div', { class: 'grid grid-3' });
    for (const a of agents) {
      grid.appendChild(h('div', { class: 'card' }, [
        h('div', { style: { display: 'flex', gap: '8px' } }, [
          h('div', { style: { flex: '1', fontWeight: '600' } }, a.name),
          h('span', { class: 'badge ' + statusBadgeClass(a.status) }, a.status || '—'),
        ]),
        h('div', { class: 'muted mt-12' }, '角色: ' + (a.role || '—')),
        a.result ? h('pre', { class: 'mt-12' }, a.result) : null,
        a.error ? h('pre', { class: 'mt-12', style: { color: 'var(--red)' } }, a.error) : null,
      ]));
    }
    panel.appendChild(grid);
  }

  function renderTeamEval(detail, panel) {
    const rounds = detail.adversaryRounds || [];
    if (!rounds.length) { panel.appendChild(h('div', { class: 'empty' }, '该团队没有对抗循环评分数据 (仅 dev workflow 多轮评估时产生)')); return; }
    const last = rounds[rounds.length - 1];
    panel.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('轮次', rounds.length, '最后一轮: round ' + last.round, 'accent'),
      statCard('综合分', fmtNum(last.avgScore, 2), '5 维平均', last.passed ? 'ok' : 'warn'),
      statCard('通过', last.passed ? 'true' : 'false', '阈值由 workflow 决定', last.passed ? 'ok' : 'err'),
      statCard('维度', '5', '正确/完整/安全/质量/对齐', 'purple'),
    ]));

    const radarCard = h('div', { class: 'card mb-16' }, [h('h3', {}, '最近一轮 · 5 维雷达'), h('div', { class: 'chart-wrap radar' }, h('canvas', { id: 'chart-eval-radar' }))]);
    panel.appendChild(radarCard);
    const lineCard = h('div', { class: 'card mb-16' }, [h('h3', {}, '逐轮质量演化'), h('div', { class: 'chart-wrap' }, h('canvas', { id: 'chart-eval-line' }))]);
    panel.appendChild(lineCard);

    const tbl = h('table', { class: 'tbl' });
    tbl.appendChild(h('thead', {}, h('tr', {}, [
      h('th', {}, '轮'), h('th', {}, '正确'), h('th', {}, '完整'), h('th', {}, '安全'),
      h('th', {}, '质量'), h('th', {}, '对齐'), h('th', {}, '均分'), h('th', {}, '通过'),
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
      const radar = document.getElementById('chart-eval-radar');
      if (radar && window.Chart) {
        state.charts.evalRadar = new Chart(radar.getContext('2d'), {
          type: 'radar',
          data: {
            labels: ['正确', '完整', '安全', '质量', '对齐'],
            datasets: [{
              label: 'round ' + last.round,
              data: [last.correctness, last.completeness, last.security, last.codeQuality, last.designAlignment],
              borderColor: '#5ef0ff',
              backgroundColor: 'rgba(94,240,255,0.2)',
              pointBackgroundColor: '#a478ff',
            }],
          },
          options: {
            responsive: true, maintainAspectRatio: false,
            plugins: { legend: { labels: { color: '#c6cbe8' } } },
            scales: {
              r: {
                min: 0, max: 10, grid: { color: 'rgba(138,160,230,0.12)' },
                angleLines: { color: 'rgba(138,160,230,0.15)' },
                pointLabels: { color: '#d6dcf2' },
                ticks: { color: '#8b93c2', backdropColor: 'transparent' },
              },
            },
          },
        });
      }
      const line = document.getElementById('chart-eval-line');
      if (line && window.Chart) {
        state.charts.evalLine = new Chart(line.getContext('2d'), {
          type: 'line',
          data: {
            labels: rounds.map(r => 'R' + r.round),
            datasets: [
              { label: '正确', data: rounds.map(r => r.correctness), borderColor: '#5ef0ff', tension: 0.2 },
              { label: '完整', data: rounds.map(r => r.completeness), borderColor: '#7cffb2', tension: 0.2 },
              { label: '安全', data: rounds.map(r => r.security), borderColor: '#ff5e7a', tension: 0.2 },
              { label: '质量', data: rounds.map(r => r.codeQuality), borderColor: '#a478ff', tension: 0.2 },
              { label: '对齐', data: rounds.map(r => r.designAlignment), borderColor: '#ffcf66', tension: 0.2 },
              { label: '均分', data: rounds.map(r => r.avgScore), borderColor: '#ffffff', borderDash: [5, 3], tension: 0.2 },
            ],
          },
          options: baseChartOpts({}),
        });
      }
    });
  }

  function renderTeamMetrics(detail, panel) {
    const series = detail.runMetrics || [];
    if (!series.length) { panel.appendChild(h('div', { class: 'empty' }, '无 per-run 指标 (labels.team 标注)')); return; }
    for (const s of series) {
      panel.appendChild(h('div', { class: 'card mb-16' }, [
        h('div', { style: { display: 'flex', gap: '8px' } }, [
          h('div', { style: { flex: '1', fontWeight: '600' } }, s.name),
          h('span', { class: 'badge' }, s.points.length + ' points'),
        ]),
        h('div', { class: 'chart-wrap short' }, h('canvas', { id: 'chart-tm-' + s.name.replace(/[^a-z0-9]/gi, '_') })),
      ]));
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
              borderColor: '#5ef0ff',
              backgroundColor: 'rgba(94,240,255,0.15)',
              tension: 0.25, fill: true, pointRadius: 2,
            }],
          },
          options: baseChartOpts({}),
        });
      }
    });
  }

  // ==================================================================
  // 6. Metrics
  // ==================================================================
  async function renderMetrics() {
    const sums = await api('/api/metrics');
    const v = $('#view');
    v.innerHTML = '';
    if (!sums.length) { v.appendChild(h('div', { class: 'empty' }, '暂无指标数据')); return; }
    const grid = h('div', { class: 'grid grid-2' });
    for (const s of sums) {
      const alerts = s.trendAlerts || [];
      const card = h('div', {
        class: 'card', style: { cursor: 'pointer' },
        onClick: () => location.hash = '#/metrics/' + encodeURIComponent(s.module),
      }, [
        h('div', { style: { display: 'flex', gap: '8px', alignItems: 'baseline' } }, [
          h('div', { style: { flex: '1', fontWeight: '700', fontSize: '15px' } }, s.module),
          h('span', { class: 'badge' }, (s.eventCount || 0) + ' events'),
          alerts.length ? h('span', { class: 'badge warn' }, alerts.length + ' alert') : null,
        ]),
        h('div', { class: 'muted mt-12', style: { fontSize: '12px' } },
          Object.keys(s.metrics || {}).slice(0, 8).join(' · ') || '—'),
        alerts.length ? h('div', { class: 'mt-12', style: { color: 'var(--amber)', fontSize: '12px' } }, alerts.slice(0, 2).join('; ')) : null,
      ]);
      grid.appendChild(card);
    }
    v.appendChild(grid);
  }

  async function renderMetricsDetail(module) {
    state.activeMetricModule = module;
    const params = new URLSearchParams(window.location.search);
    const since = params.get('since') || '';
    const qs = since ? '?since=' + encodeURIComponent(since) : '';
    const data = await api('/api/metrics/' + encodeURIComponent(module) + qs);
    const v = $('#view');
    v.innerHTML = '';
    v.appendChild(h('div', { class: 'toolbar-row' }, [
      h('h3', {}, '指标模块 · ' + module),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
        h('select', {
          class: 'select',
          onChange: (e) => {
            const q = e.target.value ? '?since=' + encodeURIComponent(e.target.value) : '';
            // 为保持 hash 路由, 不用 query; 直接用 localStorage 存 since 并重渲染
            location.hash = '#/metrics/' + encodeURIComponent(module);
          },
        }, [
          h('option', { value: '' }, '全部'),
          h('option', { value: '1h' }, '最近 1 小时'),
          h('option', { value: '24h' }, '最近 24 小时'),
          h('option', { value: '168h' }, '最近 7 天'),
        ]),
        h('a', {
          class: 'btn small', href: '/api/metrics/' + encodeURIComponent(module) + '?format=csv', target: '_blank',
        }, '⬇ CSV'),
      ]),
    ]));

    const summary = data.summary;
    if (summary) {
      v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
        statCard('事件数', summary.eventCount || 0, '', 'accent'),
        statCard('指标数', Object.keys(summary.metrics || {}).length, '', 'purple'),
        statCard('最近更新', fmtTsShort(summary.snapshotTime), '', ''),
        statCard('告警', (summary.trendAlerts || []).length, summary.trendAlerts?.[0] || '—', (summary.trendAlerts || []).length ? 'warn' : 'ok'),
      ]));
    }

    // 每个指标一张 chart
    const byName = {};
    for (const e of (data.events || [])) {
      (byName[e.name] ||= []).push(e);
    }
    const names = Object.keys(byName);
    if (!names.length) { v.appendChild(h('div', { class: 'empty' }, '无事件')); return; }
    for (const name of names) {
      const pts = byName[name];
      v.appendChild(h('div', { class: 'card mb-16' }, [
        h('div', { style: { display: 'flex', gap: '8px' } }, [
          h('div', { style: { flex: '1', fontWeight: '600' } }, name),
          h('span', { class: 'badge' }, pts.length + ' pts'),
        ]),
        h('div', { class: 'chart-wrap' }, h('canvas', { id: 'chart-m-' + name.replace(/[^a-z0-9]/gi, '_') })),
      ]));
    }
    requestAnimationFrame(() => {
      for (const name of names) {
        const pts = byName[name];
        const cid = 'chart-m-' + name.replace(/[^a-z0-9]/gi, '_');
        const cv = document.getElementById(cid);
        if (!cv || !window.Chart) continue;
        state.charts[cid] = new Chart(cv.getContext('2d'), {
          type: 'line',
          data: {
            labels: pts.map(p => fmtTsShort(p.ts || p.timestamp)),
            datasets: [{ label: name, data: pts.map(p => p.value), borderColor: '#5ef0ff', tension: 0.2, pointRadius: 1.5 }],
          },
          options: baseChartOpts({}),
        });
      }
    });
  }

  // ==================================================================
  // 7. Cron (list / cards + actions)
  // ==================================================================
  async function renderCron() {
    const list = await api('/api/cron');
    const v = $('#view');
    v.innerHTML = '';

    v.appendChild(h('div', { class: 'toolbar-row mb-12' }, [
      h('strong', {}, 'Cron 任务总数: ' + list.length),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center' } }, [
        h('input', {
          class: 'input', placeholder: '过滤: 名称 / workflow / payload',
          value: state.cronFilter,
          onInput: e => { state.cronFilter = e.target.value; renderCronBody(); },
        }),
        viewToggle(state.cronView, v => { state.cronView = v; localStorage.setItem('cronView', v); navigate(); }),
      ]),
    ]));
    v.__cronList = list;
    v.appendChild(h('div', { id: 'cron-body' }));
    renderCronBody();
  }

  function renderCronBody() {
    const v = $('#view');
    const list = (v && v.__cronList) || [];
    const body = $('#cron-body');
    body.innerHTML = '';
    const filter = (state.cronFilter || '').trim().toLowerCase();
    const filtered = !filter ? list : list.filter(c =>
      (c.name || '').toLowerCase().includes(filter)
      || (c.workflow || '').toLowerCase().includes(filter)
      || (c.payload || '').toLowerCase().includes(filter));
    if (!filtered.length) { body.appendChild(h('div', { class: 'empty' }, '无匹配')); return; }

    if (state.cronView === 'list') {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '名称'), h('th', {}, 'Schedule'), h('th', {}, 'Workflow'),
        h('th', {}, '状态'), h('th', {}, '最近运行'), h('th', {}, '成功/总'), h('th', {}, '动作'),
      ])));
      const tb = h('tbody');
      for (const c of filtered) {
        const status = c.enabled ? 'active' : 'paused';
        const success = Math.max(0, (c.runCount || 0) - (c.failCount || 0));
        tb.appendChild(h('tr', {}, [
          h('td', {}, c.name || c.id),
          h('td', {}, h('code', {}, c.schedule || '—')),
          h('td', {}, c.workflow || c.jobType || '—'),
          h('td', {}, h('span', { class: 'badge ' + statusBadgeClass(status) }, status)),
          h('td', {}, fmtRel(c.lastRunAt)),
          h('td', {}, `${success} / ${c.runCount || 0}`),
          h('td', {}, h('div', { style: { display: 'flex', gap: '4px' } }, [
            c.enabled
              ? h('button', { class: 'btn small ghost', onClick: () => execAction('cron', 'disable', c.id) }, '⏸')
              : h('button', { class: 'btn small', onClick: () => execAction('cron', 'enable', c.id) }, '▶'),
            h('button', { class: 'btn small primary', onClick: () => execAction('cron', 'trigger', c.id) }, '↻'),
            h('button', { class: 'btn small ghost danger', onClick: () => execAction('cron', 'remove', c.id, { confirm: '删除该 cron 任务?' }) }, '×'),
          ])),
        ]));
      }
      tbl.appendChild(tb);
      body.appendChild(tbl);
    } else {
      const grid = h('div', { class: 'teams-grid' });
      for (const c of filtered) {
        const status = c.enabled ? 'active' : 'paused';
        const success = Math.max(0, (c.runCount || 0) - (c.failCount || 0));
        grid.appendChild(h('div', { class: 'team-card' }, [
          h('div', { class: 'team-name' }, c.name || c.id),
          h('div', { class: 'team-meta' }, [
            h('span', { class: 'badge ' + statusBadgeClass(status) }, status),
            h('span', { class: 'badge' }, c.workflow || '—'),
            h('code', {}, c.schedule || '—'),
          ]),
          c.payload ? h('div', { class: 'team-obj' }, c.payload) : null,
          h('div', { class: 'muted', style: { fontSize: '11px' } }, `最近: ${fmtRel(c.lastRunAt)} · 成功 ${success}/${c.runCount || 0}`),
          h('div', { class: 'team-actions' }, [
            c.enabled
              ? h('button', { class: 'btn small ghost', onClick: () => execAction('cron', 'disable', c.id) }, '⏸ 暂停')
              : h('button', { class: 'btn small', onClick: () => execAction('cron', 'enable', c.id) }, '▶ 激活'),
            h('button', { class: 'btn small primary', onClick: () => execAction('cron', 'trigger', c.id) }, '↻ 立即运行'),
          ]),
        ]));
      }
      body.appendChild(grid);
    }
  }

  // ==================================================================
  // 8. Dreaming (+ 时序 + diagnosis)
  // ==================================================================
  async function renderDreaming() {
    const [data, diag] = await Promise.all([
      api('/api/dreaming'),
      api('/api/dreaming/diagnosis').catch(() => null),
    ]);
    const v = $('#view');
    v.innerHTML = '';

    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('启用', data.enabled ? 'ON' : 'OFF', data.enabled ? 'Dreamer 正常' : '未启用', data.enabled ? 'ok' : 'err'),
      statCard('整理次数', data.dreamCount || 0, '成功整理轮数', 'accent'),
      statCard('记忆文件', (data.memoryFiles || []).length, data.memoryDir || '—', 'purple'),
      statCard('最近运行', fmtRel(data.lastDreamAt), data.lastDreamAt ? fmtTime(data.lastDreamAt) : '从未', data.lastDreamAt ? 'ok' : 'warn'),
    ]));

    // 诊断
    if (diag && (!diag.dreamerWired || diag.reasons.length)) {
      const card = h('div', { class: 'card mb-16' }, [
        sectionHeader('诊断: Dreaming 为什么没跑?',
          h('span', { class: 'badge ' + (diag.dreamerWired ? 'ok' : 'err') },
            diag.dreamerWired ? 'CLI 已接入' : 'CLI 未接入')),
      ]);
      for (const r of diag.reasons) card.appendChild(h('div', { class: 'insight warn' }, [h('div', { class: 'insight-title' }, r)]));
      if (diag.recommendations.length) {
        card.appendChild(h('h4', { style: { marginTop: '12px' } }, '建议:'));
        const ul = h('ul');
        for (const r of diag.recommendations) ul.appendChild(h('li', {}, r));
        card.appendChild(ul);
      }
      v.appendChild(card);
    }

    // 时序 (来自 /api/timeseries/dreaming/*)
    try {
      const ts = await api('/api/timeseries/dreaming/memory_count?limit=500').catch(() => null);
      if (ts && ts.points && ts.points.length) {
        v.appendChild(renderTimeSeriesCard('dreaming · memory_count', ts));
      }
    } catch (_) {}

    // topics cloud (从 memoryFiles[].topics 聚合)
    const topicFreq = {};
    for (const f of (data.memoryFiles || [])) {
      for (const t of (f.topics || [])) topicFreq[t] = (topicFreq[t] || 0) + 1;
    }
    const topics = Object.entries(topicFreq).sort((a, b) => b[1] - a[1]).slice(0, 40);
    if (topics.length) {
      const tm = h('div', { class: 'treemap' });
      for (const [name, n] of topics) {
        tm.appendChild(h('div', {
          class: 'tile',
          style: { fontSize: (12 + Math.min(10, Math.sqrt(n))) + 'px' },
        }, [h('span', {}, name), h('span', { class: 'n' }, String(n))]));
      }
      v.appendChild(h('div', { class: 'card mb-16' }, [h('h3', {}, '主题云 · Topic Treemap'), tm]));
    }

    // 最近 dream logs
    if ((data.logs || []).length) {
      const grid = h('div', { class: 'grid grid-3' });
      for (const d of data.logs.slice(0, 9)) {
        grid.appendChild(h('div', { class: 'card' }, [
          h('div', { style: { fontWeight: '600' } }, d.filename),
          h('div', { class: 'muted', style: { fontSize: '11px' } }, fmtRel(d.modTime) + ' · ' + d.size + ' B'),
          d.preview ? h('pre', { class: 'mt-12', style: { maxHeight: '140px', overflow: 'auto' } }, d.preview) : null,
        ]));
      }
      v.appendChild(h('div', { class: 'card mb-16' }, [h('h3', {}, '最近 Dream 日志'), grid]));
    }

    // 记忆文件列表
    if (data.memoryFiles && data.memoryFiles.length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [h('th', {}, '文件'), h('th', {}, '大小'), h('th', {}, '修改时间'), h('th', {}, 'Topics')])));
      const tb = h('tbody');
      for (const f of data.memoryFiles) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, f.filename),
          h('td', {}, (f.size || 0) + ' B'),
          h('td', {}, fmtRel(f.modTime)),
          h('td', {}, (f.topics || []).slice(0, 5).join(', ')),
        ]));
      }
      tbl.appendChild(tb);
      v.appendChild(h('div', { class: 'card' }, [h('h3', {}, '记忆文件'), tbl]));
    }
  }

  // ==================================================================
  // 9. Evolution (+ 趋势判定)
  // ==================================================================
  async function renderEvolution() {
    const data = await api('/api/evolution');
    const v = $('#view');
    v.innerHTML = '';
    const roles = Object.keys(data.roleCounts || {});
    const cats = Object.keys(data.categoryCounts || {});
    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('经验总数', data.totalExperiences || 0, `轨迹 ${data.totalTrajectories || 0}`, 'accent'),
      statCard('角色覆盖', roles.length, roles.slice(0, 6).join(' · '), 'purple'),
      statCard('分类', cats.length, cats.slice(0, 6).join(' · '), ''),
      statCard('质量均值', fmtNum(data.avgQuality || 0, 3), `成功率 ${fmtNum((data.successRate || 0) * 100, 1)}%`, data.avgQuality > 0.7 ? 'ok' : 'warn'),
    ]));

    // 时序: 质量均值趋势 (需要后端有 evolution.avg_quality 时间序列; fallback: 从 experiences 计算)
    try {
      const ts = await api('/api/timeseries/evolution/quality?limit=500').catch(() => null);
      if (ts && ts.points && ts.points.length >= 3) {
        v.appendChild(renderTimeSeriesCard('Evolution · 质量趋势 (进化/退化)', ts));
      } else {
        v.appendChild(h('div', { class: 'card mb-16' }, [
          h('h3', {}, '质量时序 (趋势判定)'),
          h('div', { class: 'empty' }, '需要 metrics/evolution.jsonl 中写入 quality 事件; 当前样本不足。'),
        ]));
      }
    } catch (_) {}

    // Top experiences table
    if (data.topExperiences && data.topExperiences.length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, 'category'), h('th', {}, 'role'), h('th', {}, 'quality'),
        h('th', {}, 'success'), h('th', {}, 'used'), h('th', {}, 'content'),
      ])));
      const tb = h('tbody');
      for (const e of data.topExperiences) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, e.category),
          h('td', {}, e.role || '—'),
          h('td', {}, fmtNum(e.quality, 3)),
          h('td', {}, fmtNum((e.successRate || 0) * 100, 1) + '%'),
          h('td', {}, String(e.usageCount || 0)),
          h('td', {}, (e.content || '').slice(0, 200)),
        ]));
      }
      tbl.appendChild(tb);
      v.appendChild(h('div', { class: 'card mb-16' }, [h('h3', {}, 'Top 10 经验'), tbl]));
    }

    // Heatmap
    if (data.heatmap && data.heatmap.length) {
      v.appendChild(h('div', { class: 'card' }, [
        h('h3', {}, '经验热力图 · category × role'),
        renderHeatmapEl(data.heatmap),
      ]));
    }
  }

  function renderTimeSeriesCard(title, ts) {
    const card = h('div', { class: 'card mb-16' }, [
      sectionHeader(title, h('span', {
        class: 'badge ' + (ts.trend === 'improving' ? 'ok' : ts.trend === 'degrading' ? 'err' : ''),
      }, '趋势 · ' + ts.trend +
        (ts.changePct != null ? ` (${ts.changePct.toFixed(1)}%)` : ''))),
      h('div', { class: 'chart-wrap tall' }, h('canvas', { id: 'chart-ts-' + Math.random().toString(36).slice(2, 8) })),
    ]);
    const cv = $('canvas', card);
    requestAnimationFrame(() => {
      if (!cv || !window.Chart) return;
      state.charts[cv.id] = new Chart(cv.getContext('2d'), {
        type: 'line',
        data: {
          labels: ts.points.map(p => fmtTsShort(p.timestamp)),
          datasets: [{
            label: title,
            data: ts.points.map(p => p.value),
            borderColor: ts.trend === 'improving' ? '#7cffb2' : ts.trend === 'degrading' ? '#ff5e7a' : '#5ef0ff',
            backgroundColor: 'rgba(94,240,255,0.12)',
            fill: true, tension: 0.25, pointRadius: 2,
          }],
        },
        options: baseChartOpts({}),
      });
    });
    return card;
  }

  // ==================================================================
  // 10. Tasks (空数据容错)
  // ==================================================================
  async function renderTasks() {
    let list;
    try { list = await api('/api/tasks'); } catch (e) { list = []; toast('Tasks 加载失败: ' + e.message, 'err'); }
    const v = $('#view');
    v.innerHTML = '';
    if (!Array.isArray(list) || !list.length) { v.appendChild(h('div', { class: 'empty' }, '无 V2 任务数据')); return; }
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
    const tbl = h('table', { class: 'tbl' });
    tbl.appendChild(h('thead', {}, h('tr', {}, [
      h('th', {}, 'ID'), h('th', {}, '主题'), h('th', {}, '状态'),
      h('th', {}, '负责人'), h('th', {}, '优先级'), h('th', {}, '依赖'),
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
    v.appendChild(h('div', { class: 'card' }, [h('h3', {}, '任务列表'), tbl]));
  }

  // ==================================================================
  // 11. Logs (SSE tail)
  // ==================================================================
  async function renderLogs() {
    const v = $('#view');
    v.innerHTML = '';
    const ctrls = h('div', { class: 'log-ctrls' }, [
      h('span', { class: 'pulse ' + (state.logs.paused ? 'red' : '') }),
      h('strong', {}, state.logs.paused ? '已暂停' : '实时日志 · SSE'),
      h('input', { class: 'input', placeholder: '过滤关键词', onInput: e => state.logs.filter = e.target.value }),
      h('button', { class: 'btn small', onClick: () => { state.logs.paused = !state.logs.paused; renderLogs(); } },
        state.logs.paused ? '▶ 继续' : '⏸ 暂停'),
      h('button', { class: 'btn small ghost', onClick: () => { $('#log-viewer').innerHTML = ''; } }, '清空视图'),
      h('a', { class: 'btn small', href: '/api/logs/tail?n=1000', target: '_blank' }, '↓ 下载最近 1000 行'),
    ]);
    v.appendChild(ctrls);
    const viewer = h('div', { class: 'log-viewer', id: 'log-viewer' });
    v.appendChild(viewer);

    const initial = await api('/api/logs/tail?n=500').catch(() => ({ lines: [] }));
    if (initial.path) {
      const hdr = h('div', { class: 'muted', style: { marginBottom: '8px' } }, 'tail: ' + initial.path);
      v.insertBefore(hdr, viewer);
    } else {
      viewer.appendChild(h('div', { class: 'empty' }, '未找到日志文件 (.claude-go/.dashboard/dashboard.log 或 $HOME/.claude-go/history.jsonl)'));
      return;
    }
    for (const line of initial.lines) appendLogLine(viewer, line);
    viewer.scrollTop = viewer.scrollHeight;

    if (state.logs.sse) try { state.logs.sse.close(); } catch (_) {}
    state.logs.sse = new EventSource('/api/logs/stream');
    state.logs.sse.addEventListener('log', (e) => {
      if (state.logs.paused) return;
      const line = e.data;
      if (state.logs.filter && !line.toLowerCase().includes(state.logs.filter.toLowerCase())) return;
      appendLogLine(viewer, line);
      viewer.scrollTop = viewer.scrollHeight;
    });
    state.logs.sse.addEventListener('info', () => {});
    state.logs.sse.onerror = () => { /* silent */ };
  }
  function appendLogLine(viewer, line) {
    const cls = /error|ERROR|panic|FATAL/.test(line) ? 'log-line err'
              : /warn|WARN/.test(line) ? 'log-line warn'
              : /info|INFO/.test(line) ? 'log-line info'
              : 'log-line';
    viewer.appendChild(h('span', { class: cls }, line));
  }

  // ==================================================================
  // 12. Swarm (群体智能)
  // ==================================================================
  async function renderSwarm() {
    const data = await api('/api/hivemind');
    const v = $('#view');
    v.innerHTML = '';

    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('启用', data.enabled ? 'ON' : 'OFF', data.dataDir, data.enabled ? 'ok' : 'warn'),
      statCard('预测记录', data.summary.predictions, '', 'accent'),
      statCard('信息素域', data.summary.pheromones, '', 'purple'),
      statCard('指标点', data.summary.metricsPoints, '', ''),
    ]));

    if ((data.notes || []).length) {
      const card = h('div', { class: 'card mb-16' }, [h('h3', {}, '说明')]);
      for (const n of data.notes) card.appendChild(h('div', { class: 'insight info' }, h('div', { class: 'insight-title' }, n)));
      v.appendChild(card);
    }

    // 预测表
    if ((data.predictions || []).length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '时间'), h('th', {}, 'objective'), h('th', {}, 'confidence'),
        h('th', {}, 'consensus'), h('th', {}, 'agents'), h('th', {}, 'outcome'),
      ])));
      const tb = h('tbody');
      for (const p of data.predictions.slice(-50).reverse()) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, fmtRel(p.timestamp)),
          h('td', {}, (p.objective || '').slice(0, 80)),
          h('td', {}, fmtNum(p.confidence, 2)),
          h('td', {}, fmtNum(p.consensus, 2)),
          h('td', {}, String(p.agents)),
          h('td', {}, p.outcome || '—'),
        ]));
      }
      tbl.appendChild(tb);
      v.appendChild(h('div', { class: 'card mb-16' }, [h('h3', {}, '最近 50 条预测'), tbl]));
    }

    // 信息素
    if ((data.pheromones || []).length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [h('th', {}, '领域'), h('th', {}, '强度'), h('th', {}, '更新')])));
      const tb = h('tbody');
      for (const p of data.pheromones.sort((a, b) => b.strength - a.strength).slice(0, 40)) {
        tb.appendChild(h('tr', {}, [h('td', {}, p.domain), h('td', {}, fmtNum(p.strength, 3)), h('td', {}, p.updated || '—')]));
      }
      tbl.appendChild(tb);
      v.appendChild(h('div', { class: 'card mb-16' }, [h('h3', {}, '信息素 · Top 40'), tbl]));
    }

    // 维护
    v.appendChild(h('div', { class: 'card' }, [
      h('h3', {}, '维护动作'),
      h('div', { class: 'actions' }, [
        h('button', { class: 'btn', onClick: () => execAction('swarm', 'clear-pheromone', 'all', { confirm: '清空所有信息素?' }) }, '🧹 清空信息素'),
        h('button', { class: 'btn ghost', onClick: () => execAction('swarm', 'rebuild-index', 'reasoning-bank') }, '重建 ReasoningBank'),
        h('button', { class: 'btn ghost', onClick: () => execAction('swarm', 'export', 'all') }, '导出记录'),
      ]),
      h('div', { class: 'muted', style: { marginTop: '8px', fontSize: '12px' } },
        '动作会写入 .claude-go/.dashboard/actions/ 队列,由 claude-go 主进程消费。'),
    ]));
  }

  // ==================================================================
  // 13. Actions (POST /api/actions/{kind}/{action}/{target})
  // ==================================================================
  async function execAction(kind, action, target, opts = {}) {
    if (opts.confirm && !confirm(opts.confirm + '\n' + kind + ' / ' + action + ' / ' + target)) return;
    try {
      const resp = await api(`/api/actions/${encodeURIComponent(kind)}/${encodeURIComponent(action)}/${encodeURIComponent(target)}`, { method: 'POST' });
      toast(resp.message || '动作已发送', 'ok');
      setTimeout(() => navigate(), 500);
    } catch (e) {
      toast('动作失败: ' + e.message, 'err');
    }
  }

  // ==================================================================
  // 14. Global Search (topbar)
  // ==================================================================
  async function performSearch(q) {
    const panel = ensureSearchPanel();
    if (!q) { panel.style.display = 'none'; return; }
    panel.style.display = 'block';
    panel.innerHTML = '<div class="muted" style="padding:10px">搜索中…</div>';
    try {
      const hits = await api('/api/search?q=' + encodeURIComponent(q) + '&limit=30');
      if (!hits.length) {
        panel.innerHTML = '<div class="muted" style="padding:10px">无结果</div>';
        return;
      }
      panel.innerHTML = '';
      for (const hit of hits) {
        const item = h('a', {
          href: hit.link, class: 'search-hit',
          style: {
            display: 'block',
            padding: '8px 12px',
            borderBottom: '1px solid var(--border)',
            color: 'var(--text)',
          },
          onClick: () => panel.style.display = 'none',
        }, [
          h('div', {}, [
            h('span', { class: 'badge ' + (hit.kind === 'team' ? 'run' : hit.kind === 'cron' ? 'purple' : '') }, hit.kind),
            h('span', { style: { marginLeft: '8px', fontWeight: '600' } }, hit.title),
            h('span', { class: 'muted', style: { marginLeft: '8px', fontSize: '11px' } }, 'score ' + fmtNum(hit.score, 2)),
          ]),
          hit.snippet ? h('div', { class: 'muted', style: { fontSize: '12px', marginTop: '3px' } }, hit.snippet) : null,
          (hit.tags || []).length ? h('div', { style: { marginTop: '4px' } },
            hit.tags.filter(Boolean).map(t => h('span', { class: 'badge', style: { marginRight: '4px' } }, t))) : null,
        ]);
        panel.appendChild(item);
      }
    } catch (e) {
      panel.innerHTML = '<div class="muted" style="padding:10px;color:var(--red)">搜索失败: ' + e.message + '</div>';
    }
  }
  function ensureSearchPanel() {
    let p = document.getElementById('search-panel');
    if (p) return p;
    p = h('div', {
      id: 'search-panel',
      style: {
        position: 'fixed',
        top: '58px', right: '24px',
        width: '420px', maxHeight: '60vh', overflowY: 'auto',
        background: 'rgba(10,14,36,0.95)',
        border: '1px solid var(--border-strong)',
        borderRadius: 'var(--radius)',
        boxShadow: 'var(--shadow)',
        zIndex: '100',
        display: 'none',
        backdropFilter: 'blur(18px)',
      },
    });
    document.body.appendChild(p);
    // click outside to close
    document.addEventListener('click', e => {
      if (p.contains(e.target)) return;
      if (e.target.id === 'global-search') return;
      p.style.display = 'none';
    });
    return p;
  }

  // ==================================================================
  // 15. SSE overview + project switcher + health
  // ==================================================================
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
      if (state.route && /^\/?$/.test(state.route.hash)) {
        try { state.route.handler.render({ quiet: true }); } catch (_) {}
      }
    });
    state.sse.onerror = () => { $('#refresh-status').textContent = 'SSE 已断开'; };
    state.sseEnabled = true;
    $('#stream-btn').classList.add('active');
    $('#refresh-status').textContent = 'SSE · 已连接';
  }

  async function refreshProjects() {
    try {
      const resp = await api('/api/projects');
      state.projects = resp;
      const sel = $('#project-switcher');
      if (!sel) return;
      sel.innerHTML = '';
      const items = resp.projects || [];
      if (items.length <= 1) { sel.style.display = 'none'; return; }
      sel.style.display = '';
      for (const p of items) {
        const opt = document.createElement('option');
        opt.value = p.path;
        opt.textContent = (p.current ? '● ' : '') + (p.name || p.path) + ` (${p.teamsCount})`;
        if (p.current) opt.selected = true;
        sel.appendChild(opt);
      }
      sel.onchange = () => toast('切换项目需重启 Dashboard:\n  claude-go dashboard stop && start --state-dir ' + sel.value, '', 4000);
    } catch (_) {}
  }

  async function updateHealth() {
    const el = $('#health-indicator');
    try {
      const d = await api('/api/health');
      el.classList.remove('err'); el.classList.add('ok');
      $('.health-text', el).textContent = d.exists ? '在线 · 数据已连接' : '在线 · 数据目录为空';
    } catch (_) {
      el.classList.remove('ok'); el.classList.add('err');
      $('.health-text', el).textContent = '离线';
    }
  }

  // ==================================================================
  // 16. Chart base opts, Markdown, Heatmap
  // ==================================================================
  function baseChartOpts({ stacked = false, dualAxis = false } = {}) {
    const grid = 'rgba(138,160,230,0.1)';
    const tick = '#8b93c2';
    const opts = {
      responsive: true, maintainAspectRatio: false,
      interaction: { mode: 'nearest', intersect: false },
      plugins: {
        legend: { labels: { color: '#c6cbe8', boxWidth: 12 } },
        tooltip: { backgroundColor: 'rgba(10,14,36,0.95)', borderColor: '#5ef0ff44', borderWidth: 1, titleColor: '#fff', bodyColor: '#d6dcf2' },
      },
      scales: {
        x: { stacked, grid: { color: grid }, ticks: { color: tick, maxTicksLimit: 10, autoSkip: true }, type: 'category' },
        y: { stacked, beginAtZero: true, grid: { color: grid }, ticks: { color: tick } },
      },
    };
    if (dualAxis) opts.scales.y2 = { beginAtZero: true, position: 'right', grid: { drawOnChartArea: false }, ticks: { color: tick } };
    return opts;
  }

  function renderMarkdown(src) {
    if (!src) return '';
    const escapeHTML = s => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
    const lines = src.split(/\r?\n/);
    const out = [];
    let inCode = false, codeBuf = [], codeLang = '', listBuf = null, listType = null;
    const flushList = () => { if (listBuf && listBuf.length) out.push('<' + listType + '>' + listBuf.join('') + '</' + listType + '>'); listBuf = null; listType = null; };
    for (const raw of lines) {
      if (inCode) {
        if (raw.startsWith('```')) { out.push('<pre><code data-lang="' + escapeHTML(codeLang) + '">' + escapeHTML(codeBuf.join('\n')) + '</code></pre>'); inCode = false; codeBuf = []; codeLang = ''; }
        else codeBuf.push(raw);
        continue;
      }
      if (raw.startsWith('```')) { flushList(); inCode = true; codeLang = raw.slice(3).trim(); continue; }
      let m;
      if ((m = raw.match(/^(#{1,6})\s+(.*)$/))) { flushList(); out.push('<h' + m[1].length + '>' + inline(escapeHTML(m[2])) + '</h' + m[1].length + '>'); continue; }
      if ((m = raw.match(/^\s*[-*]\s+(.*)$/))) { if (listType !== 'ul') { flushList(); listBuf = []; listType = 'ul'; } listBuf.push('<li>' + inline(escapeHTML(m[1])) + '</li>'); continue; }
      if ((m = raw.match(/^\s*\d+\.\s+(.*)$/))) { if (listType !== 'ol') { flushList(); listBuf = []; listType = 'ol'; } listBuf.push('<li>' + inline(escapeHTML(m[1])) + '</li>'); continue; }
      if ((m = raw.match(/^>\s?(.*)$/))) { flushList(); out.push('<blockquote>' + inline(escapeHTML(m[1])) + '</blockquote>'); continue; }
      if (raw.trim() === '') { flushList(); continue; }
      flushList();
      out.push('<p>' + inline(escapeHTML(raw)) + '</p>');
    }
    flushList();
    if (inCode) out.push('<pre><code>' + escapeHTML(codeBuf.join('\n')) + '</code></pre>');
    return out.join('\n');
  }
  function inline(s) {
    s = s.replace(/`([^`]+)`/g, '<code>$1</code>');
    s = s.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    s = s.replace(/\*([^*]+)\*/g, '<em>$1</em>');
    s = s.replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
    return s;
  }

  function renderHeatmapEl(cells) {
    const cats = Array.from(new Set(cells.map(c => c.category)));
    const roles = Array.from(new Set(cells.map(c => c.role)));
    const idx = {};
    for (const c of cells) idx[c.category + '|' + c.role] = c.count;
    const maxV = Math.max(1, ...cells.map(c => c.count));
    const grid = h('div', { class: 'heatmap', style: { gridTemplateColumns: `minmax(120px,auto) repeat(${roles.length},minmax(60px,1fr))` } });
    grid.appendChild(h('div', { class: 'corner' }, 'category \\ role'));
    for (const r of roles) grid.appendChild(h('div', { class: 'corner' }, r));
    for (const cat of cats) {
      grid.appendChild(h('div', { class: 'corner' }, cat));
      for (const r of roles) {
        const n = idx[cat + '|' + r] || 0;
        const ratio = n / maxV;
        const bucket = n === 0 ? 0 : Math.min(5, Math.ceil(ratio * 5));
        grid.appendChild(h('div', {
          class: 'cell h' + bucket,
          title: `${cat} × ${r} = ${n}`,
        }, n === 0 ? '' : String(n)));
      }
    }
    return grid;
  }

  // ==================================================================
  // 17. init
  // ==================================================================
  window.addEventListener('DOMContentLoaded', () => {
    $$('.nav-item').forEach(el => {
      el.addEventListener('click', (e) => {
        e.preventDefault();
        location.hash = '#' + el.getAttribute('data-route');
      });
    });
    $('#refresh-btn').addEventListener('click', () => navigate());
    const sBtn = $('#stream-btn');
    if (sBtn) sBtn.addEventListener('click', toggleSSE);

    // global search
    const gs = $('#global-search');
    if (gs) {
      gs.addEventListener('input', () => {
        clearTimeout(state.searchTimer);
        state.searchTimer = setTimeout(() => performSearch(gs.value.trim()), 200);
      });
      gs.addEventListener('focus', () => { if (gs.value) performSearch(gs.value.trim()); });
      gs.addEventListener('keydown', e => { if (e.key === 'Escape') { gs.value = ''; ensureSearchPanel().style.display = 'none'; } });
    }

    window.addEventListener('hashchange', navigate);
    navigate();
    updateHealth();
    refreshProjects();
    setInterval(updateHealth, 15000);
  });
})();
