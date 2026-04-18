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
    logs: { sse: null, paused: false, filter: '', source: '' },
    teamFilter: '',
    cronFilter: '',
    cronEdit: null,
    tasksFilter: '',
    searchTimer: null,
    llmStats: { window: '24h', timer: null },
    blackboardEdit: { key: '', value: '' },
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
    { path: /^\/metrics\/?$/,     render: renderMetrics,       title: '观测指标 (含指标中心)', poll: 0 },
    { path: /^\/metrics\/(.+)$/,  render: renderMetricsDetail, title: '指标详情',      poll: 0 },
    // 兼容旧链接: /catalog 已合并进 /metrics
    { path: /^\/catalog\/?$/,     render: () => { location.hash = '#/metrics'; }, title: '指标中心 (已合并)', poll: 0 },
    { path: /^\/cron\/?$/,        render: renderCron,          title: 'Cron 任务',     poll: 30000 },
    { path: /^\/dreaming\/?$/,    render: renderDreaming,      title: 'Dreaming 机制', poll: 30000 },
    { path: /^\/evolution\/?$/,   render: renderEvolution,     title: 'Evolution 机制',poll: 30000 },
    { path: /^\/swarm\/?$/,       render: renderSwarm,         title: '群体智能',      poll: 20000 },
    { path: /^\/tasks\/?$/,       render: renderTasks,         title: '任务',          poll: 15000 },
    { path: /^\/workflows\/?$/,   render: renderWorkflows,     title: '工作流 & 团队模板', poll: 0 },
    { path: /^\/workflows\/(.+)$/,render: renderWorkflows,     title: '工作流详情',    poll: 0 },
    { path: /^\/backups\/?$/,     render: renderBackups,       title: '备份 & 恢复',   poll: 0 },
    { path: /^\/llm\/?$/,         render: renderLLMStats,      title: 'LLM 大模型监控', poll: 60000 },
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
      h('button', { class: 'btn primary', onClick: () => runTeamDiagnose(name) }, '✦ LLM 诊断'),
      h('button', { class: 'btn ghost', onClick: () => execAction('team', 'delete', name, { confirm: '删除该团队的所有记录?' }) }, '🗑 删除'),
      h('button', { class: 'btn ghost small', onClick: () => navigator.clipboard && navigator.clipboard.writeText(location.href) }, '复制链接'),
    ]));

    // 诊断结果挂点
    const diagSlot = h('div', { id: 'team-diag-slot' });
    right.appendChild(diagSlot);

    right.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('阶段', `${detail.stagesDone}/${detail.stagesTotal}`,
        `失败 ${detail.stagesFail}`, detail.stagesFail > 0 ? 'warn' : 'accent'),
      statCard('Agents', detail.agentsTotal || 0, '参与角色数', 'purple'),
      statCard('总耗时', fmtDur(detail.durationSec), fmtRel(detail.createdAt), 'accent'),
      statCard('对抗轮次', (detail.adversaryRounds || []).length, '多轮 Eval', 'accent'),
    ]));

    const tabs = ['dag', 'timeline', 'stages', 'agents', 'eval', 'metrics', 'blackboard', 'checkpoints', 'logs', 'report'];
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
      checkpoints: '检查点',
      logs: '日志',
      report: 'Report',
    })[t] || t;
  }

  async function runTeamDiagnose(name) {
    const slot = document.getElementById('team-diag-slot');
    if (!slot) return;
    await runAsyncDiagnosis({
      slot,
      kind: 'team',
      target: name,
      title: '✦ 团队 LLM 诊断',
      description: '分析运行过程 / 输出质量 / 运行状态, 提出优化建议 (可能耗时 30~90s)',
    });
  }

  // runAsyncDiagnosis: 通用异步诊断执行器。
  //   kind: team / dreaming / evolution
  //   target: team name (kind=team 时必填)
  //   渲染阶段: pending -> running(loading bar) -> done(markdown) / failed(error card)
  // 轮询策略: 指数退避, 2s -> 3s -> 5s -> 8s, 最多 180s
  async function runAsyncDiagnosis({ slot, kind, target, title, description }) {
    slot.innerHTML = '';
    const container = h('div', { class: 'card mb-16' });
    const header = h('div', { class: 'toolbar-row' }, [
      h('h3', { style: { margin: 0 } }, title),
      h('span', { class: 'badge accent' }, kind),
    ]);
    const status = h('div', { class: 'muted', style: { margin: '8px 0' } }, description || '诊断中…');
    const progress = h('div', {
      style: { height: '4px', background: 'rgba(140,160,220,0.15)',
               borderRadius: '2px', overflow: 'hidden', margin: '8px 0' } }, [
      h('div', {
        class: 'pulse-bar',
        style: { height: '100%', width: '30%',
                 background: 'linear-gradient(90deg, transparent, var(--accent, #8ab4f8), transparent)',
                 animation: 'pulseX 1.4s linear infinite' } }),
    ]);
    const result = h('div');
    container.appendChild(header);
    container.appendChild(status);
    container.appendChild(progress);
    container.appendChild(result);
    slot.appendChild(container);

    // 创建作业
    let jobID;
    try {
      const body = { kind };
      if (target) body.target = target;
      const createResp = await api('/api/diag/jobs', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      jobID = createResp.id;
      status.textContent = `已创建作业 ${jobID} · 等待 LLM 返回…`;
    } catch (e) {
      progress.style.display = 'none';
      status.innerHTML = '';
      status.appendChild(h('div', { class: 'insight err' },
        h('div', { class: 'insight-title' }, '创建诊断作业失败: ' + e.message)));
      return;
    }

    // 轮询: 指数退避
    const waits = [2000, 2000, 3000, 3000, 5000, 5000, 8000, 8000, 10000, 10000, 15000, 15000];
    const deadline = Date.now() + 180_000;
    let idx = 0;
    while (Date.now() < deadline) {
      const w = waits[Math.min(idx, waits.length - 1)];
      await new Promise(r => setTimeout(r, w));
      idx++;
      let job;
      try {
        job = await api('/api/diag/jobs/' + encodeURIComponent(jobID));
      } catch (e) {
        status.textContent = '轮询失败 (将重试): ' + e.message;
        continue;
      }
      if (!job) continue;
      if (job.status === 'running' || job.status === 'pending') {
        const elapsed = Math.floor((Date.now() - new Date(job.createdAt).getTime()) / 1000);
        status.textContent = `状态: ${job.status} · 已耗时 ${elapsed}s · 轮询周期 ${w/1000}s`;
        continue;
      }
      // done / failed
      progress.style.display = 'none';
      if (job.status === 'failed') {
        status.innerHTML = '';
        status.appendChild(h('div', { class: 'insight err' },
          h('div', { class: 'insight-title' }, '诊断失败: ' + (job.error || 'unknown'))));
      } else {
        const durSec = (job.durationMs || 0) / 1000;
        status.innerHTML = '';
        status.appendChild(h('div', { style: { display: 'flex', gap: '8px',
                                                flexWrap: 'wrap', alignItems: 'center' } }, [
          h('span', { class: 'badge ok' }, '✓ 完成'),
          h('span', { class: 'badge' }, 'LLM: ' + ((job.llm && job.llm.model) || '—')),
          h('span', { class: 'badge' }, 'provider: ' + ((job.llm && job.llm.provider) || '—')),
          h('span', { class: 'muted small' }, '耗时 ' + durSec.toFixed(1) + 's'),
        ]));
        if (job.scope && job.scope.length) {
          status.appendChild(h('div', { class: 'muted small', style: { marginTop: '4px' } },
            '分析范围: ' + job.scope.join(' · ')));
        }
        const md = h('div', {
          class: 'md',
          style: { maxHeight: '60vh', overflow: 'auto', padding: '12px',
                   background: 'rgba(255,255,255,0.02)', borderRadius: '8px',
                   border: '1px solid rgba(140,160,220,0.15)', marginTop: '8px' },
        });
        md.innerHTML = renderMarkdown(job.summary || '(empty)');
        result.innerHTML = '';
        result.appendChild(md);
      }
      return;
    }
    progress.style.display = 'none';
    status.innerHTML = '';
    status.appendChild(h('div', { class: 'insight warn' },
      h('div', { class: 'insight-title' }, '轮询超时 (180s), 作业可能仍在后台运行')));
  }

  async function renderTeamBlackboard(detail, panel) {
    panel.innerHTML = '';
    let bb = {};
    try { bb = await api('/api/teams/' + encodeURIComponent(detail.name) + '/blackboard'); } catch (_) {}

    // 黑板的真实形态是 {map: {key: value}, version, ...}; 统一拉平成 (key, value) 列表。
    // 兼容旧 shape: 直接 {key: value} map (由 provider 老版本返回)。
    const rawMap = (bb && bb.map && typeof bb.map === 'object') ? bb.map : (bb || {});
    const entries = Object.entries(rawMap).filter(([k]) => k !== 'version' && k !== 'updatedAt');
    entries.sort((a, b) => a[0].localeCompare(b[0]));

    const searchInp = h('input', { class: 'input', placeholder: '按 key / value 关键字过滤…',
      style: { minWidth: '220px' } });
    const listContainer = h('div', {
      style: { maxHeight: '60vh', overflow: 'auto', padding: '8px',
               background: 'rgba(255,255,255,0.02)', borderRadius: '8px',
               border: '1px solid rgba(140,160,220,0.15)' },
    });
    const renderItems = () => {
      listContainer.innerHTML = '';
      const q = (searchInp.value || '').trim().toLowerCase();
      const filtered = entries.filter(([k, v]) => {
        if (!q) return true;
        const vs = (typeof v === 'string' ? v : JSON.stringify(v)).toLowerCase();
        return k.toLowerCase().includes(q) || vs.includes(q);
      });
      if (!filtered.length) {
        listContainer.appendChild(h('div', { class: 'empty' },
          entries.length === 0 ? '黑板为空' : '无匹配项'));
        return;
      }
      for (const [k, v] of filtered) {
        const valStr = (typeof v === 'string') ? v : JSON.stringify(v, null, 2);
        const rawBytes = valStr.length;
        const preview = valStr.length > 320 ? valStr.slice(0, 320) + '…' : valStr;
        const row = h('div', { class: 'bb-row',
          style: { padding: '10px 12px', borderBottom: '1px solid rgba(140,160,220,0.08)' } }, [
          h('div', { style: { display: 'flex', justifyContent: 'space-between',
                              alignItems: 'center', marginBottom: '4px' } }, [
            h('strong', { style: { color: 'var(--accent, #8ab4f8)' } }, k),
            h('span', { class: 'muted small' }, fmtKilo(rawBytes) + ' 字符'),
          ]),
          h('pre', { class: 'pre',
            style: { margin: 0, whiteSpace: 'pre-wrap', fontSize: '12px', maxHeight: '200px', overflow: 'auto' } },
            preview),
        ]);
        listContainer.appendChild(row);
      }
    };
    searchInp.addEventListener('input', renderItems);

    const existing = h('div', { class: 'card mb-16' }, [
      h('div', { class: 'toolbar-row' }, [
        h('h3', { style: { margin: 0 } }, '当前黑板'),
        h('span', { class: 'muted small' }, `· ${entries.length} 条目`),
        h('div', { style: { flex: 1 } }),
        searchInp,
      ]),
      listContainer,
    ]);
    panel.appendChild(existing);
    renderItems();

    // 写入表单
    const keyInp = h('input', { class: 'input', placeholder: 'Key (如 notes / budget / hint)', value: state.blackboardEdit.key });
    const valInp = h('textarea', { class: 'input', rows: 4, style: { width: '100%' },
      placeholder: 'Value (支持纯文本 / JSON / markdown)', value: state.blackboardEdit.value });
    const out = h('div', { class: 'muted small', style: { marginTop: '6px' } });
    const submit = async () => {
      const key = (keyInp.value || '').trim();
      const value = valInp.value;
      if (!key) { toast('key 必填', 'err'); return; }
      try {
        let parsed;
        try { parsed = JSON.parse(value); } catch (_) { parsed = value; }
        const resp = await api('/api/teams/' + encodeURIComponent(detail.name) + '/blackboard', {
          method: 'POST', headers: { 'content-type': 'application/json' },
          body: JSON.stringify({ key, value: parsed }),
        });
        toast('已写入 ' + key, 'ok');
        out.textContent = 'ok · ' + (resp.size || '') + ' keys';
        state.blackboardEdit = { key: '', value: '' };
        setTimeout(() => renderTeamBlackboard(detail, panel), 300);
      } catch (e) { toast('写入失败: ' + e.message, 'err'); }
    };
    const formCard = h('div', { class: 'card' }, [
      sectionHeader('写入 / 更新黑板',
        h('span', { class: 'muted small' }, '对应 blackboard.json, 同时排队 blackboard.write action 给主进程')),
      h('div', { class: 'form-grid' }, [
        h('div', {}, [h('label', { class: 'muted' }, 'Key'), keyInp]),
        h('div', { style: { gridColumn: '1 / -1' } }, [h('label', { class: 'muted' }, 'Value'), valInp]),
        h('div', { style: { gridColumn: '1 / -1' } }, [
          h('button', { class: 'btn primary', onClick: submit }, '✓ 写入黑板'),
          out,
        ]),
      ]),
    ]);
    panel.appendChild(formCard);
  }

  // ---- Team Checkpoints ----
  async function renderTeamCheckpoints(detail, panel) {
    panel.innerHTML = '';
    let resp = null;
    try {
      resp = await api('/api/teams/' + encodeURIComponent(detail.name) + '/checkpoints');
    } catch (e) {
      panel.appendChild(h('div', { class: 'empty', style: { color: 'var(--red)' } }, '加载失败: ' + e.message));
      return;
    }
    const list = (resp && resp.checkpoints) || [];
    panel.appendChild(h('div', { class: 'muted small mb-12' },
      '共 ' + list.length + ' 个检查点 · 文件: ' + (resp.file || 'checkpoints.json') + (resp.exists === false ? ' (尚未创建)' : '')));
    if (!list.length) {
      panel.appendChild(h('div', { class: 'empty' }, '暂无 checkpoint 数据 (团队尚未执行或已清理)'));
      return;
    }
    const tbl = h('table', { class: 'tbl' });
    tbl.appendChild(h('thead', {}, h('tr', {}, [
      h('th', {}, '#'), h('th', {}, '阶段'), h('th', {}, '状态'),
      h('th', {}, '尝试次数'), h('th', {}, '保存时间'), h('th', {}, '错误'),
    ])));
    const tb = h('tbody');
    list.forEach((c, idx) => {
      tb.appendChild(h('tr', {}, [
        h('td', { class: 'muted' }, String(idx + 1)),
        h('td', {}, c.stageName || '—'),
        h('td', {}, h('span', { class: 'badge ' + statusBadgeClass(c.status) }, c.status || '—')),
        h('td', {}, String(c.attempt ?? 0)),
        h('td', {}, c.savedAt ? fmtTime(c.savedAt) : '—'),
        h('td', { style: { maxWidth: '280px', overflow: 'hidden', textOverflow: 'ellipsis' } },
          c.error ? h('span', { class: 'badge err', title: c.error }, (c.error || '').slice(0, 80)) : '—'),
      ]));
    });
    tbl.appendChild(tb);
    panel.appendChild(h('div', { class: 'card mb-16' }, [
      h('h3', {}, '检查点历史 (checkpoints.json)'),
      h('div', { style: { maxHeight: '60vh', overflow: 'auto',
                         border: '1px solid rgba(140,160,220,0.15)',
                         borderRadius: '8px' } }, tbl),
    ]));
  }

  // ---- Team Logs (filter global claude-go.log by team) ----
  async function renderTeamLogs(detail, panel) {
    panel.innerHTML = '';
    const tailInp = h('input', { class: 'input', type: 'number', min: 50, max: 5000, value: 500, style: { width: '100px' } });
    const qInp = h('input', { class: 'input', placeholder: '前端二次过滤 (本地关键词)…', style: { minWidth: '240px' } });
    const preBox = h('pre', {
      class: 'pre',
      style: { maxHeight: '60vh', overflow: 'auto', fontSize: '12px', lineHeight: '1.5' },
    }, '加载中…');
    const pill = h('span', { class: 'muted small' }, '');
    const reload = async () => {
      preBox.textContent = '加载中…';
      try {
        const data = await api('/api/teams/' + encodeURIComponent(detail.name) + '/logs?limit=' + encodeURIComponent(String(tailInp.value || 500)));
        let lines = data.lines || [];
        const q = (qInp.value || '').trim().toLowerCase();
        if (q) lines = lines.filter(l => l.toLowerCase().includes(q));
        pill.textContent = `共 ${lines.length} 行 · 源: ${data.source || 'claude-go.log'}`;
        if (!lines.length) {
          preBox.textContent = '(未匹配到该团队的日志行, 尝试放宽关键词)';
          return;
        }
        preBox.textContent = lines.join('\n');
        preBox.scrollTop = preBox.scrollHeight;
      } catch (e) {
        preBox.textContent = '加载失败: ' + e.message;
      }
    };
    qInp.addEventListener('input', reload);
    panel.appendChild(h('div', { class: 'toolbar-row mb-12' }, [
      h('strong', {}, '◉ 团队相关日志'),
      h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center' } }, [
        h('span', { class: 'muted small' }, '行数'),
        tailInp,
        qInp,
        h('button', { class: 'btn', onClick: reload }, '刷新'),
        pill,
      ]),
    ]));
    panel.appendChild(h('div', { class: 'card' }, [preBox]));
    await reload();
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
      await renderTeamBlackboard(detail, panel);
    } else if (tab === 'checkpoints') {
      await renderTeamCheckpoints(detail, panel);
    } else if (tab === 'logs') {
      await renderTeamLogs(detail, panel);
    } else if (tab === 'report') {
      if (!detail.report) panel.appendChild(h('div', { class: 'empty' }, '没有 REPORT.md'));
      else {
        // 研发团队报告通常很长 (上千行); 加滚动条 + 顶部统计摘要。
        const size = detail.report.length;
        const lines = detail.report.split('\n').length;
        panel.appendChild(h('div', { class: 'toolbar-row mb-12' }, [
          h('strong', {}, 'REPORT.md'),
          h('span', { class: 'muted small' }, `· ${lines} 行 / ${fmtKilo(size)} 字符`),
          h('button', { class: 'btn ghost small', onClick: () => {
            navigator.clipboard && navigator.clipboard.writeText(detail.report);
            toast('已复制到剪贴板', 'ok');
          }}, '📋 复制全文'),
        ]));
        const mdBox = h('div', {
          class: 'md',
          style: { maxHeight: '70vh', overflow: 'auto', padding: '16px',
                   background: 'rgba(255,255,255,0.02)', borderRadius: '10px',
                   border: '1px solid rgba(140,160,220,0.15)' }
        });
        mdBox.innerHTML = renderMarkdown(detail.report);
        panel.appendChild(mdBox);
      }
    } else if (tab === 'eval') {
      renderTeamEval(detail, panel);
    } else if (tab === 'metrics') {
      renderTeamMetrics(detail, panel);
    }
  }

  // ---- DAG ----
  async function renderTeamDAG(detail, panel) {
    // Prefer the richer /api/teams/:name/dag endpoint which merges
    // workflow + runtime stages + checkpoints + attempts + durations.
    let dag = null;
    try {
      dag = await api('/api/teams/' + encodeURIComponent(detail.name) + '/dag');
    } catch (_) { /* fallback below */ }

    if (dag && (dag.stages || []).length) {
      const nodes = dag.stages.map(s => ({
        name: s.name, role: s.role,
        dependsOn: s.dependsOn || [],
        parallel: s.parallel,
        status: s.status || 'pending',
        attempts: s.attempts || 0,
        durationSec: s.durationSec,
        error: s.error,
        // v1.5 rich fields
        taskBrief: s.taskBrief || '',
        assignedAgents: s.assignedAgents || [],
        checkpointStatus: s.checkpointStatus || '',
        checkpointSavedAt: s.checkpointSavedAt,
        checkpointError: s.checkpointError || '',
        adversaryRound: s.adversaryRound || 0,
        adversaryScore: s.adversaryScore || 0,
        adversaryPassed: s.adversaryPassed,
        outputPreview: s.outputPreview || '',
      }));
      const meta = h('div', { class: 'muted', style: { marginBottom: '10px', fontSize: '12px' } }, [
        h('strong', {}, 'workflow: '), dag.workflow || detail.workflow || '—',
        ' · stages=', String(nodes.length),
        '  ·  ',
        h('span', { class: 'badge ' + statusBadgeClass(dag.status) }, dag.status || '—'),
        h('span', { class: 'muted', style: { marginLeft: '6px' } }, 'source: ' + (dag.source || '')),
      ]);
      panel.appendChild(meta);
      panel.appendChild(renderDAGSVG(nodes, dag.source));
      panel.appendChild(h('div', { class: 'legend' }, [
        h('span', {}, [h('span', { class: 'dot ok' }), '完成']),
        h('span', {}, [h('span', { class: 'dot run' }), '运行']),
        h('span', {}, [h('span', { class: 'dot err' }), '失败']),
        h('span', {}, [h('span', { class: 'dot pending' }), '待执行']),
      ]));

      // 富信息卡片视图: 每个 stage 一个卡片, 清晰展示任务内容 / Agent / 检查点 / 对抗评分 / 错误
      // 卡片网格布局, 响应式自适应, 适合研发团队 workflow 的多维度信息浏览。
      if (nodes.length) {
        const cardsWrap = h('div', { class: 'dag-cards',
          style: { display: 'grid', gap: '12px',
                   gridTemplateColumns: 'repeat(auto-fill, minmax(320px, 1fr))' } });
        for (const n of nodes) {
          cardsWrap.appendChild(renderDAGNodeCard(n));
        }
        panel.appendChild(h('div', { class: 'card mb-16' }, [
          h('h3', {}, '📋 任务节点详情 (含任务 / Agent / 检查点 / 对抗 / 错误)'),
          cardsWrap,
        ]));
      }

      if (dag.adversaryRounds && dag.adversaryRounds.length) {
        panel.appendChild(renderDAGAdversaryRounds(dag.adversaryRounds));
      }
      return;
    }

    // Fallback path (older data / no workflow registry)
    const wfName = detail.workflow || 'development';
    let wf = null;
    try { wf = await api('/api/workflows/' + encodeURIComponent(wfName)); } catch (_) {}
    const stageStatus = {};
    (detail.stages || []).forEach(s => {
      if (!stageStatus[s.name]) stageStatus[s.name] = s.status;
      else if (s.status === 'failed') stageStatus[s.name] = 'failed';
      else if (stageStatus[s.name] === 'pending') stageStatus[s.name] = s.status;
    });
    if (!wf) {
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
    panel.appendChild(renderDAGSVG(nodes, wf.description));
  }

  // renderDAGNodeCard: 富信息 DAG 节点卡片
  //   - 顶部: 状态徽章 + 阶段名 + 角色
  //   - 任务简述 (workflow 定义的 prompt 首行)
  //   - Agents / 依赖 / 尝试次数 / 耗时
  //   - 检查点: 状态 + 保存时间 + error
  //   - 对抗: round + 分数 + 是否通过
  //   - 可折叠的输出预览
  function renderDAGNodeCard(n) {
    const card = h('div', { class: 'dag-card',
      style: { background: 'rgba(255,255,255,0.03)',
               border: '1px solid rgba(140,160,220,0.15)',
               borderRadius: '10px', padding: '12px', fontSize: '12px' } });

    const header = h('div', { style: { display: 'flex', justifyContent: 'space-between',
                                       alignItems: 'center', marginBottom: '8px' } }, [
      h('strong', { style: { fontSize: '13px' } }, n.name),
      h('span', { class: 'badge ' + statusBadgeClass(n.status) }, n.status || '—'),
    ]);
    card.appendChild(header);

    if (n.role) {
      card.appendChild(h('div', { class: 'muted small', style: { marginBottom: '4px' } },
        '角色: ' + n.role + (n.parallel ? ' · 并行' : '')));
    }
    if (n.taskBrief) {
      card.appendChild(h('div', { style: { margin: '6px 0', padding: '6px 8px',
        background: 'rgba(140,160,220,0.06)', borderRadius: '6px',
        borderLeft: '3px solid var(--accent, #8ab4f8)', fontSize: '11.5px' } },
        n.taskBrief));
    }

    const metaRow = h('div', { style: { display: 'flex', flexWrap: 'wrap', gap: '6px',
                                        marginTop: '6px' } });
    if (n.assignedAgents && n.assignedAgents.length) {
      metaRow.appendChild(h('span', { class: 'badge accent',
        title: 'Agents: ' + n.assignedAgents.join(', ') }, '👥 ' + n.assignedAgents.join(', ')));
    }
    if (n.attempts > 0) {
      metaRow.appendChild(h('span', { class: 'badge' }, '⟲ 尝试 ' + n.attempts));
    }
    if (n.durationSec) {
      metaRow.appendChild(h('span', { class: 'badge' }, '⏱ ' + fmtDurSec(n.durationSec)));
    }
    if (n.dependsOn && n.dependsOn.length) {
      metaRow.appendChild(h('span', { class: 'badge ghost',
        title: '依赖: ' + n.dependsOn.join(', ') }, '⇠ ' + n.dependsOn.join(', ')));
    }
    card.appendChild(metaRow);

    // 检查点
    if (n.checkpointStatus || n.checkpointSavedAt || n.checkpointError) {
      const cpBox = h('div', { style: { marginTop: '8px', padding: '6px 8px',
        background: 'rgba(100,200,160,0.05)', borderRadius: '6px', fontSize: '11px' } }, [
        h('span', { class: 'muted' }, '✓ 检查点: '),
        h('span', { class: 'badge ' + statusBadgeClass(n.checkpointStatus) }, n.checkpointStatus || '—'),
        n.checkpointSavedAt && n.checkpointSavedAt !== '0001-01-01T00:00:00Z'
          ? h('span', { class: 'muted', style: { marginLeft: '6px' } }, fmtTime(n.checkpointSavedAt))
          : '',
      ]);
      if (n.checkpointError) {
        cpBox.appendChild(h('div', { class: 'err', style: { marginTop: '4px', fontSize: '11px' },
          title: n.checkpointError }, '✗ ' + n.checkpointError.slice(0, 120)));
      }
      card.appendChild(cpBox);
    }

    // 对抗评审
    if (n.adversaryRound > 0) {
      const advCls = n.adversaryPassed ? 'ok' : 'err';
      card.appendChild(h('div', { style: { marginTop: '8px', padding: '6px 8px',
        background: 'rgba(200,160,100,0.05)', borderRadius: '6px', fontSize: '11px' } }, [
        h('span', { class: 'muted' }, '⚔ 对抗 Round ' + n.adversaryRound + ': '),
        h('span', { class: 'badge ' + advCls },
          (n.adversaryScore || 0).toFixed(2) + (n.adversaryPassed ? ' · PASS' : ' · FAIL')),
      ]));
    }

    if (n.error) {
      card.appendChild(h('div', { class: 'insight err',
        style: { marginTop: '8px', fontSize: '11px' } },
        h('div', { class: 'insight-title', title: n.error }, '错误: ' + n.error.slice(0, 160))));
    }

    if (n.outputPreview) {
      card.appendChild(h('details', { style: { marginTop: '8px' } }, [
        h('summary', { class: 'muted small' }, '输出预览'),
        h('pre', { class: 'pre', style: { fontSize: '11px', maxHeight: '160px',
                                          overflow: 'auto', whiteSpace: 'pre-wrap' } },
          n.outputPreview),
      ]));
    }
    return card;
  }

  function renderDAGAdversaryRounds(rounds) {
    const tbl = h('table', { class: 'tbl' });
    tbl.appendChild(h('thead', {}, h('tr', {}, [
      h('th', {}, 'Round'), h('th', {}, '平均分'),
      h('th', {}, 'Correct'), h('th', {}, 'Complete'),
      h('th', {}, 'Security'), h('th', {}, 'Quality'),
      h('th', {}, 'Design'), h('th', {}, 'Pass?'),
    ])));
    const tb = h('tbody');
    for (const r of rounds) {
      tb.appendChild(h('tr', {}, [
        h('td', {}, String(r.round)),
        h('td', {}, h('strong', {}, (r.avgScore || 0).toFixed(2))),
        h('td', {}, (r.correctness || 0).toFixed(2)),
        h('td', {}, (r.completeness || 0).toFixed(2)),
        h('td', {}, (r.security || 0).toFixed(2)),
        h('td', {}, (r.codeQuality || 0).toFixed(2)),
        h('td', {}, (r.designAlignment || 0).toFixed(2)),
        h('td', {}, h('span', { class: 'badge ' + (r.passed ? 'ok' : 'err') },
          r.passed ? 'PASS' : 'FAIL')),
      ]));
    }
    tbl.appendChild(tb);
    return h('div', { class: 'card mb-16' }, [
      h('h3', {}, '⚔ 对抗评审历史'),
      h('div', { style: { maxHeight: '40vh', overflow: 'auto' } }, tbl),
    ]);
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
  // v1.5 统一"指标"页面: 合并 /metrics + /catalog
  //   - 顶部: 概览 (总数 / 已采集 / 覆盖率 / 模块数)
  //   - 中部: 模块索引 (点击进入 detail), 显示事件数 + 告警
  //   - 底部: catalog 表 (中英双语, 最近值 / 最近采集时间 / 小型图表)
  async function renderMetrics() {
    const v = $('#view');
    v.innerHTML = '<div class="loading">加载 metrics…</div>';
    let sums, cat, cov;
    try {
      [sums, cat, cov] = await Promise.all([
        api('/api/metrics').then(r => Array.isArray(r) ? r : []),
        api('/api/metrics/catalog').catch(() => ({ catalog: [], modules: [], total: 0 })),
        api('/api/metrics/coverage').catch(() => ({ rows: [], totalDeclared: 0, totalCollected: 0, coverageRate: 0 })),
      ]);
    } catch (e) {
      v.innerHTML = '';
      v.appendChild(h('div', { class: 'empty', style: { color: 'var(--red)' } }, '加载失败: ' + e.message));
      return;
    }
    v.innerHTML = '';

    // ---- 顶部概览 ----
    const total = cat.total || (cat.catalog || []).length;
    const collected = cov.totalCollected || 0;
    const coveragePct = cov.coverageRate || (total > 0 ? collected / total : 0);
    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('指标总数', total, '已在 pkg/metrics/catalog.go 声明', 'accent'),
      statCard('已采集', collected, '至少 1 次样本', collected > 0 ? 'ok' : 'warn'),
      statCard('覆盖率', fmtPct(coveragePct),
        '有样本 / 总声明', coveragePct >= 0.8 ? 'ok' : (coveragePct >= 0.4 ? 'warn' : 'err')),
      statCard('模块数', (cat.modules || []).length,
        '按模块分组', 'purple'),
    ]));

    // ---- 模块快速跳转 ----
    const known = ['team', 'dreaming', 'evolution', 'memory', 'task', 'cron', 'swarm', 'llm', 'api'];
    const byMod = {};
    for (const s of sums) byMod[s.module] = s;
    const mods = new Set(known);
    for (const s of sums) mods.add(s.module);
    for (const m of cat.modules || []) mods.add(m);

    v.appendChild(h('div', { class: 'card mb-16' }, [
      h('h3', {}, '📊 模块导航 · 点击查看事件曲线'),
      h('div', { class: 'pill-row' }, [...mods].map(m => {
        const s = byMod[m];
        const evn = s ? (s.eventCount || 0) : 0;
        return h('a', {
          class: 'badge' + (evn > 0 ? ' accent' : ''),
          href: '#/metrics/' + encodeURIComponent(m),
          style: { cursor: 'pointer', textDecoration: 'none' },
        }, m + ' · ' + (evn > 0 ? evn + ' events' : '—'));
      })),
      (sums.some(s => (s.trendAlerts || []).length)) ? h('div', { class: 'mt-12' },
        sums.filter(s => (s.trendAlerts || []).length).map(s =>
          h('div', { class: 'insight warn' }, [
            h('div', { class: 'insight-title' }, s.module),
            h('div', {}, (s.trendAlerts || []).slice(0, 3).join('; ')),
          ]))
      ) : null,
    ]));

    // ---- Catalog 表 (中英双语, 自动选图标) ----
    const metricsList = cat.catalog || [];
    const covRows = cov.rows || [];
    const covMap = {};
    covRows.forEach(c => { covMap[c.module + ':' + c.name] = c; });

    if (!metricsList.length) {
      v.appendChild(h('div', { class: 'empty' }, 'catalog 为空, 请在 pkg/metrics/catalog.go 补充指标描述'));
      return;
    }

    if ((cov.missingDesc || []).length) {
      v.appendChild(h('div', { class: 'card mb-16' }, [
        h('h3', {}, '⚠ 有数据但缺少中英文声明的指标'),
        h('div', { class: 'muted small' }, '请在 pkg/metrics/catalog.go 补齐, 否则不会显示图表与描述'),
        h('pre', { class: 'pre', style: { maxHeight: '120px', overflow: 'auto' } }, (cov.missingDesc || []).join('\n')),
      ]));
    }

    // 搜索 / 过滤
    const qInp = h('input', { class: 'input', placeholder: '搜索: 名称 / 中文描述 / 模块…', style: { minWidth: '260px' } });
    const modSel = h('select', { class: 'select' }, [
      h('option', { value: '' }, '全部模块'),
      ...(cat.modules || []).map(m => h('option', { value: m }, m)),
    ]);
    const kindSel = h('select', { class: 'select' }, [
      h('option', { value: '' }, '全部类型'),
      h('option', { value: 'counter' }, 'counter · 计数器'),
      h('option', { value: 'gauge' }, 'gauge · 瞬时量'),
      h('option', { value: 'histogram' }, 'histogram · 分布'),
      h('option', { value: 'duration' }, 'duration · 耗时'),
      h('option', { value: 'rate' }, 'rate · 速率'),
    ]);
    const hasDataOnly = h('input', { type: 'checkbox', checked: true });
    const hostHdr = h('div', { class: 'toolbar-row mb-12' }, [
      h('strong', {}, '◈ 指标目录 · 中英双语 · 自动配图表'),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        qInp, modSel, kindSel,
        h('label', { class: 'muted small', style: { display: 'flex', alignItems: 'center', gap: '4px' } },
          [hasDataOnly, ' 仅展示有数据']),
      ]),
    ]);
    const host = h('div');

    const renderRows = () => {
      host.innerHTML = '';
      const q = (qInp.value || '').trim().toLowerCase();
      const mod = modSel.value;
      const kind = kindSel.value;
      const onlyHasData = hasDataOnly.checked;
      const rows = metricsList.filter(m => {
        if (mod && m.module !== mod) return false;
        if (kind && m.kind !== kind) return false;
        const c = covMap[m.module + ':' + m.name];
        if (onlyHasData && (!c || (c.samples || 0) === 0)) return false;
        if (q) {
          const blob = (m.name + ' ' + (m.zh || '') + ' ' + (m.en || '') + ' ' + (m.module || '')).toLowerCase();
          if (!blob.includes(q)) return false;
        }
        return true;
      });
      host.appendChild(h('div', { class: 'muted small mb-12' },
        '匹配 ' + rows.length + ' / ' + total + ' · 有数据 ' + collected));

      // 每个指标一张卡 (含迷你图)
      const grid = h('div', { class: 'grid grid-2' });
      for (const m of rows) {
        const c = covMap[m.module + ':' + m.name] || {};
        const has = (c.samples || 0) > 0;
        const card = h('div', {
          class: 'card', style: { cursor: has ? 'pointer' : 'default' },
          onClick: () => { if (has) location.hash = '#/metrics/' + encodeURIComponent(m.module); },
        }, [
          h('div', { style: { display: 'flex', gap: '8px', alignItems: 'baseline', flexWrap: 'wrap' } }, [
            h('span', { class: 'badge' }, m.module || '—'),
            h('span', { class: 'badge ' + metricKindBadge(m.kind) }, m.kind || '—'),
            h('div', { style: { flex: 1, fontWeight: 700, fontSize: '14px' } }, m.zh || m.name),
            has ? h('span', { class: 'badge ok' }, (c.samples || 0) + ' 样本') : h('span', { class: 'badge warn' }, '无样本'),
          ]),
          h('div', { class: 'muted small mt-12', style: { fontFamily: 'monospace' } }, m.name),
          m.en ? h('div', { class: 'muted small' }, m.en) : null,
          h('div', { class: 'muted small mt-12' }, [
            m.unit ? ('单位: ' + m.unit + ' · ') : '',
            has ? ('最近值 ' + fmtNum(c.lastValue || 0, 3) + ' · ' + fmtRel(c.lastSeen)) : '还没数据',
          ]),
        ]);
        grid.appendChild(card);
      }
      host.appendChild(grid);
    };
    qInp.addEventListener('input', renderRows);
    modSel.addEventListener('change', renderRows);
    kindSel.addEventListener('change', renderRows);
    hasDataOnly.addEventListener('change', renderRows);

    v.appendChild(hostHdr);
    v.appendChild(host);
    renderRows();
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
        h('button', {
          class: 'btn primary small',
          onClick: () => { state.cronEdit = { open: true, mode: 'create', item: null }; renderCron(); },
        }, '＋ 新建 Cron'),
        viewToggle(state.cronView, v => { state.cronView = v; localStorage.setItem('cronView', v); navigate(); }),
      ]),
    ]));

    if (state.cronEdit && state.cronEdit.open) {
      v.appendChild(renderCronEditor(state.cronEdit.item || {}, state.cronEdit.mode));
    }

    v.__cronList = list;
    v.appendChild(h('div', { id: 'cron-body' }));
    renderCronBody();
  }

  async function cronDelete(c) {
    if (!confirm('确认删除 cron 任务 ' + (c.name || c.id) + '?')) return;
    try {
      await api('/api/actions/cron/delete/' + encodeURIComponent(c.name || c.id), { method: 'POST' });
      toast('已删除 ' + (c.name || c.id), 'ok');
      renderCron();
    } catch (e) { toast('删除失败: ' + e.message, 'err'); }
  }

  function renderCronEditor(item, mode) {
    const card = h('div', { class: 'card mb-16' });
    card.appendChild(sectionHeader(mode === 'create' ? '新建 Cron 任务' : ('编辑 Cron: ' + (item.name || item.id)),
      h('button', { class: 'btn small ghost', onClick: () => { state.cronEdit = null; renderCron(); } }, '× 关闭')));

    const nameInp = h('input', { class: 'input', placeholder: '任务名 (唯一, 如 daily-summary)',
      value: item.name || item.id || '', disabled: mode === 'edit' });
    const schInp = h('input', { class: 'input', placeholder: 'Cron 表达式 (如 0 */4 * * *)', value: item.schedule || '' });
    const wfInp = h('input', { class: 'input', placeholder: 'Workflow 名 (development / reverse_engineering / ...)',
      value: item.workflow || '' });
    const payloadInp = h('textarea', { class: 'input', rows: 3, style: { width: '100%' },
      placeholder: 'Payload (objective 或 JSON)', value: item.payload || '' });
    const enabled = h('input', { type: 'checkbox', checked: item.enabled !== false });
    const out = h('div', { class: 'muted', style: { marginTop: '10px', fontSize: '12px' } });

    const submit = async () => {
      const body = {
        name: (nameInp.value || '').trim(),
        schedule: (schInp.value || '').trim(),
        workflow: (wfInp.value || '').trim(),
        payload: (payloadInp.value || ''),
        enabled: !!enabled.checked,
      };
      if (!body.name) { toast('name 必填', 'err'); return; }
      if (!body.schedule) { toast('schedule 必填', 'err'); return; }
      try {
        const action = mode === 'create' ? 'create' : 'update';
        const resp = await api('/api/actions/cron/' + action + '/' + encodeURIComponent(body.name), {
          method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body),
        });
        toast((mode === 'create' ? '已创建' : '已更新') + ' ' + body.name, 'ok');
        out.textContent = JSON.stringify(resp);
        state.cronEdit = null;
        setTimeout(() => renderCron(), 200);
      } catch (e) {
        toast('提交失败: ' + e.message, 'err');
      }
    };

    card.appendChild(h('div', { class: 'form-grid' }, [
      h('div', {}, [h('label', { class: 'muted' }, '名称'), nameInp]),
      h('div', {}, [h('label', { class: 'muted' }, 'Schedule'), schInp]),
      h('div', {}, [h('label', { class: 'muted' }, 'Workflow'), wfInp]),
      h('div', {}, [h('label', { class: 'muted' }, '启用'), enabled]),
      h('div', { style: { gridColumn: '1 / -1' } }, [h('label', { class: 'muted' }, 'Payload'), payloadInp]),
      h('div', { style: { gridColumn: '1 / -1' } }, [
        h('button', { class: 'btn primary', onClick: submit }, mode === 'create' ? '▶ 创建' : '✓ 保存'),
        h('button', { class: 'btn ghost', style: { marginLeft: '8px' },
          onClick: () => { state.cronEdit = null; renderCron(); } }, '取消'),
        out,
      ]),
    ]));
    return card;
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
            h('button', { class: 'btn small ghost', title: '编辑',
              onClick: () => { state.cronEdit = { open: true, mode: 'edit', item: c }; renderCron(); } }, '✎'),
            h('button', { class: 'btn small ghost danger', onClick: () => cronDelete(c) }, '×'),
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
            h('button', { class: 'btn small ghost', onClick: () => { state.cronEdit = { open: true, mode: 'edit', item: c }; renderCron(); } }, '✎ 编辑'),
            h('button', { class: 'btn small ghost danger', onClick: () => cronDelete(c) }, '× 删除'),
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

    // v1.5: 操作区 - 主动触发 + LLM 诊断
    const diagSlot = h('div', { id: 'dreaming-diag-slot' });
    v.appendChild(h('div', { class: 'card mb-16' }, [
      h('div', { class: 'toolbar-row' }, [
        h('h3', { style: { margin: 0 } }, 'Dreaming 操作与诊断'),
        h('button', { class: 'btn primary', onClick: async () => {
          try {
            const resp = await api('/api/dreaming/trigger', { method: 'POST' });
            toast('已排队: ' + (resp.message || 'triggered'), 'ok');
          } catch (e) { toast('触发失败: ' + e.message, 'err'); }
        }}, '🌙 主动触发 Dreaming'),
        h('button', { class: 'btn', onClick: () => runAsyncDiagnosis({
          slot: diagSlot, kind: 'dreaming', target: '',
          title: '✦ Dreaming LLM 诊断',
          description: '分析整理质量 / 运行状态 / 被使用情况, 给出优化建议 (耗时 30~90s)',
        }) }, '🔎 LLM 诊断'),
      ]),
      diagSlot,
    ]));

    // 诊断 (基于规则的简单诊断, 独立于 LLM 诊断)
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

    // v1.5: Evolution LLM 诊断
    const evoDiagSlot = h('div', { id: 'evolution-diag-slot' });
    v.appendChild(h('div', { class: 'card mb-16' }, [
      h('div', { class: 'toolbar-row' }, [
        h('h3', { style: { margin: 0 } }, 'Evolution 操作与诊断'),
        h('button', { class: 'btn', onClick: () => runAsyncDiagnosis({
          slot: evoDiagSlot, kind: 'evolution', target: '',
          title: '✦ Evolution LLM 诊断',
          description: '分析数据质量 / 运行状态 / 被使用情况 / 是否退化, 给出优化建议 (耗时 30~90s)',
        }) }, '🔎 LLM 诊断'),
      ]),
      evoDiagSlot,
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

    // 拉取日志源列表
    let sources = [];
    try {
      const resp = await api('/api/logs/sources');
      if (Array.isArray(resp)) sources = resp;
      else if (resp && Array.isArray(resp.sources)) sources = resp.sources;
    } catch (_) { sources = []; }
    sources = sources.filter(s => s && (s.exists !== false));

    if (!state.logs.source && sources.length > 0) {
      state.logs.source = sources[0].id;
    }

    // 左侧频道列表
    const left = h('div', { class: 'log-channels' }, [
      h('div', { class: 'log-channels-hdr' }, [
        h('strong', {}, '日志频道'),
        h('span', { class: 'muted small' }, '共 ' + sources.length),
      ]),
    ]);
    for (const s of sources) {
      const active = s.id === state.logs.source;
      left.appendChild(h('div', {
        class: 'log-channel' + (active ? ' active' : ''),
        onClick: () => { state.logs.source = s.id; renderLogs(); },
      }, [
        h('div', { class: 'log-channel-title' }, s.title || s.id),
        h('div', { class: 'log-channel-meta muted small' }, [
          h('span', {}, s.module || ''),
          h('span', {}, (s.sizeBytes ? fmtBytes(s.sizeBytes) : '—')),
        ]),
        h('div', { class: 'log-channel-path muted small' }, s.path || ''),
      ]));
    }
    if (sources.length === 0) {
      left.appendChild(h('div', { class: 'empty' }, '暂无可用日志频道'));
    }

    // 右侧控制条 + 视图
    const srcParam = state.logs.source ? ('?source=' + encodeURIComponent(state.logs.source)) : '';
    const ctrls = h('div', { class: 'log-ctrls' }, [
      h('span', { class: 'pulse ' + (state.logs.paused ? 'red' : '') }),
      h('strong', {}, (state.logs.paused ? '已暂停 · ' : '实时 · ') + (state.logs.source || 'default')),
      h('input', { class: 'input', placeholder: '过滤关键词', value: state.logs.filter || '',
                   onInput: e => state.logs.filter = e.target.value }),
      h('button', { class: 'btn small', onClick: () => { state.logs.paused = !state.logs.paused; renderLogs(); } },
        state.logs.paused ? '▶ 继续' : '⏸ 暂停'),
      h('button', { class: 'btn small ghost', onClick: () => { const el = $('#log-viewer'); if (el) el.innerHTML = ''; } }, '清空视图'),
      h('a', { class: 'btn small', href: '/api/logs/tail' + (srcParam ? srcParam + '&n=1000' : '?n=1000'), target: '_blank' }, '↓ 下载最近 1000 行'),
    ]);
    const pathBar = h('div', { class: 'muted small', id: 'log-path-bar', style: { marginBottom: '8px' } }, '');
    const viewer = h('div', { class: 'log-viewer', id: 'log-viewer' });

    const right = h('div', { class: 'log-right' }, [ctrls, pathBar, viewer]);
    const wrap = h('div', { class: 'two-col logs-layout' }, [left, right]);
    v.appendChild(wrap);

    const tailURL = '/api/logs/tail' + (srcParam ? srcParam + '&n=500' : '?n=500');
    const initial = await api(tailURL).catch(() => ({ lines: [] }));
    if (initial.path) {
      pathBar.textContent = 'tail: ' + initial.path;
    } else {
      viewer.appendChild(h('div', { class: 'empty' }, '未找到日志文件'));
      return;
    }
    for (const line of (initial.lines || [])) appendLogLine(viewer, line);
    viewer.scrollTop = viewer.scrollHeight;

    if (state.logs.sse) try { state.logs.sse.close(); } catch (_) {}
    const streamURL = '/api/logs/stream' + srcParam;
    state.logs.sse = new EventSource(streamURL);
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
  function fmtBytes(n) {
    if (!n || n < 0) return '—';
    const units = ['B', 'K', 'M', 'G'];
    let i = 0; let v = n;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return v.toFixed(v >= 10 || i === 0 ? 0 : 1) + units[i];
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

    // --- 新建群体智能预测任务 + 群体仿真 ---
    v.appendChild(h('div', { class: 'grid grid-2 mb-16' }, [
      renderSwarmCreatePanel(),
      renderSwarmSimulatePanel(),
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
  // 12b. Swarm 创建表单  (POST /api/actions/swarm/create/<name>)
  // ==================================================================
  function renderSwarmCreatePanel() {
    const card = h('div', { class: 'card mb-16' });
    card.appendChild(sectionHeader('发起新的群体智能预测', h('span', { class: 'muted', style: { fontSize: '12px' } }, '动作会写入 action queue, 主进程会真正调用 swarm_intel.Predict')));
    const nameInp = h('input', { class: 'input', placeholder: '预测任务名 (例: q4-market-size)' });
    const objInp = h('textarea', { class: 'input', placeholder: 'objective: 比如 "2027年全球 AI Agent 市场规模?"', rows: 2, style: { width: '100%' } });
    const analystsInp = h('input', { class: 'input', type: 'number', value: '5', min: '1', max: '20', style: { width: '100px' } });
    const roundsInp = h('input', { class: 'input', type: 'number', value: '3', min: '1', max: '10', style: { width: '100px' } });
    const modelInp = h('input', { class: 'input', placeholder: '可选: 覆盖模型名 (留空用全局配置)', style: { width: '240px' } });
    const btn = h('button', { class: 'btn primary', onClick: submit }, '▶ 发起预测');
    const out = h('div', { class: 'muted', style: { marginTop: '10px', fontSize: '12px' } });

    async function submit() {
      const name = (nameInp.value || '').trim() || ('swarm-' + Date.now());
      const objective = (objInp.value || '').trim();
      if (!objective) { toast('objective 必填', 'err'); return; }
      btn.disabled = true; btn.textContent = '提交中…';
      try {
        const resp = await api('/api/actions/swarm/create/' + encodeURIComponent(name), {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({
            objective,
            analysts: Number(analystsInp.value) || 5,
            rounds: Number(roundsInp.value) || 3,
            model: (modelInp.value || '').trim(),
          }),
        });
        out.innerHTML = '';
        out.appendChild(h('div', { class: 'insight ok' }, h('div', { class: 'insight-title' }, resp.message || '已排队')));
        if (resp.hint) out.appendChild(h('pre', { class: 'code-block', style: { marginTop: '8px' } }, resp.hint));
        toast('预测任务已入队 (' + (resp.actionId || '') + ')', 'ok');
      } catch (e) {
        toast('提交失败: ' + e.message, 'err');
      } finally {
        btn.disabled = false; btn.textContent = '▶ 发起预测';
      }
    }

    card.appendChild(h('div', { class: 'form-grid' }, [
      h('div', {}, [h('label', { class: 'muted' }, '名称'), nameInp]),
      h('div', { style: { gridColumn: '1 / -1' } }, [h('label', { class: 'muted' }, 'Objective'), objInp]),
      h('div', {}, [h('label', { class: 'muted' }, 'Analysts'), analystsInp]),
      h('div', {}, [h('label', { class: 'muted' }, 'Rounds'), roundsInp]),
      h('div', { style: { gridColumn: '1 / -1' } }, [h('label', { class: 'muted' }, '模型 (可选)'), modelInp]),
      h('div', { style: { gridColumn: '1 / -1' } }, [btn, out]),
    ]));
    return card;
  }

  // ==================================================================
  // 12b2. Swarm 仿真面板  (POST /api/actions/swarm/simulate/<name>)
  // ==================================================================
  function renderSwarmSimulatePanel() {
    const card = h('div', { class: 'card mb-16' });
    card.appendChild(sectionHeader('发起群体仿真',
      h('span', { class: 'muted', style: { fontSize: '12px' } }, '对复杂场景做多轮 Monte Carlo 仿真, 由主进程调用 swarm_intel.Simulate')));
    const nameInp = h('input', { class: 'input', placeholder: '仿真任务名 (例: pricing-stress)' });
    const scenarioInp = h('textarea', {
      class: 'input',
      placeholder: '场景描述: 比如 "某大型 SaaS 把 API 定价从 $20/M 调到 $12/M 后,未来 30 天的用户增长与收入变化"',
      rows: 3, style: { width: '100%' },
    });
    const trialsInp = h('input', { class: 'input', type: 'number', value: '50', min: '5', max: '500', style: { width: '100px' } });
    const agentsInp = h('input', { class: 'input', type: 'number', value: '6', min: '2', max: '20', style: { width: '100px' } });
    const horizonInp = h('input', { class: 'input', placeholder: 'horizon: 7d / 30d / 90d', style: { width: '160px' } });
    const modelInp = h('input', { class: 'input', placeholder: '可选: 覆盖模型名', style: { width: '240px' } });
    const btn = h('button', { class: 'btn primary', onClick: submit }, '▶ 发起仿真');
    const out = h('div', { class: 'muted', style: { marginTop: '10px', fontSize: '12px' } });

    async function submit() {
      const name = (nameInp.value || '').trim() || ('sim-' + Date.now());
      const scenario = (scenarioInp.value || '').trim();
      if (!scenario) { toast('场景必填', 'err'); return; }
      btn.disabled = true; btn.textContent = '提交中…';
      try {
        const resp = await api('/api/actions/swarm/simulate/' + encodeURIComponent(name), {
          method: 'POST',
          headers: { 'content-type': 'application/json' },
          body: JSON.stringify({
            scenario,
            trials: Number(trialsInp.value) || 50,
            agents: Number(agentsInp.value) || 6,
            horizon: (horizonInp.value || '').trim() || '30d',
            model: (modelInp.value || '').trim(),
          }),
        });
        out.innerHTML = '';
        out.appendChild(h('div', { class: 'insight ok' }, h('div', { class: 'insight-title' }, resp.message || '已排队')));
        if (resp.hint) out.appendChild(h('pre', { class: 'code-block', style: { marginTop: '8px' } }, resp.hint));
        toast('仿真任务已入队 (' + (resp.actionId || '') + ')', 'ok');
      } catch (e) {
        toast('提交失败: ' + e.message, 'err');
      } finally {
        btn.disabled = false; btn.textContent = '▶ 发起仿真';
      }
    }

    card.appendChild(h('div', { class: 'form-grid' }, [
      h('div', {}, [h('label', { class: 'muted' }, '名称'), nameInp]),
      h('div', { style: { gridColumn: '1 / -1' } }, [h('label', { class: 'muted' }, '场景'), scenarioInp]),
      h('div', {}, [h('label', { class: 'muted' }, 'Trials'), trialsInp]),
      h('div', {}, [h('label', { class: 'muted' }, 'Agents'), agentsInp]),
      h('div', {}, [h('label', { class: 'muted' }, 'Horizon'), horizonInp]),
      h('div', { style: { gridColumn: '1 / -1' } }, [h('label', { class: 'muted' }, '模型 (可选)'), modelInp]),
      h('div', { style: { gridColumn: '1 / -1' } }, [btn, out]),
    ]));
    return card;
  }

  // ==================================================================
  // 12c. Workflows 页面  /api/workflows[/:name]  + DAG + 创建团队
  // ==================================================================
  async function renderWorkflows(arg) {
    const v = $('#view');
    v.innerHTML = '<div class="loading">加载工作流…</div>';
    const list = await api('/api/workflows');
    v.innerHTML = '';

    v.appendChild(h('div', { class: 'grid grid-3 mb-16' }, [
      statCard('内置工作流', list.length, '来自 agent.ListWorkflows()', 'accent'),
      statCard('支持模式', new Set(list.map(w => w.mode || 'sequential')).size, '', 'purple'),
      statCard('总 Stage 数', list.reduce((s, w) => s + (w.stages || []).length, 0), '各 workflow stages 合计', ''),
    ]));

    // 左: 列表, 右: 详情 + DAG + 创建表单
    const grid = h('div', { class: 'workflow-grid mb-16' });
    const left = h('div', { class: 'card' }, [h('h3', {}, '可用工作流')]);
    const right = h('div', { class: 'card workflow-detail' });

    let active = arg || (list[0] && list[0].name) || null;
    function renderDetail(name) {
      right.innerHTML = '';
      const wf = list.find(x => x.name === name);
      if (!wf) { right.appendChild(h('div', { class: 'empty' }, '未找到工作流')); return; }
      right.appendChild(h('h3', {}, wf.name));
      right.appendChild(h('div', { class: 'muted', style: { marginBottom: '10px' } }, wf.description || '—'));
      right.appendChild(h('div', { class: 'pill-row mb-16' }, [
        h('span', { class: 'badge purple' }, 'mode: ' + (wf.mode || 'sequential')),
        wf.rounds ? h('span', { class: 'badge' }, 'rounds: ' + wf.rounds) : null,
        h('span', { class: 'badge' }, 'stages: ' + (wf.stages || []).length),
      ]));
      // Stages 列表
      const stbl = h('table', { class: 'tbl' });
      stbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '#'), h('th', {}, 'Name'), h('th', {}, 'Role'), h('th', {}, 'Depends'), h('th', {}, 'Parallel'),
      ])));
      const tb = h('tbody');
      (wf.stages || []).forEach((s, i) => {
        tb.appendChild(h('tr', {}, [
          h('td', {}, String(i + 1)),
          h('td', {}, s.name),
          h('td', {}, h('span', { class: 'badge' }, s.role || '—')),
          h('td', {}, (s.dependsOn || []).length ? (s.dependsOn || []).join(', ') : '—'),
          h('td', {}, s.parallel ? '✓' : '—'),
        ]));
      });
      stbl.appendChild(tb);
      right.appendChild(h('div', { class: 'mb-16' }, stbl));

      // DAG 可视化 (简单版 Canvas)
      const dagWrap = h('div', { class: 'card', style: { padding: '10px' } }, [
        h('h3', { style: { marginTop: 0 } }, 'Stage 依赖 DAG'),
      ]);
      const canvas = h('canvas', { style: { width: '100%', height: '260px' } });
      dagWrap.appendChild(canvas);
      right.appendChild(dagWrap);
      setTimeout(() => drawWorkflowDAG(canvas, wf), 0);

      // 创建团队表单
      right.appendChild(renderTeamCreatePanel(wf.name));
    }

    for (const wf of list) {
      const item = h('a', {
        href: '#/workflows/' + encodeURIComponent(wf.name),
        class: 'team-card' + (wf.name === active ? ' active' : ''),
        style: { display: 'block', marginBottom: '6px' },
        onClick: (e) => { e.preventDefault(); active = wf.name; renderDetail(active); location.hash = '#/workflows/' + encodeURIComponent(wf.name); },
      }, [
        h('div', { class: 'team-name' }, wf.name),
        h('div', { class: 'team-meta' }, [
          h('span', { class: 'badge purple' }, (wf.stages || []).length + ' stages'),
          h('span', {}, (wf.mode || 'sequential')),
        ]),
      ]);
      left.appendChild(item);
    }

    grid.appendChild(left);
    grid.appendChild(right);
    v.appendChild(grid);
    if (active) renderDetail(active);
  }

  function drawWorkflowDAG(canvas, wf) {
    const ctx = canvas.getContext('2d');
    const dpr = window.devicePixelRatio || 1;
    const cw = canvas.clientWidth || 500;
    const ch = 260;
    canvas.width = cw * dpr; canvas.height = ch * dpr;
    ctx.scale(dpr, dpr);
    ctx.clearRect(0, 0, cw, ch);

    // 层次分层: depth(n) = max(depth(d)+1 for d in deps) else 0
    const stages = wf.stages || [];
    const byName = {}; stages.forEach(s => byName[s.name] = s);
    const depth = {};
    function d(n) {
      if (depth[n] != null) return depth[n];
      const s = byName[n]; if (!s) return 0;
      if (!s.dependsOn || !s.dependsOn.length) return depth[n] = 0;
      return depth[n] = 1 + Math.max.apply(null, s.dependsOn.map(x => d(x)));
    }
    stages.forEach(s => d(s.name));
    const levels = {};
    stages.forEach(s => { (levels[depth[s.name]] = levels[depth[s.name]] || []).push(s.name); });
    const depths = Object.keys(levels).map(Number).sort((a, b) => a - b);

    const margin = 24;
    const nodeW = 120, nodeH = 34;
    const colGap = (cw - margin * 2 - nodeW) / Math.max(1, depths.length - 1 || 1);
    const pos = {};
    depths.forEach((lv, colIdx) => {
      const col = levels[lv];
      col.forEach((name, rowIdx) => {
        const x = margin + colIdx * colGap;
        const rowGap = (ch - margin * 2 - nodeH) / Math.max(1, col.length - 1 || 1);
        const y = col.length === 1 ? (ch - nodeH) / 2 : margin + rowIdx * rowGap;
        pos[name] = { x, y };
      });
    });

    // edges
    ctx.strokeStyle = 'rgba(120,150,255,0.45)';
    ctx.lineWidth = 1.5;
    for (const s of stages) {
      for (const dep of (s.dependsOn || [])) {
        const p1 = pos[dep], p2 = pos[s.name]; if (!p1 || !p2) continue;
        const x1 = p1.x + nodeW, y1 = p1.y + nodeH / 2;
        const x2 = p2.x, y2 = p2.y + nodeH / 2;
        ctx.beginPath();
        ctx.moveTo(x1, y1);
        ctx.bezierCurveTo(x1 + 40, y1, x2 - 40, y2, x2, y2);
        ctx.stroke();
      }
    }

    // nodes
    for (const s of stages) {
      const p = pos[s.name]; if (!p) continue;
      ctx.fillStyle = 'rgba(72,92,180,0.35)';
      ctx.strokeStyle = 'rgba(140,170,255,0.9)';
      ctx.lineWidth = 1;
      roundRect(ctx, p.x, p.y, nodeW, nodeH, 8, true, true);
      ctx.fillStyle = '#E9ECFF';
      ctx.font = '12px system-ui,-apple-system,sans-serif';
      ctx.textBaseline = 'middle';
      const label = s.name.length > 14 ? s.name.slice(0, 13) + '…' : s.name;
      ctx.fillText(label, p.x + 10, p.y + 13);
      ctx.fillStyle = 'rgba(200,210,255,0.7)';
      ctx.font = '10px system-ui';
      ctx.fillText(s.role || '', p.x + 10, p.y + 26);
    }
  }

  function roundRect(ctx, x, y, w, hpx, r, fill, stroke) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.lineTo(x + w - r, y);
    ctx.quadraticCurveTo(x + w, y, x + w, y + r);
    ctx.lineTo(x + w, y + hpx - r);
    ctx.quadraticCurveTo(x + w, y + hpx, x + w - r, y + hpx);
    ctx.lineTo(x + r, y + hpx);
    ctx.quadraticCurveTo(x, y + hpx, x, y + hpx - r);
    ctx.lineTo(x, y + r);
    ctx.quadraticCurveTo(x, y, x + r, y);
    if (fill) ctx.fill();
    if (stroke) ctx.stroke();
  }

  function renderTeamCreatePanel(workflowName) {
    const card = h('div', { class: 'card', style: { marginTop: '16px' } });
    card.appendChild(h('h3', {}, '用该工作流创建新团队'));
    const nameInp = h('input', { class: 'input', placeholder: '团队名 (字母/数字/-)' });
    const objInp = h('textarea', { class: 'input', placeholder: 'objective: 具体要达成的目标', rows: 2, style: { width: '100%' } });
    const langSel = h('select', { class: 'select' }, [
      h('option', { value: 'zh' }, '中文 (zh)'),
      h('option', { value: 'en' }, 'English'),
    ]);
    const btn = h('button', { class: 'btn primary' }, '▶ 发起创建');
    const out = h('div', { class: 'muted', style: { marginTop: '10px', fontSize: '12px' } });
    btn.onclick = async () => {
      const name = (nameInp.value || '').trim();
      const objective = (objInp.value || '').trim();
      if (!name || !objective) { toast('团队名 & objective 必填', 'err'); return; }
      btn.disabled = true; btn.textContent = '提交中…';
      try {
        const resp = await api('/api/actions/team/create/' + encodeURIComponent(name), {
          method: 'POST', headers: { 'content-type': 'application/json' },
          body: JSON.stringify({
            workflow: workflowName,
            objective,
            lang: langSel.value,
          }),
        });
        out.innerHTML = '';
        out.appendChild(h('div', { class: 'insight ok' }, h('div', { class: 'insight-title' }, resp.message || '已排队')));
        if (resp.hint) out.appendChild(h('pre', { class: 'code-block', style: { marginTop: '8px' } }, resp.hint));
        toast('团队创建任务已入队', 'ok');
      } catch (e) {
        toast('提交失败: ' + e.message, 'err');
      } finally {
        btn.disabled = false; btn.textContent = '▶ 发起创建';
      }
    };
    card.appendChild(h('div', { class: 'form-grid' }, [
      h('div', {}, [h('label', { class: 'muted' }, '团队名'), nameInp]),
      h('div', {}, [h('label', { class: 'muted' }, '语言'), langSel]),
      h('div', { style: { gridColumn: '1 / -1' } }, [h('label', { class: 'muted' }, 'Objective'), objInp]),
      h('div', { style: { gridColumn: '1 / -1' } }, [btn, out]),
    ]));
    return card;
  }

  // ==================================================================
  // 12d. Backups 页面  /api/backups  + create + restore
  // ==================================================================
  async function renderBackups() {
    const v = $('#view');
    v.innerHTML = '<div class="loading">加载备份列表…</div>';
    let llm = null;
    try { llm = await api('/api/llm/status'); } catch (_) {}

    const data = await api('/api/backups');
    v.innerHTML = '';

    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('StateDir', data.stateDir ? '✓' : '✗', data.stateDir || '—', data.stateDir ? 'ok' : 'err'),
      statCard('归档数', data.count || 0, '位于 stateDir/backups/', 'accent'),
      statCard('总大小', fmtMB(data.entries || []), '', 'purple'),
      statCard('LLM 就绪', llm && llm.ready ? 'ON' : 'OFF',
        llm ? ((llm.profile && llm.profile.provider) + ' / ' + ((llm.profile && llm.profile.source) || '')) : '未知',
        llm && llm.ready ? 'ok' : 'warn'),
    ]));

    // 创建备份表单
    const createCard = h('div', { class: 'card mb-16' });
    createCard.appendChild(h('h3', {}, '创建新备份'));
    const labelInp = h('input', { class: 'input', placeholder: 'label (例: weekly / before-upgrade)', style: { maxWidth: '280px' } });
    const incReports = h('input', { type: 'checkbox', checked: true });
    const btnCreate = h('button', { class: 'btn primary' }, '📦 创建');
    btnCreate.onclick = async () => {
      btnCreate.disabled = true; btnCreate.textContent = '打包中…';
      try {
        const resp = await api('/api/backups/create', {
          method: 'POST', headers: { 'content-type': 'application/json' },
          body: JSON.stringify({
            label: (labelInp.value || '').trim(),
            includeReports: incReports.checked,
          }),
        });
        toast('备份已创建: ' + resp.path, 'ok');
        setTimeout(() => renderBackups(), 500);
      } catch (e) { toast('创建失败: ' + e.message, 'err'); }
      finally { btnCreate.disabled = false; btnCreate.textContent = '📦 创建'; }
    };
    createCard.appendChild(h('div', { class: 'form-grid' }, [
      h('div', {}, [h('label', { class: 'muted' }, 'Label'), labelInp]),
      h('div', {}, [
        h('label', { class: 'muted' }, 'Include REPORT.md'),
        h('div', { style: { marginTop: '4px' } }, [incReports, h('span', { class: 'muted', style: { marginLeft: '6px', fontSize: '12px' } }, '(体积较大但内容完整)')]),
      ]),
      h('div', { style: { gridColumn: '1 / -1' } }, btnCreate),
    ]));
    v.appendChild(createCard);

    // 备份列表
    const listCard = h('div', { class: 'card' });
    listCard.appendChild(h('h3', {}, '归档列表 (按时间倒序)'));
    if (!(data.entries || []).length) {
      listCard.appendChild(h('div', { class: 'empty' }, '尚无备份'));
    } else {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '时间'), h('th', {}, 'Label'), h('th', {}, '大小 (MB)'), h('th', {}, '文件'), h('th', {}, '动作'),
      ])));
      const tb = h('tbody');
      for (const e of data.entries) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, fmtTime(e.createdAt)),
          h('td', {}, e.label || '—'),
          h('td', {}, (e.size / 1024 / 1024).toFixed(2)),
          h('td', {}, h('code', { style: { fontSize: '11px' } }, e.name)),
          h('td', {}, [
            h('button', {
              class: 'btn small',
              title: '在浏览器下载 .tar.gz',
              onClick: () => { location.href = '/api/backups/download?path=' + encodeURIComponent(e.path); },
            }, '⬇ 下载'),
            h('button', {
              class: 'btn small ghost',
              style: { marginLeft: '4px' },
              onClick: async () => {
                try {
                  const m = await api('/api/backups/manifest?path=' + encodeURIComponent(e.path));
                  alert('MANIFEST:\n' + JSON.stringify(m, null, 2));
                } catch (err) { toast('读取失败: ' + err.message, 'err'); }
              },
            }, 'Manifest'),
            h('button', {
              class: 'btn small warn',
              style: { marginLeft: '4px' },
              onClick: async () => {
                if (!confirm('从该备份恢复?\n⚠️ 会覆盖当前 .claude-go 中的对应目录\n(默认会先自动生成一份 safety 快照)\n\n' + e.path)) return;
                try {
                  const resp = await api('/api/backups/restore', {
                    method: 'POST', headers: { 'content-type': 'application/json' },
                    body: JSON.stringify({ path: e.path }),
                  });
                  toast('已恢复 ' + resp.restoredFiles + ' 个文件; safety=' + (resp.safetyBackup || '(未生成)'), 'ok');
                } catch (err) { toast('恢复失败: ' + err.message, 'err'); }
              },
            }, '♻ 恢复'),
            h('button', {
              class: 'btn small err',
              style: { marginLeft: '4px' },
              onClick: async () => {
                if (!confirm('删除该备份?\n' + e.path)) return;
                try {
                  await api('/api/backups?path=' + encodeURIComponent(e.path), { method: 'DELETE' });
                  toast('已删除', 'ok');
                  setTimeout(() => renderBackups(), 300);
                } catch (err) { toast('删除失败: ' + err.message, 'err'); }
              },
            }, '🗑 删除'),
          ]),
        ]));
      }
      tbl.appendChild(tb);
      listCard.appendChild(tbl);
    }
    v.appendChild(listCard);
  }

  function fmtMB(entries) {
    const total = (entries || []).reduce((s, e) => s + (e.size || 0), 0);
    return (total / 1024 / 1024).toFixed(2) + ' MB';
  }

  // ==================================================================
  // 12d-1. Metrics Catalog (中英双语 / Grafana 风格指标中心)
  // ==================================================================
  async function renderMetricsCatalog() {
    const v = $('#view');
    v.innerHTML = '<div class="loading">加载 metrics catalog…</div>';
    let cat, cov;
    try {
      [cat, cov] = await Promise.all([
        api('/api/metrics/catalog'),
        api('/api/metrics/coverage'),
      ]);
    } catch (e) {
      v.innerHTML = '';
      v.appendChild(h('div', { class: 'empty', style: { color: 'var(--red)' } }, '加载失败: ' + e.message));
      return;
    }
    v.innerHTML = '';

    // Backend 返回结构:
    //   catalog 端点: { total, modules, catalog: MetricDesc[], byModule: {...} }
    //   coverage 端点: { now, totalDeclared, totalCollected, coverageRate, rows, missingDesc }
    //   MetricDesc 字段: { module, name, zh, en, unit, kind, panel }
    //   coverage row 字段: { module, name, zh, en, unit, kind, panel, declared, collected, samples, lastValue, lastSeen }
    const metricsList = cat.catalog || [];
    const covRows = (cov && cov.rows) || [];
    // key = module + ':' + name (coverage 里有同名不同模块)
    const covMap = {};
    covRows.forEach(m => { covMap[m.module + ':' + m.name] = m; });
    const total = cat.total || metricsList.length;
    const collected = (cov && cov.totalCollected) || 0;
    const coveragePct = (cov && cov.coverageRate) || (total > 0 ? collected / total : 0);

    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('指标总数', total, '已声明在 pkg/metrics/catalog.go', 'accent'),
      statCard('有采集数据', collected, '至少 1 个事件', collected > 0 ? 'ok' : 'warn'),
      statCard('覆盖率', fmtPct(coveragePct),
        '= 有样本指标 / 总指标', coveragePct >= 0.8 ? 'ok' : (coveragePct >= 0.4 ? 'warn' : 'err')),
      statCard('模块数', (cat.modules || []).length, '按模块分组的指标', 'purple'),
    ]));

    if (cov && (cov.missingDesc || []).length) {
      const card = h('div', { class: 'card mb-16' }, [
        h('h3', {}, '⚠ 已采集但未在 catalog 声明的 metric (请补中英文描述)'),
        h('div', { class: 'muted small' }, '补全位置: pkg/metrics/catalog.go'),
        h('pre', { class: 'pre', style: { maxHeight: '160px', overflow: 'auto' } }, cov.missingDesc.join('\n')),
      ]);
      v.appendChild(card);
    }

    // 过滤/搜索
    const qInp = h('input', { class: 'input', placeholder: '搜索: 名称 / 描述 / 模块…', style: { minWidth: '260px' } });
    const modSel = h('select', { class: 'select' }, [
      h('option', { value: '' }, '全部模块'),
      ...(cat.modules || []).map(m => h('option', { value: m }, m)),
    ]);
    const kindSel = h('select', { class: 'select' }, [
      h('option', { value: '' }, '全部类型'),
      h('option', { value: 'counter' }, 'counter 计数器'),
      h('option', { value: 'gauge' }, 'gauge 瞬时量'),
      h('option', { value: 'histogram' }, 'histogram 分布'),
      h('option', { value: 'duration' }, 'duration 耗时'),
      h('option', { value: 'rate' }, 'rate 速率'),
    ]);
    const onlyMissing = h('input', { type: 'checkbox' });
    const listHost = h('div', { class: 'card' });

    const render = () => {
      const q = (qInp.value || '').trim().toLowerCase();
      const mod = modSel.value;
      const kind = kindSel.value;
      const missingOnly = onlyMissing.checked;

      const rows = metricsList.filter(m => {
        if (mod && m.module !== mod) return false;
        if (kind && m.kind !== kind) return false;
        const c = covMap[m.module + ':' + m.name];
        if (missingOnly && c && (c.samples || 0) > 0) return false;
        if (q) {
          const blob = (m.name + ' ' + (m.zh || '') + ' ' + (m.en || '') + ' ' + (m.module || '')).toLowerCase();
          if (!blob.includes(q)) return false;
        }
        return true;
      });

      listHost.innerHTML = '';
      listHost.appendChild(h('div', { class: 'muted small', style: { marginBottom: '6px' } },
        '匹配 ' + rows.length + ' / ' + total + ' 个指标'));
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '模块'), h('th', {}, '指标名 (EN)'), h('th', {}, '中文描述'),
        h('th', {}, '类型'), h('th', {}, '单位'),
        h('th', {}, '样本数'), h('th', {}, '最近值'), h('th', {}, '最近上报'),
      ])));
      const tb = h('tbody');
      for (const m of rows) {
        const c = covMap[m.module + ':' + m.name] || {};
        const has = (c.samples || 0) > 0;
        tb.appendChild(h('tr', {}, [
          h('td', {}, h('span', { class: 'badge' }, m.module || '—')),
          h('td', { style: { fontFamily: 'monospace', fontSize: '12px' } }, [
            h('div', {}, m.name),
            m.en ? h('div', { class: 'muted small', title: m.en }, (m.en || '').slice(0, 90)) : null,
          ]),
          h('td', {}, [
            h('div', {}, m.zh || '—'),
            m.panel ? h('div', { class: 'muted small' }, 'panel: ' + m.panel) : null,
          ]),
          h('td', {}, h('span', { class: 'badge ' + metricKindBadge(m.kind) }, m.kind || '—')),
          h('td', {}, m.unit || '—'),
          h('td', {}, h('span', { class: 'badge ' + (has ? 'ok' : 'warn') }, String(c.samples || 0))),
          h('td', {}, c.lastValue != null ? fmtNum(c.lastValue, 3) : '—'),
          h('td', {}, c.lastSeen ? fmtRel(c.lastSeen) : '—'),
        ]));
      }
      tbl.appendChild(tb);
      listHost.appendChild(tbl);
    };

    v.appendChild(h('div', { class: 'toolbar-row mb-12' }, [
      h('strong', {}, '◈ 指标中心 · 中英双语 · 覆盖率审计'),
      h('div', { style: { display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' } }, [
        qInp, modSel, kindSel,
        h('label', { class: 'muted small', style: { display: 'flex', alignItems: 'center', gap: '4px' } },
          [onlyMissing, ' 仅显示未上报的指标']),
        h('button', { class: 'btn', onClick: render }, '刷新'),
      ]),
    ]));
    qInp.addEventListener('input', render);
    modSel.addEventListener('change', render);
    kindSel.addEventListener('change', render);
    onlyMissing.addEventListener('change', render);
    v.appendChild(listHost);
    render();
  }

  function metricKindBadge(k) {
    return ({
      counter: 'accent',
      gauge: 'purple',
      histogram: 'warn',
      duration: 'run',
      rate: 'ok',
    })[k] || '';
  }

  // ==================================================================
  // 12d-2. LLM rate (1s/60s/300s) + Guard (rate limit + circuit breaker)
  // ==================================================================
  async function renderLLMRateAndGuard(host) {
    host.innerHTML = '';
    host.appendChild(h('h3', {}, '⚡ 实时速率 / 限流 / 熔断器'));

    let rate = null, guard = null;
    try { rate = await api('/api/llm/rate?windows=1,60,300'); } catch (_) {}
    try { guard = await api('/api/llm/guard'); } catch (_) {}

    if (!rate && !guard) {
      host.appendChild(h('div', { class: 'empty' }, '暂无数据 (尚未发起 LLM 调用, 或 metrics/llm.jsonl 为空)'));
      return;
    }

    if (rate && (rate.buckets || []).length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '窗口'), h('th', {}, 'QPS'), h('th', {}, '调用'),
        h('th', {}, '成功率'), h('th', {}, '平均耗时'), h('th', {}, 'P95'),
        h('th', {}, '总Tokens'), h('th', {}, '429限流'),
        h('th', {}, '准入等待'), h('th', {}, '熔断阻断'), h('th', {}, '熔断触发'),
      ])));
      const tb = h('tbody');
      for (const w of rate.buckets) {
        const label = (w.windowSec >= 60) ? (w.windowSec / 60) + 'min' : (w.windowSec + 's');
        tb.appendChild(h('tr', {}, [
          h('td', {}, h('span', { class: 'badge accent' }, label)),
          h('td', {}, fmtNum(w.qps || 0, 2)),
          h('td', {}, String(w.calls || 0)),
          h('td', {}, h('span', { class: 'badge ' + ((w.successRate || 0) >= 0.9 ? 'ok' : 'warn') }, fmtPct(w.successRate))),
          h('td', {}, fmtDurSec(w.avgDurationSec)),
          h('td', {}, fmtDurSec(w.p95DurationSec)),
          h('td', {}, fmtKilo(w.totalTokens || 0)),
          h('td', {}, h('span', { class: 'badge ' + ((w.rateLimited || 0) > 0 ? 'warn' : '') }, String(w.rateLimited || 0))),
          h('td', {}, fmtNum(w.avgGuardWaitSec || 0, 2) + 's'),
          h('td', {}, h('span', { class: 'badge ' + ((w.circuitBlocked || 0) > 0 ? 'err' : '') }, String(w.circuitBlocked || 0))),
          h('td', {}, h('span', { class: 'badge ' + ((w.circuitTrips || 0) > 0 ? 'err' : '') }, String(w.circuitTrips || 0))),
        ]));
      }
      tbl.appendChild(tb);
      host.appendChild(tbl);
      host.appendChild(h('div', { class: 'muted small', style: { marginTop: '4px' } },
        '滑动窗口基于 metrics/llm.jsonl 内最近事件计算; 窗口越小越能捕获瞬时洪峰。'));
    }

    if (guard) {
      const g = guard.guard || {};
      const c = guard.circuit || {};
      const hints = guard.hints || [];
      const circuitState = c.open ? 'open' : 'closed';
      const grid = h('div', { class: 'grid grid-4', style: { marginTop: '12px' } }, [
        statCard('限流 · 并发',
          (g.inFlight || 0) + ' / ' + (g.maxParallel || 0),
          '硬限 ' + (g.hardMin || 0) + '~' + (g.hardMax || 0) + ' · 等待 ' + (g.totalWaits || 0),
          (g.inFlight || 0) >= (g.maxParallel || 1) ? 'warn' : 'accent'),
        statCard('限流 · RPM 令牌',
          fmtNum(g.rpmAvailable || 0, 1) + ' / ' + fmtNum(g.rpmCapacity || 0, 0),
          '已获取 ' + (g.totalAcquires || 0),
          (g.rpmAvailable || 0) < 1 ? 'warn' : 'ok'),
        statCard('熔断器',
          circuitState,
          '连续失败 ' + (c.consecutiveFails || 0) + '/' + (c.threshold || 5) + ' · 触发 ' + (c.circuitTrips || 0),
          c.open ? 'err' : 'ok'),
        statCard('AIMD 降速 / 429',
          (g.aimdCuts || 0) + ' / ' + (g.total429s || 0),
          (g.pauseSecondsLeft || 0) > 0 ? ('pause ' + fmtNum(g.pauseSecondsLeft, 1) + 's') : 'no backoff',
          (g.total429s || 0) > 0 ? 'warn' : ''),
      ]);
      host.appendChild(grid);
      if (hints.length) {
        const hints_card = h('div', { style: { marginTop: '8px' } });
        for (const t of hints) {
          hints_card.appendChild(h('div', { class: 'insight warn' }, h('div', { class: 'insight-title' }, t)));
        }
        host.appendChild(hints_card);
      }
    }
  }

  // ==================================================================
  // 12e. LLM 大模型监控 /api/llm/stats
  // ==================================================================
  async function renderLLMStats() {
    const v = $('#view');
    if (!state.llmStats.window) state.llmStats.window = '24h';
    v.innerHTML = '<div class="loading">加载 LLM 统计…</div>';
    let data;
    try {
      data = await api('/api/llm/stats?window=' + encodeURIComponent(state.llmStats.window));
    } catch (e) {
      v.innerHTML = '';
      v.appendChild(h('div', { class: 'empty', style: { color: 'var(--red)' } }, '加载失败: ' + e.message));
      return;
    }
    v.innerHTML = '';

    // 工具栏
    const toolbar = h('div', { class: 'toolbar-row mb-12' }, [
      h('strong', {}, '✦ LLM 大模型运行质量'),
      h('div', { style: { display: 'flex', gap: '6px', alignItems: 'center' } }, [
        h('span', { class: 'muted small' }, '时间窗口'),
        ...['1h', '6h', '24h', '72h', '168h'].map(w => h('button', {
          class: 'btn small' + (w === state.llmStats.window ? ' primary' : ' ghost'),
          onClick: () => { state.llmStats.window = w; renderLLMStats(); },
        }, w)),
        h('span', { class: 'muted small', style: { marginLeft: '10px' } }, 'source: ' + (data.source || '—')),
      ]),
    ]);
    v.appendChild(toolbar);

    // ⭐ 实时速率 / 限流 / 熔断器 (新)
    const rateHost = h('div', { class: 'card mb-16' }, [
      h('h3', {}, '⚡ 实时速率 / 限流 / 熔断器'),
      h('div', { class: 'muted small' }, '加载中…'),
    ]);
    v.appendChild(rateHost);
    renderLLMRateAndGuard(rateHost).catch(() => {});

    // 概览
    v.appendChild(h('div', { class: 'grid grid-4 mb-16' }, [
      statCard('总调用', data.totalCalls || 0,
        (data.totalRetries ? ('重试 ' + data.totalRetries) : '—'),
        data.totalCalls > 0 ? 'accent' : 'warn'),
      statCard('成功率', fmtPct(data.successRate), '成功 ' + (data.success || 0) + ' / 错误 ' + (data.errors || 0),
        (data.successRate || 0) >= 0.9 ? 'ok' : 'warn'),
      statCard('平均时长', fmtDurSec(data.avgDuration), 'P95 ' + fmtDurSec(data.p95Duration),
        (data.p95Duration || 0) > 25 ? 'warn' : ''),
      statCard('Token 消耗',
        fmtKilo((data.inputTokens || 0) + (data.outputTokens || 0) + (data.cacheReadTokens || 0) + (data.cacheCreationTokens || 0)),
        'in ' + fmtKilo(data.inputTokens || 0) + ' · out ' + fmtKilo(data.outputTokens || 0),
        'purple'),
    ]));

    // 告警
    if ((data.alerts || []).length) {
      const ac = h('div', { class: 'card mb-16' }, [h('h3', {}, '⚠ 告警 / 观察')]);
      for (const a of data.alerts) ac.appendChild(h('div', { class: 'insight warn' }, h('div', { class: 'insight-title' }, a)));
      v.appendChild(ac);
    }

    // 按模型聚合
    if ((data.byModel || []).length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '模型'), h('th', {}, '调用'), h('th', {}, '成功率'),
        h('th', {}, '平均时长'), h('th', {}, 'Input'), h('th', {}, 'Output'),
        h('th', {}, 'Cache(读/写)'), h('th', {}, '重试'),
      ])));
      const tb = h('tbody');
      for (const m of data.byModel) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, m.model),
          h('td', {}, String(m.calls)),
          h('td', {}, h('span', { class: 'badge ' + (m.successRate >= 0.9 ? 'ok' : 'warn') }, fmtPct(m.successRate))),
          h('td', {}, fmtDurSec(m.avgDuration)),
          h('td', {}, fmtKilo(m.inputTokens || 0)),
          h('td', {}, fmtKilo(m.outputTokens || 0)),
          h('td', {}, fmtKilo(m.cacheReadTokens || 0) + ' / ' + fmtKilo(m.cacheCreationTokens || 0)),
          h('td', {}, String(m.retries || 0)),
        ]));
      }
      tbl.appendChild(tb);
      v.appendChild(h('div', { class: 'card mb-16' }, [h('h3', {}, '按模型聚合'), tbl]));
    }

    // 按来源聚合 (cli / feishu / team / dashboard / unknown)
    if ((data.bySource || []).length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '来源'), h('th', {}, '调用'), h('th', {}, '成功率'),
        h('th', {}, '平均时长'), h('th', {}, 'Input'), h('th', {}, 'Output'), h('th', {}, '错误'),
      ])));
      const tb = h('tbody');
      for (const s of data.bySource) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, h('span', { class: 'badge' }, s.source)),
          h('td', {}, String(s.calls)),
          h('td', {}, h('span', { class: 'badge ' + (s.successRate >= 0.9 ? 'ok' : 'warn') }, fmtPct(s.successRate))),
          h('td', {}, fmtDurSec(s.avgDuration)),
          h('td', {}, fmtKilo(s.inputTokens || 0)),
          h('td', {}, fmtKilo(s.outputTokens || 0)),
          h('td', {}, String(s.errors || 0)),
        ]));
      }
      tbl.appendChild(tb);
      v.appendChild(h('div', { class: 'card mb-16' }, [h('h3', {}, '按来源聚合'), tbl]));
    }

    // 错误分布
    if ((data.errorBuckets || []).length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [h('th', {}, '错误类别'), h('th', {}, '次数')])));
      const tb = h('tbody');
      for (const b of data.errorBuckets) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, h('span', { class: 'badge ' + errBadgeClass(b.kind) }, b.kind)),
          h('td', {}, String(b.count)),
        ]));
      }
      tbl.appendChild(tb);
      v.appendChild(h('div', { class: 'card mb-16' }, [h('h3', {}, '错误分布'), tbl]));
    }

    // 时序曲线
    if ((data.timeseries || []).length) {
      const wrap = h('div', { class: 'card mb-16' }, [
        h('h3', {}, '调用时序 (按小时)'),
        h('div', { class: 'chart-wrap' }, h('canvas', { id: 'chart-llm-ts' })),
        h('div', { class: 'chart-wrap' }, h('canvas', { id: 'chart-llm-tok' })),
      ]);
      v.appendChild(wrap);
      requestAnimationFrame(() => {
        const c1 = document.getElementById('chart-llm-ts');
        if (c1 && window.Chart) {
          state.charts['llm-ts'] = new Chart(c1.getContext('2d'), {
            type: 'line',
            data: {
              labels: data.timeseries.map(p => fmtTsShort(p.ts)),
              datasets: [
                { label: '调用', data: data.timeseries.map(p => p.calls), borderColor: '#5ef0ff', tension: 0.3, pointRadius: 1 },
                { label: '成功', data: data.timeseries.map(p => p.success), borderColor: '#7CFF8C', tension: 0.3, pointRadius: 1 },
                { label: '错误', data: data.timeseries.map(p => p.errors), borderColor: '#ff6680', tension: 0.3, pointRadius: 1 },
              ],
            },
            options: baseChartOpts({}),
          });
        }
        const c2 = document.getElementById('chart-llm-tok');
        if (c2 && window.Chart) {
          state.charts['llm-tok'] = new Chart(c2.getContext('2d'), {
            type: 'line',
            data: {
              labels: data.timeseries.map(p => fmtTsShort(p.ts)),
              datasets: [
                { label: 'tokens/小时', data: data.timeseries.map(p => p.totalTokens), borderColor: '#C084FF', tension: 0.3, pointRadius: 1, fill: true, backgroundColor: 'rgba(192,132,255,0.12)' },
                { label: '平均耗时 s', data: data.timeseries.map(p => p.avgDuration), borderColor: '#ffbe55', tension: 0.3, pointRadius: 1, yAxisID: 'y2' },
              ],
            },
            options: baseChartOpts({
              scales: { y2: { position: 'right', grid: { drawOnChartArea: false } } },
            }),
          });
        }
      });
    } else {
      v.appendChild(h('div', { class: 'card' }, [
        h('h3', {}, '暂无数据'),
        h('div', { class: 'muted' },
          '窗口 ' + state.llmStats.window + ' 内没有 LLM 调用记录。首次启动需要等待主进程 / 飞书 bot / dashboard 自身发起至少 1 次 LLM 调用, 指标会写到 metrics/llm.jsonl。'),
      ]));
    }

    // ⭐ 提示词缓存效果 (v1.5)
    //   - Anthropic prompt caching: cacheRead 按 10% 计费, cacheCreate 按 125% 计费
    //   - HitRate = cacheRead / (input + cacheRead), 越高越省钱
    //   - 时序图: 每小时桶的命中率 + 读/写 token 量
    if (data.cache) {
      v.appendChild(renderPromptCachePanel(data.cache));
    }
  }

  // 提示词缓存效果面板
  function renderPromptCachePanel(cache) {
    const hitRate = cache.hitRate || 0;
    const cov = cache.cacheCoverage || 0;
    const wrap = h('div', { class: 'card mb-16' }, [
      h('h3', {}, '💾 提示词缓存效果 (Prompt Cache)'),
      h('div', { class: 'muted small mb-12' },
        '命中率 = cacheRead / (input + cacheRead); 越高越省钱。Anthropic cacheRead 按 10% 计费, cacheCreate 按 125% 计费。'),
      h('div', { class: 'grid grid-4 mb-16' }, [
        statCard('命中率', (hitRate * 100).toFixed(1) + '%',
          hitRate > 0.5 ? '优秀 (>50%)' : hitRate > 0.2 ? '一般' : '可优化',
          hitRate > 0.5 ? 'ok' : hitRate > 0.2 ? 'accent' : 'warn'),
        statCard('节省 Token', fmtKilo(cache.savedTokens || 0),
          '≈ ' + fmtKilo(Math.floor((cache.savedTokens || 0) * 0.9)) + ' 折算节省',
          'purple'),
        statCard('调用覆盖率',
          (cov * 100).toFixed(1) + '%',
          (cache.callsWithCache || 0) + ' 次命中缓存',
          cov > 0.5 ? 'ok' : ''),
        statCard('缓存写入', fmtKilo(cache.cacheCreateTokens || 0),
          '首次写入成本 (125%)', ''),
      ]),
    ]);

    // 按模型
    if ((cache.byModel || []).length) {
      const tbl = h('table', { class: 'tbl' });
      tbl.appendChild(h('thead', {}, h('tr', {}, [
        h('th', {}, '模型'), h('th', {}, '调用'), h('th', {}, '命中次数'),
        h('th', {}, '命中率'), h('th', {}, '命中 Token'), h('th', {}, '写入 Token'), h('th', {}, 'Input Token'),
      ])));
      const tb = h('tbody');
      for (const m of cache.byModel) {
        tb.appendChild(h('tr', {}, [
          h('td', {}, m.model),
          h('td', {}, String(m.calls || 0)),
          h('td', {}, String(m.callsWithCache || 0)),
          h('td', {}, h('span', {
            class: 'badge ' + ((m.hitRate || 0) > 0.5 ? 'ok' : (m.hitRate || 0) > 0.2 ? '' : 'warn')
          }, ((m.hitRate || 0) * 100).toFixed(1) + '%')),
          h('td', {}, fmtKilo(m.cacheReadTokens || 0)),
          h('td', {}, fmtKilo(m.cacheCreateTokens || 0)),
          h('td', {}, fmtKilo(m.inputTokens || 0)),
        ]));
      }
      tbl.appendChild(tb);
      wrap.appendChild(h('div', { class: 'card mb-12' }, [
        h('h4', {}, '按模型细分'), tbl,
      ]));
    }

    // 时序图
    if ((cache.timeseries || []).length) {
      const chartID = 'chart-cache-ts-' + Math.random().toString(36).slice(2, 8);
      const rateID  = 'chart-cache-rate-' + Math.random().toString(36).slice(2, 8);
      const chartCard = h('div', {}, [
        h('h4', {}, '缓存效果时序 (按小时)'),
        h('div', { class: 'chart-wrap' }, h('canvas', { id: chartID })),
        h('div', { class: 'chart-wrap' }, h('canvas', { id: rateID })),
      ]);
      wrap.appendChild(chartCard);
      requestAnimationFrame(() => {
        const c1 = document.getElementById(chartID);
        if (c1 && window.Chart) {
          state.charts[chartID] = new Chart(c1.getContext('2d'), {
            type: 'line',
            data: {
              labels: cache.timeseries.map(p => fmtTsShort(p.ts)),
              datasets: [
                { label: 'cacheRead (命中)', data: cache.timeseries.map(p => p.cacheReadTokens), borderColor: '#7CFF8C', backgroundColor: 'rgba(124,255,140,0.1)', fill: true, tension: 0.3, pointRadius: 1 },
                { label: 'cacheCreate (写入)', data: cache.timeseries.map(p => p.cacheCreateTokens), borderColor: '#C084FF', backgroundColor: 'rgba(192,132,255,0.08)', fill: true, tension: 0.3, pointRadius: 1 },
                { label: 'input (未命中)', data: cache.timeseries.map(p => p.inputTokens), borderColor: '#ffbe55', tension: 0.3, pointRadius: 1 },
              ],
            },
            options: baseChartOpts({}),
          });
        }
        const c2 = document.getElementById(rateID);
        if (c2 && window.Chart) {
          state.charts[rateID] = new Chart(c2.getContext('2d'), {
            type: 'line',
            data: {
              labels: cache.timeseries.map(p => fmtTsShort(p.ts)),
              datasets: [
                { label: '命中率 (%)', data: cache.timeseries.map(p => (p.hitRate || 0) * 100), borderColor: '#5ef0ff', backgroundColor: 'rgba(94,240,255,0.12)', fill: true, tension: 0.3, pointRadius: 1 },
              ],
            },
            options: baseChartOpts({ scales: { y: { suggestedMin: 0, suggestedMax: 100 } } }),
          });
        }
      });
    }
    return wrap;
  }

  function fmtPct(v) {
    if (v == null || isNaN(v)) return '—';
    return (v * 100).toFixed(1) + '%';
  }
  function fmtDurSec(v) {
    if (!v || isNaN(v)) return '—';
    if (v < 1) return (v * 1000).toFixed(0) + ' ms';
    if (v < 60) return v.toFixed(2) + ' s';
    return (v / 60).toFixed(1) + ' m';
  }
  function fmtKilo(n) {
    if (!n) return '0';
    if (n < 1000) return String(n);
    if (n < 1e6) return (n / 1000).toFixed(1) + 'K';
    if (n < 1e9) return (n / 1e6).toFixed(2) + 'M';
    return (n / 1e9).toFixed(2) + 'B';
  }
  function errBadgeClass(kind) {
    return ({
      rate_limit: 'warn',
      overloaded: 'warn',
      timeout: 'warn',
      refusal: 'err',
      prompt_too_long: 'err',
      auth: 'err',
      server: 'err',
    })[kind] || '';
  }

  // ==================================================================
  // 13. Actions (POST /api/actions/{kind}/{action}/{target})
  // ==================================================================
  async function execAction(kind, action, target, opts = {}) {
    if (opts.confirm && !confirm(opts.confirm + '\n' + kind + ' / ' + action + ' / ' + target)) return;
    try {
      const fetchOpts = { method: 'POST' };
      if (opts.payload) {
        fetchOpts.headers = { 'content-type': 'application/json' };
        fetchOpts.body = JSON.stringify(opts.payload);
      }
      const resp = await api(`/api/actions/${encodeURIComponent(kind)}/${encodeURIComponent(action)}/${encodeURIComponent(target)}`, fetchOpts);
      toast(resp.message || '动作已发送', 'ok');
      if (resp.hint) console.info('[action hint]', resp.hint);
      if (!opts.noReload) setTimeout(() => navigate(), 500);
      return resp;
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
