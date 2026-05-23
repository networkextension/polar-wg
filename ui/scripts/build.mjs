// Build the wg plugin UI.
//
//   src/wg-tokens.ts -> dist/scripts/wg-tokens.js  (esbuild bundle, ESM)
//   public/*         -> dist/                      (HTML + any static here)
//   tsc --noEmit                                   (type check first)
//
// Run from `ui/`: `npm run build`.

import { spawn } from "node:child_process";
import { cp, mkdir, rm } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";
import * as esbuild from "esbuild";

const root = path.dirname(fileURLToPath(import.meta.url)) + "/..";
const srcDir = path.join(root, "src");
const publicDir = path.join(root, "public");
const distDir = path.join(root, "dist");
const scriptsDir = path.join(distDir, "scripts");

async function runTsc() {
    await new Promise((resolve, reject) => {
        const child = spawn(
            process.execPath,
            [
                path.join(root, "node_modules", "typescript", "bin", "tsc"),
                "-p",
                path.join(root, "tsconfig.json"),
            ],
            { cwd: root, stdio: "inherit" },
        );
        child.on("exit", (code) => (code === 0 ? resolve() : reject(new Error(`tsc exit ${code}`))));
        child.on("error", reject);
    });
}

await rm(distDir, { recursive: true, force: true });
await mkdir(scriptsDir, { recursive: true });

await runTsc();

await esbuild.build({
    entryPoints: [path.join(srcDir, "wg-tokens.ts")],
    bundle: true,
    format: "esm",
    target: ["es2022"],
    platform: "browser",
    outdir: scriptsDir,
    sourcemap: true,
    logLevel: "info",
});

// Copy public/ assets verbatim — wg-tokens.html and anything else the
// plugin owns. polar-ui-common's static/styles.css + static/assets/*
// are deployed separately by scripts/deploy-ui.sh from the installed
// node_modules so the plugin doesn't carry a CSS copy.
await cp(publicDir, distDir, { recursive: true });

console.log("built wg plugin UI -> dist/");
