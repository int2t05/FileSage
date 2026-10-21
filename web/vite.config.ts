import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import thirdPartyLicenses from './build/thirdPartyLicenses.mjs';

export default defineConfig({
  plugins: [react(), thirdPartyLicenses()],
  server: {
    proxy: {
      // 保留浏览器 Origin，供后端校验同源写请求。
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
    },
  },
});
