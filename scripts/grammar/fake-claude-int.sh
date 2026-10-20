#!/usr/bin/env node
// Fake `claude` for the $0 integration end-to-end: it checks that the sidecars
// are reachable on the run network and the skill plugin is mounted, copies the
// reference solution into the working directory, and builds it.
const { spawnSync } = require("child_process");
const fs = require("fs");
const readline = require("readline");
const args = process.argv.slice(2);
const emit = (event) => process.stdout.write(JSON.stringify(event) + "\n");
async function checks() {
  const reach = {};
  for (const host of ["hubspot", "gmail", "openai", "enrich"]) {
    try { reach[host] = (await fetch(`http://${host}:8080/__health`)).status; } catch (e) { reach[host] = String(e.cause || e); }
  }
  const pluginDir = args[args.lastIndexOf("--plugin-dir") + 1];
  const skillFile = `${pluginDir}/skills/ballerina/SKILL.md`;
  const skill = fs.existsSync(skillFile) ? fs.readFileSync(skillFile, "utf8") : "";
  spawnSync("cp", ["-R", "/opt/reference/.", "."]);
  const build = spawnSync("bal", ["build"], { encoding: "utf8" });
  const discover = spawnSync("bal", ["discover", "--help"], { encoding: "utf8" });
  return { reach, pluginDir, skillNeutral: skill.includes("run `bal discover --help` first"), build: build.status, discover: discover.status, cwd: fs.readdirSync(".") };
}
const input = readline.createInterface({ input: process.stdin });
emit({ type: "system", subtype: "init", model: "fake", tools: ["Bash"] });
input.on("line", async (line) => {
  if (!line.trim() || JSON.parse(line).type !== "user") return;
  const report = await checks();
  emit({ type: "assistant", message: { id: "msg_fake_int", model: "claude-sonnet-5-5", usage: { input_tokens: 10, output_tokens: 5 },
    content: [{ type: "tool_use", id: "tool_int", name: "Bash", input: { command: "bal build" } }] } });
  emit({ type: "result", subtype: "success", is_error: false, result: "FAKE-CHECKS " + JSON.stringify(report), total_cost_usd: 0,
    usage: { input_tokens: 10, output_tokens: 5 }, modelUsage: { "claude-sonnet-5-5": { inputTokens: 10, outputTokens: 5, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 } } });
});
input.on("close", () => process.exit(0));
