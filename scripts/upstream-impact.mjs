import { readFileSync, writeFileSync } from "node:fs";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

const repoRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const manifestPath = join(repoRoot, "docs", "maintenance-manifest.json");
const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));

function usage() {
  console.log(`用法: pnpm upstream:impact [-- --no-fetch] [--base <ref>] [--target <ref>] [--all-files] [--run-checks]

选项:
  --no-fetch          不执行 git fetch，使用本地已有的 upstream 引用
  --base <ref>        覆盖清单中的上次同步基线
  --target <ref>      覆盖默认目标 upstream/dev
  --all-files         输出全部上游变更文件；默认只显示摘要和命中项
  --run-checks        运行命中功能登记的定向检查
  --set-baseline      将配置的 upstream/dev 写为新的 lastSyncedCommit（仅在合并和验证完成后使用）
  --help              显示帮助
`);
}

function parseArgs(argv) {
  const options = {
    fetch: true,
    allFiles: false,
    runChecks: false,
    setBaseline: false,
    base: "",
    target: "",
  };
  const args = argv.filter((arg) => arg !== "--");
  for (let index = 0; index < args.length; index += 1) {
    const arg = args[index];
    switch (arg) {
      case "--no-fetch":
        options.fetch = false;
        break;
      case "--all-files":
        options.allFiles = true;
        break;
      case "--run-checks":
        options.runChecks = true;
        break;
      case "--set-baseline":
        options.setBaseline = true;
        break;
      case "--base":
      case "--target": {
        const value = args[index + 1];
        if (!value || value.startsWith("--")) {
          throw new Error(`${arg} 需要一个 Git ref`);
        }
        options[arg.slice(2)] = value;
        index += 1;
        break;
      }
      case "--help":
      case "-h":
        usage();
        process.exit(0);
        break;
      default:
        throw new Error(`未知参数: ${arg}`);
    }
  }
  return options;
}

function run(command, args, options = {}) {
  const result = spawnSync(command, args, {
    cwd: options.cwd ?? repoRoot,
    encoding: "utf8",
    env: options.env ?? process.env,
    maxBuffer: 16 * 1024 * 1024,
    shell: options.shell ?? false,
    stdio: options.stdio ?? "pipe",
  });
  if (result.error) {
    throw result.error;
  }
  if (result.status !== 0 && !options.allowFailure) {
    const detail = (result.stderr || result.stdout || "").trim();
    throw new Error(`${command} ${args.join(" ")} 执行失败${detail ? `: ${detail}` : ""}`);
  }
  return result;
}

function git(args, options = {}) {
  return run("git", args, options);
}

function resolveRef(ref) {
  return git(["rev-parse", "--verify", `${ref}^{commit}`]).stdout.trim();
}

function isAncestor(base, target) {
  return git(["merge-base", "--is-ancestor", base, target], { allowFailure: true }).status === 0;
}

function globToRegExp(pattern) {
  const normalized = pattern.replaceAll("\\", "/");
  let source = "^";
  for (let index = 0; index < normalized.length; index += 1) {
    const char = normalized[index];
    if (char === "*") {
      if (normalized[index + 1] === "*") {
        source += ".*";
        index += 1;
      } else {
        source += "[^/]*";
      }
      continue;
    }
    if (char === "?") {
      source += "[^/]";
      continue;
    }
    source += /[\\^$+?.()|{}[\]]/u.test(char) ? `\\${char}` : char;
  }
  source += "$";
  return new RegExp(source, "u");
}

function parseChanges(raw) {
  if (!raw.trim()) {
    return [];
  }
  return raw.trimEnd().split("\n").map((line) => {
    const columns = line.replace(/\r$/u, "").split("\t");
    const status = columns[0];
    if (status.startsWith("R") || status.startsWith("C")) {
      return {
        status,
        paths: [columns[1], columns[2]].filter(Boolean),
        display: `${status}\t${columns[1]} -> ${columns[2]}`,
      };
    }
    return {
      status,
      paths: columns[1] ? [columns[1]] : [],
      display: `${status}\t${columns[1] ?? ""}`,
    };
  });
}

function matchesAny(path, patterns) {
  return patterns.some((pattern) => globToRegExp(pattern).test(path));
}

function uniqueChecks(features) {
  const seen = new Set();
  const checks = [];
  for (const feature of features) {
    for (const check of feature.checks ?? []) {
      const key = `${check.cwd}\u0000${check.command}`;
      if (!seen.has(key)) {
        seen.add(key);
        checks.push(check);
      }
    }
  }
  return checks;
}

function printList(items, limit = 30) {
  for (const item of items.slice(0, limit)) {
    console.log(`  - ${item}`);
  }
  if (items.length > limit) {
    console.log(`  - ... 另有 ${items.length - limit} 项，使用 --all-files 查看全部`);
  }
}

function updateBaseline(targetRef, targetSha) {
  if (!isAncestor(targetSha, "HEAD")) {
    throw new Error(`拒绝更新基线：当前 HEAD 尚未包含 ${targetRef} (${targetSha.slice(0, 12)})`);
  }
  manifest.upstream.lastSyncedCommit = targetSha;
  writeFileSync(manifestPath, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
  console.log(`已更新上游同步基线: ${targetSha}`);
  console.log(`请将 ${relative(repoRoot, manifestPath).replaceAll("\\", "/")} 与本次同步结果一起提交。`);
}

function runChecks(checks) {
  if (checks.length === 0) {
    console.log("\n没有命中本地定制，不运行定向检查。");
    return;
  }
  const env = {
    ...process.env,
    GOTOOLCHAIN: manifest.toolchain?.goToolchain ?? process.env.GOTOOLCHAIN,
  };
  for (const [index, check] of checks.entries()) {
    console.log(`\n[${index + 1}/${checks.length}] (${check.cwd}) ${check.command}`);
    const result = run(check.command, [], {
      cwd: join(repoRoot, check.cwd),
      env,
      shell: true,
      stdio: "inherit",
      allowFailure: true,
    });
    if (result.status !== 0) {
      throw new Error(`定向检查失败: ${check.command}`);
    }
  }
  console.log("\n全部定向检查通过。仍需由全量 CI 完成最终收尾验证。");
}

const options = parseArgs(process.argv.slice(2));
const remote = manifest.upstream.remote;
const branch = manifest.upstream.branch;
const configuredTargetRef = `${remote}/${branch}`;
const targetRef = options.target || configuredTargetRef;

if (options.fetch) {
  console.log(`正在更新 ${remote}/${branch} ...`);
  const fetchResult = git(["fetch", "--quiet", remote, branch], { allowFailure: true });
  if (fetchResult.status !== 0) {
    const detail = (fetchResult.stderr || fetchResult.stdout || "").trim();
    throw new Error(`fetch ${remote}/${branch} 失败${detail ? `：${detail}` : ""}；如需离线使用已有引用，请显式传入 --no-fetch`);
  }
}

const baseRef = options.base || manifest.upstream.lastSyncedCommit;
const baseSha = resolveRef(baseRef);
const targetSha = resolveRef(targetRef);

if (options.setBaseline) {
  if (options.target && options.target !== configuredTargetRef) {
    throw new Error(`--set-baseline 只能使用配置的目标 ${configuredTargetRef}，不能指定本地或其他 ref`);
  }
  updateBaseline(targetRef, targetSha);
  process.exit(0);
}

if (!isAncestor(baseSha, targetSha)) {
  throw new Error(`基线 ${baseRef} 不是目标 ${targetRef} 的祖先，请核对 maintenance-manifest.json`);
}

const changes = parseChanges(
  git(["diff", "--name-status", "--find-renames", "--find-copies", `${baseSha}..${targetSha}`, "--"]).stdout,
);
const changedPathSet = new Set(changes.flatMap((change) => change.paths));
const changedPaths = [...changedPathSet];
const affectedFeatures = manifest.features
  .map((feature) => {
    const directChanges = changes.filter((change) => change.paths.some((path) => matchesAny(path, feature.watch)));
    return { ...feature, directChanges };
  })
  .filter((feature) => feature.directChanges.length > 0);

console.log("\n上游增量影响报告");
console.log("=".repeat(64));
console.log(`基线: ${baseSha}`);
console.log(`目标: ${targetSha}`);
console.log(`变更文件: ${changedPaths.length}`);
console.log(`命中本地定制: ${affectedFeatures.length}`);

if (options.allFiles && changes.length > 0) {
  console.log("\n全部上游变更");
  printList(changes.map((change) => change.display), changes.length);
}

if (affectedFeatures.length === 0) {
  console.log("\n本次上游更新没有直接触碰已登记的本地定制锚点。");
  console.log("无需扩大人工审查；正常合并后依赖项目全量 CI 收尾即可。");
  process.exit(0);
}

for (const feature of affectedFeatures) {
  console.log(`\n[${feature.risk.toUpperCase()}] ${feature.title} (${feature.id})`);
  console.log("直接命中的上游变更:");
  printList(feature.directChanges.map((change) => change.display));
  console.log("只需额外查看这些已知关联位置:");
  printList(feature.related ?? [], 20);
  console.log("必须保持:");
  printList(feature.invariants ?? [], 20);
}

const checks = uniqueChecks(affectedFeatures);
console.log("\n建议定向检查");
for (const check of checks) {
  console.log(`  - (${check.cwd}) ${check.command}`);
}
console.log("\n合并和修复完成后，可运行: pnpm upstream:check");
console.log("全部 CI 通过后，再运行: pnpm upstream:baseline");

if (options.runChecks) {
  runChecks(checks);
}
