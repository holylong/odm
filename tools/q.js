// Query helper: reads re/queries.json and greps re/idman.ascii.txt + re/idman.utf16.txt.
// Avoids shell quoting problems entirely.
const fs = require('fs');
const path = require('path');

const base = process.argv[2] || 're';
const qfile = process.argv[3] || 'queries.json';
const queries = JSON.parse(fs.readFileSync(path.join(base, qfile), 'utf8'));
const files = ['idman.ascii.txt', 'idman.utf16.txt']
  .map((f) => path.join(base, f))
  .filter((f) => fs.existsSync(f));

const corpora = files.map((f) => ({ file: path.basename(f), lines: fs.readFileSync(f, 'utf8').split('\n') }));

for (const q of queries) {
  const re = new RegExp(q.re, q.flags || 'i');
  const max = q.max || 30;
  let hits = 0;
  console.log(`\n### ${q.name}  /${q.re}/${q.flags || 'i'}`);
  for (const c of corpora) {
    for (let i = 0; i < c.lines.length; i++) {
      const line = c.lines[i];
      if (!line || line.length > 400) continue;
      if (!re.test(line)) continue;
      hits++;
      if (hits <= max) console.log(`  [${c.file}:${i + 1}] ${line.trim()}`);
      if (hits > max) break;
    }
    if (hits > max) break;
  }
  console.log(`  -> total hits ${hits}${hits > max ? ` (showing ${max})` : ''}`);
}
