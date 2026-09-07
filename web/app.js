'use strict';

// Состояние страницы. Прогон живёт на сервере; здесь его снимок, журнал и
// фильтры журнала. Результат и журнал приходят разными событиями потока и
// рисуются разными функциями: ни одна не трогает панель другой.
const state = {
  agents: [],
  view: null,
  events: [],
  source: null,
  filter: { agent: 'all', tools: true, llm: true }
};

const el = {
  query: document.getElementById('query'),
  agent: document.getElementById('agent'),
  agentNote: document.getElementById('agent-note'),
  optClassification: document.getElementById('opt-classification'),
  optHabitat: document.getElementById('opt-habitat'),
  optDiet: document.getElementById('opt-diet'),
  runButton: document.getElementById('run-button'),
  askError: document.getElementById('ask-error'),
  work: document.getElementById('work'),
  resultStatus: document.getElementById('result-status'),
  result: document.getElementById('result'),
  logCount: document.getElementById('log-count'),
  agentChips: document.getElementById('agent-chips'),
  showTools: document.getElementById('show-tools'),
  showLLM: document.getElementById('show-llm'),
  log: document.getElementById('log'),
  totals: document.getElementById('totals'),
  prompts: document.getElementById('prompts'),
  reportPath: document.getElementById('report-path'),
  reportButton: document.getElementById('report-button'),
  reportDialog: document.getElementById('report-dialog'),
  reportClose: document.getElementById('report-close'),
  reportFile: document.getElementById('report-file'),
  reportBody: document.getElementById('report-body')
};

const KIND_LABEL = {
  'agent.start': 'агент',
  'agent.done': 'готово',
  'agent.error': 'ошибка',
  'llm.request': 'модель ←',
  'llm.response': 'модель →',
  'tool.call': 'инструмент ←',
  'tool.result': 'инструмент →',
  'tool.error': 'инструмент ✕',
  'note': 'заметка',
  'prompt': 'промпт'
};

async function init() {
  if (window.marked) {
    marked.use({
      gfm: true,
      breaks: true,
      renderer: {
        // Сырой HTML из ответа модели показываем как текст.
        html(token) {
          const raw = typeof token === 'string' ? token : (token.raw ?? token.text ?? '');
          return escapeHTML(raw);
        }
      }
    });
  }

  try {
    const res = await fetch('/api/agents');
    const data = await res.json();
    state.agents = data.agents || [];
    el.reportPath.textContent = data.reportPath || '—';
  } catch (err) {
    showError('Не удалось получить список агентов: ' + err.message);
  }

  el.agent.innerHTML = '';
  for (const a of state.agents) {
    const opt = document.createElement('option');
    opt.value = a.key;
    opt.textContent = a.title;
    el.agent.appendChild(opt);
  }
  updateAgentNote();

  el.agent.addEventListener('change', updateAgentNote);
  el.runButton.addEventListener('click', startRun);
  el.query.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || !e.shiftKey)) { e.preventDefault(); startRun(); }
  });
  el.showTools.addEventListener('change', () => { state.filter.tools = el.showTools.checked; applyFilter(); });
  el.showLLM.addEventListener('change', () => { state.filter.llm = el.showLLM.checked; applyFilter(); });
  el.reportButton.addEventListener('click', showReport);
  el.reportClose.addEventListener('click', () => el.reportDialog.close());

  // Прогон переживает перезагрузку страницы: идентификатор в адресе.
  const id = new URLSearchParams(location.search).get('run');
  if (id) attach(id);
}

function updateAgentNote() {
  const a = state.agents.find((x) => x.key === el.agent.value);
  el.agentNote.textContent = a ? a.description : '';
}

function currentAgentInfo() {
  return state.agents.find((x) => x.key === (state.view ? state.view.agentKey : el.agent.value));
}

async function startRun() {
  const query = el.query.value.trim();
  if (!query) { showError('Введите название животного.'); return; }
  showError('');

  el.runButton.disabled = true;
  try {
    const res = await fetch('/api/runs', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        agent: el.agent.value,
        query,
        options: {
          classification: el.optClassification.checked,
          habitat: el.optHabitat.checked,
          diet: el.optDiet.checked
        }
      })
    });
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || res.statusText);

    history.replaceState(null, '', '?run=' + encodeURIComponent(data.id));
    attach(data.id);
  } catch (err) {
    showError('Не удалось запустить: ' + err.message);
    el.runButton.disabled = false;
  }
}

// attach открывает поток событий прогона. Снимок приходит первым и целиком:
// состояние плюс весь журнал. Дальше состояние и журнал идут раздельно.
function attach(id) {
  if (state.source) state.source.close();
  state.view = null;
  state.events = [];
  // Фильтр по агенту принадлежит прогону: у следующего прогона другой
  // набор агентов, и выбранный раньше «identifier» спрятал бы весь журнал
  // одиночного агента.
  state.filter.agent = 'all';
  chipsKey = '';
  el.agentChips.innerHTML = '';
  el.log.innerHTML = '';
  el.result.innerHTML = '';
  el.prompts.innerHTML = '';
  el.work.hidden = false;
  el.runButton.disabled = true;

  const source = new EventSource('/api/runs/' + encodeURIComponent(id) + '/events');
  state.source = source;

  source.addEventListener('snapshot', (e) => {
    const snap = JSON.parse(e.data);
    state.view = snap.view;
    state.events = [];
    el.log.innerHTML = '';
    el.prompts.innerHTML = '';
    for (const ev of snap.events || []) appendEvent(ev);
    renderState();
  });
  source.addEventListener('state', (e) => {
    state.view = JSON.parse(e.data);
    renderState();
  });
  source.addEventListener('log', (e) => {
    appendEvent(JSON.parse(e.data));
  });
  source.addEventListener('done', () => {
    source.close();
    state.source = null;
    el.runButton.disabled = false;
    renderState();
  });
  source.onerror = () => {
    if (source.readyState === EventSource.CLOSED) {
      el.runButton.disabled = false;
      showError('Поток событий закрылся. Возможно, сервер перезапускали.');
    }
  };
}

/* ---------- Результат ---------- */

function renderState() {
  const v = state.view;
  if (!v) return;

  const done = v.status !== 'running';
  el.resultStatus.innerHTML = '';
  const dot = document.createElement('span');
  dot.className = 'dot ' + v.status;
  el.resultStatus.appendChild(dot);
  el.resultStatus.appendChild(document.createTextNode(
    v.status === 'running' ? 'агент работает…' : v.status === 'done' ? 'готово' : 'ошибка'));

  renderResult(v);
  renderTotals(v);
  if (done) el.runButton.disabled = false;
}

function renderResult(v) {
  const box = el.result;
  box.innerHTML = '';

  if (v.status === 'failed') {
    const p = document.createElement('p');
    p.className = 'none';
    p.textContent = 'Прогон завершился ошибкой: ' + (v.error || 'без объяснения');
    box.appendChild(p);
    return;
  }

  const res = v.result;
  if (!res) {
    const p = document.createElement('p');
    p.className = 'empty';
    p.textContent = v.status === 'running' ? 'Результата ещё нет: агент собирает сведения.' : 'Результата нет.';
    box.appendChild(p);
    return;
  }

  if (res.kind === 'none') {
    const p = document.createElement('p');
    p.className = 'none';
    p.textContent = 'Сведений нет. ' + (res.text || '');
    box.appendChild(p);
    return;
  }
  if (res.kind === 'text') {
    const div = document.createElement('div');
    div.className = 'markdown';
    div.innerHTML = renderMarkdown(res.text || '');
    box.appendChild(div);
    return;
  }
  if (res.kind === 'card' && res.card) {
    renderCard(box, res.card, v);
  }
}

function renderCard(box, card, v) {
  const opts = v.task.options || {};
  const running = v.status === 'running';

  const title = document.createElement('p');
  title.className = 'card-title';
  title.textContent = card.name || '—';
  box.appendChild(title);

  const latin = document.createElement('p');
  latin.className = 'card-latin';
  latin.textContent = card.latin || '';
  if (card.rank) {
    const rank = document.createElement('span');
    rank.className = 'rank';
    rank.textContent = ' · ' + rankRu(card.rank);
    latin.appendChild(rank);
  }
  box.appendChild(latin);

  if (card.summary) {
    const p = document.createElement('p');
    p.textContent = card.summary;
    box.appendChild(p);
  }

  if (opts.classification) {
    section(box, 'Классификация');
    if (card.classification && card.classification.length) {
      const ul = document.createElement('ul');
      ul.className = 'tree';
      card.classification.forEach((n, i) => {
        const li = document.createElement('li');
        li.style.setProperty('--depth', i);
        const rank = document.createElement('span');
        rank.className = 'rank';
        rank.textContent = n.rankRu || (n.rank || '').toLowerCase();
        const name = document.createElement('span');
        name.className = 'latin';
        name.textContent = n.name;
        li.appendChild(rank);
        li.appendChild(name);
        if (n.nameRu) {
          const ru = document.createElement('span');
          ru.className = 'ru';
          ru.textContent = ' — ' + n.nameRu;
          li.appendChild(ru);
        }
        ul.appendChild(li);
      });
      box.appendChild(ul);
    } else {
      pending(box, running, 'дерево ещё строится', 'дерево не получено');
    }
  }
  if (opts.habitat) {
    section(box, 'Среда обитания');
    if (card.habitat) paragraphs(box, card.habitat);
    else pending(box, running, 'раздел ещё читается', 'сведений нет, см. оговорки');
  }
  if (opts.diet) {
    section(box, 'Питание');
    if (card.diet) paragraphs(box, card.diet);
    else pending(box, running, 'раздел ещё читается', 'сведений нет, см. оговорки');
  }

  if (card.notes && card.notes.length) {
    section(box, 'Оговорки');
    const ul = document.createElement('ul');
    ul.className = 'plain notes';
    for (const n of card.notes) {
      const li = document.createElement('li');
      li.textContent = n;
      ul.appendChild(li);
    }
    box.appendChild(ul);
  }
  if (card.sources && card.sources.length) {
    section(box, 'Источники');
    const ul = document.createElement('ul');
    ul.className = 'plain sources';
    for (const s of card.sources) {
      const li = document.createElement('li');
      if (s.url) {
        const a = document.createElement('a');
        a.href = s.url;
        a.target = '_blank';
        a.rel = 'noopener';
        a.textContent = s.title || s.url;
        li.appendChild(a);
      } else {
        li.textContent = s.title;
      }
      ul.appendChild(li);
    }
    box.appendChild(ul);
  }
}

function section(box, text) {
  const h = document.createElement('h3');
  h.textContent = text;
  box.appendChild(h);
}

function paragraphs(box, text) {
  for (const part of String(text).split(/\n{2,}/)) {
    const p = document.createElement('p');
    p.textContent = part.trim();
    if (p.textContent) box.appendChild(p);
  }
}

function pending(box, running, whileRunning, whenDone) {
  const p = document.createElement('p');
  p.className = 'pending';
  p.textContent = running ? whileRunning + '…' : whenDone;
  box.appendChild(p);
}

const RANKS = { SPECIES: 'вид', GENUS: 'род', FAMILY: 'семейство', ORDER: 'отряд', CLASS: 'класс', PHYLUM: 'тип', KINGDOM: 'царство', SUBSPECIES: 'подвид' };
function rankRu(rank) { return RANKS[String(rank).toUpperCase()] || String(rank).toLowerCase(); }

/* ---------- Журнал ---------- */

function appendEvent(ev) {
  if (ev.kind === 'prompt') {
    appendPrompt(ev);
    return;
  }
  state.events.push(ev);
  const li = document.createElement('li');
  li.className = 'entry ' + ev.kind;
  li.dataset.agent = ev.agent;
  li.dataset.kind = ev.kind;

  const line = document.createElement('div');
  line.className = 'entry-line';

  const t = document.createElement('span');
  t.className = 't';
  t.textContent = '+' + offsetSeconds(ev.time).toFixed(1) + 'с';
  line.appendChild(t);

  const agent = document.createElement('span');
  agent.className = 'agent';
  agent.textContent = ev.agent;
  line.appendChild(agent);

  const kind = document.createElement('span');
  kind.className = 'kind';
  kind.textContent = KIND_LABEL[ev.kind] || ev.kind;
  line.appendChild(kind);

  const title = document.createElement('span');
  title.className = 'title';
  title.textContent = ev.title;
  line.appendChild(title);

  if (ev.usage) {
    const u = document.createElement('span');
    u.className = 'usage';
    let text = ev.usage.prompt + '→' + ev.usage.completion + ' ток.';
    if (ev.seconds) text += ' · ' + ev.seconds.toFixed(1) + 'с';
    u.textContent = text;
    line.appendChild(u);
  } else if (ev.seconds && ev.kind !== 'agent.start') {
    const u = document.createElement('span');
    u.className = 'usage';
    u.textContent = ev.seconds.toFixed(1) + 'с';
    line.appendChild(u);
  }
  li.appendChild(line);

  if (ev.detail) {
    const details = document.createElement('details');
    const summary = document.createElement('summary');
    summary.textContent = 'подробности';
    const pre = document.createElement('pre');
    pre.textContent = ev.detail;
    details.appendChild(summary);
    details.appendChild(pre);
    li.appendChild(details);
  }

  li.hidden = !passesFilter(ev);
  const stick = el.log.scrollHeight - el.log.scrollTop - el.log.clientHeight < 40;
  el.log.appendChild(li);
  if (stick) el.log.scrollTop = el.log.scrollHeight;

  el.logCount.textContent = state.events.length + ' событий';
  renderChips();
}

/* ---------- Промпты ---------- */

// appendPrompt показывает, что получила модель на старте агента. Событие
// в ленту не попадает: это условия задачи, а не ход работы.
function appendPrompt(ev) {
  let p = null;
  try { p = JSON.parse(ev.detail); } catch (_) { p = null; }

  const box = document.createElement('details');
  box.className = 'prompt';
  box.dataset.agent = ev.agent;

  const summary = document.createElement('summary');
  const agent = document.createElement('span');
  agent.className = 'agent';
  agent.textContent = ev.agent;
  summary.appendChild(agent);
  const meta = document.createElement('span');
  meta.className = 'meta';
  const tools = p && p.tools ? p.tools : [];
  meta.textContent = tools.length
    ? 'инструментов: ' + tools.length + ' · ' + tools.map((t) => t.name).join(', ')
    : 'без инструментов';
  summary.appendChild(meta);
  box.appendChild(summary);

  if (!p) {
    const pre = document.createElement('pre');
    pre.textContent = ev.detail || '';
    box.appendChild(pre);
  } else {
    promptBlock(box, 'Сообщение system', p.system);
    promptBlock(box, 'Сообщение user', p.user);
    if (tools.length) {
      const h = document.createElement('h4');
      h.textContent = 'Инструменты, как они описаны модели';
      box.appendChild(h);
      const ul = document.createElement('ul');
      for (const t of tools) {
        const li = document.createElement('li');
        const code = document.createElement('code');
        code.textContent = t.name;
        li.appendChild(code);
        if (t.final) {
          const f = document.createElement('span');
          f.className = 'final';
          f.textContent = ' завершающий';
          li.appendChild(f);
        }
        li.appendChild(document.createTextNode(' — ' + (t.description || '')));
        ul.appendChild(li);
      }
      box.appendChild(ul);
    }
  }
  el.prompts.appendChild(box);
}

function promptBlock(box, title, text) {
  const h = document.createElement('h4');
  h.textContent = title;
  box.appendChild(h);
  const pre = document.createElement('pre');
  pre.textContent = text || '';
  box.appendChild(pre);
}

function offsetSeconds(time) {
  if (!state.view) return 0;
  return Math.max(0, (new Date(time) - new Date(state.view.started)) / 1000);
}

function passesFilter(ev) {
  const f = state.filter;
  if (f.agent !== 'all' && ev.agent !== f.agent) return false;
  if (!f.tools && ev.kind.startsWith('tool.')) return false;
  if (!f.llm && ev.kind.startsWith('llm.')) return false;
  return true;
}

function applyFilter() {
  const entries = el.log.children;
  for (let i = 0; i < entries.length; i++) {
    entries[i].hidden = !passesFilter(state.events[i]);
  }
}

// renderChips перестраивает набор кнопок-агентов, только если он изменился:
// иначе каждое событие сбрасывало бы фокус.
let chipsKey = '';
function renderChips() {
  const info = currentAgentInfo();
  const seen = new Set(state.events.map((e) => e.agent));
  const names = (info ? info.agents : []).filter((a) => seen.has(a));
  for (const a of seen) if (!names.includes(a)) names.push(a);
  if (names.length < 2) {
    el.agentChips.innerHTML = '';
    chipsKey = '';
    if (state.filter.agent !== 'all') { state.filter.agent = 'all'; applyFilter(); }
    return;
  }

  const key = names.join('|');
  if (key === chipsKey) return;
  chipsKey = key;

  el.agentChips.innerHTML = '';
  const all = chip('все', 'all');
  el.agentChips.appendChild(all);
  for (const a of names) el.agentChips.appendChild(chip(a, a));
  syncChips();
}

function chip(label, value) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'chip';
  b.textContent = label;
  b.dataset.value = value;
  if (value !== 'all') b.dataset.agent = value;
  b.addEventListener('click', () => {
    state.filter.agent = value;
    syncChips();
    applyFilter();
  });
  return b;
}

function syncChips() {
  for (const b of el.agentChips.children) {
    b.classList.toggle('active', b.dataset.value === state.filter.agent);
  }
}

function renderTotals(v) {
  const t = v.totals || {};
  const u = t.usage || {};
  const parts = [
    ['время', (t.seconds || 0).toFixed(1) + ' с'],
    ['к модели', String(t.llmCalls || 0)],
    ['инструментов', String(t.toolCalls || 0)],
    ['токены', (u.prompt || 0) + '→' + (u.completion || 0)]
  ];
  if (t.cost && t.cost.known) parts.push(['стоимость', formatUSD(t.cost.usd) + ' (' + t.cost.tariff + ')']);
  el.totals.innerHTML = '';
  for (const [k, val] of parts) {
    const span = document.createElement('span');
    span.textContent = k + ' ';
    const strong = document.createElement('strong');
    strong.textContent = val;
    span.appendChild(strong);
    el.totals.appendChild(span);
  }
  if (v.saveError) {
    const span = document.createElement('span');
    span.style.color = 'var(--error)';
    span.textContent = 'файл не записан: ' + v.saveError;
    el.totals.appendChild(span);
  }
}

function formatUSD(usd) {
  return usd >= 0.01 ? '$' + usd.toFixed(4) : '$' + usd.toFixed(6);
}

/* ---------- Отчёт ---------- */

async function showReport() {
  try {
    const res = await fetch('/api/report');
    const data = await res.json();
    if (!res.ok) throw new Error(data.error || res.statusText);
    el.reportFile.textContent = data.path;
    el.reportBody.innerHTML = data.markdown ? renderMarkdown(data.markdown) : '<p class="hint">Файл пока пуст.</p>';
    el.reportDialog.showModal();
  } catch (err) {
    showError('Не удалось прочитать отчёт: ' + err.message);
  }
}

/* ---------- Утилиты ---------- */

function renderMarkdown(text) {
  if (!window.marked) return '<pre>' + escapeHTML(text) + '</pre>';
  return marked.parse(text);
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function showError(text) {
  el.askError.textContent = text;
  el.askError.hidden = !text;
}

init();
