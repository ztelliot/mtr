import { readdirSync, readFileSync } from "node:fs";

// Ship the project license and original notices alongside bundled assets.
/** @returns {import("vite").Plugin} */
export function licenseNotices() {
  return {
    name: "license-notices",
    apply: "build",
    generateBundle() {
      for (const [name, fileName] of [["LICENSE", "AGPL-3.0-only.txt"], ["NOTICE", "NOTICE.txt"]]) {
        this.emitFile({
          type: "asset",
          fileName: `licenses/${fileName}`,
          source: readFileSync(new URL(`../../${name}`, import.meta.url))
        });
      }
      /** @param {URL} directory @param {string} prefix */
      const emitDirectory = (directory, prefix) => {
        for (const entry of readdirSync(directory, { withFileTypes: true })) {
          const source = new URL(entry.name + (entry.isDirectory() ? "/" : ""), directory);
          const destination = `${prefix}${entry.name}`;
          if (entry.isDirectory()) {
            emitDirectory(source, `${destination}/`);
          } else if (entry.name.endsWith(".txt")) {
            this.emitFile({ type: "asset", fileName: destination, source: readFileSync(source) });
          }
        }
      };
      emitDirectory(new URL("../src/assets/fonts/", import.meta.url), "licenses/fonts/");
    }
  };
}
