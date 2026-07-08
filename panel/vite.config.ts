import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

// Панель — статика, встраиваемая в бинарь master (panel/build.sh кладёт
// dist/ в master/internal/panelui/static). В dev-режиме API проксируется
// на локальный master (порт из master.example.yaml).
export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    proxy: { '/v1': 'http://localhost:8100' },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
  },
});
