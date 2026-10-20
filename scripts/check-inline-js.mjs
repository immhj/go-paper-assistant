// Syntax-checks every inline <script> in web/**/*.html without executing it.
// The pages are single files with no build step, so a syntax error would only show up in a
// browser. Usage: node scripts/check-inline-js.mjs
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
import { Script } from 'node:vm';

const root = join(import.meta.dirname, '..');

function htmlFiles(directory) {
  return readdirSync(directory).flatMap((name) => {
    const path = join(directory, name);
    if (statSync(path).isDirectory()) return htmlFiles(path);
    return name.endsWith('.html') ? [path] : [];
  });
}

const files = htmlFiles(join(root, 'web'));
let scripts = 0;
let failures = 0;
for (const file of files) {
  const html = readFileSync(file, 'utf8');
  for (const match of html.matchAll(/<script(?![^>]*\bsrc=)[^>]*>([\s\S]*?)<\/script>/gi)) {
    scripts++;
    const line = html.slice(0, match.index).split('\n').length;
    try {
      new Script(match[1], { filename: `${relative(root, file)} (script at line ${line})` });
    } catch (error) {
      failures++;
      console.error(`${relative(root, file)}:${line}: ${error.message}`);
    }
  }
}
if (scripts === 0) {
  console.error('no inline scripts found under web/; the check is misconfigured');
  process.exit(1);
}
console.log(`checked ${scripts} inline script(s) in ${files.length} file(s)`);
process.exit(failures ? 1 : 0);
