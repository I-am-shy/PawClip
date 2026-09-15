#!/usr/bin/env node
/**
 * PawClip 图标资源生成脚本 (v2 — 栅格源图版)
 *
 * 输入：assets/icon/pawclip-source.png   成品图标稿（822×782，不带 alpha）
 * 输出：assets/icon/dist/
 *         pawclip-master.png               1024 主图（Apple 栅格）
 *         pawclip-{16..1024}.png           全尺寸 PNG
 *         pawclip.icns / pawclip.ico       macOS / Windows
 *         tray/                            托盘图（由主图派生单色模板版）
 *         preview-icon.png                 尺寸阶梯 + 明暗底对比
 *         preview-dock.png                 Dock 相对尺寸模拟
 *         preview-tray.png                 托盘图明暗栏效果预览
 *         preview-mask-check.png           遮罩校验（四角放大，紫底穿透检测）
 *
 * 依赖：sharp（裁剪 / 缩放 / 遮罩 / SVG 光栅化）+ macOS 自带 iconutil
 * 运行：NODE_PATH=<node_modules> node scripts/build-icons.cjs
 *
 * ─────────────────────────────────────────────────────────────────────────
 * 源图的关键性质（决定了实现方式，换图后必须重跑 scripts/probe-icon-source.cjs）：
 *
 *   1. 没有 alpha 通道，整图不透明；
 *   2. 圆角方块之外不是纯白，而是一圈柔和的环境阴影
 *      （左上角灰度 250–252，右下角低至 212）——阴影比方块填充（244–250）更暗，
 *      两者灰度区间重叠，因此任何"按灰度阈值抠背景"或"洪泛填充"的方案都必然失败；
 *   3. 唯一可靠的手段是按几何形状生成圆角矩形遮罩。
 *      实测方块圆角半径 ≈129.6px（占宽度 17.6%），
 *      遮罩半径必须 ≥ 该值，否则四角会漏出背景或阴影。
 * ─────────────────────────────────────────────────────────────────────────
 */

const fs = require('fs');
const path = require('path');
const os = require('os');
const { execFileSync } = require('child_process');
const sharp = require('sharp');

const ROOT = path.resolve(__dirname, '..');
const ICON_DIR = path.join(ROOT, 'assets', 'icon');
const DIST = path.join(ICON_DIR, 'dist');
const TRAY_DIST = path.join(DIST, 'tray');

const SRC = path.join(ICON_DIR, 'pawclip-source.png');

// ── 源图几何（probe-icon-source.cjs 实测）─────────────────────────────────
// 圆角方块外接框：左 49 / 上 16，宽 737 / 高 744
const CROP = { left: 49, top: 16, width: 737, height: 744 };
// 遮罩圆角半径占方块宽度的比例。源图实测 17.6%，这里留 1.4% 余量，
// 保证遮罩严格落在方块内部，彻底切净四角的背景与阴影。
const MASK_RADIUS_RATIO = 0.19;

// ── 输出规格 ────────────────────────────────────────────────────────────
const APP_MASTER = 1024; // .icns 最大边长
const APP_FILL = 824; // Apple macOS 图标栅格：圆角方块占 1024 画布的 824（80.47%）
const APP_SIZES = [16, 24, 32, 48, 64, 128, 256, 512, 1024];
const ICO_SIZES = [16, 24, 32, 48, 64, 128, 256];
// 托盘图导出尺寸：16pt 逻辑尺寸对应 @1x/@2x/@3x/@4x
const TRAY_SIZES = [16, 32, 48, 64];
const TRAY_PREVIEW_SIZES = [16, 32, 64]; // 预览只列三档，避免过密

// 托盘单色模板图的灰度映射（浅底→透明，深线稿→不透明）
const TRAY_BG_LUM = 242;
const TRAY_LINE_LUM = 80;
const TRAY_BOOST = 1.7; // 让绿色对勾（灰度≈130）也达到全不透明

const ensureDir = (d) => fs.mkdirSync(d, { recursive: true });
const luminance = (r, g, b) => 0.2126 * r + 0.7152 * g + 0.0722 * b;

const roundedRectSvg = (w, h, r, fill = '#fff') =>
  `<svg xmlns="http://www.w3.org/2000/svg" width="${w}" height="${h}">` +
  `<rect x="0" y="0" width="${w}" height="${h}" rx="${r}" ry="${r}" fill="${fill}"/></svg>`;

const pngAt = (src, size) => sharp(src).resize(size, size, { kernel: 'lanczos3' }).png().toBuffer();

/** 手写 ICO 封装：Vista 起 ICO 可直接内嵌 PNG，无需 BMP 转换 */
function writeIco(entries, outFile) {
  const header = Buffer.alloc(6);
  header.writeUInt16LE(0, 0);
  header.writeUInt16LE(1, 2);
  header.writeUInt16LE(entries.length, 4);

  const dir = Buffer.alloc(16 * entries.length);
  let offset = 6 + 16 * entries.length;
  entries.forEach((entry, i) => {
    const b = 16 * i;
    const dim = entry.size >= 256 ? 0 : entry.size;
    dir.writeUInt8(dim, b + 0);
    dir.writeUInt8(dim, b + 1);
    dir.writeUInt8(0, b + 2);
    dir.writeUInt8(0, b + 3);
    dir.writeUInt16LE(1, b + 4);
    dir.writeUInt16LE(32, b + 6);
    dir.writeUInt32LE(entry.data.length, b + 8);
    dir.writeUInt32LE(offset, b + 12);
    offset += entry.data.length;
  });

  fs.writeFileSync(outFile, Buffer.concat([header, dir, ...entries.map((e) => e.data)]));
}

/**
 * 抠出圆角方块 → 824×824 方形 PNG（带 alpha）。
 * 裁剪方块外接框 → 缩放到 Apple 栅格尺寸 → 套几何圆角遮罩（dest-in）。
 */
async function extractSquircle() {
  const { width, height } = CROP;
  const r = Math.round(APP_FILL * MASK_RADIUS_RATIO);

  const maskPng = await sharp(Buffer.from(roundedRectSvg(APP_FILL, APP_FILL, r)))
    .png()
    .toBuffer();

  const squircle = await sharp(SRC)
    .extract(CROP)
    .resize(APP_FILL, APP_FILL, { kernel: 'lanczos3' })
    .composite([{ input: maskPng, blend: 'dest-in' }])
    .png()
    .toBuffer();

  console.log(
    `  抠图：裁剪 ${width}×${height} → 缩放 ${APP_FILL}² → 圆角遮罩 r=${r}px (${(
      MASK_RADIUS_RATIO * 100
    ).toFixed(1)}%)`
  );
  return squircle;
}

/** 把 824 方块放到 1024 透明画布中央，得到 App 图标主图 */
async function buildMaster(squirclePng) {
  const offset = Math.round((APP_MASTER - APP_FILL) / 2);
  return sharp({
    create: {
      width: APP_MASTER,
      height: APP_MASTER,
      channels: 4,
      background: { r: 0, g: 0, b: 0, alpha: 0 },
    },
  })
    .composite([{ input: squirclePng, top: offset, left: offset }])
    .png()
    .toBuffer();
}

/** 由方块图推导单色托盘模板图（黑 + alpha），用于 macOS 菜单栏 / Windows 通知区 */
async function buildTrayFromArt(squirclePng) {
  const { data, info } = await sharp(squirclePng).ensureAlpha().raw().toBuffer({ resolveWithObject: true });
  const { width: W, height: H } = info;
  const out = Buffer.alloc(W * H * 4);
  const span = TRAY_BG_LUM - TRAY_LINE_LUM;
  for (let i = 0; i < W * H; i++) {
    const l = luminance(data[i * 4], data[i * 4 + 1], data[i * 4 + 2]);
    let a = (TRAY_BG_LUM - l) / span;
    a = Math.min(1, Math.max(0, a) * TRAY_BOOST);
    a *= data[i * 4 + 3] / 255; // 保留方块外缘抗锯齿
    out[i * 4] = 0;
    out[i * 4 + 1] = 0;
    out[i * 4 + 2] = 0;
    out[i * 4 + 3] = Math.round(a * 255);
  }
  return sharp(out, { raw: { width: W, height: H, channels: 4 } }).png().toBuffer();
}

/**
 * 把黑色模板图翻成白色版本，用于模拟 macOS 在深色菜单栏下的自动反转。
 * 模板图只由 alpha 承载形状，系统会按菜单栏明暗重新着色，因此预览必须跟着反转，
 * 否则会误判成"深色栏下看不见"。
 */
async function whitenTemplate(png) {
  const { data, info } = await sharp(png).ensureAlpha().raw().toBuffer({ resolveWithObject: true });
  const out = Buffer.alloc(data.length);
  for (let i = 0; i < data.length; i += 4) {
    out[i] = 255;
    out[i + 1] = 255;
    out[i + 2] = 255;
    out[i + 3] = data[i + 3];
  }
  return sharp(out, { raw: { width: info.width, height: info.height, channels: 4 } }).png().toBuffer();
}

/** 遮罩校验：把方块叠在品红底上，四角放大——任何紫底穿透都说明遮罩没切干净 */
async function buildMaskCheck(squirclePng) {
  const ZOOM = 2;
  const PANEL = 240;
  const corners = [
    { name: 'TL', left: 0, top: 0 },
    { name: 'TR', left: APP_FILL - PANEL / ZOOM, top: 0 },
    { name: 'BL', left: 0, top: APP_FILL - PANEL / ZOOM },
    { name: 'BR', left: APP_FILL - PANEL / ZOOM, top: APP_FILL - PANEL / ZOOM },
  ];
  const panels = [];
  for (const c of corners) {
    const patch = await sharp(squirclePng)
      .extract({ left: Math.round(c.left), top: Math.round(c.top), width: PANEL / ZOOM, height: PANEL / ZOOM })
      .resize(PANEL, PANEL, { kernel: 'nearest' })
      .png()
      .toBuffer();
    panels.push({ name: c.name, buf: patch });
  }
  const gap = 16;
  const W = PANEL * 4 + gap * 5;
  const H = PANEL + gap * 2 + 24;
  const comps = [
    {
      input: Buffer.from(
        `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}">
           <rect width="${W}" height="${H}" fill="#FF00FF"/>
           <text x="${gap}" y="${PANEL + gap * 2 + 8}" font-family="Helvetica" font-size="14" fill="#FFFFFF">
             遮罩校验 · 四角放大 ${ZOOM}x · 品红=穿透区域（应完全不可见）
           </text>
         </svg>`
      ),
      top: 0,
      left: 0,
    },
  ];
  panels.forEach((p, i) => {
    comps.push({ input: p.buf, top: gap, left: gap + i * (PANEL + gap) });
  });
  fs.writeFileSync(
    path.join(DIST, 'preview-mask-check.png'),
    await sharp({ create: { width: W, height: H, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
      .composite(comps)
      .png()
      .toBuffer()
  );
}

/** 生成三张观感预览图 */
async function buildPreviews(master, trayPng) {
  // A. 尺寸阶梯：明 / 暗两条横带，各含一枚大图 + 128→16 递减阶梯 + 16px 四倍放大
  const BAND = 252;
  const W = 800;
  const H = BAND * 2;
  const HERO = 160;
  const LADDER = [128, 64, 48, 32, 24, 16];
  const LABEL_Y_OFF = 104;

  const bg = `<rect width="${W}" height="${H}" fill="#F4F4F6"/>
    <rect y="${BAND}" width="${W}" height="${BAND}" fill="#1D1D21"/>
    <line x1="0" y1="${BAND}" x2="${W}" y2="${BAND}" stroke="#00000018"/>`;
  const comps = [
    { input: Buffer.from(`<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}">${bg}</svg>`), top: 0, left: 0 },
  ];
  const labels = [
    { x: 30, y: 30, t: 'light background', c: '#8A8F98', s: 13, w: '600' },
    { x: 30, y: BAND + 30, t: 'dark background', c: '#8A8F98', s: 13, w: '600' },
    { x: W - 30, y: 30, t: 'PawClip · pawclip-master 1024', c: '#B0B4BB', s: 12, w: '400', anchor: 'end' },
    { x: W - 30, y: BAND + 30, t: 'pawclip-master 1024', c: '#6E737C', s: 12, w: '400', anchor: 'end' },
  ];

  for (let b = 0; b < 2; b++) {
    const cy = b * BAND + BAND / 2 + 10;
    const labelColor = b === 0 ? '#9AA0A8' : '#6E737C';
    comps.push({ input: await pngAt(master, HERO), top: Math.round(cy - HERO / 2), left: 30 });
    labels.push({ x: 30 + HERO / 2, y: cy + LABEL_Y_OFF, t: '160', c: labelColor, s: 11, w: '400', anchor: 'middle' });

    let x = 240;
    for (const size of LADDER) {
      comps.push({ input: await pngAt(master, size), top: Math.round(cy - size / 2), left: x });
      labels.push({ x: x + size / 2, y: cy + LABEL_Y_OFF, t: String(size), c: labelColor, s: 11, w: '400', anchor: 'middle' });
      x += Math.max(size, 30) + 22;
    }

    // 16px 四倍放大（最近邻，看清像素级辨识度）
    const zx = x + 16;
    const zoom16 = await sharp(master)
      .resize(16, 16, { kernel: 'lanczos3' })
      .resize(64, 64, { kernel: 'nearest' })
      .png()
      .toBuffer();
    comps.push({ input: zoom16, top: Math.round(cy - 32), left: zx });
    labels.push({ x: zx + 32, y: cy + LABEL_Y_OFF, t: '16 ×4', c: labelColor, s: 11, w: '400', anchor: 'middle' });
  }

  const labelSvg = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}">${labels
    .map(
      (l) =>
        `<text x="${l.x}" y="${l.y}" font-family="-apple-system,Helvetica,sans-serif" font-size="${l.s}" font-weight="${l.w}" fill="${l.c}" text-anchor="${l.anchor || 'start'}">${l.t}</text>`
    )
    .join('')}</svg>`;
  comps.push({ input: Buffer.from(labelSvg), top: 0, left: 0 });

  fs.writeFileSync(
    path.join(DIST, 'preview-icon.png'),
    await sharp({ create: { width: W, height: H, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
      .composite(comps)
      .png()
      .toBuffer()
  );

  // B. Dock 相对尺寸模拟：本项目图标与"Apple 标准栅格灰块"交替排列
  const dockW = 640;
  const dockH = 200;
  const tile = 128;
  const nativePng = await sharp(
    Buffer.from(
      `<svg xmlns="http://www.w3.org/2000/svg" width="${APP_MASTER}" height="${APP_MASTER}">
         <rect x="100" y="100" width="824" height="824" rx="185" fill="#D4D6DB"/></svg>`
    )
  )
    .resize(tile, tile)
    .png()
    .toBuffer();
  const minePng = await pngAt(master, tile);
  const dockBg = `<rect width="${dockW}" height="${dockH}" fill="#E9E9EC"/>
    <rect x="16" y="16" width="${dockW - 32}" height="76" rx="20" fill="#F7F7F9" stroke="#DCDCE1"/>
    <rect x="16" y="108" width="${dockW - 32}" height="76" rx="20" fill="#2A2A30" stroke="#3A3A42"/>
    <text x="24" y="24" font-family="Helvetica" font-size="11" fill="#9CA3AF">light dock</text>
    <text x="24" y="116" font-family="Helvetica" font-size="11" fill="#6B7280">dark dock</text>`;
  const dockComps = [
    { input: Buffer.from(`<svg xmlns="http://www.w3.org/2000/svg" width="${dockW}" height="${dockH}">${dockBg}</svg>`), top: 0, left: 0 },
  ];
  let dx = 40;
  for (const img of [minePng, nativePng, minePng, nativePng]) {
    dockComps.push({ input: img, top: -10, left: dx });
    dockComps.push({ input: img, top: 82, left: dx });
    dx += tile + 18;
  }
  fs.writeFileSync(
    path.join(DIST, 'preview-dock.png'),
    await sharp({ create: { width: dockW, height: dockH, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
      .composite(dockComps)
      .png()
      .toBuffer()
  );

  // C. 托盘图效果预览：明 / 暗两条菜单栏，各列 16 / 32 / 64 px
  const trayW = 680;
  const ROW_H = 138;
  const HEAD = 46;
  const trayH = HEAD + ROW_H;
  const COL_L = 340; // 浅色菜单栏 / 深色菜单栏分界
  const trayBg = `<rect width="${trayW}" height="${trayH}" fill="#FFFFFF"/>
    <rect x="0" y="${HEAD}" width="${COL_L}" height="${trayH - HEAD}" fill="#F1F1F4"/>
    <rect x="${COL_L}" y="${HEAD}" width="${trayW - COL_L}" height="${trayH - HEAD}" fill="#232329"/>
    <line x1="0" y1="${HEAD}" x2="${trayW}" y2="${HEAD}" stroke="#E2E2E6"/>`;
  const trayComps = [
    { input: Buffer.from(`<svg xmlns="http://www.w3.org/2000/svg" width="${trayW}" height="${trayH}">${trayBg}</svg>`), top: 0, left: 0 },
  ];
  const trayLabels = [
    { x: 20, y: 20, t: '托盘图实际效果', c: '#3C3F45', s: 14, w: '600' },
    { x: 20, y: 38, t: '深色栏为 macOS 模板图自动反转（黑→白）后的实际效果', c: '#9AA0A8', s: 11, w: '400' },
    { x: COL_L / 2, y: 26, t: 'light menubar', c: '#6B7280', s: 12, w: '600', anchor: 'middle' },
    { x: COL_L + (trayW - COL_L) / 2, y: 26, t: 'dark menubar', c: '#9CA3AF', s: 12, w: '600', anchor: 'middle' },
    {
      x: 20,
      y: HEAD + 26,
      t: '单色模板图 · 由主图灰度推导（纯黑 + alpha，颜色交由系统渲染）',
      c: '#6B7280',
      s: 11,
      w: '400',
    },
  ];
  const trayViews = [trayPng, await whitenTemplate(trayPng)]; // 深色栏 = 系统自动反转后的白色版本
  const cy = HEAD + ROW_H / 2 + 6;
  for (let c = 0; c < 2; c++) {
    let tx = (c === 0 ? 0 : COL_L) + 40;
    for (let k = 0; k < TRAY_PREVIEW_SIZES.length; k++) {
      const size = TRAY_PREVIEW_SIZES[k];
      const px = await sharp(trayViews[c]).resize(size, size, { kernel: 'lanczos3' }).png().toBuffer();
      trayComps.push({ input: px, top: Math.round(cy - size / 2), left: tx });
      trayLabels.push({
        x: tx + size / 2,
        y: HEAD + ROW_H - 14,
        t: String(size),
        c: c === 0 ? '#AEB3BA' : '#6E737C',
        s: 10,
        w: '400',
        anchor: 'middle',
      });
      tx += size + 22;
    }
  }
  trayComps.push({
    input: Buffer.from(
      `<svg xmlns="http://www.w3.org/2000/svg" width="${trayW}" height="${trayH}">${trayLabels
        .map(
          (l) =>
            `<text x="${l.x}" y="${l.y}" font-family="-apple-system,Helvetica,sans-serif" font-size="${l.s}" font-weight="${l.w}" fill="${l.c}" text-anchor="${l.anchor || 'start'}">${l.t}</text>`
        )
        .join('')}</svg>`
    ),
    top: 0,
    left: 0,
  });
  fs.writeFileSync(
    path.join(DIST, 'preview-tray.png'),
    await sharp({ create: { width: trayW, height: trayH, channels: 4, background: { r: 0, g: 0, b: 0, alpha: 0 } } })
      .composite(trayComps)
      .png()
      .toBuffer()
  );
}

async function main() {
  ensureDir(DIST);
  ensureDir(TRAY_DIST);
  console.log('PawClip 图标生成');
  console.log('  源图：' + path.relative(ROOT, SRC));

  // 1. 抠出圆角方块 → 主图（1024 画布 + 824 方块）
  const squircle = await extractSquircle();
  const master = await buildMaster(squircle);
  fs.writeFileSync(path.join(DIST, 'pawclip-master.png'), master);
  fs.writeFileSync(path.join(DIST, 'pawclip-squircle.png'), squircle);

  // 2. PNG 全尺寸集
  const pngReport = [];
  for (const size of APP_SIZES) {
    const out = path.join(DIST, `pawclip-${size}.png`);
    fs.writeFileSync(out, await pngAt(master, size));
    pngReport.push({ file: path.basename(out), bytes: fs.statSync(out).size });
  }

  // 3. macOS .icns
  const iconset = path.join(os.tmpdir(), `pawclip-${Date.now()}.iconset`);
  ensureDir(iconset);
  const iconsetMap = [
    ['icon_16x16.png', 16],
    ['icon_16x16@2x.png', 32],
    ['icon_32x32.png', 32],
    ['icon_32x32@2x.png', 64],
    ['icon_128x128.png', 128],
    ['icon_128x128@2x.png', 256],
    ['icon_256x256.png', 256],
    ['icon_256x256@2x.png', 512],
    ['icon_512x512.png', 512],
    ['icon_512x512@2x.png', 1024],
  ];
  for (const [name, size] of iconsetMap) {
    fs.writeFileSync(path.join(iconset, name), await pngAt(master, size));
  }
  execFileSync('iconutil', ['-c', 'icns', iconset, '-o', path.join(DIST, 'pawclip.icns')]);
  fs.rmSync(iconset, { recursive: true, force: true });

  // 4. Windows .ico
  const icoEntries = [];
  for (const size of ICO_SIZES) icoEntries.push({ size, data: await pngAt(master, size) });
  writeIco(icoEntries, path.join(DIST, 'pawclip.ico'));

  // 5. 托盘图：由主图派生单色模板版
  const derivedTray = await buildTrayFromArt(squircle);
  for (const size of TRAY_SIZES) {
    const suffix = size === 16 ? '' : `@${size / 16}x`;
    fs.writeFileSync(
      path.join(TRAY_DIST, `trayTemplate${suffix}.png`),
      await sharp(derivedTray).resize(size, size, { kernel: 'lanczos3' }).png().toBuffer()
    );
  }
  const trayEntries = [];
  for (const size of TRAY_SIZES) {
    trayEntries.push({ size, data: await sharp(derivedTray).resize(size, size, { kernel: 'lanczos3' }).png().toBuffer() });
  }
  writeIco(trayEntries, path.join(TRAY_DIST, 'tray.ico'));

  // 6. 预览与校验图
  await buildMaskCheck(squircle);
  await buildPreviews(master, derivedTray);

  // 7. 汇总
  const files = [
    'pawclip.icns',
    'pawclip.ico',
    'pawclip-master.png',
    'preview-mask-check.png',
    'preview-icon.png',
    'preview-dock.png',
    'preview-tray.png',
    ...pngReport.map((p) => p.file),
    'tray/tray.ico',
    'tray/trayTemplate.png',
  ];
  console.log('\n生成完成 →', path.relative(ROOT, DIST));
  for (const f of files) {
    const p = path.join(DIST, f);
    if (fs.existsSync(p)) console.log('  ' + f.padEnd(26) + String(fs.statSync(p).size).padStart(9) + ' B');
  }
}

main().catch((e) => {
  console.error('生成失败：', e);
  process.exit(1);
});
