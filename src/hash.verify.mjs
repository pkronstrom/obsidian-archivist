// Verified against `git hash-object --stdin`, which produced every expected
// value below. Run with: node src/hash.verify.mjs

import { execFileSync } from "node:child_process";


const { gitHash } = await import("./hash.mjs");

const cases = ["hello\n", "", "ä 🎉\n", "# note\n\nbody\n", "no trailing newline"];
let bad = 0;
for (const s of cases) {
  const want = execFileSync("git", ["hash-object", "--stdin"], { input: s }).toString().trim();
  const got = await gitHash(new TextEncoder().encode(s));
  const ok = want === got;
  if (!ok) bad++;
  console.log(`${ok ? "OK  " : "FAIL"} ${JSON.stringify(s).slice(0, 24).padEnd(26)} git=${want} ours=${got}`);
}
console.log(bad === 0 ? "\nall match git hash-object" : `\n${bad} MISMATCHES`);
process.exit(bad === 0 ? 0 : 1);
