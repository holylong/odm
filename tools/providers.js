const { execFileSync } = require('child_process');
let out = '';
try {
  out = execFileSync('rea', ['providers', '--format', 'json'], { encoding: 'utf8', maxBuffer: 200 * 1024 * 1024, shell: true });
} catch (e) {
  out = (e.stdout || '') + (e.stderr || '');
}
let j;
try { j = JSON.parse(out); } catch { console.log('parse failed, head:', out.slice(0, 2000)); process.exit(0); }
const root = j.result || j;
console.log('binding =', JSON.stringify(root.analysis_provider_binding));
const cands = root.analysis_provider_candidates || [];
for (const c of cands) {
  const p = c.provider || {};
  const a = c.availability || {};
  console.log(`- ${p.id} v${p.version || '?'} status=${a.status} code=${a.code} selected=${c.selected}`);
  if (a.reason) console.log('    reason:', String(a.reason).slice(0, 200));
}
