import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';

// The Go binary embeds the built console, so the build target is not just a
// dist directory: it has to land where core/internal/gateway/webui can embed
// it. The copy is a separate step in the Makefile rather than an outDir here,
// so that `vite dev` and `vite build` agree on where sources live.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    // A source map for a bundle this size costs more to ship than it saves
    // in a bug report, and the console is not a public site.
    sourcemap: false,
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      output: {
        // antd and React change far less often than Fleet's own code, so
        // splitting them lets a console-only redeploy stay a small download.
        manualChunks: {
          react: ['react', 'react-dom', 'react-router-dom'],
          antd: ['antd', '@ant-design/icons'],
        },
      },
    },
  },
  server: {
    port: 5173,
    proxy: {
      // `npm run dev` talks to a locally running gateway and control plane
      // without CORS or a second origin.
      '/v1': 'http://127.0.0.1:8080',
      '/fleet': 'http://127.0.0.1:8080',
      '/api': 'http://127.0.0.1:8081',
    },
  },
});
