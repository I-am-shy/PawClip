import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// M1 只要求"能构建通过"，所以这里刻意保持最小：
// 没有别名、没有代理、没有 CSS 预处理器。
//
// 与 docs/DESIGN.md §14 D 相关的两项已经就位：
//   - assetsInlineLimit 调高（§14 第 13 条）：小图标/SVG 内联进 JS，少几个请求；
//   - 不引入 UI 组件库 / router / 全局状态库（§14 第 12 条）。
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // 8 KB 以下的资源直接内联成 data URL
    assetsInlineLimit: 8192,
    target: 'es2020',
    // 目标 gzip < 150 KB（§14 第 14 条）。超过就告警，别等发布时才发现。
    chunkSizeWarningLimit: 150,
  },
})
