import { defineConfig } from 'vite';
import solid from 'vite-plugin-solid';

export default defineConfig({
  // 只有房间页用 Solid（.tsx）；其余视图仍是原生 TS，不经 solid 插件编译
  plugins: [solid({ include: ['**/*.tsx'] })],
  server: {
    port: 5173,
    host: '0.0.0.0', // 显式 IPv4 全绑(host:true 在 macOS 绑成 v6-only)
  },
});
