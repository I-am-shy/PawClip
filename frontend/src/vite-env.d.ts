// Vite 的客户端类型（import.meta.env / import.meta.hot / 静态资源导入）。
//
// 不加这一行的话，App.tsx 里的 `import.meta.env.DEV` 会报
//   TS2339: Property 'env' does not exist on type 'ImportMeta'
// 因为默认的 ImportMeta 只有 url/resolve。
//
// 走 `/// <reference types="vite/client" />` 而不是往 tsconfig 的 `types`
// 里塞 "vite/client"：后者会把**默认的自动包含**关掉，导致以后要手动
// 枚举每一个 @types 包。reference 只加不减，没有这个副作用。
/// <reference types="vite/client" />
