import fs from 'node:fs';
import path from 'node:path';
import ts from '../node_modules/typescript/lib/typescript.js';

const frontendRoot = path.resolve(process.argv[2] ?? '.');
const sourceRoot = path.join(frontendRoot, 'src');
const localeRoots = { en: path.join(sourceRoot, 'locales', 'en'), 'zh-CN': path.join(sourceRoot, 'locales', 'zh-CN') };

function collect(directory, predicate, output = []) {
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const filename = path.join(directory, entry.name);
    if (entry.isDirectory()) collect(filename, predicate, output);
    else if (predicate(filename)) output.push(filename);
  }
  return output;
}

function flatten(value, prefix = '', output = new Map()) {
  for (const [key, child] of Object.entries(value)) {
    const fullKey = prefix ? `${prefix}.${key}` : key;
    if (key.includes('.')) output.set(key, child);
    else if (child && typeof child === 'object' && !Array.isArray(child)) flatten(child, fullKey, output);
    else output.set(fullKey, child);
  }
  return output;
}

function loadCatalog(directory) {
  const catalog = new Map();
  for (const filename of collect(directory, (file) => file.endsWith('.json'))) flatten(JSON.parse(fs.readFileSync(filename, 'utf8')), '', catalog);
  return catalog;
}

function isTranslationCall(node) {
  if (!ts.isCallExpression(node)) return false;
  if (ts.isIdentifier(node.expression)) return node.expression.text === 't';
  return ts.isPropertyAccessExpression(node.expression) && ts.isIdentifier(node.expression.expression) && node.expression.expression.text === 'i18n' && node.expression.name.text === 't';
}

function staticKey(expression) {
  return ts.isStringLiteralLike(expression) || ts.isNoSubstitutionTemplateLiteral(expression) ? expression.text : null;
}

function catalogResolves(catalog, locale, key, hasCount) {
  if (catalog.has(key)) return true;
  return hasCount && locale === 'en' && catalog.has(`${key}_one`) && catalog.has(`${key}_other`);
}

const catalogs = Object.fromEntries(Object.entries(localeRoots).map(([locale, directory]) => [locale, loadCatalog(directory)]));
const calls = [];
const dynamicCalls = [];

for (const filename of collect(sourceRoot, (file) => /\.tsx?$/.test(file) && !file.includes(`${path.sep}locales${path.sep}`))) {
  const sourceText = fs.readFileSync(filename, 'utf8');
  const sourceFile = ts.createSourceFile(filename, sourceText, ts.ScriptTarget.Latest, true, filename.endsWith('.tsx') ? ts.ScriptKind.TSX : ts.ScriptKind.TS);
  function visit(node) {
    if (isTranslationCall(node) && node.arguments.length > 0) {
      const position = sourceFile.getLineAndCharacterOfPosition(node.getStart(sourceFile));
      const location = `${path.relative(process.cwd(), filename)}:${position.line + 1}:${position.character + 1}`;
      const key = staticKey(node.arguments[0]);
      if (key === null) dynamicCalls.push({ location, expression: node.arguments[0].getText(sourceFile) });
      else {
        const options = node.arguments[1];
        const hasCount = Boolean(options && ts.isObjectLiteralExpression(options) && options.properties.some((property) => property.name?.getText(sourceFile) === 'count'));
        calls.push({ location, key, hasCount, hasDefaultValue: Boolean(options && ts.isStringLiteralLike(options)) });
      }
    }
    ts.forEachChild(node, visit);
  }
  visit(sourceFile);
}

const missing = [];
for (const call of calls) {
  if (call.hasDefaultValue) continue;
  for (const [locale, catalog] of Object.entries(catalogs)) if (!catalogResolves(catalog, locale, call.key, call.hasCount)) missing.push({ ...call, locale });
}

console.log(`Static translation calls: ${calls.length}`);
console.log(`Dynamic translation calls: ${dynamicCalls.length}`);
console.log(`Missing static translations: ${missing.length}`);
for (const finding of missing) console.log(`${finding.location} [${finding.locale}] ${finding.key}`);
if (process.argv.includes('--show-dynamic')) {
  console.log('\nDynamic calls requiring family review:');
  for (const finding of dynamicCalls) console.log(`${finding.location} ${finding.expression}`);
}
process.exitCode = missing.length === 0 ? 0 : 1;
