// 跨语言对拍用 JS runner：在 Node 中直接执行 src/index.js 的核心纯函数
// （processFrame / rewriteDelta / rewriteNonStream），跑完同一批用例后输出 js_out.json。
//
// 用法：node /tmp/compat/run_js.js
// 依赖：/home/gyue/cline2api/go/testdata/compat_cases.json
const fs = require('fs');
const vm = require('vm');
const path = require('path');

// 定位原版 src/index.js：优先环境变量 CLINE2API_JS_INDEX，否则在常见布局里探测
// （原 monorepo 布局 go/testdata -> 根/src；独立仓库需显式指定原版 index.js 路径）。
function locateIndexJs() {
  if (process.env.CLINE2API_JS_INDEX) return process.env.CLINE2API_JS_INDEX;
  const candidates = [
    path.resolve(__dirname, '..', '..', 'src', 'index.js'), // monorepo：go/testdata -> 根/src
    path.resolve(__dirname, '..', 'src', 'index.js'),       // index.js 与 go/ 同级
  ];
  for (const c of candidates) {
    if (fs.existsSync(c)) return c;
  }
  throw new Error(
    'src/index.js not found. ' +
    'Set CLINE2API_JS_INDEX=/path/to/cline2api/src/index.js to run the cross-language compat check.'
  );
}

const INDEX_JS = locateIndexJs();
const casesPath = path.join(__dirname, 'compat_cases.json');
const cases = JSON.parse(fs.readFileSync(casesPath, 'utf8'));

// 把 ESM export default 改写成普通赋值，再用 vm 执行整个文件；
// 顶层 function 声明会成为 context 全局，可直接取用。
let src = fs.readFileSync(INDEX_JS, 'utf8');
src = src.replace('export default', 'this.__default =');

const sandbox = {
  TextEncoder,
  TextDecoder,
  URL,
  Headers,
  fetch,
  console,
  setTimeout,
  clearTimeout,
  AbortController,
  Date,
  Map,
  JSON,
  Math,
};
sandbox.globalThis = sandbox;
vm.createContext(sandbox);
vm.runInContext(src, sandbox, { filename: 'index.js' });

function runDelta(input) {
  let obj;
  try {
    obj = JSON.parse(input);
  } catch (e) {
    return { changed: false, out: input };
  }
  const changed = sandbox.rewriteDelta(obj);
  return { changed, out: changed ? JSON.stringify(obj) : input };
}

function runFrame(input) {
  const out = sandbox.processFrame(input); // string 或 null
  return { out: out === null ? null : out };
}

function runMsg(input) {
  let obj;
  try {
    obj = JSON.parse(input);
  } catch (e) {
    return { ok: false, out: input };
  }
  sandbox.rewriteNonStream(obj);
  return { ok: true, out: JSON.stringify(obj) };
}

const results = [];
for (const c of cases) {
  let result;
  if (c.kind === 'delta') result = runDelta(c.input);
  else if (c.kind === 'frame') result = runFrame(c.input);
  else if (c.kind === 'msg') result = runMsg(c.input);
  results.push({ id: c.id, result });
}

fs.writeFileSync('/tmp/compat/js_out.json', JSON.stringify(results));
console.log(`wrote /tmp/compat/js_out.json (${results.length} cases)`);