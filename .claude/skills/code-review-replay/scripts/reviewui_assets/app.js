'use strict';
/* Ground-truth review UI.
 *
 * Layout principle: the LEDGER is primary, the code surface is context.
 * Measured across 40 renderable records, only 70.8% of judged findings land
 * inside a rendered diff hunk — 29.2% sit off-hunk, in an untouched file, or
 * are file-level. A diff-first UI would hide them, so every finding is listed
 * in the ledger under its anchor class and the right pane follows selection.
 */

const $ = (sel, root = document) => root.querySelector(sel);
const el = (tag, attrs = {}, ...kids) => {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') n.className = v;
    else if (k === 'text') n.textContent = v;
    else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
    else if (v !== null && v !== undefined) n.setAttribute(k, v);
  }
  for (const kid of kids.flat()) {
    if (kid == null) continue;
    n.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return n;
};

const ANCHOR_LABELS = {
  'in-hunk': ['In the diff', 'anchored to a changed line'],
  'in-file-off-hunk': ['In a changed file, outside the diff',
                       'the PR touched this file but not these lines'],
  'file-level': ['File-level', 'no line — concerns the file as a whole'],
  'not-in-diff': ['Not in the diff',
                  'the PR did not change this file at all'],
};
const ANCHOR_ORDER = ['in-hunk', 'in-file-off-hunk', 'file-level',
                      'not-in-diff'];

let PR = null;         // current PR payload
let SELECTED = null;   // selected ledger index

async function api(path, body) {
  const opts = body
    ? {method: 'POST', headers: {'Content-Type': 'application/json'},
       body: JSON.stringify(body)}
    : {};
  const res = await fetch(path, opts);
  return res.json();
}

/* ------------------------------------------------------------------ list */

async function renderList() {
  const app = $('#app');
  app.replaceChildren(el('p', {class: 'loading', text: 'Loading records…'}));
  const data = await api('/api/records');

  $('#mode-badge').replaceChildren(
    el('span', {class: 'badge ' + (data.allow_write ? 'rw' : 'ro'),
                text: data.allow_write ? 'read-write (overlay)' : 'read-only'}),
    document.createTextNode(' '),
    el('span', {class: 'badge', text: data.reviewer}));

  const t = data.totals;
  const head = el('p', {class: 'totals'},
    `${t.records} records with frozen ground truth · ${t.renderable} renderable · `,
    `${t.adjudicated}/${t.entries} entries adjudicated`);

  const rows = data.records.map(r => el('tr', {},
    el('td', {},
      el('a', {href: `#/pr/${r.target}`, text: r.target}),
      r.harvest_source === 'github'
        ? [' ', el('span', {class: 'badge github', text: 'github',
                            title: 'bot-derived GT; excluded from bake-off ' +
                                   'scoring pool'})]
        : null),
    el('td', {class: 'num', text: r.true_positives}),
    el('td', {class: 'num', text: r.false_positives}),
    el('td', {class: 'num', text: r.contested || ''}),
    el('td', {class: 'num'},
      r.suggestions ? el('span', {class: 'bucket unmatched',
                                  text: String(r.suggestions)}) : ''),
    el('td', {text: r.census_converged ? 'yes' : 'no'}),
    el('td', {class: 'num',
              text: `${r.adjudicated}/${r.entries}`}),
    el('td', {}, el('span', {class: 'state-' + r.render_state,
                             title: r.render_detail || '',
                             text: r.render_state})),
    el('td', {class: 'goal', title: r.goal_text, text: r.goal_text})));

  const table = el('table', {class: 'records'},
    el('thead', {}, el('tr', {},
      el('th', {text: 'PR'}), el('th', {class: 'num', text: 'TP'}),
      el('th', {class: 'num', text: 'FP'}),
      el('th', {class: 'num', text: 'Cont'}),
      el('th', {class: 'num', title: 'recurrent unmatched replay findings',
                text: 'Sugg'}),
      el('th', {text: 'Converged'}),
      el('th', {class: 'num', text: 'Reviewed'}),
      el('th', {text: 'Render'}), el('th', {text: 'Goal'}))),
    el('tbody', {}, rows));

  app.replaceChildren(head, table);
}

/* -------------------------------------------------------------- PR detail */

async function renderPR(target, select) {
  const app = $('#app');
  app.replaceChildren(el('p', {class: 'loading', text: `Loading ${target}…`}));
  PR = await api(`/api/pr/${target}`);
  SELECTED = null;
  if (PR.error) {
    app.replaceChildren(el('p', {class: 'warn', text: PR.error}));
    return;
  }

  const head = el('div', {class: 'pr-head'},
    el('h2', {}, target,
      PR.harvest_source === 'github'
        ? [' ', el('span', {class: 'badge github', text: 'github-sourced'})]
        : null),
    el('div', {class: 'pr-meta'},
      `${PR.ledger.length} ledger rows · ${PR.stats.adjudicated}/`,
      `${PR.stats.entries} adjudicated · rounds ${PR.rounds_run} · `,
      `frozen ${(PR.frozen_at || '').slice(0, 10)}`),
    // The goal is the full PR body — on a real record that runs to thousands
    // of characters and would push the ledger and code surface below the
    // fold, defeating the point of a fast review pass. Collapsed by default,
    // one click away when the reviewer needs the change's intent.
    PR.goal_text
      ? el('details', {class: 'goal-details'},
          el('summary', {text: `PR description (${PR.goal_text.length} chars)`}),
          el('div', {class: 'goal-body', text: PR.goal_text}))
      : null);

  const warns = [];
  if (!PR.census_converged) {
    warns.push(el('div', {class: 'warn'},
      'Census did not converge — the true-positive set is a lower bound. ',
      'This is where a human pass adds the most, since the judge likely ',
      'missed defects.'));
  }
  if (PR.render.state !== 'renderable') {
    warns.push(el('div', {class: 'warn'},
      `Diff unavailable (${PR.render.state}): ${PR.render.detail}. `,
      'Findings are still listed and adjudicable below.'));
  } else if (PR.render.base_source === 'recomputed') {
    warns.push(el('div', {class: 'warn'}, PR.render.detail));
  }
  if (PR.harvest_source === 'github') {
    warns.push(el('div', {class: 'warn'},
      'GitHub-sourced record: its ground truth is bot-derived and sits ',
      'outside the bake-off scoring pool. Reviewing it corrects the record ',
      'but will not move any config score.'));
  }
  const rw = PR.replay_window || {};
  if (rw.archives_total > rw.archives_read) {
    warns.push(el('div', {class: 'warn'},
      `Suggestions come from the ${rw.archives_read} most recent scored `,
      `replays of ${rw.archives_total} on disk. Older archives span earlier `,
      'reviewer regimes (e.g. before the diff-scope fix), so aggregating all ',
      'of them would mix findings no current config would produce.'));
  }

  const ledger = el('div', {class: 'ledger', id: 'ledger'});
  const code = el('div', {class: 'code', id: 'code'},
    el('div', {class: 'code-head'}, el('span', {text: 'Select a finding'})),
    el('div', {class: 'code-body'},
      el('p', {class: 'empty', text: 'Pick a finding from the ledger.'})));

  app.replaceChildren(head, ...warns,
    el('div', {class: 'split'}, ledger, code));

  // `#/pr/<target>/<index>` deep-links a specific finding, so a reviewer can
  // bookmark or share the exact row under discussion.
  const idx = Number.isInteger(select) ? select : -1;
  if (idx >= 0 && idx < PR.ledger.length) SELECTED = idx;
  drawLedger();
  if (SELECTED !== null) drawCode(PR.ledger[SELECTED]);
}

function stagedFor(row) {
  const verdicts = (PR.overlay && PR.overlay.verdicts) || [];
  return verdicts.find(v => sameLoc(v, row)) || null;
}

// Mirrors collect_lib._entry_matches: null matches only null, else ±3 rows.
function sameLoc(a, b) {
  if ((a.line === null) !== (b.line === null)) return false;
  if (norm(a.file) !== norm(b.file)) return false;
  if (a.line === null) return true;
  return Math.abs(a.line - b.line) <= 3;
}
function norm(p) { return String(p || '').replace(/^\.\//, ''); }

function drawLedger() {
  const groups = {};
  PR.ledger.forEach((row, i) => {
    (groups[row.anchor] = groups[row.anchor] || []).push({row, i});
  });

  const nodes = [];
  for (const anchor of ANCHOR_ORDER) {
    const items = groups[anchor];
    if (!items || !items.length) continue;
    const [label, note] = ANCHOR_LABELS[anchor];
    nodes.push(el('div', {class: 'anchor-group'},
      el('h3', {}, `${label} (${items.length}) `,
        el('span', {class: 'anchor-note', text: `— ${note}`})),
      items.map(({row, i}) => findingNode(row, i))));
  }
  $('#ledger').replaceChildren(...nodes);
}

function findingNode(row, i) {
  const staged = stagedFor(row);
  const hv = row.human_verdict;
  const node = el('div', {
    class: 'finding' + (SELECTED === i ? ' selected' : ''),
    onclick: (e) => {
      if (e.target.tagName === 'BUTTON') return;
      SELECTED = (SELECTED === i) ? null : i;
      drawLedger();
      if (SELECTED !== null) drawCode(row);
    },
  },
    el('div', {},
      row.severity
        ? el('span', {class: 'sev ' + row.severity, text: row.severity + ' '})
        : null,
      el('span', {class: 'bucket ' + row.bucket,
                  text: row.kind === 'suggestion'
                    ? 'suggestion' : row.bucket.replace('_', ' ')}),
      row.cross_config
        ? el('span', {class: 'bucket unmatched', text: ' · cross-config'})
        : null),
    el('div', {class: 'loc', text: `${row.file}:${row.line === null ? '—' : row.line}`}),
    el('div', {class: 'topic', text: row.topic}),
    row.judge_reason
      ? el('div', {class: 'why', text: row.judge_reason}) : null,
    el('div', {class: 'meta'},
      row.surfaced_by && row.surfaced_by.length
        ? `surfaced by ${row.surfaced_by.join(', ')}` : '',
      row.judge_severity ? ` · judge said ${row.judge_severity}` : ''),
    hv ? el('div', {class: 'human-stamp' + (hv.verdict === 'reject' ? ' reject' : ''),
                    text: `applied: ${hv.verdict} by ${hv.reviewer}`}) : null,
    staged ? el('div', {class: 'staged', text: `staged: ${staged.op}`}) : null,
    actionRow(row, staged));
  return node;
}

function actionRow(row, staged) {
  if (!PR.allow_write) {
    return el('div', {class: 'actions'},
      el('span', {class: 'meta',
                  text: 'read-only — restart with --allow-write'}));
  }
  const isSugg = row.kind === 'suggestion';
  const btns = [];
  if (isSugg) {
    btns.push(el('button', {
      text: 'promote to true positive',
      onclick: () => stage(row, {op: 'add', severity: 'medium',
                                 topic: row.topic}),
    }));
  } else {
    btns.push(
      el('button', {text: 'confirm', onclick: () => stage(row, {op: 'confirm'})}),
      el('button', {class: 'danger', text: 'reject',
                    onclick: () => stage(row, {op: 'reject'})}),
      el('button', {text: 'severity…', onclick: () => reseverity(row)}));
  }
  if (staged) {
    btns.push(el('button', {text: 'unstage',
                            onclick: () => stage(row, {op: '_clear'})}));
  }
  return el('div', {class: 'actions'}, btns);
}

async function reseverity(row) {
  const sev = prompt(
    `New severity for ${row.file}:${row.line}\n(high, medium, low, nit)`,
    row.severity || 'medium');
  if (!sev) return;
  if (!['high', 'medium', 'low', 'nit'].includes(sev)) {
    alert('severity must be one of: high, medium, low, nit');
    return;
  }
  stage(row, {op: 'reseverity', severity: sev});
}

async function stage(row, extra) {
  const reason = extra.op === '_clear' ? '' :
    (prompt('Why? (recorded with your verdict)', '') ?? null);
  if (reason === null && extra.op !== '_clear') return;
  const res = await api(`/api/verdict/${PR.target}`, Object.assign({
    file: row.file, line: row.line, reason,
  }, extra));
  if (res.error) { alert(res.error); return; }
  PR.overlay = res.overlay;
  drawLedger();
}

/* --------------------------------------------------------- code surface */

function drawCode(row) {
  const codeEl = $('#code');
  const anchor = row.anchor;

  if (anchor === 'in-hunk') {
    const file = PR.diff.find(f => norm(f.path) === norm(row.file));
    if (file) return drawDiffFile(codeEl, file, row);
  }
  if (anchor === 'file-level') {
    return drawNote(codeEl, row,
      'File-level finding — it concerns the file as a whole, so there is no ' +
      'line to anchor to.');
  }
  const ctx = PR.contexts[`${row.file}:${row.line}`];
  if (ctx) return drawContext(codeEl, ctx, row, anchor);
  drawNote(codeEl, row,
    anchor === 'not-in-diff'
      ? 'This file is not part of the PR diff, and its contents could not be ' +
        'read at head_before.'
      : 'No rendered diff line and no file context available.');
}

function drawDiffFile(codeEl, file, row) {
  const rows = file.lines.map(l => {
    const isTarget = l.new !== null && l.new === row.line;
    const kindClass = l.kind === 'hunk' ? 'hunk' : l.kind;
    return el('tr', {class: kindClass + (isTarget ? ' target' : '')},
      el('td', {class: 'ln', title: 'click to add a finding here',
                onclick: () => addAt(file.path, l.new),
                text: l.new === null ? '' : String(l.new)}),
      el('td', {class: 'code-text',
                text: (l.kind === 'add' ? '+' : l.kind === 'del' ? '-' :
                       l.kind === 'hunk' ? '' : ' ') + l.text}));
  });
  codeEl.replaceChildren(
    el('div', {class: 'code-head'},
      el('span', {text: file.path}),
      el('span', {class: 'ctx-label', text: 'diff · merge_base..head_before'})),
    el('div', {class: 'code-body'},
      el('table', {class: 'src'}, el('tbody', {}, rows))));
  scrollToTarget(codeEl);
}

function drawContext(codeEl, ctx, row, anchor) {
  const rows = ctx.lines.map(l => el('tr', {
    class: l.n === row.line ? 'target' : '',
  },
    el('td', {class: 'ln', title: 'click to add a finding here',
              onclick: () => addAt(ctx.path, l.n), text: String(l.n)}),
    el('td', {class: 'code-text', text: l.text})));
  const why = anchor === 'not-in-diff'
    ? 'file content at head_before — this file is NOT in the PR diff'
    : 'file content at head_before — these lines are NOT part of the diff';
  codeEl.replaceChildren(
    el('div', {class: 'code-head'},
      el('span', {text: `${ctx.path} (lines ${ctx.start}–${ctx.end} of ${ctx.total})`}),
      el('span', {class: 'ctx-label', text: why})),
    el('div', {class: 'code-body'},
      el('table', {class: 'src'}, el('tbody', {}, rows))));
  scrollToTarget(codeEl);
}

function drawNote(codeEl, row, msg) {
  codeEl.replaceChildren(
    el('div', {class: 'code-head'}, el('span', {text: row.file})),
    el('div', {class: 'code-body'}, el('p', {class: 'empty', text: msg})));
}

function scrollToTarget(codeEl) {
  const t = codeEl.querySelector('tr.target');
  if (t) t.scrollIntoView({block: 'center'});
}

async function addAt(path, line) {
  if (!PR.allow_write) {
    alert('read-only — restart the server with --allow-write');
    return;
  }
  if (line === null || line === undefined) return;
  // Enforce defect identity at INPUT: a "new" finding within ±3 rows of an
  // existing entry is an edit of that entry, not an addition. Catching it
  // here keeps the human's intent aligned with what the scorer will match.
  const chk = await api(`/api/check/${PR.target}`, {file: path, line});
  if (chk.collision &&
      !confirm(`${chk.message}\n\nStage an "add" here anyway?`)) return;

  const topic = prompt(`New finding at ${path}:${line}\nOne-line topic:`, '');
  if (!topic) return;
  const severity = prompt('Severity (high, medium, low, nit)', 'medium');
  if (!['high', 'medium', 'low', 'nit'].includes(severity || '')) {
    alert('severity must be one of: high, medium, low, nit');
    return;
  }
  const reason = prompt('Why is this a real defect?', '') ?? '';
  const res = await api(`/api/verdict/${PR.target}`,
    {op: 'add', file: path, line, topic, severity, reason});
  if (res.error) { alert(res.error); return; }
  PR = await api(`/api/pr/${PR.target}`);
  drawLedger();
}

/* ------------------------------------------------------------------ route */

function route() {
  const hash = location.hash || '#/';
  const m = hash.match(/^#\/pr\/([^/]+)(?:\/(\d+))?$/);
  if (m) {
    renderPR(decodeURIComponent(m[1]),
             m[2] === undefined ? undefined : parseInt(m[2], 10));
  } else {
    renderList();
  }
}

window.addEventListener('hashchange', route);
route();
