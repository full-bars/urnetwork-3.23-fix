'use strict';

// Read one decimal Go constant; missing, malformed or duplicate values fail closed.
function extractConst(content, name) {
  const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const lines = content.match(new RegExp('^\\s*const\\s+' + escaped + '\\s*=.*$', 'gm')) || [];
  if (lines.length !== 1) throw new Error(`expected one ${name} constant, found ${lines.length}`);
  const match = lines[0].match(new RegExp('^\\s*const\\s+' + escaped + '\\s*=\\s*(\\d+)\\s*$'));
  if (!match) throw new Error(`invalid ${name} constant`);
  const count = Number(match[1]);
  if (!Number.isSafeInteger(count)) throw new Error(`${name} exceeds safe integer range`);
  return count;
}

module.exports = { extractConst };
