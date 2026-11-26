const fs = require('fs');
const link = 'D:\\WorkStation\\odm\\_target';
const src = 'C:\\Program Files (x86)\\Internet Download Manager';
if (!fs.existsSync(link)) {
  fs.symlinkSync(src, link, 'junction');
  console.log('junction created ->', fs.realpathSync(link));
} else {
  console.log('junction exists ->', fs.realpathSync(link));
}
console.log('IDMan.exe readable =', fs.existsSync(link + '\\IDMan.exe'));
