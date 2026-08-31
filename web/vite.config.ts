import { defineConfig } from 'vite';

export default defineConfig({
  server: {
    port: 5173,
    host: '0.0.0.0', // 显式 IPv4 全绑(host:true 在 macOS 绑成 v6-only)
  },
});
