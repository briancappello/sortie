import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
  base: "/ui/",
  publicDir: false,
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../internal/server/ui/dist",
    emptyOutDir: true,
    sourcemap: false,
    assetsDir: "assets",
  },
});
