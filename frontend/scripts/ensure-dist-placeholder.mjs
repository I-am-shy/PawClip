// 把 scripts/dist-placeholder.txt 复制成 dist/gitkeep。
//
// 为什么需要这一步：
//   1. main.go 用 `//go:embed all:frontend/dist`，dist 目录不存在就直接编译
//      失败——所以仓库里必须常驻一个占位文件；
//   2. 但 Vite 的 build.emptyOutDir 默认会在每次构建前清空 dist，
//      `vite build` 跑完占位文件就没了。
// 结果是"每构建一次，git 工作区就多一条 frontend/dist/gitkeep 删除记录"。
// 万一有人顺手把它一起提交了，全新 clone 的 `go build ./...` 就会挂。
//
// 所以在 vite build 之后无条件补写一次，让构建后工作区保持干净。
// 内容以 scripts/dist-placeholder.txt 为唯一来源，避免两份文案漂移。
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const src = join(here, "dist-placeholder.txt");
// dist 与 scripts 同级，都在 frontend/ 下。
const dist = join(here, "..", "dist");
const dst = join(dist, "gitkeep");

mkdirSync(dist, { recursive: true });

const want = readFileSync(src, "utf8");
let needWrite = true;
try {
  // 已经一致就别动它，避免无谓地改 mtime（也就不会触发多余的重新编译）。
  needWrite = readFileSync(dst, "utf8") !== want;
} catch {
  needWrite = true;
}

if (needWrite) {
  copyFileSync(src, dst);
  console.log("ensure-dist-placeholder: 已补写 frontend/dist/gitkeep");
} else {
  console.log("ensure-dist-placeholder: frontend/dist/gitkeep 已是最新");
}

// 兜底：万一 copyFileSync 之后文件仍不可读，直接写一遍，宁多不漏。
try {
  readFileSync(dst, "utf8");
} catch (err) {
  writeFileSync(dst, want);
  console.log("ensure-dist-placeholder: copy 后仍不可读，已直接写入");
}
