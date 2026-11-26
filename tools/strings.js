// Extract ASCII and UTF-16LE strings from a binary. No external deps.
const fs = require('fs');
const src = process.argv[2];
const outAscii = process.argv[3];
const outUtf16 = process.argv[4];
const minLen = Number(process.argv[5] || 5);

const buf = fs.readFileSync(src);
console.error('read', src, buf.length, 'bytes');

// ASCII / latin1 printable runs
const ascii = [];
let cur = [];
for (let i = 0; i < buf.length; i++) {
  const c = buf[i];
  if (c >= 0x20 && c < 0x7f) {
    cur.push(c);
  } else {
    if (cur.length >= minLen) ascii.push(Buffer.from(cur).toString('latin1'));
    cur = [];
  }
}
if (cur.length >= minLen) ascii.push(Buffer.from(cur).toString('latin1'));

// UTF-16LE printable runs
const utf16 = [];
let run = [];
for (let i = 0; i + 1 < buf.length; i += 2) {
  const c = buf[i] | (buf[i + 1] << 8);
  const printable = (c >= 0x20 && c < 0x7f) || (c >= 0xa0 && c <= 0x2fff && c !== 0xfffd);
  if (printable) {
    run.push(c);
  } else {
    if (run.length >= minLen) utf16.push(String.fromCharCode.apply(null, run));
    run = [];
  }
}
if (run.length >= minLen) utf16.push(String.fromCharCode.apply(null, run));

fs.writeFileSync(outAscii, ascii.join('\n'), 'utf8');
fs.writeFileSync(outUtf16, utf16.join('\n'), 'utf8');
console.error('ascii strings:', ascii.length, '->', outAscii);
console.error('utf16 strings:', utf16.length, '->', outUtf16);
