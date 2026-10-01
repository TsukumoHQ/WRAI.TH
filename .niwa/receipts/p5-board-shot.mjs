// node shot.mjs <url> <png> — headless Chrome via CDP: screenshot + per-column DOM dump.
import { spawn } from 'node:child_process';
import { writeFileSync } from 'node:fs';
const [url, png] = process.argv.slice(2);
const port = 9333 + Math.floor(Math.random() * 500);
const chrome = spawn('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  ['--headless=new', '--disable-gpu', '--no-first-run', `--remote-debugging-port=${port}`,
   `--user-data-dir=${png}.profile`, '--window-size=1500,820', '--hide-scrollbars', 'about:blank'], { stdio: 'ignore' });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let targets;
for (let i = 0; i < 50; i++) { try { targets = await (await fetch(`http://127.0.0.1:${port}/json`)).json(); break; } catch { await sleep(200); } }
const ws = new WebSocket(targets.find((t) => t.type === 'page').webSocketDebuggerUrl);
await new Promise((r) => ws.addEventListener('open', r));
let seq = 0; const pending = new Map();
ws.addEventListener('message', (e) => { const m = JSON.parse(e.data); if (pending.has(m.id)) { pending.get(m.id)(m.result); pending.delete(m.id); } });
const send = (method, params = {}) => new Promise((r) => { const id = ++seq; pending.set(id, r); ws.send(JSON.stringify({ id, method, params })); });
await send('Page.enable');
await send('Page.navigate', { url });
await sleep(5000);
// open the backlog rail if it starts collapsed, so its cards are visible
await send('Runtime.evaluate', { expression: `(() => { const r = document.querySelector('section.col-rail.closed .col-head'); if (r) r.click(); })()` });
await sleep(800);
const dump = await send('Runtime.evaluate', { returnByValue: true, expression: `[...document.querySelectorAll('section[data-col]')].map((s) => ({ col: s.dataset.col, count: (s.querySelector('.col-count')||{}).textContent, cards: [...s.querySelectorAll('.kcard')].map((c) => c.innerText.split('\\n').filter((l) => /Groomed|Ready/.test(l))[0] || '') }))` });
console.log(JSON.stringify(dump.result.value, null, 1));
const shot = await send('Page.captureScreenshot', { format: 'png' });
writeFileSync(png, Buffer.from(shot.data, 'base64'));
ws.close(); chrome.kill(); process.exit(0);
