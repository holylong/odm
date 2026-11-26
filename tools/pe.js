// Parse PE headers + import table + section table. No dependencies.
const fs = require('fs');
const file = process.argv[2];
const buf = fs.readFileSync(file);

const e_lfanew = buf.readUInt32LE(0x3c);
if (buf.readUInt32LE(e_lfanew) !== 0x00004550) throw new Error('not PE');
const coff = e_lfanew + 4;
const machine = buf.readUInt16LE(coff);
const numSections = buf.readUInt16LE(coff + 2);
const timeDate = buf.readUInt32LE(coff + 4);
const optSize = buf.readUInt16LE(coff + 16);
const characteristics = buf.readUInt16LE(coff + 18);
const opt = coff + 20;
const magic = buf.readUInt16LE(opt);
const pe32plus = magic === 0x20b;

console.log('machine        =', '0x' + machine.toString(16), machine === 0x14c ? '(x86)' : machine === 0x8664 ? '(x64)' : '');
console.log('timestamp      =', new Date(timeDate * 1000).toISOString(), `(${timeDate})`);
console.log('sections       =', numSections);
console.log('characteristics= 0x' + characteristics.toString(16));
console.log('magic          =', pe32plus ? 'PE32+' : 'PE32');
const imageBase = pe32plus ? Number(buf.readBigUInt64LE(opt + 24)) : buf.readUInt32LE(opt + 28);
console.log('imageBase      = 0x' + imageBase.toString(16));
console.log('entryPoint RVA = 0x' + buf.readUInt32LE(opt + 16).toString(16));
console.log('subsystem      =', buf.readUInt16LE(opt + 68), '(2=GUI 3=CUI)');
const dllChars = buf.readUInt16LE(opt + 70);
console.log('dllChars       = 0x' + dllChars.toString(16), (dllChars & 0x20) ? 'DYNAMIC_BASE' : '', (dllChars & 0x40) ? 'FORCE_INTEGRITY' : '', (dllChars & 0x100) ? 'NX_COMPAT' : '', (dllChars & 0x400) ? 'NO_SEH' : '', (dllChars & 0x8000) ? 'TERMINAL_SERVER_AWARE' : '');

const secStart = opt + optSize;
const sections = [];
console.log('\n-- sections --');
for (let i = 0; i < numSections; i++) {
  const s = secStart + i * 40;
  const name = buf.subarray(s, s + 8).toString('latin1').replace(/\0+$/, '');
  const vsize = buf.readUInt32LE(s + 8);
  const vaddr = buf.readUInt32LE(s + 12);
  const rawsize = buf.readUInt32LE(s + 16);
  const rawptr = buf.readUInt32LE(s + 20);
  const chars = buf.readUInt32LE(s + 36);
  sections.push({ name, vaddr, vsize, rawptr, rawsize, chars });
  console.log(
    `  ${name.padEnd(10)} vaddr=0x${vaddr.toString(16).padStart(8, '0')} vsize=0x${vsize.toString(16).padStart(8, '0')} raw=0x${rawsize.toString(16).padStart(8, '0')} ptr=0x${rawptr.toString(16).padStart(8, '0')} ${chars & 0x20000000 ? 'X' : ' '}${chars & 0x40000000 ? 'R' : ' '}${chars & 0x80000000 ? 'W' : ' '}`
  );
}

function rvaToOff(rva) {
  for (const s of sections) {
    if (rva >= s.vaddr && rva < s.vaddr + Math.max(s.vsize, s.rawsize)) return s.rawptr + (rva - s.vaddr);
  }
  return -1;
}

// data directories
const ddOff = opt + (pe32plus ? 112 : 96);
const dirNames = ['export', 'import', 'resource', 'exception', 'security', 'reloc', 'debug', 'arch', 'globalptr', 'tls', 'loadcfg', 'boundimport', 'iat', 'delayimport', 'clr', 'reserved'];
console.log('\n-- data directories --');
const dirs = {};
for (let i = 0; i < 16; i++) {
  const va = buf.readUInt32LE(ddOff + i * 8);
  const sz = buf.readUInt32LE(ddOff + i * 8 + 4);
  if (va || sz) { dirs[dirNames[i]] = { va, sz }; console.log(`  ${dirNames[i].padEnd(14)} rva=0x${va.toString(16)} size=${sz}`); }
}

// imports
if (dirs.import) {
  console.log('\n-- imports --');
  let o = rvaToOff(dirs.import.va);
  while (o > 0) {
    const origFirstThunk = buf.readUInt32LE(o);
    const nameRva = buf.readUInt32LE(o + 12);
    const firstThunk = buf.readUInt32LE(o + 16);
    if (!nameRva) break;
    const nameOff = rvaToOff(nameRva);
    const dll = buf.subarray(nameOff, buf.indexOf(0, nameOff)).toString('latin1');
    const funcs = [];
    let tOff = rvaToOff(origFirstThunk || firstThunk);
    if (tOff > 0) {
      for (let k = 0; ; k++) {
        const step = pe32plus ? 8 : 4;
        const val = pe32plus ? Number(buf.readBigUInt64LE(tOff + k * step)) : buf.readUInt32LE(tOff + k * step);
        if (val === 0) break;
        const ordFlag = pe32plus ? (val & (1n << 63n)) !== 0n : (val & 0x80000000) !== 0;
        if (ordFlag) { funcs.push('#' + (val & 0xffff)); continue; }
        const hintOff = rvaToOff(Number(val) & 0x7fffffff);
        if (hintOff < 0) { funcs.push('<?>'); continue; }
        funcs.push(buf.subarray(hintOff + 2, buf.indexOf(0, hintOff + 2)).toString('latin1'));
        if (funcs.length > 5000) break;
      }
    }
    console.log(`\n  ${dll}  (${funcs.length} functions)`);
    console.log('    ' + funcs.join(', '));
    o += 20;
  }
}
