// Minimal ZIP extractor (deflate + stored). Handles CRX by skipping the CRX header.
const fs = require('fs');
const path = require('path');
const zlib = require('zlib');

function findZipStart(buf) {
  if (buf[0] === 0x50 && buf[1] === 0x4b) return 0;
  const i = buf.indexOf(Buffer.from([0x50, 0x4b, 0x03, 0x04]));
  if (i < 0) throw new Error('no zip local header found');
  return i;
}

function extract(zipPath, outDir) {
  let buf = fs.readFileSync(zipPath);
  const start = findZipStart(buf);
  if (start > 0) {
    console.log(`  (CRX/prefix header ${start} bytes skipped)`);
    buf = buf.subarray(start);
  }
  // locate EOCD
  let eocd = -1;
  for (let i = buf.length - 22; i >= 0 && i > buf.length - 70000; i--) {
    if (buf.readUInt32LE(i) === 0x06054b50) { eocd = i; break; }
  }
  if (eocd < 0) throw new Error('EOCD not found');
  const count = buf.readUInt16LE(eocd + 10);
  let off = buf.readUInt32LE(eocd + 16);
  let files = 0;
  for (let n = 0; n < count; n++) {
    if (buf.readUInt32LE(off) !== 0x02014b50) break;
    const method = buf.readUInt16LE(off + 10);
    const compSize = buf.readUInt32LE(off + 20);
    const nameLen = buf.readUInt16LE(off + 28);
    const extraLen = buf.readUInt16LE(off + 30);
    const commentLen = buf.readUInt16LE(off + 32);
    const localOff = buf.readUInt32LE(off + 42);
    const name = buf.subarray(off + 46, off + 46 + nameLen).toString('utf8');
    off += 46 + nameLen + extraLen + commentLen;
    if (name.endsWith('/')) continue;
    const lNameLen = buf.readUInt16LE(localOff + 26);
    const lExtraLen = buf.readUInt16LE(localOff + 28);
    const dataStart = localOff + 30 + lNameLen + lExtraLen;
    const raw = buf.subarray(dataStart, dataStart + compSize);
    let data;
    if (method === 0) data = raw;
    else if (method === 8) data = zlib.inflateRawSync(raw);
    else { console.log(`  ! unsupported method ${method} for ${name}`); continue; }
    const dest = path.join(outDir, name.replace(/\\/g, '/'));
    fs.mkdirSync(path.dirname(dest), { recursive: true });
    fs.writeFileSync(dest, data);
    files++;
  }
  return files;
}

const zipPath = process.argv[2];
const outDir = process.argv[3];
console.log('extracting', zipPath);
const n = extract(zipPath, outDir);
console.log('wrote', n, 'files ->', outDir);
