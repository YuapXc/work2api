import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    proxy: {
      // 开发时把管理、模型及用户门户接口代理到后端
      '/admin': { target: 'http://127.0.0.1:8787', changeOrigin: true },
      '/v1': { target: 'http://127.0.0.1:8787', changeOrigin: true },
      '/user': { target: 'http://127.0.0.1:8787', changeOrigin: true },
      '/health': { target: 'http://127.0.0.1:8787', changeOrigin: true },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 900,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes('node_modules')) {
            if (id.includes('echarts') || id.includes('zrender')) return 'vendor-charts'
            if (id.includes('vue-router') || id.includes('node_modules/vue')) return 'vendor-vue'
            if (id.includes('axios') || id.includes('dayjs')) return 'vendor-util'
            return 'vendor'
          }
        },
      },
    },
  },
})
