#!/usr/bin/env node
// Fake `claude` for the $0 container end-to-end test. It speaks just enough
// stream-json for the harness, checks the key plumbing without revealing the
// key, makes real `bal` calls through the logging wrapper and answers the
// question with a fixed (correct for dq-D01) answer.
const { execFileSync, spawnSync } = require("child_process");
const fs = require("fs");
const readline = require("readline");

const args = process.argv.slice(2);
const settingsIndex = args.indexOf("--settings");
const settings = settingsIndex >= 0 ? JSON.parse(args[settingsIndex + 1]) : {};
const disallowed = args[args.indexOf("--disallowedTools") + 1] || "";
const emit = (event) => process.stdout.write(JSON.stringify(event) + "\n");

function checks() {
  const helper = settings.apiKeyHelper ? spawnSync("sh", ["-c", settings.apiKeyHelper], { encoding: "utf8" }) : null;
  const keyLength = helper && helper.status === 0 ? helper.stdout.trim().length : 0;
  const envHasKey = Object.keys(process.env).some((name) => /ANTHROPIC|CLAUDE_CODE_OAUTH/.test(name));
  const home = fs.readdirSync(process.env.HOME || "/home/node");
  const calls = [["version"], ["discover", "--help"], ["discover", "ballerinax/kafka:4.6.6", "client", "Producer"]].map((argv) => {
    const run = spawnSync("bal", argv, { encoding: "utf8" });
    return { argv, status: run.status, stdoutBytes: (run.stdout || "").length };
  });
  return { keyLength, envHasKey, home, calls, disallowed, cwd: process.cwd(), cwdEntries: fs.readdirSync(".") };
}

const input = readline.createInterface({ input: process.stdin });
let turns = 0;
emit({ type: "system", subtype: "init", model: "fake", tools: ["Bash"] });
input.on("line", (line) => {
  if (!line.trim()) return;
  const message = JSON.parse(line);
  if (message.type !== "user") return;
  turns++;
  const report = checks();
  fs.writeFileSync("answer.json", JSON.stringify({ operations: ["close", "flush", "getTopicPartitions", "send", "sendWithMetadata"] }));
  emit({
    type: "assistant",
    message: {
      id: "msg_fake_" + turns, model: "claude-haiku-5-5",
      usage: { input_tokens: 10, output_tokens: 5, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
      content: [{ type: "tool_use", id: "tool_" + turns, name: "Bash", input: { command: "bal discover ballerinax/kafka:4.6.6 client Producer" } }],
    },
  });
  emit({
    type: "result", subtype: "success", is_error: false, result: "FAKE-CHECKS " + JSON.stringify(report),
    total_cost_usd: 0, usage: { input_tokens: 10, output_tokens: 5 },
    modelUsage: { "claude-haiku-5-5": { inputTokens: 10, outputTokens: 5, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 } },
  });
});
input.on("close", () => process.exit(0));
