// Runs `bal discover <argv>` for every command in a JSON list inside an arm
// image and saves each one's stdout, stderr and exit code. Used by the parity
// preflight and the cache-seed builder.
//   node run-commands.js <commands.json> <grammar A|B> <outDir>
// commands.json: [{"id": "...", "A": [...], "B": [...]}]
const { spawnSync } = require("child_process");
const fs = require("fs");
const [file, grammar, outDir] = process.argv.slice(2);
fs.mkdirSync(outDir, { recursive: true });
const results = [];
for (const command of JSON.parse(fs.readFileSync(file, "utf8"))) {
  const argv = command[grammar];
  if (!argv) continue;
  const started = Date.now();
  const run = spawnSync("bal", ["discover", ...argv, "--output", "json"], { encoding: "utf8", maxBuffer: 64 << 20 });
  results.push({ id: command.id, argv, exit: run.status, ms: Date.now() - started, stdout: run.stdout, stderr: run.stderr });
}
fs.writeFileSync(`${outDir}/${grammar}.json`, JSON.stringify(results, null, 1));
