import {
  chmod,
  copyFile,
  mkdir,
  readFile,
  rm,
  unlink,
  writeFile,
} from "node:fs/promises";
import { dirname, join, relative, resolve } from "node:path";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const defaultTargetsFile = "targets.json";
const defaultTargetsPath = join(here, defaultTargetsFile);
const outputDir = "output";
const sourceBinary = "agent";
const outputBinaryDir = "binary";
let config;
const configuredTargetsPath =
  parseArgs(process.argv.slice(2)) ??
  process.env.FC_TARGETS_FILE ??
  defaultTargetsFile;
const targetsPath = resolve(here, configuredTargetsPath);
const targetsDisplayPath =
  relative(here, targetsPath).replaceAll("\\", "/") || defaultTargetsFile;

const providerOrder = ["aliyun", "ctyun", "qcloud", "gcloud"];
const providerConfigs = {
  aliyun: {
    kind: "devs3",
    defaultName: "mtr-agent-aliyun",
    defaultOutput: "s.aliyun.yaml",
    defaultSrc: "./binary",
    isp: "Aliyun",
    renderer: renderAliyunResource,
  },
  ctyun: {
    kind: "devs3",
    defaultName: "mtr-agent-ctyun",
    defaultOutput: "s.ctyun.yaml",
    defaultSrc: "./binary",
    isp: "CTYun",
    renderer: renderCtyunResource,
  },
  qcloud: {
    kind: "qcloudScf",
    defaultName: "mtr-agent-qcloud",
    defaultOutput: "qcloud",
    defaultSrc: "./binary",
    isp: "QCloud",
    renderer: renderQcloudServerlessYaml,
  },
  gcloud: {
    kind: "gcloudRun",
    defaultName: "mtr-agent-gcloud",
    defaultOutput: "gcloud",
    isp: "GCP",
  },
};

const qcloudCodeDirName = "code";
const qcloudDefaultImageRegistries = {
  default: "ccr.ccs.tencentyun.com",
  "ap-hongkong": "hkccr.ccs.tencentyun.com",
  "ap-singapore": "sgccr.ccs.tencentyun.com",
  "ap-bangkok": "thccr.ccs.tencentyun.com",
  "ap-jakarta": "jktccr.ccs.tencentyun.com",
  "ap-tokyo": "jpccr.ccs.tencentyun.com",
  "ap-seoul": "krccr.ccs.tencentyun.com",
  "eu-frankfurt": "deccr.ccs.tencentyun.com",
  "na-ashburn": "useccr.ccs.tencentyun.com",
  "na-siliconvalley": "uswccr.ccs.tencentyun.com",
  "sa-saopaulo": "saoccr.ccs.tencentyun.com",
};
const requiredTargetFields = [
  "key",
  "region",
  "functionName",
  "country",
  "id",
  "label",
];

function assertNonEmptyString(value, path) {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${path} must be a non-empty string`);
  }
}

function assertTarget(providerName, target, index, seenKeys) {
  for (const field of requiredTargetFields) {
    assertNonEmptyString(
      target[field],
      `providers.${providerName}.targets[${index}].${field}`,
    );
  }

  if (!/^[a-z][a-z0-9-]*$/.test(target.key)) {
    throw new Error(
      `providers.${providerName}.targets[${index}].key must use lowercase letters, numbers, and hyphens`,
    );
  }

  const uniqueKey = `${providerName}.${target.key}`;
  if (seenKeys.has(uniqueKey)) {
    throw new Error(`Duplicate target key: ${uniqueKey}`);
  }
  seenKeys.add(uniqueKey);
  assertOptionalPlainObject(
    target.env,
    `providers.${providerName}.targets[${index}].env`,
  );
  assertOptionalPlainObject(
    target.run,
    `providers.${providerName}.targets[${index}].run`,
  );
  assertOptionalImage(
    target.image,
    `providers.${providerName}.targets[${index}].image`,
  );
}

function assertOptionalString(value, path) {
  if (value !== undefined && typeof value !== "string") {
    throw new Error(`${path} must be a string`);
  }
}

function assertOptionalPlainObject(value, path) {
  if (value !== undefined && !isPlainObject(value)) {
    throw new Error(`${path} must be an object`);
  }
}

function assertOptionalBoolean(value, path) {
  if (value !== undefined && typeof value !== "boolean") {
    throw new Error(`${path} must be a boolean`);
  }
}

function assertOptionalImage(value, path) {
  if (value === undefined || value === null || value === false) {
    return;
  }
  if (!isPlainObject(value)) {
    throw new Error(`${path} must be an object, false, or null`);
  }

  assertOptionalString(value.sourceImage, `${path}.sourceImage`);
  assertOptionalString(value.imageType, `${path}.imageType`);
  assertOptionalString(value.imageUrl, `${path}.imageUrl`);
  assertOptionalString(value.registry, `${path}.registry`);
  assertOptionalString(value.runtime, `${path}.runtime`);
  assertOptionalPlainObject(value.registries, `${path}.registries`);
  if (value.registries) {
    for (const [key, registry] of Object.entries(value.registries)) {
      assertNonEmptyString(registry, `${path}.registries.${key}`);
    }
  }
  assertOptionalBoolean(
    value.containerImageAccelerate,
    `${path}.containerImageAccelerate`,
  );
  assertOptionalPlainObject(
    value.containerImageAccelerates,
    `${path}.containerImageAccelerates`,
  );
  if (value.containerImageAccelerates) {
    for (const [key, accelerate] of Object.entries(value.containerImageAccelerates)) {
      assertOptionalBoolean(accelerate, `${path}.containerImageAccelerates.${key}`);
    }
  }
}

function assertOptionalBinary(value, path) {
  if (value === undefined) {
    return;
  }
  if (!isPlainObject(value)) {
    throw new Error(`${path} must be an object`);
  }
  assertOptionalString(value.path, `${path}.path`);
  assertOptionalPlainObject(value.fromImage, `${path}.fromImage`);
  if (value.fromImage) {
    assertOptionalString(value.fromImage.image, `${path}.fromImage.image`);
    assertOptionalString(value.fromImage.path, `${path}.fromImage.path`);
    assertOptionalString(value.fromImage.platform, `${path}.fromImage.platform`);
    assertOptionalBoolean(value.fromImage.pull, `${path}.fromImage.pull`);
  }
}

function parseArgs(args) {
  let configPath;

  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i];
    if (arg === "--config") {
      const next = args[i + 1];
      if (!next) {
        throw new Error("--config requires a file path");
      }
      configPath = next;
      i += 1;
      continue;
    }
    if (arg.startsWith("--config=")) {
      configPath = arg.slice("--config=".length);
      if (!configPath) {
        throw new Error("--config requires a file path");
      }
      continue;
    }
    if (arg === "--help" || arg === "-h") {
      console.log("Usage: node generate.mjs [--config <path>]");
      process.exit(0);
    }
    throw new Error(`Unsupported argument: ${arg}`);
  }

  return configPath;
}

function envObject(providerName, provider, target) {
  let envsOverride = {};
  if (provider.envs) {
    if (provider.envs[target.region] !== undefined) {
      envsOverride = provider.envs[target.region];
    } else if (provider.envs[target.key] !== undefined) {
      envsOverride = provider.envs[target.key];
    }
  }

  const merged = {
    MTR_COUNTRY: target.country,
    MTR_ID: target.id,
    MTR_ISP: providerIsp(providerName, provider),
    MTR_REGION: target.label,
    TZ: "Asia/Shanghai",
    ...config.env,
    ...provider.env,
    ...envsOverride,
    ...target.env,
  };

  for (const [key, value] of Object.entries(merged)) {
    if (value === null || value === undefined) {
      delete merged[key];
    } else {
      merged[key] = String(value);
    }
  }

  return merged;
}

function deepMerge(base, override) {
  if (override === undefined) {
    return base;
  }
  if (!isPlainObject(base) || !isPlainObject(override)) {
    return override;
  }

  const merged = { ...base };
  for (const [key, value] of Object.entries(override)) {
    merged[key] = deepMerge(merged[key], value);
  }
  return merged;
}

function isPlainObject(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function providerDisplayName(providerName, provider) {
  return provider.name ?? providerConfigs[providerName].defaultName;
}

function providerOutput(providerName, provider = {}) {
  return provider.output ?? providerConfigs[providerName].defaultOutput;
}

function providerSrc(providerName, provider) {
  return provider.src ?? providerConfigs[providerName].defaultSrc;
}

function providerIsp(providerName, provider) {
  return provider.isp ?? providerConfigs[providerName].isp;
}

function renderTemplate(value, target, variables = {}) {
  let text = String(value)
    .replaceAll("{key}", target.key)
    .replaceAll("{region}", target.region)
    .replaceAll("{functionName}", target.functionName);
  for (const [key, replacement] of Object.entries(variables)) {
    text = text.replaceAll(`{${key}}`, replacement);
  }
  return text;
}

function qcloudImageConfig(target, provider) {
  if (target.image === false || target.image === null) {
    return undefined;
  }
  if (
    (provider.image === undefined ||
      provider.image === null ||
      provider.image === false) &&
    target.image === undefined
  ) {
    return undefined;
  }

  const image = deepMerge(
    isPlainObject(provider.image) ? provider.image : {},
    target.image ?? {},
  );
  assertNonEmptyString(
    image.imageUrl,
    `providers.qcloud.targets.${target.key}.image.imageUrl`,
  );

  return image;
}

function qcloudImageUrl(target, provider) {
  const image = qcloudImageConfig(target, provider);
  if (!image) {
    return undefined;
  }
  return renderTemplate(image.imageUrl, target, {
    registry: qcloudImageRegistry(target, provider),
  });
}

function qcloudSourceImage(target, provider) {
  const image = qcloudImageConfig(target, provider);
  if (!image) {
    return undefined;
  }
  assertNonEmptyString(
    image.sourceImage,
    `providers.qcloud.targets.${target.key}.image.sourceImage`,
  );
  return renderTemplate(image.sourceImage, target);
}

function qcloudImageRegistry(target, provider) {
  const image = qcloudImageConfig(target, provider);
  if (!image) {
    return undefined;
  }
  if (image.registry) {
    return renderTemplate(image.registry, target);
  }

  const registries = deepMerge(
    qcloudDefaultImageRegistries,
    image.registries ?? {},
  );
  return registries[target.region] ?? registries[target.key] ?? registries.default;
}

function gcloudImageConfig(target, provider) {
  if (target.image === false || target.image === null) {
    return undefined;
  }
  const image = deepMerge(
    isPlainObject(provider.image) ? provider.image : {},
    target.image ?? {},
  );
  assertNonEmptyString(
    image.imageUrl,
    `providers.gcloud.targets.${target.key}.image.imageUrl`,
  );
  return image;
}

function gcloudImageUrl(target, provider) {
  return renderTemplate(gcloudImageConfig(target, provider).imageUrl, target);
}

function renderAliyunResource(target, provider) {
  const layers = aliyunLayers(target);
  const props = deepMerge(
    {
      region: target.region,
      handler: "index.handler",
      role: "",
      description: "",
      timeout: 300,
      diskSize: 512,
      internetAccess: true,
      customRuntimeConfig: {
        port: 9000,
        command: ["./agent"],
      },
      functionName: target.functionName,
      runtime: "custom.debian10",
      cpu: 0.05,
      instanceConcurrency: 20,
      memorySize: 128,
      environmentVariables: envObject("aliyun", provider, target),
      code: providerSrc("aliyun", provider),
      triggers: [
        {
          triggerConfig: {
            methods: ["GET", "POST"],
            authType: "anonymous",
            disableURLInternet: false,
          },
          triggerName: "defaultTrigger",
          description: "",
          qualifier: "LATEST",
          triggerType: "http",
        },
      ],
      concurrencyConfig: {
        reservedConcurrency: 1,
      },
    },
    deepMerge(provider.props, target.props),
  );
  if (layers) {
    props.layers = layers;
  } else {
    delete props.layers;
  }
  const logConfig = resolveAliyunLogConfig(target, provider);

  delete props.logConfig;
  if (logConfig) {
    props.logConfig = logConfig;
  }

  return {
    component: "fc3",
    props,
  };
}

function aliyunLayers(target) {
  const layer = target.layer ?? "go";

  if (layer === null || layer === false || layer === "") {
    return undefined;
  }
  if (layer === "go") {
    return [`acs:fc:${target.region}:official:layers/Go1/versions/1`];
  }
  if (layer === "python-flask") {
    return [
      `acs:fc:${target.region}:official:layers/Python3-Flask2x/versions/2`,
    ];
  }

  throw new Error(`Unsupported aliyun layer for ${target.key}: ${layer}`);
}

function resolveAliyunLogConfig(target, provider) {
  if (target.log !== true) {
    return undefined;
  }

  const projectId = target.logProjectId ?? provider.logProjectId;
  assertNonEmptyString(
    projectId,
    `providers.aliyun.logProjectId when providers.aliyun.targets.${target.key}.log is true`,
  );

  return {
    project: `serverless-${target.region}-${projectId}`,
    logstore: target.logStore ?? target.logstore ?? "default-logs",
    enableLlmMetrics: target.logEnableLlmMetrics ?? false,
    enableRequestMetrics: target.logEnableRequestMetrics ?? true,
    enableInstanceMetrics: target.logEnableInstanceMetrics ?? true,
    logBeginRule: target.logBeginRule ?? "DefaultRegex",
  };
}

function renderCtyunResource(target, provider) {
  const props = deepMerge(
    {
      region: target.region,
      functionName: target.functionName,
      code: providerSrc("ctyun", provider),
      createType: 2,
      runtime: {
        handleType: "http",
        handler: "index.handler",
        runtime: "go1.x",
        executeTimeout: 300,
        instanceConcurrency: 20,
      },
      container: {
        memorySize: 128,
        cpu: 0.05,
        diskSize: 512,
        timeZone: "Asia/Shanghai",
        listenPort: 9000,
        runCommand: "./agent",
        environmentVariables: envObject("ctyun", provider, target),
      },
      // log: {
      //   logEnabled: true,
      //   logAutoConfig: true,
      // },
      network: {
        internetOutAllow: true,
      },
    },
    deepMerge(provider.props, target.props),
  );

  return {
    component: "faas-cf",
    props,
  };
}

function renderQcloudServerlessYaml(target, provider, targetOutputDir, codeDir) {
  const image = qcloudImageConfig(target, provider);
  const inputs = deepMerge(
    {
      name: target.functionName,
      namespace: provider.namespace ?? "default",
      src: relative(
        resolve(here, outputDir, targetOutputDir),
        resolve(here, outputDir, codeDir),
      ).replaceAll("\\", "/"),
      type: "web",
      runtime: "Go1",
      region: target.region,
      memorySize: 64,
      timeout: 60,
      environment: {
        variables: envObject("qcloud", provider, target),
      },
      publicAccess: true,
      instanceConcurrencyConfig: {
        enable: true,
        dynamicEnabled: false,
        maxConcurrency: 20,
      },
      aliasFunctionVersion: "$LATEST",
      events: [
        {
          http: {
            parameters: {
              netConfig: {
                enableIntranet: true,
                enableExtranet: true,
              },
              qualifier: "$DEFAULT",
              authType: "NONE",
            },
          },
        },
      ],
    },
    deepMerge(provider.inputs, target.inputs),
  );
  if (image) {
    delete inputs.src;
    inputs.runtime = image.runtime ?? "CustomImage";
    inputs.image = {
      imageType: image.imageType ?? "personal",
      imageUrl: qcloudImageUrl(target, provider),
    };
    let accelerate = image.containerImageAccelerate;
    if (image.containerImageAccelerates) {
      if (image.containerImageAccelerates[target.region] !== undefined) {
        accelerate = image.containerImageAccelerates[target.region];
      } else if (image.containerImageAccelerates[target.key] !== undefined) {
        accelerate = image.containerImageAccelerates[target.key];
      }
    }
    if (accelerate !== undefined) {
      inputs.image.containerImageAccelerate = accelerate;
    }
  }

  return yamlString({
    app: providerDisplayName("qcloud", provider),
    stage: "dev",
    component: provider.component ?? "scf",
    name: target.functionName,
    inputs,
  });
}

function renderQcloudDeployScript(providerName, provider) {
  const outputRoot = providerOutput(providerName, provider);
  const lines = [
    "#!/usr/bin/env bash",
    "set -euo pipefail",
    "",
    'ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"',
    "",
  ];
  const imageTargets = provider.targets.filter((target) =>
    qcloudImageConfig(target, provider),
  );

  if (imageTargets.length > 0) {
    lines.push("declare -A MIRRORED_QCLOUD_IMAGES=()");
    lines.push("");
    lines.push("mirror_qcloud_image() {");
    lines.push("  local source_image=\"$1\"");
    lines.push("  local target_tag=\"$2\"");
    lines.push("");
    lines.push("  if [[ -n \"${MIRRORED_QCLOUD_IMAGES[${target_tag}]:-}\" ]]; then");
    lines.push("    echo \"Reusing mirrored image ${target_tag}\" >&2");
    lines.push("    printf '%s\\n' \"${MIRRORED_QCLOUD_IMAGES[${target_tag}]}\"");
    lines.push("    return");
    lines.push("  fi");
    lines.push("");
    lines.push("  echo \"Syncing ${source_image} -> ${target_tag}\" >&2");
    lines.push("  docker pull \"${source_image}\" >&2");
    lines.push("  docker tag \"${source_image}\" \"${target_tag}\" >&2");
    lines.push("  docker push \"${target_tag}\" >&2");
    lines.push("");
    lines.push("  MIRRORED_QCLOUD_IMAGES[${target_tag}]=\"${target_tag}\"");
    lines.push("  printf '%s\\n' \"${MIRRORED_QCLOUD_IMAGES[${target_tag}]}\"");
    lines.push("}");
    lines.push("");
  }

  for (const target of provider.targets) {
    lines.push(`echo "Deploying ${providerName}.${target.key}"`);
    if (qcloudImageConfig(target, provider)) {
      lines.push(
        `mirror_qcloud_image ${shellQuote(qcloudSourceImage(target, provider))} ${shellQuote(qcloudImageUrl(target, provider))} >/dev/null`,
      );
    }
    lines.push(`(cd "$ROOT"/${shellQuote(target.key)} && scf deploy)`);
    lines.push("");
  }

  return `${lines.join("\n")}\n`;
}

function renderGcloudDeployScript(providerName, provider) {
  const lines = [
    "#!/usr/bin/env bash",
    "set -euo pipefail",
    "",
  ];

  for (const target of provider.targets) {
    const run = gcloudRunConfig(target, provider);
    const args = [
      "gcloud",
      "alpha",
      "run",
      "deploy",
      target.functionName,
      `--image=${gcloudImageUrl(target, provider)}`,
    ];

    args.push("--allow-unauthenticated");
    args.push("--public");
    args.push(`--startup-probe=timeoutSeconds=30,periodSeconds=30,tcpSocket.port=${run.port}`);
    args.push(`--port=${run.port}`);
    args.push(`--concurrency=${run.concurrency}`);
    args.push(`--timeout=${run.timeout}`);
    args.push(`--cpu=${run.cpu}`);
    args.push(`--memory=${run.memory}`);
    args.push(`--max=${run.maxInstances}`);
    args.push(`--min-instances=${run.minInstances}`);
    args.push(`--max-instances=${run.maxInstances}`);
    for (const [key, value] of Object.entries(gcloudEnvObject(providerName, provider, target))) {
      args.push(gcloudEnvArg(key, value));
    }
    if (run.noCpuBoost) {
      args.push("--no-cpu-boost");
    }
    args.push(`--region=${target.region}`);

    lines.push(`echo "Deploying ${providerName}.${target.key}"`);
    lines.push(renderShellCommand(args));
    lines.push("");
  }

  return `${lines.join("\n")}\n`;
}

function gcloudRunConfig(target, provider) {
  return deepMerge(
    {
      port: 9000,
      concurrency: 1,
      timeout: 60,
      cpu: "0.08",
      memory: "128Mi",
      minInstances: 0,
      maxInstances: 4,
      noCpuBoost: true,
    },
    deepMerge(provider.run, target.run),
  );
}

function gcloudEnvObject(providerName, provider, target) {
  let envsOverride = {};
  if (provider.envs) {
    if (provider.envs[target.region] !== undefined) {
      envsOverride = provider.envs[target.region];
    } else if (provider.envs[target.key] !== undefined) {
      envsOverride = provider.envs[target.key];
    }
  }

  const merged = {
    MTR_COUNTRY: target.country,
    MTR_ID: target.id,
    MTR_ISP: providerIsp(providerName, provider),
    MTR_REGION: target.label,
    ...config.env,
    ...provider.env,
    ...envsOverride,
    ...target.env,
  };

  for (const [key, value] of Object.entries(merged)) {
    if (value === null || value === undefined) {
      delete merged[key];
    } else {
      merged[key] = String(value);
    }
  }

  return merged;
}

function gcloudEnvArg(key, value) {
  const renderedValue = String(value);
  const assignment = `${key}=${renderedValue}`;
  if (renderedValue.includes(",")) {
    return `--set-env-vars=^#^${assignment}`;
  }
  return `--set-env-vars=${assignment}`;
}

function renderShellCommand(args) {
  return args
    .map((arg, index) => `${index === 0 ? "" : "  "}${shellArg(arg)}`)
    .join(" \\\n");
}

function shellArg(value) {
  const text = String(value);
  if (/^[A-Za-z0-9_./:=@-]+$/.test(text)) {
    return text;
  }
  return shellQuote(text);
}

function binarySourcePath() {
  return config.binary?.path ?? join(outputDir, outputBinaryDir, sourceBinary);
}

function dockerArgsWithPlatform(command, platform) {
  if (!platform) {
    return command;
  }
  const [name, ...rest] = command;
  return [name, "--platform", platform, ...rest];
}

async function prepareSourceBinary() {
  assertOptionalBinary(config.binary, "binary");
  if (!config.binary?.fromImage) {
    return;
  }

  const fromImage = config.binary.fromImage;
  assertNonEmptyString(fromImage.image, "binary.fromImage.image");
  const binaryPath = fromImage.path ?? "/usr/local/bin/mtr-agent";
  const outputPath = binarySourcePath();
  const containerName = `mtr-faas-agent-${process.pid}`;

  await mkdir(dirname(join(here, outputPath)), { recursive: true });
  if (fromImage.pull !== false) {
    await runCommand(
      "docker",
      dockerArgsWithPlatform(["pull", fromImage.image], fromImage.platform),
    );
  }
  try {
    await runCommand(
      "docker",
      dockerArgsWithPlatform(
        ["create", "--name", containerName, fromImage.image],
        fromImage.platform,
      ),
    );
    await runCommand("docker", [
      "cp",
      `${containerName}:${binaryPath}`,
      join(here, outputPath),
    ]);
  } finally {
    await runCommand("docker", ["rm", "-f", containerName], {
      allowFailure: true,
    });
  }
  await chmod(join(here, outputPath), 0o755);
  console.log(`Extracted ${outputPath} from ${fromImage.image}:${binaryPath}`);
}

async function runCommand(command, args, options = {}) {
  await new Promise((resolvePromise, reject) => {
    const child = spawn(command, args, {
      cwd: here,
      stdio: "inherit",
    });
    child.on("error", reject);
    child.on("close", (code) => {
      if (code === 0 || options.allowFailure) {
        resolvePromise();
      } else {
        reject(new Error(`${command} ${args.join(" ")} exited with ${code}`));
      }
    });
  });
}

function renderDevs3Yaml(providerName, provider, resources) {
  return yamlString({
    edition: "3.0.0",
    name: providerDisplayName(providerName, provider),
    access: provider.access,
    resources,
  });
}

function renderProviderYaml(providerName, provider) {
  const providerConfig = providerConfigs[providerName];

  const resources = Object.fromEntries(
    provider.targets.map((target) => [
      target.key,
      providerConfig.renderer(target, provider),
    ]),
  );

  return renderDevs3Yaml(providerName, provider, resources);
}

function yamlString(value) {
  return `${renderYaml(value)}\n`;
}

function renderYaml(value, indent = 0) {
  if (Array.isArray(value)) {
    return renderYamlArray(value, indent);
  }
  if (isPlainObject(value)) {
    return renderYamlObject(value, indent);
  }
  return scalar(value);
}

function renderYamlObject(value, indent) {
  const pad = " ".repeat(indent);
  const lines = [];

  for (const [key, item] of Object.entries(value)) {
    if (Array.isArray(item)) {
      lines.push(`${pad}${key}:`);
      lines.push(renderYamlArray(item, indent + 2));
    } else if (isPlainObject(item)) {
      lines.push(`${pad}${key}:`);
      lines.push(renderYamlObject(item, indent + 2));
    } else {
      lines.push(`${pad}${key}: ${scalar(item)}`);
    }
  }

  return lines.join("\n");
}

function renderYamlArray(value, indent) {
  const pad = " ".repeat(indent);

  if (value.length === 0) {
    return `${pad}[]`;
  }

  return value
    .map((item) => {
      if (Array.isArray(item)) {
        return `${pad}-\n${renderYamlArray(item, indent + 2)}`;
      }
      if (isPlainObject(item)) {
        const entries = Object.entries(item);
        if (entries.length === 0) {
          return `${pad}- {}`;
        }

        const [firstKey, firstValue] = entries[0];
        const rest = Object.fromEntries(entries.slice(1));
        const firstLine = renderYamlInlinePair(firstKey, firstValue, indent);
        const restYaml =
          Object.keys(rest).length === 0
            ? ""
            : `\n${renderYamlObject(rest, indent + 2)}`;
        return `${pad}- ${firstLine}${restYaml}`;
      }
      return `${pad}- ${scalar(item)}`;
    })
    .join("\n");
}

function renderYamlInlinePair(key, value, indent) {
  if (Array.isArray(value)) {
    return `${key}:\n${renderYamlArray(value, indent + 4)}`;
  }
  if (isPlainObject(value)) {
    return `${key}:\n${renderYamlObject(value, indent + 4)}`;
  }
  return `${key}: ${scalar(value)}`;
}

function scalar(value) {
  if (value === null) {
    return "null";
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  if (value === "") {
    return "''";
  }

  const text = String(value);
  if (
    /^[A-Za-z_./$-][A-Za-z0-9_./$-]*$/.test(text) &&
    !/^(true|false|null|yes|no|on|off)$/i.test(text) &&
    !/^-?[0-9]+(\.[0-9]+)?$/.test(text)
  ) {
    return text;
  }
  return JSON.stringify(text);
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", "'\\''")}'`;
}

async function writeGeneratedFile(filename, body) {
  const header =
    `# Generated by deploy/fc/generate.mjs. Edit ${targetsDisplayPath} instead.\n`;
  await mkdir(dirname(join(here, outputDir, filename)), { recursive: true });
  await writeFile(join(here, outputDir, filename), `${header}${body}`, "utf8");
}

async function writeGeneratedScript(filename, body) {
  await mkdir(dirname(join(here, outputDir, filename)), { recursive: true });
  await writeFile(join(here, outputDir, filename), body, "utf8");
  await chmod(join(here, outputDir, filename), 0o755);
}

async function copyGeneratedExecutable(from, to) {
  const fromPath = resolve(here, from);
  const toPath = resolve(here, outputDir, to);
  if (fromPath === toPath) {
    return;
  }
  await mkdir(dirname(toPath), { recursive: true });
  await copyFile(fromPath, toPath);
  await chmod(toPath, 0o755);
}

async function copySourceBinary(to) {
  await copyGeneratedExecutable(binarySourcePath(), to);
}

async function deleteGeneratedFile(filename) {
  try {
    await unlink(join(here, outputDir, filename));
  } catch (error) {
    if (error.code !== "ENOENT") {
      throw error;
    }
  }
}

async function deleteRootFile(filename) {
  try {
    await unlink(join(here, filename));
  } catch (error) {
    if (error.code !== "ENOENT") {
      throw error;
    }
  }
}

async function deleteProviderOutput(providerName, provider) {
  const output = providerOutput(providerName, provider);

  if (
    providerConfigs[providerName].kind === "qcloudScf" ||
    providerConfigs[providerName].kind === "gcloudRun"
  ) {
    await deleteGeneratedFile("serverless.yaml");
    await deleteGeneratedFile(join(output, "deploy.sh"));
    try {
      await rm(join(here, outputDir, output), { recursive: true, force: true });
    } catch (error) {
      if (error.code !== "ENOENT") {
        throw error;
      }
    }
    return;
  }

  await deleteGeneratedFile(output);
}

async function deleteLegacyOutputs() {
  await deleteRootFile("s.yaml");
  await deleteRootFile(providerConfigs.aliyun.defaultOutput);
  await deleteRootFile(providerConfigs.ctyun.defaultOutput);
  await deleteRootFile("serverless.yaml");
  await rm(join(here, "binary"), { recursive: true, force: true });
  await rm(join(here, "qcloud"), { recursive: true, force: true });
  await rm(join(here, "gcloud"), { recursive: true, force: true });
}

function assertProvider(providerName, provider, seenKeys) {
  const providerConfig = providerConfigs[providerName];

  assertOptionalString(provider.name, `providers.${providerName}.name`);
  assertOptionalString(provider.output, `providers.${providerName}.output`);
  assertOptionalString(provider.src, `providers.${providerName}.src`);
  assertOptionalString(provider.isp, `providers.${providerName}.isp`);
  assertOptionalPlainObject(provider.env, `providers.${providerName}.env`);
  assertOptionalPlainObject(provider.envs, `providers.${providerName}.envs`);
  if (provider.envs) {
    for (const [key, env] of Object.entries(provider.envs)) {
      assertOptionalPlainObject(env, `providers.${providerName}.envs.${key}`);
    }
  }
  assertOptionalPlainObject(provider.run, `providers.${providerName}.run`);
  assertOptionalImage(provider.image, `providers.${providerName}.image`);
  if (providerConfig.kind === "devs3") {
    assertNonEmptyString(provider.access, `providers.${providerName}.access`);
  }
  if (!Array.isArray(provider.targets)) {
    throw new Error(`providers.${providerName}.targets must be an array`);
  }

  provider.targets.forEach((target, index) =>
    assertTarget(providerName, target, index, seenKeys),
  );
}

async function generateQcloudProvider(providerName, provider) {
  const outputRoot = providerOutput(providerName, provider);
  const providerConfig = providerConfigs[providerName];
  const codeDir = outputBinaryDir;
  if (provider.targets.length === 0) {
    return;
  }

  for (const target of provider.targets) {
    const targetOutputDir = join(outputRoot, target.key);
    const output = join(targetOutputDir, "serverless.yml");
    const body = providerConfig.renderer(target, provider, targetOutputDir, codeDir);

    await writeGeneratedFile(output, body);
    console.log(`Generated ${output} for ${providerName}.${target.key}`);
  }

  const requiresFileDeployment = provider.targets.some((target) => !qcloudImageConfig(target, provider));
  if (requiresFileDeployment) {
    await copySourceBinary(join(codeDir, "scf_bootstrap"));
    console.log(`Generated ${join(codeDir, "scf_bootstrap")} for ${providerName}`);
  }

  const deployScript = join(outputRoot, "deploy.sh");
  await writeGeneratedScript(deployScript, renderQcloudDeployScript(providerName, provider));
  console.log(`Generated ${deployScript} for ${providerName}`);
}

async function generateGcloudProvider(providerName, provider) {
  const outputRoot = providerOutput(providerName, provider);

  await deleteProviderOutput(providerName, provider);

  if (provider.targets.length === 0) {
    return;
  }

  const deployScript = join(outputRoot, "deploy.sh");
  await writeGeneratedScript(
    deployScript,
    renderGcloudDeployScript(providerName, provider),
  );
  console.log(`Generated ${deployScript} for ${providerName}`);
}

async function generateProvider(providerName, provider) {
  const output = providerOutput(providerName, provider);

  if (provider.targets.length === 0) {
    await deleteProviderOutput(providerName, provider);
    return;
  }

  const providerConfig = providerConfigs[providerName];
  if (providerConfig.kind === "qcloudScf") {
    await generateQcloudProvider(providerName, provider);
    return;
  }
  if (providerConfig.kind === "gcloudRun") {
    await generateGcloudProvider(providerName, provider);
    return;
  }

  const body = renderProviderYaml(providerName, provider);

  await writeGeneratedFile(output, body);
  console.log(`Generated ${output} for ${providerName}`);
}

config = JSON.parse(await readFile(targetsPath, "utf8"));
if (!isPlainObject(config.providers)) {
  throw new Error(`${targetsDisplayPath} must contain a providers object`);
}
if (targetsPath === defaultTargetsPath) {
  assertPublicConfigIsRedacted(config);
}
assertOptionalPlainObject(config.env, "env");
assertOptionalBinary(config.binary, "binary");
for (const providerName of Object.keys(config.providers)) {
  if (!providerConfigs[providerName]) {
    throw new Error(`Unsupported provider: ${providerName}`);
  }
}

const seenKeys = new Set();
for (const providerName of Object.keys(config.providers)) {
  assertProvider(providerName, config.providers[providerName], seenKeys);
}

await deleteLegacyOutputs();
await rm(join(here, outputDir), { recursive: true, force: true });
await prepareSourceBinary();
await copySourceBinary(join(outputBinaryDir, "agent"));

for (const providerName of providerOrder) {
  const provider = config.providers[providerName];
  if (!provider) {
    await deleteProviderOutput(providerName);
    continue;
  }
  await generateProvider(providerName, provider);
}

function assertPublicConfigIsRedacted(value) {
  const findings = [];
  collectSensitiveValues(value, [], findings);
  if (findings.length === 0) {
    return;
  }

  throw new Error(
    [
      `${defaultTargetsFile} contains deployment-specific values:`,
      ...findings.map((path) => `- ${path}`),
      "Move real values to deploy/fc/.targets.json and run: node generate.mjs --config .targets.json",
    ].join("\n"),
  );
}

function collectSensitiveValues(value, path, findings) {
  if (Array.isArray(value)) {
    value.forEach((item, index) =>
      collectSensitiveValues(item, [...path, String(index)], findings),
    );
    return;
  }
  if (!isPlainObject(value)) {
    return;
  }

  for (const [key, item] of Object.entries(value)) {
    const nextPath = [...path, key];
    if (
      typeof item === "string" &&
      isSensitivePublicPath(nextPath) &&
      !isPlaceholderValue(item)
    ) {
      findings.push(nextPath.join("."));
      continue;
    }
    collectSensitiveValues(item, nextPath, findings);
  }
}

function isSensitivePublicPath(path) {
  const key = path[path.length - 1] ?? "";
  if (
    /TOKEN|SECRET|PASSWORD|PRIVATE_KEY|API_KEY|ACCESS_KEY|ACCESSKEY/i.test(key)
  ) {
    return true;
  }
  if (key === "MTR_HTTP_PATH_PREFIX") {
    return true;
  }
  if (key === "logProjectId" || key === "logProjectID") {
    return true;
  }
  return false;
}

function isPlaceholderValue(value) {
  const text = value.trim();
  return text === "" || /^\/?<[^<>]+>$/.test(text);
}
