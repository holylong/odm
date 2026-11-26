const fs = require('fs');
const path = process.argv[2];
console.log('path =', path);
console.log('exists =', fs.existsSync(path));
if (!fs.existsSync(path)) process.exit(0);
let total = 0;
for (const f of fs.readdirSync(path)) {
  const full = path + '\\' + f;
  let st;
  try { st = fs.statSync(full); } catch { continue; }
  const kind = st.isDirectory() ? 'DIR ' : 'FILE';
  if (!st.isDirectory()) total += st.size;
  console.log(kind, String(st.size).padStart(10), f);
}
console.log('total bytes =', total);
