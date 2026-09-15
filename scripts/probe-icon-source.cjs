const sharp = require('sharp');
const SRC = process.argv[2];
(async () => {
  const { data, info } = await sharp(SRC).ensureAlpha().raw().toBuffer({ resolveWithObject: true });
  const { width: W, height: H, channels: C } = info;
  const lum = (x, y) => { const i = (y * W + x) * C; return 0.2126 * data[i] + 0.7152 * data[i + 1] + 0.0722 * data[i + 2]; };

  const prof = (label, fn, a, b) => {
    const out = [];
    for (let n = a; n <= b; n++) out.push(Math.round(fn(n)));
    console.log(label.padEnd(26) + out.map((v) => String(v).padStart(4)).join(''));
    console.log(' '.repeat(26) + `(n = ${a} .. ${b})`);
  };

  console.log('############ 原始灰度剖面 ############');
  for (const yOff of [-150, 0, 150]) {
    const y = Math.round(H / 2) + yOff;
    console.log(`\n--- 横切 y=${y} ---`);
    prof('  left  x=40..66', (x) => lum(x, y), 40, 66);
    prof('  right x=772..798', (x) => lum(x, y), 772, 798);
  }
  for (const xOff of [-150, 0, 150]) {
    const x = Math.round(W / 2) + xOff;
    console.log(`\n--- 纵切 x=${x} ---`);
    prof('  top   y=8..32', (y) => lum(x, y), 8, 32);
    prof('  bot   y=746..772', (y) => lum(x, y), 746, 772);
  }

  // 自动定边: 受限窗口内找最大负跳变
  function edgeAt(fixed, isRow, lo, hi) {
    const get = (n) => (isRow ? lum(n, fixed) : lum(fixed, n));
    let best = { n: null, g: 0 };
    for (let n = lo + 1; n <= hi; n++) {
      const g = get(n) - get(n - 1);
      if (g < best.g) best = { n: n - 0.5, g };
    }
    return best;
  }
  console.log('\n############ 四边定位（受限窗口 + 最大负跳变）############');
  const rows = [], cols = [];
  for (let y = 120; y <= H - 120; y += 15) rows.push(y);
  for (let x = 120; x <= W - 120; x += 15) cols.push(x);
  const pick = (arr) => { const s = arr.slice().sort((a, b) => a - b); return s[s.length >> 1]; };
  const Ls = rows.map((y) => edgeAt(y, true, 20, 120).n);
  const Rs = rows.map((y) => edgeAt(y, true, 700, W - 1).n);
  const Ts = cols.map((x) => edgeAt(x, false, 4, 90).n);
  const Bs = cols.map((x) => edgeAt(x, false, 700, 780).n);
  const L = pick(Ls), R = pick(Rs), T = pick(Ts), B = pick(Bs);
  console.log('  L 中位=' + L.toFixed(1) + '  n=' + Ls.length + '  范围[' + Math.min(...Ls) + ',' + Math.max(...Ls) + ']');
  console.log('  R 中位=' + R.toFixed(1) + '  n=' + Rs.length + '  范围[' + Math.min(...Rs) + ',' + Math.max(...Rs) + ']');
  console.log('  T 中位=' + T.toFixed(1) + '  n=' + Ts.length + '  范围[' + Math.min(...Ts) + ',' + Math.max(...Ts) + ']');
  console.log('  B 中位=' + B.toFixed(1) + '  n=' + Bs.length + '  范围[' + Math.min(...Bs) + ',' + Math.max(...Bs) + ']');
  const w = R - L, h = B - T;
  console.log('\n  方块尺寸 = ' + w.toFixed(1) + ' x ' + h.toFixed(1) + '   aspect=' + (w / h).toFixed(4));
  console.log('  四边留白: 左=' + L.toFixed(1) + ' 右=' + (W - R).toFixed(1) + ' 上=' + T.toFixed(1) + ' 下=' + (H - B).toFixed(1));
  console.log('  (右/下剪影被裁切 => 阴影外溢，取 L/T 作为可信基准)');
})();
