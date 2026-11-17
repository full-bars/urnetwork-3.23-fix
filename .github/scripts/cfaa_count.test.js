'use strict';
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { extractConst } = require('./cfaa_count');
assert.equal(extractConst('const example = 0\n', 'example'), 0);
assert.equal(extractConst('const example = 42\n', 'example'), 42);
for (const input of ['', 'const example = nope', 'const example = -1',
  'const example = 12garbage', 'const example = 1\nconst example = 2',
  'const example = 9007199254740992']) {
  assert.throws(() => extractConst(input, 'example'));
}
for (const [file, names] of [
  ['ip_security_cfaa_block.go', ['cfaaBlockedPrefixCount', 'cfaaBlockedPrefix6Count']],
  ['ip_blocker_block.go', ['blockerBlockedHostCount']],
]) {
  const text = fs.readFileSync(file, 'utf8');
  for (const name of names) assert.ok(extractConst(text, name) > 0);
}
console.log('cfaa_count: valid, missing, malformed, duplicate and checked-in constants passed');
