'use strict';
// restic tile. Helpers (api, esc, $, setStatus, describe, initTheme, safeGet, safeSet) come from common.js.

const API = '/restic';
const open = new Set(); // jobs whose snapshot list is unfolded

function armed(btn) {
  if (btn.dataset.armed) return true;
  btn.dataset.armed = '1';
  const label = btn.textContent;
  btn.textContent = 'Click again';
  setTimeout(() => { delete btn.dataset.armed; btn.textContent = label; }, 4000);
  return false;
}

function when(s) {
  if (!s) return '—';
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? s : d.toLocaleString();
}

function bytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${u[i]}`;
}

// A run is good only with rc 0 AND every path present: restic itself exits 0
// when a path is gone (measured 0.19.1), restic-zima counts those.
function lastHTML(l) {
  if (!l) return '<span class="pill pill-off">never run</span>';
  const s = l.summary || {};
  let pill;
  if (l.rc === 0 && !l.missing_paths) pill = '<span class="pill pill-ok">ok</span>';
  else if (l.rc === 0 || l.rc === 3) pill = '<span class="pill pill-warn">warning</span>';
  else pill = `<span class="pill pill-danger">failed (exit ${esc(l.rc)})</span>`;
  const parts = [`${pill} ${esc(when(l.end))}`];
  if (l.summary) parts.push(`${esc(s.files_new)} new, ${esc(s.files_changed)} changed, ${esc(bytes(s.data_added_packed))} added`);
  if (l.missing_paths) parts.push(`<span class="app-err">${esc(l.missing_paths)} folder(s) missing, first: <span class="mono">${esc(l.first_missing)}</span></span>`);
  if (l.rc !== 0 && l.last_message) parts.push(`<span class="app-err">${esc(l.last_message)}</span>`);
  return parts.join(' · ');
}

function restoreHTML(r) {
  if (!r) return '';
  const pill = r.rc === 0 ? '<span class="pill pill-ok">restored</span>' : `<span class="pill pill-danger">failed (exit ${esc(r.rc)})</span>`;
  return `<dt>Last restore</dt><dd>${pill} ${esc(r.snapshot.slice(0, 8))} → <span class="mono">${esc(r.target)}</span> ${esc(when(r.end))}${r.rc !== 0 && r.last_message ? ` <span class="app-err">${esc(r.last_message)}</span>` : ''}</dd>`;
}

function jobHTML(j) {
  const running = j.state === 'activating' || j.state === 'active';
  const state = running ? '<span class="pill pill-accent">backing up …</span>' : '';
  const sched = j.timer ? `<span class="pill pill-accent">daily ${esc(j.schedule)}</span>` : '<span class="pill pill-off">by hand</span>';
  return `
  <section class="card" data-job="${esc(j.name)}">
    <div class="job-head">
      <h3 class="mono">${esc(j.name)}</h3>${sched}${state}
      <div class="app-actions">
        <button class="btn-primary" data-act="run" ${running ? 'disabled' : ''}>Back up now</button>
        <button class="btn-secondary" data-act="snaps">${open.has(j.name) ? 'Hide snapshots' : 'Snapshots'}</button>
        <button class="btn-secondary" data-act="password">Show password</button>
        <button class="btn-danger" data-act="delete">Delete job</button>
      </div>
    </div>
    <dl class="kv">
      <dt>Repository</dt><dd class="mono">${esc(j.repository)}</dd>
      <dt>Folders</dt><dd class="mono">${j.paths.map(esc).join('<br>')}</dd>
      ${j.excludes.length ? `<dt>Excluded</dt><dd class="mono">${j.excludes.map(esc).join('<br>')}</dd>` : ''}
      <dt>Last backup</dt><dd>${lastHTML(j.last)}</dd>
      ${j.timer ? `<dt>Next backup</dt><dd>${esc(j.next || '—')}</dd>` : ''}
      ${restoreHTML(j.restore)}
    </dl>
    <form class="schedule row" style="margin-top:10px" autocomplete="off">
      <label class="row" style="font-size:13px">Daily at <input type="time" name="schedule" value="${esc(j.schedule)}"></label>
      <button class="btn-small btn-secondary" type="submit">Save time</button>
      <span class="muted" style="font-size:12px">empty: by hand only</span>
    </form>
    <div class="snaps"></div>
  </section>`;
}

async function loadSnaps(name) {
  const el = document.querySelector(`[data-job="${CSS.escape(name)}"] .snaps`);
  if (!el) return;
  el.innerHTML = '<div class="loading">Reading snapshots&hellip;</div>';
  try {
    const d = await api(`${API}/snapshots?name=${encodeURIComponent(name)}`);
    const s = d.snapshots.slice().reverse();
    el.innerHTML = s.length ? `<div class="table-wrap"><table>
      <thead><tr><th>Snapshot</th><th>Time</th><th>Host</th><th>Folders</th><th></th></tr></thead>
      <tbody>${s.map((x) => `<tr>
        <td class="mono">${esc(x.short_id)}</td><td>${esc(when(x.time))}</td><td>${esc(x.hostname)}</td>
        <td class="mono" style="font-size:11px">${x.paths.map(esc).join('<br>')}</td>
        <td><button class="btn-small btn-secondary" data-act="restore" data-snap="${esc(x.id)}">Restore to new folder</button></td>
      </tr>`).join('')}</tbody></table></div>` : '<p class="empty">No snapshot in this repository yet.</p>';
  } catch (e) {
    el.innerHTML = `<p class="app-err">${esc(describe(e))}</p>`;
  }
}

async function load() {
  const d = await api(`${API}/jobs`);
  $('#base-dir').textContent = d.base_dir;
  $('#restore-dir').textContent = d.restore_dir;
  $('#jobs').innerHTML = d.jobs.length ? d.jobs.map(jobHTML).join('')
    : '<section class="card"><p class="empty">No backup job yet. Create one below.</p></section>';
  for (const n of open) loadSnaps(n);
  // A running backup finishes on its own: look again until it has.
  if (d.jobs.some((j) => j.state === 'activating' || j.state === 'active')) setTimeout(() => load().catch(() => {}), 5000);
}

async function refresh() {
  try {
    await load();
    setStatus('');
  } catch (e) {
    $('#jobs').innerHTML = `<section class="card"><p class="app-err">${esc(describe(e))}</p></section>`;
    setStatus(describe(e), 'err');
  }
}

async function act(label, fn) {
  setStatus(`${label} …`);
  try {
    const r = await fn();
    setStatus(`${label}: done${r && r.note ? ` — ${r.note}` : ''}.`, 'ok');
    await load();
    return r;
  } catch (e) {
    setStatus(`${label}: ${describe(e)}`, 'err');
    return null;
  }
}

function showSecret(name, secret) {
  const node = $('#secret-dialog').content.firstElementChild.cloneNode(true);
  node.querySelector('.job-name').textContent = name;
  node.querySelector('pre').textContent = secret;
  node.addEventListener('click', async (e) => {
    const a = e.target.dataset.act;
    if (a === 'copy') { await navigator.clipboard.writeText(secret).catch(() => {}); e.target.textContent = 'Copied'; }
    if (a === 'close') node.remove();
  });
  document.body.appendChild(node);
}

const lines = (s) => (s || '').split('\n').map((x) => x.trim()).filter(Boolean);

document.addEventListener('click', async (e) => {
  const btn = e.target.closest('button[data-act]');
  if (!btn || btn.closest('.modal')) return;
  const card = btn.closest('[data-job]');
  const name = card && card.dataset.job;
  switch (btn.dataset.act) {
    case 'run':
      await act(`Starting backup ${name}`, async () => { await api(`${API}/jobs/run`, { method: 'POST', body: { name } }); return { note: 'runs in the background' }; });
      break;
    case 'snaps':
      if (open.has(name)) open.delete(name); else open.add(name);
      await load();
      break;
    case 'password': {
      try { const r = await api(`${API}/jobs/password`, { method: 'POST', body: { name } }); showSecret(name, r.password); } catch (err) { setStatus(describe(err), 'err'); }
      break;
    }
    case 'restore':
      if (!armed(btn)) return;
      await act('Starting restore', async () => {
        const r = await api(`${API}/restore`, { method: 'POST', body: { name, snapshot: btn.dataset.snap } });
        return { note: `files go to ${r.target}` };
      });
      break;
    case 'delete':
      if (!armed(btn)) return;
      open.delete(name);
      await act(`Deleting job ${name} (repository untouched, job files kept in jobs/.deleted)`, () => api(`${API}/jobs/delete`, { method: 'POST', body: { name } }));
      break;
    default:
  }
});

document.addEventListener('submit', async (e) => {
  const f = e.target;
  e.preventDefault();
  const v = Object.fromEntries(new FormData(f).entries());
  if (f.id === 'new-job') {
    safeSet('restic.repository', v.repository);
    const r = await act(`Saving ${v.name}`, () => api(`${API}/jobs`, { method: 'POST', body: {
      name: v.name, repository: v.repository.trim(), password: v.password, schedule: v.schedule || '',
      paths: lines(v.paths), excludes: lines(v.excludes), init: !!v.init } }));
    if (r) {
      f.querySelector('input[name=password]').value = '';
      if (r.generated_password) showSecret(v.name, r.generated_password);
    }
    return;
  }
  if (f.classList.contains('schedule')) {
    const name = f.closest('[data-job]').dataset.job;
    await act(`Saving the time of ${name}`, () => api(`${API}/jobs/schedule`, { method: 'POST', body: { name, schedule: v.schedule || '' } }));
  }
});

initTheme('restic.theme');
$('#refresh-btn').addEventListener('click', refresh);
{ const saved = safeGet('restic.repository'); if (saved) $('#new-job input[name=repository]').value = saved; }
refresh();
