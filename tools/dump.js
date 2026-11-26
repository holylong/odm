// Print a line range from a strings dump.
const fs = require('fs');
const file = process.argv[2];
const from = Number(process.argv[3]);
const to = Number(process.argv[4]);
const lines = fs.readFileSync(file, 'utf8').split('\n');
for (let i = from - 1; i < Math.min(to, lines.length); i++) {
  const l = lines[i];
  if (l === undefined) break;
  console.log(String(i + 1).padStart(6), '|', l);
}
