const fs = require('fs');
const TARGET = 'C:\\Program Files (x86)\\Internet Download Manager';
console.log('exists =', fs.existsSync(TARGET));
if (!fs.existsSync(TARGET)) process.exit(0);
for (const f of fs.readdirSync(TARGET)) {
  const full = TARGET + '\\' + f;
  let st; try { st = fs.statSync(full); } catch { continue; }
  console.log(st.isDirectory() ? 'DIR ' : 'FILE', String(st.size).padStart(10), f);
}
