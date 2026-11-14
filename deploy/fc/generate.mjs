import {
  access,
  chmod,
  copyFile,
  mkdir,
  mkdtemp,
  readFile,
  realpath,
  rm,
  unlink,
  writeFile,
} from "node:fs/promises";
import { existsSync } from "node:fs";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const defaultTargetsFile = "targets.json";
const defaultTargetsPath = join(here, defaultTargetsFile);
const outputDir = "output";
const outputRootPath = resolve(here, outputDir);
const sourceBinary = "agent";
const outputBinaryDir = "binary";
let config;
const cli = parseArgs(process.argv.slice(2));
const configuredTargetsPath =
  cli.config ??
  process.env.FC_TARGETS_FILE ??
  (cli.command === "sql" && existsSync(join(here, ".targets.json"))
    ? ".targets.json"
    : defaultTargetsFile);
const targetsPath = resolve(here, configuredTargetsPath);
const targetsDisplayPath =
  relative(here, targetsPath).replaceAll("\\", "/") || defaultTargetsFile;

const providerOrder = [
  "aliyun",
  "ctyun",
  "qcloud",
  "huaweicloud",
  "gcloud",
  "aws",
  "azure",
];
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
  huaweicloud: {
    kind: "huaweicloudFunctionGraph",
    defaultName: "mtr-agent-huaweicloud",
    defaultOutput: "huaweicloud",
    isp: "HWCloud",
  },
  gcloud: {
    kind: "gcloudRun",
    defaultName: "mtr-agent-gcloud",
    defaultOutput: "gcloud",
    isp: "GCP",
  },
  aws: {
    kind: "awsLambda",
    defaultName: "mtr-agent-aws",
    defaultOutput: "aws",
    isp: "AWS",
  },
  azure: {
    kind: "azureContainerApps",
    defaultName: "mtr-agent-azure",
    defaultOutput: "azure",
    isp: "Azure",
  },
};

const qcloudCodeDirName = "code";
const qcloudDefaultImageRegistries = {
  default: "ccr.ccs.tencentyun.com",
  "ap-hongkong": "hkccr.ccs.tencentyun.com",
  "ap-singapore": "sgccr.ccs.tencentyun.com",
  "ap-bangkok": "thccr.ccs.tencentyun.com",
  "ap-jakarta": "jktccr.ccs.tencentyun.com",
  "ap-johorbahru": "jbccr.ccs.tencentyun.com",
  "ap-tokyo": "jpccr.ccs.tencentyun.com",
  "ap-seoul": "krccr.ccs.tencentyun.com",
  "eu-frankfurt": "deccr.ccs.tencentyun.com",
  "na-ashburn": "useccr.ccs.tencentyun.com",
  "na-siliconvalley": "uswccr.ccs.tencentyun.com",
  "sa-saopaulo": "saoccr.ccs.tencentyun.com",
  "me-saudi-arabia": "saccr.ccs.tencentyun.com",
};
const huaweicloudDefaultImageRegistries = {
  default: "swr.{region}.myhuaweicloud.com",
  "eu-west-101": "swr.eu-west-101.myhuaweicloud.eu",
};
const huaweicloudFunctionOverrideFields = new Set([
  "memory_size",
  "timeout",
  "strategy_config",
  "description",
  "func_vpc",
  "mount_config",
  "log_config",
  "network_controller",
  "enable_lts_log",
  "lts_custom_tag",
  "domain_names",
  "enterprise_project_id",
  "enable_dynamic_memory",
  "is_stateful_function",
  "gpu_memory",
  "gpu_type",
  "initializer_handler",
  "initializer_timeout",
  "pre_stop_handler",
  "pre_stop_timeout",
  "encrypted_user_data",
  "user_data_encrypt_kms_key_id",
  "app_xrole",
]);
const huaweicloudFunctionObjectFields = new Set([
  "strategy_config",
  "func_vpc",
  "mount_config",
  "log_config",
  "network_controller",
  "lts_custom_tag",
]);
const huaweicloudFunctionBooleanFields = new Set([
  "enable_lts_log",
  "enable_dynamic_memory",
  "is_stateful_function",
]);
const huaweicloudFunctionIntegerFields = new Set([
  "memory_size",
  "timeout",
  "gpu_memory",
  "initializer_timeout",
  "pre_stop_timeout",
]);
const huaweicloudFunctionStringFields = new Set([
  "description",
  "domain_names",
  "enterprise_project_id",
  "gpu_type",
  "initializer_handler",
  "pre_stop_handler",
  "encrypted_user_data",
  "user_data_encrypt_kms_key_id",
  "app_xrole",
]);
const huaweicloudMemorySizes = new Set([
  128, 256, 512, 768, 1024, 1280, 1536, 1792, 2048, 2560, 3072, 3584,
  4096,
]);
const huaweicloudSensitivePublicKeys = new Set([
  "imageUrl",
  "project_id",
  "enterprise_project_id",
  "vpc_id",
  "vpc_name",
  "subnet_id",
  "subnet_name",
  "namespace",
  "security_groups",
  "group_id",
  "group_name",
  "stream_id",
  "stream_name",
  "mount_resource",
  "mount_share_path",
  "domain_names",
  "encrypted_user_data",
  "user_data_encrypt_kms_key_id",
  "app_xrole",
]);
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
  assertOptionalString(
    target.projectId,
    `providers.${providerName}.targets[${index}].projectId`,
  );
  assertOptionalString(
    target.agencyName,
    `providers.${providerName}.targets[${index}].agencyName`,
  );
  assertOptionalString(
    target.profile,
    `providers.${providerName}.targets[${index}].profile`,
  );
  assertOptionalString(
    target.package,
    `providers.${providerName}.targets[${index}].package`,
  );
  assertOptionalPlainObject(
    target.function,
    `providers.${providerName}.targets[${index}].function`,
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
  assertOptionalString(value.platform, `${path}.platform`);
  assertOptionalString(value.command, `${path}.command`);
  assertOptionalString(value.args, `${path}.args`);
  assertOptionalString(value.workingDir, `${path}.workingDir`);
  assertOptionalString(value.uid, `${path}.uid`);
  assertOptionalString(value.gid, `${path}.gid`);
  assertOptionalBoolean(value.isPublic, `${path}.isPublic`);
  assertOptionalBoolean(value.sync, `${path}.sync`);
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
  if (value.path !== undefined) {
    assertNonEmptyString(value.path, `${path}.path`);
  }
  assertOptionalPlainObject(value.fromImage, `${path}.fromImage`);
  if (value.path !== undefined && value.fromImage !== undefined) {
    throw new Error(`${path}.path and ${path}.fromImage are mutually exclusive`);
  }
  if (value.fromImage) {
    assertNonEmptyString(value.fromImage.image, `${path}.fromImage.image`);
    if (value.fromImage.path !== undefined) {
      assertNonEmptyString(value.fromImage.path, `${path}.fromImage.path`);
    }
    if (value.fromImage.platform !== undefined) {
      assertNonEmptyString(value.fromImage.platform, `${path}.fromImage.platform`);
    }
    assertOptionalBoolean(value.fromImage.pull, `${path}.fromImage.pull`);
  }
}

function parseArgs(args) {
  const options = { command: "deploy" };
  let start = 0;
  if (args[0] === "sql" || args[0] === "deploy") {
    options.command = args[0];
    start = 1;
  }

  for (let i = start; i < args.length; i += 1) {
    const arg = args[i];
    if (arg === "--help" || arg === "-h") {
      console.log([
        "Usage:",
        "  node generate.mjs [deploy] [--config <path>]",
        "  node generate.mjs sql [--config <path>] [--output <file.sql>]",
        "    [--provider all|aliyun,ctyun,qcloud,huaweicloud,gcloud,aws,azure]",
        "    [--regions <region,...>] [--source-id <provider=ID,...>]",
        "",
        "Config paths are relative to deploy/fc; SQL output paths are relative to the working directory.",
        "The sql command only reads deployment information and never clears deployment output or executes SQL.",
      ].join("\n"));
      process.exit(0);
    }
    const equals = arg.indexOf("=");
    const name = equals < 0 ? arg : arg.slice(0, equals);
    const allowed = options.command === "sql"
      ? ["--config", "--output", "--provider", "--regions", "--source-id"]
      : ["--config"];
    if (!allowed.includes(name)) throw new Error(`Unsupported argument: ${arg}`);
    const value = equals < 0 ? args[++i] : arg.slice(equals + 1);
    if (!value || value.startsWith("--")) throw new Error(`${name} requires a value`);
    options[name.slice(2)] = value;
  }

  return options;
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

function huaweicloudImageConfig(target, provider) {
  if (
    target.image === false || target.image === null ||
    provider.image === false || provider.image === null
  ) {
    throw new Error(
      `providers.huaweicloud.targets.${target.key}.image cannot disable the custom image`,
    );
  }

  const image = deepMerge(
    { platform: "linux/amd64", sync: true },
    deepMerge(provider.image ?? {}, target.image ?? {}),
  );
  if (image.sync) {
    assertNonEmptyString(
      image.sourceImage,
      `providers.huaweicloud.targets.${target.key}.image.sourceImage when sync is true`,
    );
  }
  assertNonEmptyString(
    image.imageUrl,
    `providers.huaweicloud.targets.${target.key}.image.imageUrl`,
  );

  return image;
}

function huaweicloudImageRegistry(target, provider) {
  const image = huaweicloudImageConfig(target, provider);
  if (image.registry) {
    return renderTemplate(image.registry, target);
  }

  const registries = deepMerge(
    huaweicloudDefaultImageRegistries,
    image.registries ?? {},
  );
  const registry =
    registries[target.region] ??
    registries[target.key] ??
    registries.default;
  return renderTemplate(registry, target);
}

function huaweicloudImageUrl(target, provider) {
  const image = huaweicloudImageConfig(target, provider);
  return renderTemplate(image.imageUrl, target, {
    registry: huaweicloudImageRegistry(target, provider),
  });
}

function huaweicloudSourceImage(target, provider) {
  return renderTemplate(
    huaweicloudImageConfig(target, provider).sourceImage,
    target,
  );
}

function assertHuaweicloudMirrorConsistency(providerName, provider) {
  const mirrors = new Map();
  for (const target of provider.targets) {
    if (!huaweicloudImageConfig(target, provider).sync) {
      continue;
    }
    const destination = huaweicloudImageUrl(target, provider);
    const source = huaweicloudSourceImage(target, provider);
    const platform = huaweicloudImageConfig(target, provider).platform;
    const previous = mirrors.get(destination);
    if (previous && (previous.source !== source || previous.platform !== platform)) {
      throw new Error(
        `providers.${providerName}.targets.${target.key}.image maps ${destination} from ${source} (${platform}), but target ${previous.key} maps the same destination from ${previous.source} (${previous.platform})`,
      );
    }
    mirrors.set(destination, { key: target.key, source, platform });
  }
}

function huaweicloudProjectId(target, provider) {
  const projectId = target.projectId ?? provider.projectId;
  if (projectId === undefined) {
    return undefined;
  }
  assertNonEmptyString(
    projectId,
    `providers.huaweicloud.targets.${target.key}.projectId`,
  );
  if (!isPlaceholderValue(projectId) && !/^[A-Za-z0-9]{32}$/.test(projectId)) {
    throw new Error(
      `providers.huaweicloud.targets.${target.key}.projectId must be a 32-character project ID`,
    );
  }
  return projectId;
}

function huaweicloudPackage(target, provider) {
  const packageName = target.package ?? provider.package ?? "default";
  assertNonEmptyString(
    packageName,
    `providers.huaweicloud.targets.${target.key}.package`,
  );
  return packageName;
}

function huaweicloudProfile(providerName, target, provider) {
  const profile = target.profile ?? provider.profile;
  if (profile !== undefined) {
    assertNonEmptyString(
      profile,
      `providers.${providerName}.targets.${target.key}.profile`,
    );
  }
  return profile;
}

function huaweicloudEnvObject(providerName, provider, target) {
  const env = envObject(providerName, provider, target);

  // FunctionGraph custom-image event functions have a fixed plaintext HTTP
  // contract. Do not inherit settings intended for the public HTTP agents.
  env.MTR_MODE = "event";
  env.MTR_HTTP_ADDR = ":8000";
  env.MTR_HTTP_AUTH_TYPE = "none";
  env.MTR_HTTP_TLS_ENABLED = "false";
  delete env.MTR_HTTP_AUTH_HEADER;
  delete env.MTR_HTTP_AUTH_SECRET;
  delete env.MTR_HTTP_PATH_PREFIX;

  return env;
}

function assertHuaweicloudUserId(value, path) {
  if (typeof value !== "string" || !/^-?\d+$/.test(value)) {
    throw new Error(`${path} must be an integer string`);
  }
  const id = Number(value);
  if (id < -1 || id > 65534 || id === 0 || id === 1000 || id === 1002) {
    throw new Error(
      `${path} must be between -1 and 65534 and cannot be 0, 1000, or 1002`,
    );
  }
}

function huaweicloudCustomImage(target, provider) {
  const image = huaweicloudImageConfig(target, provider);
  const customImage = {
    enabled: true,
    image: huaweicloudImageUrl(target, provider),
    uid: image.uid ?? "1003",
    gid: image.gid ?? "1003",
    is_public: image.isPublic ?? false,
  };

  if (image.command !== undefined) {
    customImage.command = image.command;
  }
  if (image.args !== undefined) {
    customImage.args = image.args;
  }
  if (image.workingDir !== undefined) {
    customImage.working_dir = image.workingDir;
  }

  return customImage;
}

function huaweicloudFunctionOverrides(providerName, provider, target) {
  const overrides = deepMerge(provider.function, target.function) ?? {};
  const path = `providers.${providerName}.targets.${target.key}.function`;
  for (const key of Object.keys(overrides)) {
    if (!huaweicloudFunctionOverrideFields.has(key)) {
      throw new Error(
        `${path}.${key} is not supported; only common CreateFunction/UpdateFunctionConfig fields and update-only strategy_config are accepted`,
      );
    }
  }

  for (const [key, value] of Object.entries(overrides)) {
    const fieldPath = `${path}.${key}`;
    if (
      huaweicloudFunctionIntegerFields.has(key) &&
      !Number.isInteger(value)
    ) {
      throw new Error(`${fieldPath} must be an integer`);
    }
    if (
      huaweicloudFunctionBooleanFields.has(key) &&
      typeof value !== "boolean"
    ) {
      throw new Error(`${fieldPath} must be a boolean`);
    }
    if (
      huaweicloudFunctionStringFields.has(key) &&
      typeof value !== "string"
    ) {
      throw new Error(`${fieldPath} must be a string`);
    }
    if (
      huaweicloudFunctionObjectFields.has(key) &&
      !isPlainObject(value)
    ) {
      throw new Error(`${fieldPath} must be an object`);
    }
  }

  if (overrides.strategy_config) {
    for (const [key, value] of Object.entries(overrides.strategy_config)) {
      if (key !== "concurrency" && key !== "concurrent_num") {
        throw new Error(`${path}.strategy_config.${key} is not supported`);
      }
      const minimum = key === "concurrency" ? -1 : 1;
      if (!Number.isInteger(value) || value < minimum || value > 1000) {
        throw new Error(`${path}.strategy_config.${key} must be an integer between ${minimum} and 1000`);
      }
    }
  }

  if (
    overrides.memory_size !== undefined &&
    !huaweicloudMemorySizes.has(overrides.memory_size)
  ) {
    throw new Error(
      `${path}.memory_size must be one of ${[...huaweicloudMemorySizes].join(", ")}`,
    );
  }
  if (
    overrides.timeout !== undefined &&
    (overrides.timeout < 3 || overrides.timeout > 259200)
  ) {
    throw new Error(`${path}.timeout must be between 3 and 259200 seconds`);
  }
  if (
    overrides.initializer_timeout !== undefined &&
    (overrides.initializer_timeout < 1 || overrides.initializer_timeout > 300)
  ) {
    throw new Error(
      `${path}.initializer_timeout must be between 1 and 300 seconds`,
    );
  }
  if (
    overrides.pre_stop_timeout !== undefined &&
    (overrides.pre_stop_timeout < 1 || overrides.pre_stop_timeout > 90)
  ) {
    throw new Error(
      `${path}.pre_stop_timeout must be between 1 and 90 seconds`,
    );
  }
  if (
    overrides.description !== undefined &&
    overrides.description.length > 512
  ) {
    throw new Error(`${path}.description must not exceed 512 characters`);
  }
  if (overrides.network_controller?.disable_public_network !== undefined) {
    assertOptionalBoolean(
      overrides.network_controller.disable_public_network,
      `${path}.network_controller.disable_public_network`,
    );
  }
  if (
    overrides.network_controller?.trigger_access_vpcs !== undefined &&
    !Array.isArray(overrides.network_controller.trigger_access_vpcs)
  ) {
    throw new Error(
      `${path}.network_controller.trigger_access_vpcs must be an array`,
    );
  }
  if (overrides.lts_custom_tag) {
    for (const [key, value] of Object.entries(overrides.lts_custom_tag)) {
      if (typeof value !== "string") {
        throw new Error(`${path}.lts_custom_tag.${key} must be a string`);
      }
    }
  }
  for (const prefix of ["initializer", "pre_stop"]) {
    const handler = overrides[`${prefix}_handler`];
    const timeout = overrides[`${prefix}_timeout`];
    if ((handler === undefined) !== (timeout === undefined)) {
      throw new Error(
        `${path}.${prefix}_handler and ${path}.${prefix}_timeout must be configured together`,
      );
    }
  }
  return overrides;
}

function huaweicloudFunctionBody(providerName, provider, target) {
  if (
    !/^[A-Za-z](?:[A-Za-z0-9_-]{0,58}[A-Za-z0-9])?$/.test(
      target.functionName,
    )
  ) {
    throw new Error(
      `providers.${providerName}.targets.${target.key}.functionName must start with a letter, end with a letter or digit, and contain at most 60 letters, digits, underscores, or hyphens`,
    );
  }
  const env = huaweicloudEnvObject(providerName, provider, target);
  if (Object.keys(env).length > 20) {
    throw new Error(
      `providers.${providerName}.targets.${target.key} environment exceeds FunctionGraph's 20-variable limit`,
    );
  }
  const userData = JSON.stringify(env);
  if (Buffer.byteLength(userData, "utf8") > 4096) {
    throw new Error(
      `providers.${providerName}.targets.${target.key} environment exceeds FunctionGraph's 4096-byte limit`,
    );
  }

  const agencyName = target.agencyName ?? provider.agencyName;
  assertNonEmptyString(
    agencyName,
    `providers.${providerName}.targets.${target.key}.agencyName for a FunctionGraph custom image`,
  );

  const defaults = {
    func_name: target.functionName,
    runtime: "Custom Image",
    handler: "-",
    memory_size: 128,
    timeout: 600,
    description: `MTR agent (${target.key})`,
    user_data: userData,
    network_controller: {
      disable_public_network: false,
    },
    custom_image: huaweicloudCustomImage(target, provider),
    xrole: agencyName,
  };

  const body = deepMerge(
    defaults,
    huaweicloudFunctionOverrides(providerName, provider, target),
  );
  // Keep identity and the custom-image runtime contract in their dedicated
  // schema fields. The update-only concurrency policy must never be sent to
  // CreateFunction; it is attached separately in huaweicloudUpdateInput.
  delete body.strategy_config;
  body.func_name = defaults.func_name;
  body.runtime = defaults.runtime;
  body.handler = defaults.handler;
  body.user_data = defaults.user_data;
  body.custom_image = defaults.custom_image;
  body.xrole = defaults.xrole;
  for (const field of ["uid", "gid"]) {
    assertHuaweicloudUserId(
      body.custom_image[field],
      `providers.${providerName}.targets.${target.key}.image.${field}`,
    );
  }

  return body;
}

function huaweicloudFunctionUrn(provider, target) {
  const projectId = huaweicloudProjectId(target, provider);
  return projectId === undefined ? undefined
    : `urn:fss:${target.region}:${projectId}:function:${huaweicloudPackage(target, provider)}:${target.functionName}:latest`;
}

function huaweicloudListInput(providerName, provider, target) {
  return {
    query: {
      func_name: target.functionName,
      package_name: huaweicloudPackage(target, provider),
      precise_search: true,
      maxitems: "400",
    },
  };
}

function parseHuaweicloudCliJson(output) {
  // KooCLI writes this notice to stdout even with --cli-output=json.
  // Remove only this known prefix, rather than searching arbitrary output
  // for a JSON fragment that could hide an error or incomplete response.
  const notice = "API parameters transferred for the path, query, body, formData, header, and cookies in cli-jsonInput take precedence over command transfers.";
  const clean = output.replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "").trim();
  const cliError = clean.match(/^\s*\[(USE_ERROR|CLI_ERROR|NETWORK_ERROR|OPENAPI_ERROR|APIE_ERROR)\]/m);
  if (cliError) {
    const missingProject = /(?:parameters are required:|缺少必填参数[:：])\s*project_id\b/i.test(clean);
    throw new Error(`KooCLI reported [${cliError[1]}]${missingProject ? ": missing project_id" : "; check the command, credentials, and permissions"}`);
  }
  const lines = clean.split(/\r?\n/);
  while (lines.length > 0 && (lines[0].trim() === "" || lines[0].trim() === notice)) {
    lines.shift();
  }
  let result;
  try {
    result = JSON.parse(lines.join("\n"));
  } catch {
    throw new Error("KooCLI did not return valid JSON after its cli-jsonInput notice");
  }
  if (result === null || typeof result !== "object" || Array.isArray(result)) {
    throw new Error("KooCLI response must be a JSON object");
  }
  const errorCode = result.error_code || result.error?.code;
  if (errorCode) {
    const code = String(errorCode).replace(/[^A-Za-z0-9_.-]/g, "").slice(0, 80);
    throw new Error(`KooCLI API error: ${code}`);
  }
  return result;
}

function resolveHuaweicloudProjectId(response, region) {
  if (!Array.isArray(response.projects) || response.links?.next) {
    throw new Error("IAM did not return a complete project list");
  }
  const matches = response.projects.filter(project => project.name === region && project.enabled !== false);
  if (matches.length !== 1 || !/^[A-Za-z0-9]{32}$/.test(matches[0].id)) {
    throw new Error(`Expected one enabled IAM project for ${region}; check IAM permissions or explicitly configure projectId`);
  }
  return matches[0].id;
}

function redactHuaweicloudCliOutput(output) {
  const clean = output
    .replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "")
    .replace(/(\bauthorization\s*[:=]\s*)[^\r\n]*/gi, "$1[REDACTED]");
  return clean.replace(
    /((?:["']?)(?:[A-Za-z0-9_-]*(?:secret|token|password|access[_-]?key|authorization|user[_-]?data)[A-Za-z0-9_-]*|auth)["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;}]+)/gi,
    "$1[REDACTED]",
  ).trim() || "(empty)";
}

function renderHuaweicloudCliHelper() {
  return [
    'import { readFile, writeFile } from "node:fs/promises";',
    parseHuaweicloudCliJson.toString(),
    redactHuaweicloudCliOutput.toString(),
    resolveHuaweicloudProjectId.toString(),
    resolveHuaweicloudFunctionUrn.toString(),
    'const [action, ...args] = process.argv.slice(2);',
    'let rawOutput = "";',
    'try {',
    '  rawOutput = await readFile(args[0], "utf8");',
    '  if (action === "dump") {',
    '    console.error(redactHuaweicloudCliOutput(rawOutput));',
    '  } else {',
    '  const result = parseHuaweicloudCliJson(rawOutput);',
    '  if (action === "project") {',
    '    process.stdout.write(resolveHuaweicloudProjectId(result, args[1]));',
    '  } else if (action === "urn") {',
    '    const page = args[4] === "created" ? { functions: [result], count: 1 } : result;',
    '    process.stdout.write(resolveHuaweicloudFunctionUrn(page, args[1], args[2], args[3]));',
    '  } else if (action === "prepare") {',
    '    const [, destination, projectId, functionUrn] = args;',
    '    if (!/^[A-Za-z0-9]{32}$/.test(projectId)) throw new Error("Resolved project ID is invalid");',
    '    result.path = { ...result.path, project_id: projectId };',
    '    if (functionUrn) {',
    '      if (functionUrn.split(":")[3] !== projectId) throw new Error("Function URN belongs to another project");',
    '      result.path.function_urn = functionUrn;',
    '    }',
    '    await writeFile(destination, JSON.stringify(result), { mode: 0o600 });',
    '  } else if (action !== "validate") {',
    '    throw new Error("Unknown KooCLI response action");',
    '  }',
    '  }',
    '} catch (error) {',
    '  console.error(error.message);',
    '  if (action === "project" || action === "urn") {',
    '    console.error("Raw KooCLI response (credentials redacted):");',
    '    console.error(redactHuaweicloudCliOutput(rawOutput));',
    '  }',
    '  process.exitCode = 1;',
    '}',
    "",
  ].join("\n");
}

function resolveHuaweicloudFunctionUrn(page, region, packageName, functionName) {
  if (page.error_code || !Array.isArray(page.functions)) {
    throw new Error("ListFunctions did not return a valid function list");
  }
  if (
    page.functions.length > 1 ||
    (page.count !== undefined && page.count !== page.functions.length)
  ) {
    throw new Error("ListFunctions returned an ambiguous or incomplete result");
  }
  if (page.functions.length === 0) {
    return "";
  }
  const fn = page.functions[0];
  const parts = typeof fn.func_urn === "string" ? fn.func_urn.split(":") : [];
  if (
    fn.func_name !== functionName || fn.package !== packageName ||
    (parts.length !== 7 && parts.length !== 8) ||
    parts[0] !== "urn" || parts[1] !== "fss" || parts[2] !== region ||
    !/^[A-Za-z0-9]{32}$/.test(parts[3]) || parts[4] !== "function" ||
    parts[5] !== packageName || parts[6] !== functionName
  ) {
    throw new Error("ListFunctions returned an unexpected function identity");
  }
  return `${parts.slice(0, 7).join(":")}:latest`;
}

function huaweicloudCreateInput(providerName, provider, target) {
  const body = huaweicloudFunctionBody(providerName, provider, target);
  return {
    path: {
      project_id: huaweicloudProjectId(target, provider),
    },
    body: {
      ...body,
      package: huaweicloudPackage(target, provider),
      type: "v2",
      code_type: "Custom-Image-Swr",
    },
  };
}

function huaweicloudUpdateInput(providerName, provider, target) {
  return {
    path: {
      project_id: huaweicloudProjectId(target, provider),
      function_urn: huaweicloudFunctionUrn(provider, target),
    },
    body: {
      ...huaweicloudFunctionBody(providerName, provider, target),
      strategy_config: {
        concurrency: 1,
        concurrent_num: 400,
        ...huaweicloudFunctionOverrides(providerName, provider, target).strategy_config,
      },
    },
  };
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

function renderHuaweicloudDeployScript(providerName, provider) {
  const syncTargets = provider.targets.filter(
    (target) => huaweicloudImageConfig(target, provider).sync,
  );
  const lines = [
    "#!/usr/bin/env bash",
    "set -euo pipefail",
    "",
    'ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"',
    "",
    'command -v hcloud >/dev/null || { echo "Huawei Cloud KooCLI (hcloud) is required" >&2; exit 1; }',
    'command -v node >/dev/null || { echo "Node.js is required to prepare FunctionGraph API requests" >&2; exit 1; }',
    "",
    'WORK_DIR="$(mktemp -d)"',
    'SHOW_RESULT="$WORK_DIR/response.json"',
    'STDERR_RESULT="$WORK_DIR/stderr.log"',
    'REQUEST_FILE="$WORK_DIR/request.json"',
    'trap \'rm -f "$SHOW_RESULT" "$STDERR_RESULT" "$REQUEST_FILE"; rmdir "$WORK_DIR"\' EXIT',
    "",
    "hcloud_json() {",
    "  local cli_status=0 validation_status=0",
    '  hcloud "$@" --cli-output=json >"$SHOW_RESULT" 2>"$STDERR_RESULT" || cli_status=$?',
    '  node "$ROOT/hcloud-helper.mjs" validate "$SHOW_RESULT" || validation_status=$?',
    '  if [[ "$cli_status" -ne 0 || "$validation_status" -ne 0 ]]; then',
    '    echo "KooCLI $1 $2 failed (exit code: $cli_status; response validation: $validation_status)" >&2',
    '    echo "--- stdout (credentials redacted) ---" >&2',
    '    node "$ROOT/hcloud-helper.mjs" dump "$SHOW_RESULT"',
    '    echo "--- stderr (credentials redacted) ---" >&2',
    '    node "$ROOT/hcloud-helper.mjs" dump "$STDERR_RESULT"',
    '    if [[ "$cli_status" -ne 0 ]]; then return "$cli_status"; fi',
    "    return 1",
    "  fi",
    '  if [[ -s "$STDERR_RESULT" ]]; then node "$ROOT/hcloud-helper.mjs" dump "$STDERR_RESULT"; fi',
    "}",
    "",
    "HUAWEICLOUD_PROJECT_IDS=()",
    "",
  ];

  // Resolve every project before any Docker push or FunctionGraph mutation.
  for (const target of provider.targets) {
    const projectId = huaweicloudProjectId(target, provider);
    if (projectId !== undefined) {
      lines.push(`HUAWEICLOUD_PROJECT_IDS+=(${shellQuote(projectId)})`);
      continue;
    }
    const profile = huaweicloudProfile(providerName, target, provider);
    const profileOption = profile === undefined ? "" : ` --cli-profile=${shellQuote(profile)}`;
    lines.push(
      `echo "Resolving project for ${providerName}.${target.key}"`,
      `hcloud_json IAM KeystoneListProjects/v3 --cli-region=${shellQuote(target.region)}${profileOption} --name=${shellQuote(target.region)}`,
      `PROJECT_ID="$(node "$ROOT/hcloud-helper.mjs" project "$SHOW_RESULT" ${shellQuote(target.region)})"`,
      'HUAWEICLOUD_PROJECT_IDS+=("$PROJECT_ID")',
      "",
    );
  }

  if (syncTargets.length > 0) {
    lines.push(
      'command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }',
      "",
      'PULLED_HUAWEICLOUD_IMAGE=""',
      "MIRRORED_HUAWEICLOUD_IMAGES=()",
      "",
      "mirror_huaweicloud_image() {",
      '  local source_image="$1" target_tag="$2" platform="$3"',
      '  local image source_key="${1}|${3}"',
      '  for image in "${MIRRORED_HUAWEICLOUD_IMAGES[@]}"; do',
      '    if [[ "$image" == "$target_tag" ]]; then return; fi',
      "  done",
      '  echo "Syncing ${source_image} -> ${target_tag}" >&2',
      '  if [[ "$PULLED_HUAWEICLOUD_IMAGE" != "$source_key" ]]; then',
      '    if [[ -n "$platform" ]]; then',
      '      docker pull --platform "$platform" "$source_image"',
      "    else",
      '      docker pull "$source_image"',
      "    fi",
      '    PULLED_HUAWEICLOUD_IMAGE="$source_key"',
      "  fi",
      '  docker tag "$source_image" "$target_tag"',
      '  docker push "$target_tag"',
      '  MIRRORED_HUAWEICLOUD_IMAGES+=("$target_tag")',
      "}",
      "",
    );

    for (const target of syncTargets) {
      lines.push(
        `mirror_huaweicloud_image ${shellQuote(huaweicloudSourceImage(target, provider))} ${shellQuote(huaweicloudImageUrl(target, provider))} ${shellQuote(huaweicloudImageConfig(target, provider).platform)}`,
      );
    }
    lines.push("");
  }

  for (const [index, target] of provider.targets.entries()) {
    const prefix = target.key;
    const profile = huaweicloudProfile(providerName, target, provider);
    const profileOption =
      profile === undefined
        ? ""
        : ` --cli-profile=${shellQuote(profile)}`;
    lines.push(`echo "Deploying ${providerName}.${target.key}"`);
    const identityArgs = `${shellQuote(target.region)} ${shellQuote(huaweicloudPackage(target, provider))} ${shellQuote(target.functionName)}`;
    lines.push(
      `PROJECT_ID="\${HUAWEICLOUD_PROJECT_IDS[${index}]}"`,
      `node "$ROOT/hcloud-helper.mjs" prepare "$ROOT/${prefix}.list.json" "$REQUEST_FILE" "$PROJECT_ID"`,
      `hcloud_json FunctionGraph ListFunctions --cli-region=${shellQuote(target.region)}${profileOption} --cli-jsonInput="$REQUEST_FILE"`,
      `FUNCTION_URN="$(node "$ROOT/hcloud-helper.mjs" urn "$SHOW_RESULT" ${identityArgs})"`,
      'if [[ -z "$FUNCTION_URN" ]]; then',
      `  echo "Creating ${providerName}.${target.key}"`,
      `  node "$ROOT/hcloud-helper.mjs" prepare "$ROOT/${prefix}.create.json" "$REQUEST_FILE" "$PROJECT_ID"`,
      `  hcloud_json FunctionGraph CreateFunction --cli-region=${shellQuote(target.region)}${profileOption} --cli-jsonInput="$REQUEST_FILE"`,
      `  FUNCTION_URN="$(node "$ROOT/hcloud-helper.mjs" urn "$SHOW_RESULT" ${identityArgs} created)"`,
      "fi",
      `echo "Applying configuration for ${providerName}.${target.key}"`,
      `node "$ROOT/hcloud-helper.mjs" prepare "$ROOT/${prefix}.update.json" "$REQUEST_FILE" "$PROJECT_ID" "$FUNCTION_URN"`,
      `hcloud_json FunctionGraph UpdateFunctionConfig --cli-region=${shellQuote(target.region)}${profileOption} --cli-jsonInput="$REQUEST_FILE"`,
      "",
    );
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

function generatedOutputPath(filename, field = "generated output path") {
  assertNonEmptyString(filename, field);
  if (filename !== filename.trim() || /[\0\r\n]/.test(filename)) {
    throw new Error(`${field} must be a clean relative path`);
  }
  const candidate = resolve(outputRootPath, filename);
  const rel = relative(outputRootPath, candidate);
  if (
    rel === "" ||
    rel === ".." ||
    rel.startsWith(`..${sep}`) ||
    isAbsolute(rel)
  ) {
    throw new Error(`${field} must stay within ${outputDir}/`);
  }
  return candidate;
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
  const outputPath = generatedOutputPath(
    join(outputBinaryDir, sourceBinary),
    "binary extraction output",
  );
  const containerName = `mtr-faas-agent-${process.pid}`;

  await mkdir(dirname(outputPath), { recursive: true });
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
      outputPath,
    ]);
  } finally {
    await runCommand("docker", ["rm", "-f", containerName], {
      allowFailure: true,
    });
  }
  await chmod(outputPath, 0o755);
  console.log(
    `Extracted ${relative(here, outputPath)} from ${fromImage.image}:${binaryPath}`,
  );
}

function requiresSourceBinary() {
  return Object.entries(config.providers).some(([providerName, provider]) => {
    if (provider.targets.length === 0) {
      return false;
    }
    const kind = providerConfigs[providerName].kind;
    if (kind === "devs3") {
      return true;
    }
    if (kind === "qcloudScf") {
      return provider.targets.some(
        (target) => !qcloudImageConfig(target, provider),
      );
    }
    return false;
  });
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
  const outputPath = generatedOutputPath(filename);
  await mkdir(dirname(outputPath), { recursive: true });
  await writeFile(outputPath, `${header}${body}`, "utf8");
}

async function writeGeneratedJson(filename, value) {
  const outputPath = generatedOutputPath(filename);
  await mkdir(dirname(outputPath), { recursive: true });
  await writeFile(outputPath, `${JSON.stringify(value, null, 2)}\n`, "utf8");
}

async function writeGeneratedScript(filename, body) {
  const outputPath = generatedOutputPath(filename);
  await mkdir(dirname(outputPath), { recursive: true });
  await writeFile(outputPath, body, "utf8");
  await chmod(outputPath, 0o755);
}

async function copyGeneratedExecutable(from, to) {
  const fromPath = resolve(here, from);
  const toPath = generatedOutputPath(to);
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
    await unlink(generatedOutputPath(filename));
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
    providerConfigs[providerName].kind === "huaweicloudFunctionGraph" ||
    providerConfigs[providerName].kind === "gcloudRun" ||
    providerConfigs[providerName].kind === "awsLambda" ||
    providerConfigs[providerName].kind === "azureContainerApps"
  ) {
    await deleteGeneratedFile("serverless.yaml");
    await deleteGeneratedFile(join(output, "deploy.sh"));
    try {
      await rm(generatedOutputPath(output, `providers.${providerName}.output`), {
        recursive: true,
        force: true,
      });
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
  if (provider.output !== undefined) {
    assertNonEmptyString(provider.output, `providers.${providerName}.output`);
  }
  generatedOutputPath(
    providerOutput(providerName, provider),
    `providers.${providerName}.output`,
  );
  if (provider.src !== undefined) {
    assertNonEmptyString(provider.src, `providers.${providerName}.src`);
    generatedOutputPath(provider.src, `providers.${providerName}.src`);
  }
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
  assertOptionalString(
    provider.projectId,
    `providers.${providerName}.projectId`,
  );
  assertOptionalString(
    provider.agencyName,
    `providers.${providerName}.agencyName`,
  );
  assertOptionalString(provider.profile, `providers.${providerName}.profile`);
  assertOptionalString(provider.package, `providers.${providerName}.package`);
  assertOptionalPlainObject(
    provider.function,
    `providers.${providerName}.function`,
  );
  if (providerConfig.kind === "devs3") {
    assertNonEmptyString(provider.access, `providers.${providerName}.access`);
  }
  if (!Array.isArray(provider.targets)) {
    throw new Error(`providers.${providerName}.targets must be an array`);
  }

  provider.targets.forEach((target, index) =>
    assertTarget(providerName, target, index, seenKeys),
  );

  if (providerConfig.kind === "huaweicloudFunctionGraph") {
    provider.targets.forEach((target) => {
      huaweicloudProjectId(target, provider);
      huaweicloudProfile(providerName, target, provider);
      huaweicloudFunctionBody(providerName, provider, target);
    });
    assertHuaweicloudMirrorConsistency(providerName, provider);
  }
  if (providerConfig.kind === "awsLambda") {
    const mirrors = new Map();
    const functions = new Set();
    for (const target of provider.targets) {
      const plan = awsLambdaTarget(provider, target, envObject(providerName, provider, target));
      const identity = `${plan.profile ?? ""}:${plan.region}:${plan.functionName}`;
      if (functions.has(identity)) throw new Error(`Duplicate AWS Lambda function: ${target.functionName} in ${target.region}`);
      functions.add(identity);
      if (!plan.image.sync) continue;
      const source = `${plan.image.sourceImage}|${plan.image.platform}`;
      const previous = mirrors.get(plan.image.imageUrl);
      if (previous && previous !== source) throw new Error(`AWS image ${plan.image.imageUrl} maps to conflicting source images or platforms`);
      mirrors.set(plan.image.imageUrl, source);
    }
  }
  if (providerConfig.kind === "azureContainerApps") {
    const apps = new Set();
    for (const target of provider.targets) {
      const plan = azureContainerTarget(provider, target, envObject(providerName, provider, target));
      const identity = `${plan.subscription ?? ""}:${plan.resourceGroup}:${plan.functionName}`;
      if (apps.has(identity)) throw new Error(`Duplicate Azure Container App: ${plan.functionName}`);
      apps.add(identity);
    }
  }
}

function azureContainerTarget(provider, target, env) {
  const context = `providers.azure.targets.${target.key}`;
  const placeholder = value => /<[^<>]+>/.test(String(value));
  for (const key of ["resourceGroup", "environment", "subscription"]) {
    for (const value of [provider[key], target[key]]) {
      if (value !== undefined && (typeof value !== "string" || !value.trim())) throw new Error(`${context}.${key} must be a non-empty string`);
    }
  }
  const resourceGroup = renderTemplate(target.resourceGroup ?? provider.resourceGroup ?? "mtr-agents-{region}", target);
  const environment = renderTemplate(target.environment ?? provider.environment ?? "mtr-{region}", target);
  const subscription = target.subscription ?? provider.subscription;
  if (!/^[a-z][a-z0-9]+$/.test(target.region)) throw new Error(`${context}.region must be an Azure location name such as southeastasia`);
  if (!placeholder(target.functionName) && (!/^[a-z][a-z0-9-]{0,30}[a-z0-9]$/.test(target.functionName) || target.functionName.includes("--"))) throw new Error(`${context}.functionName must be a Container App name (2-32 lowercase letters, digits or hyphens)`);
  if (/[\0\r\n/\\]/.test(resourceGroup)) throw new Error(`${context}.resourceGroup is invalid`);
  if (!placeholder(environment) && !/^[a-z][a-z0-9-]{0,58}[a-z0-9]$/.test(environment)) throw new Error(`${context}.environment is invalid`);
  if (provider.function || target.function || provider.storageAccount || target.storageAccount) throw new Error(`${context}: Container Apps uses image/run/environment; remove Functions-only function and storageAccount settings`);
  if (provider.image === false || provider.image === null || target.image === false || target.image === null) throw new Error(`${context}.image cannot disable the container image`);
  const image = { imageUrl: "ghcr.io/ztelliot/mtr-agent:dev", platform: "linux/amd64", ...provider.image, ...target.image };
  if (image.platform !== "linux/amd64") throw new Error(`${context}.image.platform must be linux/amd64 for Azure Container Apps`);
  if (image.sync || image.sourceImage) throw new Error(`${context}: Container Apps pulls image.imageUrl directly; sourceImage/sync are not used`);
  image.imageUrl = renderTemplate(image.imageUrl, target);
  if (!image.imageUrl || /[\s\0]/.test(image.imageUrl)) throw new Error(`${context}.image.imageUrl is invalid`);
  const run = { cpu: 0.25, memory: "0.5Gi", minReplicas: 0, maxReplicas: 1, httpConcurrency: 1, ...provider.run, ...target.run };
  for (const key of Object.keys(run)) {
    if (!["cpu", "memory", "minReplicas", "maxReplicas", "httpConcurrency"].includes(key)) throw new Error(`${context}.run.${key} is unsupported`);
  }
  if (typeof run.cpu !== "number" || run.cpu < 0.25 || run.cpu > 4 || !Number.isInteger(run.cpu * 4) || !/^\d+(?:\.\d+)?Gi$/.test(run.memory) || parseFloat(run.memory) !== run.cpu * 2) throw new Error(`${context}.run must use a Consumption CPU/memory pair such as 0.25 CPU and 0.5Gi`);
  for (const [key, min, max] of [["minReplicas", 0, 1000], ["maxReplicas", 1, 1000], ["httpConcurrency", 1, 1000]]) {
    if (!Number.isInteger(run[key]) || run[key] < min || run[key] > max) throw new Error(`${context}.run.${key} is out of range`);
  }
  if (run.minReplicas > run.maxReplicas) throw new Error(`${context}.run.minReplicas exceeds maxReplicas`);
  const variables = {
    MTR_LOG_LEVEL: "info", MTR_PROTOCOLS: "1", MTR_CAPABILITIES: "ping,traceroute,mtr,http,dns,port",
    MTR_HTTP_AUTH_TYPE: "header", MTR_HTTP_AUTH_HEADER: "X-MTR-Agent-Token", ...env,
    MTR_MODE: "http", MTR_HTTP_ADDR: ":9000", MTR_HTTP_TLS_ENABLED: "false", MTR_SPEEDTEST_MAX_BYTES: "0",
  };
  if (!["header", "bearer"].includes(variables.MTR_HTTP_AUTH_TYPE) || !variables.MTR_HTTP_AUTH_SECRET?.trim()) throw new Error(`${context} requires header or bearer HTTP authentication and MTR_HTTP_AUTH_SECRET`);
  if (variables.MTR_HTTP_AUTH_TYPE === "header" && !variables.MTR_HTTP_AUTH_HEADER) throw new Error(`${context} requires MTR_HTTP_AUTH_HEADER`);
  if (variables.MTR_HTTP_AUTH_TYPE === "bearer" && variables.MTR_HTTP_AUTH_HEADER) throw new Error(`${context}: bearer auth requires an empty MTR_HTTP_AUTH_HEADER`);
  return { key: target.key, region: target.region, functionName: target.functionName, resourceGroup, environment, subscription, image, run, env: variables };
}

function azureContainerManifest(target, environmentId, revisionSuffix) {
  const secrets = [];
  const env = Object.entries(target.env).map(([name, value]) => {
    if (/secret|token|password|key/i.test(name)) {
      const secretName = `env-${name.toLowerCase().replaceAll("_", "-")}`;
      secrets.push({ name: secretName, value });
      return { name, secretRef: secretName };
    }
    return { name, value };
  });
  return {
    location: target.region,
    properties: {
      environmentId,
      workloadProfileName: "Consumption",
      configuration: { activeRevisionsMode: "Single", secrets,
        ingress: { external: true, targetPort: 9000, transport: "http", allowInsecure: false } },
      template: {
        revisionSuffix,
        containers: [{ name: "mtr-agent", image: target.image.imageUrl, env,
          resources: { cpu: target.run.cpu, memory: target.run.memory },
          probes: [
            { type: "Startup", tcpSocket: { port: 9000 }, initialDelaySeconds: 1, periodSeconds: 1, timeoutSeconds: 1, failureThreshold: 60 },
            { type: "Readiness", tcpSocket: { port: 9000 }, periodSeconds: 5, timeoutSeconds: 1, failureThreshold: 3 },
          ] }],
        scale: { minReplicas: target.run.minReplicas, maxReplicas: target.run.maxReplicas,
          rules: [{ name: "http", http: { metadata: { concurrentRequests: String(target.run.httpConcurrency) } } }] },
      },
    },
  };
}

function executeAzureCommand(command, args, options) {
  return spawnSync(command, args, { encoding: "utf8", timeout: 900000, maxBuffer: 32 * 1024 * 1024,
    env: { ...process.env, AZURE_CORE_COLLECT_TELEMETRY: "no" }, ...options });
}

async function deployAzureContainers(targets, { execute = executeAzureCommand, log = console.log } = {}) {
  if (!Array.isArray(targets) || !targets.length) throw new Error("No Azure targets configured");
  const secrets = targets.flatMap(t => Object.entries(t.env ?? {}).filter(([key]) => /secret|token|password|key/i.test(key)).map(([, value]) => value)).filter(Boolean);
  const redact = value => secrets.reduce((result, secret) => result.replaceAll(secret, "[REDACTED]"), String(value));
  function az(target, args, { missing, ...options } = {}) {
    const result = execute("az", [...args, ...(target.subscription ? ["--subscription", target.subscription] : []), "--only-show-errors", "--output", "json"], options);
    if (result.error) throw new Error(`az: ${result.error.code ?? "could not start command"}`);
    if (result.status !== 0) {
      if (missing && /\((?:ResourceNotFound|ResourceGroupNotFound|ParentResourceNotFound|ContainerAppNotFound)\)/.test(result.stderr ?? "")) return null;
      throw new Error(`az ${args.slice(0, 3).join(" ")} failed: ${redact(result.stderr || `exit ${result.status}`).slice(0, 2000)}`);
    }
    return result.stdout?.trim() ? JSON.parse(result.stdout) : null;
  }
  const normalizeRegion = value => String(value).replace(/\s/g, "").toLowerCase();
  const subscriptions = new Set();
  const succeeded = [], failed = [];
  const work = await mkdtemp(join(tmpdir(), "mtr-azure-"));
  try {
    for (const target of targets) {
      const label = `azure.${target.key} (${target.region})`;
      try {
        log(`Checking ${label}`);
        if (/<[^<>]+>/.test(JSON.stringify(target))) throw new Error("replace placeholders in the local targets file before deployment");
        const subscription = target.subscription ?? "default";
        if (!subscriptions.has(subscription)) {
          const provider = az(target, ["provider", "show", "--namespace", "Microsoft.App"]);
          if (provider.registrationState !== "Registered") {
            log("Registering Microsoft.App resource provider");
            az(target, ["provider", "register", "--namespace", "Microsoft.App", "--wait"]);
          }
          subscriptions.add(subscription);
        }
        if (!az(target, ["group", "exists", "--name", target.resourceGroup])) {
          az(target, ["group", "create", "--name", target.resourceGroup, "--location", target.region]);
        }
        const environmentArgs = ["--name", target.environment, "--resource-group", target.resourceGroup];
        let environment = az(target, ["containerapp", "env", "show", ...environmentArgs], { missing: true });
        if (!environment) {
          log(`Creating environment ${target.environment} (${target.region})`);
          environment = az(target, ["containerapp", "env", "create", ...environmentArgs, "--location", target.region,
            "--enable-workload-profiles", "true", "--logs-destination", "none"]);
        }
        if (normalizeRegion(environment.location) !== target.region || !environment.id) throw new Error("Container Apps environment has no ID or belongs to a different region");
        const logs = environment.properties?.appLogsConfiguration?.destination;
        if (logs && logs !== "none") throw new Error("existing environment stores logs; use a dedicated environment with logs-destination none");
        const appArgs = ["--name", target.functionName, "--resource-group", target.resourceGroup];
        const existing = az(target, ["containerapp", "show", ...appArgs], { missing: true });
        if (existing) {
          const existingEnvironment = existing.properties?.environmentId ?? existing.properties?.managedEnvironmentId;
          if (normalizeRegion(existing.location) !== target.region || existingEnvironment?.toLowerCase() !== environment.id.toLowerCase()) throw new Error("existing Container App belongs to a different region or environment");
        }
        const manifest = azureContainerManifest(target, environment.id, `mtr-${Date.now().toString(36)}`);
        const manifestFile = join(work, "containerapp.json");
        await writeFile(manifestFile, JSON.stringify(manifest), { mode: 0o600 });
        log(`Deploying ${label}: ${target.image.imageUrl}`);
        az(target, ["containerapp", existing ? "update" : "create", ...appArgs,
          ...(!existing ? ["--environment", environment.id] : []), "--yaml", manifestFile]);
        const app = az(target, ["containerapp", "show", ...appArgs]);
        const fqdn = app?.properties?.configuration?.ingress?.fqdn;
        if (!fqdn) throw new Error("Azure returned no Container App hostname");
        log(`Succeeded ${label}: https://${fqdn}${target.env.MTR_HTTP_PATH_PREFIX ?? ""}`);
        succeeded.push(label);
      } catch (error) {
        log(`Failed ${label}: ${redact(error.message)}`);
        failed.push(label);
      }
    }
    log(`Azure deployment finished: ${succeeded.length} succeeded, ${failed.length} failed.`);
    if (failed.length) throw new Error(`Failed targets: ${failed.join(", ")}`);
  } finally {
    await rm(work, { recursive: true, force: true });
  }
}

function renderAzureDeployHelper() {
  return [
    'import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";',
    'import { spawnSync } from "node:child_process";',
    'import { tmpdir } from "node:os";',
    'import { dirname, join } from "node:path";',
    'import { fileURLToPath } from "node:url";',
    azureContainerManifest.toString(), executeAzureCommand.toString(), deployAzureContainers.toString(),
    'try {',
    '  const root = dirname(fileURLToPath(import.meta.url));',
    '  await deployAzureContainers(JSON.parse(await readFile(join(root, "targets.json"), "utf8")));',
    '} catch (error) { console.error(error.message); process.exitCode = 1; }', '',
  ].join("\n");
}

async function generateAzureProvider(providerName, provider) {
  const outputRoot = providerOutput(providerName, provider);
  const plans = provider.targets.map(target => azureContainerTarget(provider, target, envObject(providerName, provider, target)));
  const planFile = generatedOutputPath(join(outputRoot, "targets.json"));
  await mkdir(dirname(planFile), { recursive: true });
  await writeFile(planFile, JSON.stringify(plans, null, 2) + "\n", { mode: 0o600 });
  await writeGeneratedScript(join(outputRoot, "azure-deploy.mjs"), renderAzureDeployHelper());
  await writeGeneratedScript(join(outputRoot, "deploy.sh"), [
    '#!/usr/bin/env bash', 'set -euo pipefail',
    'ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"',
    'exec node "$ROOT/azure-deploy.mjs"', '',
  ].join("\n"));
  console.log(`Generated ${outputRoot}/targets.json and deploy.sh for Azure Container Apps`);
}

function parseEcrImage(uri, region) {
  const match = String(uri).match(/^(\d{12}|<account-id>)\.dkr\.ecr\.([a-z0-9-]+)\.(amazonaws\.com(?:\.cn)?)\/([a-z0-9][a-z0-9._/-]*)(?::([A-Za-z0-9_][A-Za-z0-9_.-]{0,127})|@(sha256:[a-f0-9]{64}))$/);
  if (!match || match[2] !== region) {
    throw new Error("image.imageUrl must be a tagged or digest-pinned private ECR image in the target region");
  }
  return {
    registry: uri.split("/")[0],
    accountId: match[1],
    repository: match[4],
    imageId: match[5] ? `imageTag=${match[5]}` : `imageDigest=${match[6]}`,
    tag: match[5],
  };
}

function awsLambdaTarget(provider, target, env) {
  const context = `providers.aws.targets.${target.key}`;
  if (!/^[a-z]{2}(?:-[a-z]+)+-\d+$/.test(target.region)) throw new Error(`${context}.region is invalid`);
  if (!/^[A-Za-z0-9_-]{1,64}$/.test(target.functionName)) throw new Error(`${context}.functionName is invalid`);
  if (provider.image === false || provider.image === null || target.image === false || target.image === null) {
    throw new Error(`${context}.image cannot disable the Lambda image`);
  }
  const image = { platform: "linux/arm64", sync: true, ...provider.image, ...target.image };
  if (!["linux/amd64", "linux/arm64"].includes(image.platform)) throw new Error(`${context}.image.platform must be linux/amd64 or linux/arm64`);
  if (typeof image.sync !== "boolean") throw new Error(`${context}.image.sync must be a boolean`);
  const arch = image.platform.split("/")[1];
  const render = value => String(value).replaceAll("{region}", target.region).replaceAll("{key}", target.key).replaceAll("{functionName}", target.functionName).replaceAll("{arch}", arch);
  image.sourceImage = render(image.sourceImage ?? `ghcr.io/ztelliot/mtr-agent-lambda:dev-${arch}`);
  image.imageUrl = render(image.imageUrl ?? "");
  const ecr = parseEcrImage(image.imageUrl, target.region);
  if (image.sync && !ecr.tag) throw new Error(`${context}.image.imageUrl must use a tag when sync is true`);
  if (image.sync && (!image.sourceImage || /\s/.test(image.sourceImage))) throw new Error(`${context}.image.sourceImage is invalid`);
  const roleArn = target.roleArn ?? provider.roleArn;
  if (typeof roleArn !== "string" || !/^arn:aws(?:-cn|-us-gov)?:iam::(?:\d{12}|<account-id>):role\/[A-Za-z0-9_+=,.@/-]+$/.test(roleArn)) {
    throw new Error(`${context}.roleArn must be a Lambda execution role ARN`);
  }
  const profile = target.profile ?? provider.profile;
  if (profile !== undefined && (typeof profile !== "string" || !profile.trim())) throw new Error(`${context}.profile must be a non-empty string`);
  const settings = { timeout: 300, memorySize: 128, description: `MTR Lambda agent (${target.key})`, ...provider.function, ...target.function };
  for (const key of Object.keys(settings)) {
    if (!["timeout", "memorySize", "description", "reservedConcurrency"].includes(key)) throw new Error(`${context}.function.${key} is unsupported`);
  }
  for (const [key, min, max] of [["timeout", 1, 900], ["memorySize", 128, 10240], ["reservedConcurrency", 0, Infinity]]) {
    if (settings[key] !== undefined && (!Number.isInteger(settings[key]) || settings[key] < min || settings[key] > max)) throw new Error(`${context}.function.${key} is out of range`);
  }
  if (typeof settings.description !== "string" || Buffer.byteLength(settings.description) > 256) throw new Error(`${context}.function.description must fit in 256 bytes`);
  const variables = {
    MTR_LOG_LEVEL: "info", MTR_PROTOCOLS: "1", MTR_CAPABILITIES: "ping,traceroute,mtr,http,dns,port",
    MTR_HTTP_AUTH_TYPE: "header", MTR_HTTP_AUTH_HEADER: "X-MTR-Agent-Token",
    ...env,
    // Both ends must agree on streaming and port; inherited settings can be
    // intended for a different provider (including event-mode FunctionGraph).
    MTR_MODE: "http", MTR_HTTP_ADDR: ":9000", MTR_HTTP_TLS_ENABLED: "false",
    MTR_SPEEDTEST_MAX_BYTES: "0", AWS_LWA_PORT: "9000",
    AWS_LWA_READINESS_CHECK_PORT: "9000", AWS_LWA_READINESS_CHECK_PROTOCOL: "tcp",
    AWS_LWA_INVOKE_MODE: "response_stream",
  };
  if (!["header", "bearer"].includes(variables.MTR_HTTP_AUTH_TYPE) || !variables.MTR_HTTP_AUTH_SECRET?.trim()) {
    throw new Error(`${context} requires header or bearer HTTP authentication and MTR_HTTP_AUTH_SECRET`);
  }
  if (variables.MTR_HTTP_AUTH_TYPE === "header" && !/^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/.test(variables.MTR_HTTP_AUTH_HEADER ?? "")) throw new Error(`${context}.MTR_HTTP_AUTH_HEADER is invalid`);
  if (variables.MTR_HTTP_AUTH_TYPE === "bearer" && variables.MTR_HTTP_AUTH_HEADER) throw new Error(`${context}: bearer auth requires an empty MTR_HTTP_AUTH_HEADER`);
  let bytes = 0;
  for (const [key, value] of Object.entries(variables)) {
    if (!/^[A-Za-z][A-Za-z0-9_]+$/.test(key) || /^(?:AWS_LAMBDA_|AWS_EXECUTION_ENV$|AWS_REGION$|AWS_DEFAULT_REGION$|AWS_ACCESS_KEY_ID$|AWS_SECRET_ACCESS_KEY$|AWS_SESSION_TOKEN$|_HANDLER$|LAMBDA_TASK_ROOT$|LAMBDA_RUNTIME_DIR$)/.test(key)) throw new Error(`${context}.env contains a reserved or invalid Lambda key: ${key}`);
    bytes += Buffer.byteLength(key) + Buffer.byteLength(String(value));
    variables[key] = String(value);
  }
  if (bytes > 4096) throw new Error(`${context}.env exceeds Lambda's 4096-byte limit`);
  return {
    key: target.key, region: target.region, profile, functionName: target.functionName,
    image, configuration: {
      Role: roleArn, Timeout: settings.timeout, MemorySize: settings.memorySize,
      Description: settings.description, Environment: { Variables: variables },
    },
    architecture: arch === "amd64" ? "x86_64" : "arm64",
    reservedConcurrency: settings.reservedConcurrency,
  };
}

function executeAwsCommand(command, args, options) {
  return spawnSync(command, args, {
    encoding: "utf8", timeout: 300000, maxBuffer: 32 * 1024 * 1024,
    env: { ...process.env, AWS_PAGER: "", AWS_CLI_AUTO_PROMPT: "off" }, ...options,
  });
}

async function deployAwsLambda(targets, { execute = executeAwsCommand, log = console.log } = {}) {
  if (!Array.isArray(targets) || !targets.length) throw new Error("No AWS Lambda targets configured");
  const secrets = targets.flatMap(t => Object.entries(t.configuration?.Environment?.Variables ?? {})
    .filter(([key]) => /secret|token|password|key/i.test(key)).map(([, value]) => value)).filter(Boolean);
  const redact = text => secrets.reduce((result, secret) => result.replaceAll(secret, "[REDACTED]"), String(text));
  function run(command, args, { missing, ...options } = {}) {
    const result = execute(command, args, options);
    if (result.error) throw new Error(`${command}: ${result.error.code ?? "could not start command"}`);
    if (result.status !== 0) {
      const code = result.stderr?.match(/An error occurred \(([^)]+)\)/)?.[1];
      if (missing && code === missing) return null;
      throw new Error(`${command} ${args.slice(0, 2).join(" ")} failed: ${redact(result.stderr || `exit ${result.status}`).slice(0, 2000)}`);
    }
    return result.stdout?.trim() ?? "";
  }
  function aws(target, service, operation, args = [], options = {}) {
    try {
      return run("aws", [service, operation, "--region", target.region,
        ...(target.profile ? ["--profile", target.profile] : []), "--output", "json", "--no-cli-pager", ...args], options);
    } catch (error) {
      const hint = service === "lambda" && operation.includes("function-url-config") &&
        /Unable to determine service\/operation name to be authorized/.test(error.message)
        ? "\nCheck Function URL availability in this region and the configured Lambda endpoint before changing IAM permissions."
        : "";
      throw new Error(`aws.${target.key} (${target.region}): ${error.message}${hint}`);
    }
  }
  const json = value => value === null ? null : JSON.parse(value);
  const succeeded = [];
  const failed = [];
  const work = await mkdtemp(join(tmpdir(), "mtr-lambda-"));
  try {
    const request = join(work, "request.json");
    async function input(target, operation, value) {
      await writeFile(request, JSON.stringify(value), { mode: 0o600 });
      return aws(target, "lambda", operation, ["--cli-input-json", `file://${request}`]);
    }
    const mirrored = new Set();
    // Finish each target before touching the next region. A regional failure
    // must not prevent healthy regions from being deployed.
    for (const target of targets) {
      const label = `aws.${target.key} (${target.region})`;
      try {
        log(`Checking ${label}`);
        if (/<[^<>]+>/.test(JSON.stringify(target))) throw new Error("replace placeholders in the local targets file before deployment");
        const ecr = parseEcrImage(target.image.imageUrl, target.region);
        const functionArgs = ["--function-name", target.functionName];
        // Only an explicit ResourceNotFound is absence; permission and network
        // failures stop this target and are reported before proceeding.
        const info = json(aws(target, "lambda", "get-function", functionArgs, { missing: "ResourceNotFoundException" }));
        if (info && info.Configuration?.PackageType !== "Image") throw new Error("existing function must use package type Image");
        const existingUrl = info
          ? json(aws(target, "lambda", "get-function-url-config", functionArgs, { missing: "ResourceNotFoundException" }))
          : null;
        if (existingUrl && existingUrl.AuthType !== "NONE") throw new Error("existing Function URL uses AWS_IAM; refusing to change its authentication");
        const { image } = target;
        if (image.sync && !mirrored.has(image.imageUrl)) {
          log(`Checking ECR repository ${ecr.registry}/${ecr.repository}`);
          const repoArgs = ["--registry-id", ecr.accountId, "--repository-names", ecr.repository];
          if (aws(target, "ecr", "describe-repositories", repoArgs, { missing: "RepositoryNotFoundException" }) === null) {
            const caller = json(aws(target, "sts", "get-caller-identity"));
            if (caller?.Account !== ecr.accountId) throw new Error(`aws.${target.key}: create the ECR repository in its owning account before deployment`);
            aws(target, "ecr", "create-repository", ["--repository-name", ecr.repository]);
          }
          log(`Authenticating ECR registry ${ecr.registry}`);
          const password = json(aws(target, "ecr", "get-authorization-token", ["--registry-ids", ecr.accountId]))?.authorizationData?.[0]?.authorizationToken;
          if (!password) throw new Error(`aws.${target.key}: ECR returned no authorization token`);
          const credentials = Buffer.from(password, "base64").toString();
          if (!credentials.startsWith("AWS:")) throw new Error("ECR returned invalid Docker credentials");
          // An isolated Docker config avoids changing the user's registry logins.
          const docker = args => ["--config", work, ...args];
          run("docker", docker(["login", "--username", "AWS", "--password-stdin", ecr.registry]), { input: credentials.slice(4) + "\n" });
          // Pull with the user's config so private source registries keep working.
          log(`Pulling ${image.sourceImage} (${image.platform})`);
          run("docker", ["pull", "--platform", image.platform, image.sourceImage]);
          const platform = run("docker", ["image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", image.sourceImage]);
          if (platform !== image.platform) throw new Error(`aws.${target.key}: source image architecture does not match ${image.platform}`);
          run("docker", ["tag", image.sourceImage, image.imageUrl]);
          log(`Pushing ${image.imageUrl}`);
          run("docker", docker(["push", image.imageUrl]));
          mirrored.add(image.imageUrl);
        }
        const result = json(aws(target, "ecr", "batch-get-image", ["--registry-id", ecr.accountId,
          "--repository-name", ecr.repository, "--image-ids", ecr.imageId]));
        const entry = result?.images?.[0];
        const manifest = entry && JSON.parse(entry.imageManifest);
        if (result?.failures?.length || result?.images?.length !== 1 || !manifest || manifest.manifests || !manifest.config || !/^sha256:[a-f0-9]{64}$/.test(entry.imageId?.imageDigest)) {
          throw new Error(`aws.${target.key}: ECR must contain a single-architecture image manifest without attestations`);
        }
        const imageUri = `${ecr.registry}/${ecr.repository}@${entry.imageId.imageDigest}`;
        log(`Deploying ${label}`);
        const base = { FunctionName: target.functionName, ...target.configuration };
        if (info) {
          aws(target, "lambda", "wait", ["function-updated-v2", ...functionArgs]);
          await input(target, "update-function-configuration", base);
          aws(target, "lambda", "wait", ["function-updated-v2", ...functionArgs]);
          await input(target, "update-function-code", { FunctionName: target.functionName,
            ImageUri: imageUri, Architectures: [target.architecture] });
          aws(target, "lambda", "wait", ["function-updated-v2", ...functionArgs]);
        } else {
          await input(target, "create-function", { ...base, PackageType: "Image",
            Code: { ImageUri: imageUri }, Architectures: [target.architecture] });
          aws(target, "lambda", "wait", ["function-active-v2", ...functionArgs]);
        }
        if (target.reservedConcurrency !== undefined) aws(target, "lambda", "put-function-concurrency", [...functionArgs, "--reserved-concurrent-executions", String(target.reservedConcurrency)]);
        const url = json(aws(target, "lambda", existingUrl ? "update-function-url-config" : "create-function-url-config",
          [...functionArgs, "--auth-type", "NONE", "--invoke-mode", "RESPONSE_STREAM"]));
        const policyResult = json(aws(target, "lambda", "get-policy", functionArgs, { missing: "ResourceNotFoundException" }));
        const statements = policyResult ? JSON.parse(policyResult.Policy).Statement : [];
        const permissions = [
          ["mtr-function-url", "lambda:InvokeFunctionUrl", "lambda:FunctionUrlAuthType", "StringEquals", "NONE", ["--function-url-auth-type", "NONE"]],
          ["mtr-function-url-invoke", "lambda:InvokeFunction", "lambda:InvokedViaFunctionUrl", "Bool", "true", ["--invoked-via-function-url"]],
        ];
        for (const [sid, action, condition, operator, value, options] of permissions) {
          const existing = statements.find(statement => statement.Sid === sid);
          if (existing) {
            const publicPrincipal = existing.Principal === "*" || existing.Principal?.AWS === "*";
            if (existing.Effect !== "Allow" || existing.Action !== action || !publicPrincipal || String(existing.Condition?.[operator]?.[condition]) !== value) {
              throw new Error(`aws.${target.key}: permission ${sid} differs from the managed Function URL policy`);
            }
            continue;
          }
          aws(target, "lambda", "add-permission", [...functionArgs, "--statement-id", sid, "--action", action, "--principal", "*", ...options]);
        }
        if (!url?.FunctionUrl) throw new Error(`aws.${target.key}: Lambda returned no Function URL`);
        log(`Succeeded ${label}: ${url.FunctionUrl}`);
        succeeded.push(label);
      } catch (error) {
        const message = redact(error.message);
        const prefix = `${label}: `;
        log(`Failed ${label}: ${message.startsWith(prefix) ? message.slice(prefix.length) : message}`);
        failed.push(label);
      }
    }
    log(`AWS deployment finished: ${succeeded.length} succeeded, ${failed.length} failed.`);
    if (failed.length) throw new Error(`Failed targets: ${failed.join(", ")}`);
  } finally {
    await rm(work, { recursive: true, force: true });
  }
}

function renderAwsLambdaDeployHelper() {
  return [
    'import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";',
    'import { spawnSync } from "node:child_process";',
    'import { tmpdir } from "node:os";',
    'import { join, resolve } from "node:path";',
    'import { fileURLToPath } from "node:url";',
    parseEcrImage.toString(),
    executeAwsCommand.toString(),
    deployAwsLambda.toString(),
    'if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {',
    '  try {',
    '    if (!process.argv[2]) throw new Error("Usage: node aws-lambda-deploy.mjs <targets.json>");',
    '    await deployAwsLambda(JSON.parse(await readFile(process.argv[2], "utf8")));',
    '  } catch (error) {',
    '    console.error(error.message);',
    '    process.exitCode = 1;',
    '  }',
    '}',
    '',
  ].join("\n");
}

async function generateAwsProvider(providerName, provider) {
  const outputRoot = providerOutput(providerName, provider);
  const plans = provider.targets.map(target => awsLambdaTarget(provider, target, envObject(providerName, provider, target)));
  const planFile = generatedOutputPath(join(outputRoot, "targets.json"));
  await mkdir(dirname(planFile), { recursive: true });
  await writeFile(planFile, `${JSON.stringify(plans, null, 2)}\n`, { mode: 0o600 });
  await writeGeneratedScript(join(outputRoot, "aws-lambda-deploy.mjs"), renderAwsLambdaDeployHelper());
  await writeGeneratedScript(join(outputRoot, "deploy.sh"), [
    "#!/usr/bin/env bash", "set -euo pipefail",
    'ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"',
    'exec node "$ROOT/aws-lambda-deploy.mjs" "$ROOT/targets.json"', "",
  ].join("\n"));
  console.log(`Generated ${outputRoot}/targets.json and deploy.sh for ${providerName}`);
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

async function generateHuaweicloudProvider(providerName, provider) {
  const outputRoot = providerOutput(providerName, provider);

  await deleteProviderOutput(providerName, provider);
  if (provider.targets.length === 0) {
    return;
  }

  for (const target of provider.targets) {
    const files = [
      [
        `${target.key}.list.json`,
        huaweicloudListInput(providerName, provider, target),
      ],
      [
        `${target.key}.create.json`,
        huaweicloudCreateInput(providerName, provider, target),
      ],
      [
        `${target.key}.update.json`,
        huaweicloudUpdateInput(providerName, provider, target),
      ],
    ];
    for (const [filename, value] of files) {
      const output = join(outputRoot, filename);
      await writeGeneratedJson(output, value);
      console.log(`Generated ${output} for ${providerName}.${target.key}`);
    }
  }

  const deployScript = join(outputRoot, "deploy.sh");
  await writeFile(
    generatedOutputPath(join(outputRoot, "hcloud-helper.mjs")),
    renderHuaweicloudCliHelper(),
    "utf8",
  );
  await writeGeneratedScript(
    deployScript,
    renderHuaweicloudDeployScript(providerName, provider),
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
  if (providerConfig.kind === "huaweicloudFunctionGraph") {
    await generateHuaweicloudProvider(providerName, provider);
    return;
  }
  if (providerConfig.kind === "awsLambda") {
    await generateAwsProvider(providerName, provider);
    return;
  }
  if (providerConfig.kind === "azureContainerApps") {
    await generateAzureProvider(providerName, provider);
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
if (cli.command === "sql") {
  await generateOutboundAgentSql({
    config,
    configPath: targetsPath,
    output: cli.output,
    provider: cli.provider,
    regions: cli.regions,
    sourceId: cli["source-id"],
  });
} else {
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
  if (requiresSourceBinary()) {
    await prepareSourceBinary();
    await copySourceBinary(join(outputBinaryDir, "agent"));
  }

  for (const providerName of providerOrder) {
    const provider = config.providers[providerName];
    if (!provider) {
      await deleteProviderOutput(providerName);
      continue;
    }
    await generateProvider(providerName, provider);
  }
}

async function generateOutboundAgentSql({
  config,
  configPath,
  output = join(here, outputDir, "outbound-agents.sql"),
  provider: providerFilter = "all",
  regions: regionFilter,
  sourceId = "huaweicloud=hwc.cn-north-1.fc",
}) {
  const fcDir = here;
  const options = {
    config: configPath,
    output,
    provider: providerFilter,
    regions: regionFilter,
    "source-id": sourceId,
  };
  const supported = ["aliyun", "ctyun", "qcloud", "huaweicloud", "gcloud", "aws", "azure"];
  const selected = options.provider === "all" ? supported.filter(p => config.providers?.[p]) : options.provider.split(",");
  for (const p of selected) if (!supported.includes(p) || !config.providers?.[p]) throw new Error(`Provider is not configured: ${p}`);
  const regions = options.regions ? new Set(options.regions.split(",")) : null;
  if (regions) for (const region of regions) if (!selected.some(p => config.providers[p].targets.some(t => t.region === region))) throw new Error(`Region is not in selected targets: ${region}`);
  const sources = { huaweicloud: "hwc.cn-north-1.fc" };
  for (const entry of options["source-id"].split(",")) {
    const [provider, id] = entry.includes("=") ? entry.split("=") : ["huaweicloud", entry];
    if (!supported.includes(provider) || !id || !/^[A-Za-z0-9_.-]+$/.test(id)) throw new Error(`Invalid source agent mapping: ${entry}`);
    sources[provider] = id;
  }
  const literal = value => "E'" + String(value).replaceAll("\\", "\\\\").replaceAll("'", "''") + "'";
  const json = value => value == null ? "NULL::jsonb" : `${literal(JSON.stringify(value))}::jsonb`;
  const clean = value => value.replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, "").replace(/\r/g, "");
  const redact = value => clean(value)
    .replace(/(\bauthorization\s*[:=]\s*)[^\n]*/gi, "$1[REDACTED]")
    .replace(/(["']?[\w-]*(?:secret|token|password|access[_-]?key|authorization|credential|signature|downloadurl|codeurl|user[_-]?data)[\w-]*["']?\s*[:=]\s*)("(?:\\.|[^"\\])*"|'[^']*'|[^\s,;}]+)/gi, "$1[REDACTED]");
  const placeholder = value => !value || /<[^<>]+>/.test(String(value));
  const object = value => value !== null && typeof value === "object" && !Array.isArray(value);
  const cache = new Map();

  function run(command, args, cwd = fcDir, { missing } = {}) {
    const result = spawnSync(command, args, { cwd, encoding: "utf8", timeout: 180000, maxBuffer: 32 * 1024 * 1024, env: { ...process.env, NO_COLOR: "1", FORCE_COLOR: "0" } });
    if (result.error) throw new Error(`${command}: ${result.error.message}`);
    const output = clean(result.stdout);
    if (result.status !== 0 || /^\s*\[(USE_ERROR|CLI_ERROR|NETWORK_ERROR|OPENAPI_ERROR|APIE_ERROR)\]/m.test(output)) {
      const code = result.stderr?.match(/An error occurred \(([^)]+)\)/)?.[1] ?? result.stderr?.match(/ERROR:\s*\(([^)]+)\)/)?.[1];
      if (missing && (Array.isArray(missing) ? missing : [missing]).includes(code)) return null;
      throw new Error(`${command} ${args.slice(0, 2).join(" ")} failed (exit ${result.status})\n${redact(result.stderr || output)}`);
    }
    return output;
  }

  function parseJson(output, context) {
    const notice = "API parameters transferred for the path, query, body, formData, header, and cookies in cli-jsonInput take precedence over command transfers.";
    const body = clean(output).split("\n").filter(line => line.trim() !== notice).join("\n").trim();
    let result;
    try { result = JSON.parse(body); } catch { throw new Error(`${context}: expected JSON output`); }
    if (result?.error || result?.error_code) throw new Error(`${context}: API reported an error`);
    return result;
  }

  function mergedEnv(providerName, provider, target) {
    const env = { ...config.env, ...provider.env, ...(provider.envs?.[target.region] ?? provider.envs?.[target.key]), ...target.env };
    const get = (o, path) => path.reduce((value, key) => value?.[key], o);
    const path = providerName === "aliyun" ? ["props", "environmentVariables"]
      : providerName === "ctyun" ? ["props", "container", "environmentVariables"]
      : providerName === "qcloud" ? ["inputs", "environment", "variables"] : null;
    if (path) Object.assign(env, get(provider, path), get(target, path));
    for (const [key, value] of Object.entries(env)) if (value == null) delete env[key]; else env[key] = String(value);
    return env;
  }

  function httpAuth(env, context) {
    if (placeholder(env.MTR_HTTP_AUTH_SECRET)) throw new Error(`${context}: configure MTR_HTTP_AUTH_SECRET in the local targets file, or reuse an existing row with --source-id ${context.split(".")[0]}=ID`);
    if (env.MTR_HTTP_AUTH_TYPE === "bearer") return { type: "bearer", secret: env.MTR_HTTP_AUTH_SECRET };
    if (env.MTR_HTTP_AUTH_TYPE === "header" && env.MTR_HTTP_AUTH_HEADER) return { type: "header", header: env.MTR_HTTP_AUTH_HEADER, secret: env.MTR_HTTP_AUTH_SECRET };
    throw new Error(`${context}: HTTP agents require header or bearer authentication`);
  }

  function httpUrl(base, prefix, context) {
    if (placeholder(base) || (prefix && placeholder(prefix))) throw new Error(`${context}: URL/path prefix still contains a placeholder`);
    let url;
    try { url = new URL(/^https?:\/\//i.test(base) ? base : `https://${base}`); } catch { throw new Error(`${context}: invalid service URL`); }
    if (!["http:", "https:"].includes(url.protocol) || url.search || url.hash || url.username || url.password) throw new Error(`${context}: service URL must not contain query parameters or credentials`);
    const value = (prefix ?? "").trim();
    if (/[\s?#]/.test(value)) throw new Error(`${context}: invalid Agent path prefix`);
    const normalized = !value || value === "/" ? "" : (value.startsWith("/") ? value : "/" + value).replace(/\/+$/g, "");
    const path = url.pathname.replace(/\/+$/g, "");
    url.pathname = normalized && !path.endsWith(normalized) ? path + normalized : path;
    return url.toString().replace(/\/$/, "");
  }

  function urlsFrom(value, path = [], result = []) {
    if (!object(value) && !Array.isArray(value)) return result;
    for (const [key, child] of Object.entries(value)) {
      if (/code|download|secret|password|credential|environment|internal|intranet|console|doclink/i.test(key)) continue;
      const next = [...path, key];
      if (/^(triggerDesc|triggerConfig)$/i.test(key) && typeof child === "string") {
        try { urlsFrom(JSON.parse(child), next, result); } catch { /* Not every trigger description is JSON. */ }
      } else if (typeof child === "string" && /url|domain|endpoint|address/i.test(key) && !placeholder(child)) {
        try { const url = new URL(/^https?:\/\//i.test(child) ? child : `https://${child}`); if (url.hostname.includes(".") && !url.search && !url.hash) result.push(url.toString()); } catch { /* Not a URL field. */ }
      } else if (object(child) || Array.isArray(child)) urlsFrom(child, next, result);
    }
    return result;
  }

  function serviceUrl(info, context) {
    const preferred = info?.url?.system_url ?? info?.httpTrigger?.urlInternet ?? info?.externalDomain ?? info?.publicDomain;
    if (preferred) return preferred;
    const candidates = [...new Set(urlsFrom(info))];
    const https = candidates.filter(url => url.startsWith("https://"));
    const choices = https.length ? https : candidates;
    if (choices.length === 1) return choices[0];
    if (choices.length > 1) throw new Error(`${context}: multiple invocation URLs; set target.agentUrl explicitly`);
    if (info?.internalDomain) {
      console.error(`${context}: s info only returned internalDomain; the Server must be able to reach this private endpoint (override with target.agentUrl if needed)`);
      return info.internalDomain;
    }
    throw new Error(`${context}: info returned no invocation URL; set target.agentUrl explicitly`);
  }

  async function scfRecords(provider) {
    if (cache.has("qcloud")) return cache.get("qcloud");
    console.error("qcloud: scf info (all subdirectories)");
    const output = run("scf", ["info"], join(fcDir, "output", provider.output ?? "qcloud"));
    let yaml;
    for (const directory of (process.env.PATH ?? "").split(":")) {
      const executable = join(directory, "scf");
      if (!existsSync(executable)) continue;
      try { yaml = createRequire(await realpath(executable))("js-yaml"); break; } catch { /* Try another installation. */ }
    }
    if (!yaml) throw new Error("qcloud: cannot load js-yaml from the scf installation");
    const records = [], lines = output.split("\n");
    for (let i = 0; i < lines.length; i++) {
      const start = lines[i].match(/^(\s*)Output:\s*$/);
      if (!start) continue;
      const block = [lines[i]], indent = start[1].length;
      for (i++; i < lines.length; i++) {
        if (lines[i].trim() && lines[i].match(/^\s*/)[0].length <= indent) { i--; break; }
        block.push(lines[i]);
      }
      try { const data = yaml.load(block.join("\n"), { schema: yaml.JSON_SCHEMA }); if (object(data?.Output)) records.push(data.Output); }
      catch { throw new Error("qcloud: scf info output could not be parsed as YAML"); }
    }
    if (!records.length) throw new Error("qcloud: scf info returned no Output records");
    cache.set("qcloud", records);
    return records;
  }

  async function invocationUrl(providerName, provider, target, temp) {
    if (target.agentUrl) return target.agentUrl;
    const context = `${providerName}.${target.key}`;
    if (providerName === "azure") {
      const resourceGroup = renderTemplate(target.resourceGroup ?? provider.resourceGroup ?? "mtr-agents-{region}", target);
      const subscription = target.subscription ?? provider.subscription;
      if ([target.functionName, resourceGroup, subscription].some(value => value && placeholder(value))) throw new Error(`${context}: replace Azure deployment placeholders in the local targets file`);
      console.error(`${context}: az containerapp show`);
      const args = ["containerapp", "show", "--name", target.functionName, "--resource-group", resourceGroup,
        "--query", "{fqdn:properties.configuration.ingress.fqdn,location:location}", "--output", "json", "--only-show-errors"];
      if (subscription) args.push("--subscription", subscription);
      const result = run("az", args, fcDir, { missing: ["ResourceNotFound", "ResourceGroupNotFound", "ParentResourceNotFound", "ContainerAppNotFound"] });
      if (result === null) { console.error(`${context}: not deployed, skipping`); return null; }
      const app = parseJson(result, context);
      if (String(app.location).replace(/\s/g, "").toLowerCase() !== target.region || !app.fqdn) throw new Error(`${context}: missing hostname or Container App region mismatch`);
      return `https://${app.fqdn}`;
    }
    if (providerName === "aws") {
      console.error(`${context}: aws lambda get-function-url-config`);
      const args = ["lambda", "get-function-url-config", "--function-name", target.functionName,
        "--region", target.region, "--output", "json", "--no-cli-pager"];
      const profile = target.profile ?? provider.profile;
      if (profile) args.push("--profile", profile);
      const result = run("aws", args, fcDir, { missing: "ResourceNotFoundException" });
      if (result === null) {
        console.error(`${context}: no deployed Function URL, skipping`);
        return null;
      }
      const info = parseJson(result, context);
      if (info.AuthType !== "NONE" || info.InvokeMode !== "RESPONSE_STREAM") throw new Error(`${context}: Function URL must use NONE auth and RESPONSE_STREAM`);
      if (!info.FunctionUrl) throw new Error(`${context}: Lambda returned no Function URL`);
      return info.FunctionUrl;
    }
    if (providerName === "aliyun" || providerName === "ctyun") {
      const template = resolve(fcDir, "output", provider.output ?? `s.${providerName}.yaml`);
      await access(template);
      const output = join(temp, `${providerName}-${target.key}.json`);
      console.error(`${context}: s info`);
      run("s", [target.key, "info", "-t", template, "--output-format", "json", "--output-file", output, "--skip-actions"], dirname(template));
      const info = parseJson(await readFile(output, "utf8"), context);
      const record = info[target.key] ?? info;
      if (record.functionName && record.functionName !== target.functionName) throw new Error(`${context}: info returned another function`);
      return serviceUrl(record, context);
    }
    if (providerName === "qcloud") {
      const records = await scfRecords(provider);
      const matches = records.filter(record => record.functionName === target.functionName && (!record.region || record.region === target.region));
      if (!matches.length) { console.error(`${context}: not deployed, skipping`); return null; }
      if (matches.length !== 1) throw new Error(`${context}: ambiguous scf info result`);
      return serviceUrl(matches[0], context);
    }
    if (!cache.has("gcloud")) {
      console.error("gcloud: gcloud alpha run services list");
      const args = ["alpha", "run", "services", "list", "--format=json", "--quiet"];
      const project = provider.project ?? provider.projectId;
      if (project && !placeholder(project)) args.push(`--project=${project}`);
      const data = parseJson(run("gcloud", args), "gcloud services list");
      if (!Array.isArray(data)) throw new Error("gcloud: expected a service list");
      cache.set("gcloud", data);
    }
    const matches = cache.get("gcloud").filter(service => service.metadata?.name === target.functionName && service.metadata?.labels?.["cloud.googleapis.com/location"] === target.region);
    if (!matches.length) { console.error(`${context}: not deployed, skipping`); return null; }
    if (matches.length !== 1 || !matches[0].status?.url) throw new Error(`${context}: missing or ambiguous service URL`);
    return matches[0].status.url;
  }

  function huaweiProject(provider, target) {
    const configured = target.projectId ?? provider.projectId;
    if (!placeholder(configured)) {
      if (!/^[A-Za-z0-9]{32}$/.test(configured)) throw new Error(`${target.key}: invalid projectId`);
      return configured;
    }
    const profile = target.profile ?? provider.profile;
    const key = `huawei:${profile ?? "default"}:${target.region}`;
    if (!cache.has(key)) {
      console.error(`huaweicloud.${target.key}: resolving IAM project`);
      const args = ["IAM", "KeystoneListProjects/v3", `--name=${target.region}`, `--cli-region=${target.region}`, "--cli-output=json"];
      if (profile) args.push(`--cli-profile=${profile}`);
      const data = parseJson(run("hcloud", args), target.region);
      if (!Array.isArray(data.projects) || data.links?.next) throw new Error(`${target.region}: incomplete IAM project list`);
      const projects = data.projects.filter(project => project.name === target.region && project.enabled !== false);
      if (projects.length > 1 || projects.length && !/^[A-Za-z0-9]{32}$/.test(projects[0].id)) throw new Error(`${target.region}: ambiguous or invalid project`);
      cache.set(key, projects[0]?.id ?? null);
    }
    return cache.get(key);
  }

  const temp = await mkdtemp(join(tmpdir(), "mtr-outbound-info-"));
  try {
    const rows = [], guards = new Map(), ids = new Set();
    for (const providerName of selected) {
      const provider = config.providers[providerName];
      if (!Array.isArray(provider.targets)) throw new Error(`${providerName}: targets must be an array`);
      for (const target of provider.targets) {
        if (regions && !regions.has(target.region)) continue;
        if (!/^[A-Za-z0-9_.-]+$/.test(target.id ?? "") || ids.has(target.id)) throw new Error(`Invalid or duplicate agent ID: ${target.id}`);
        ids.add(target.id);
        const context = `${providerName}.${target.key}`;
        const source = sources[providerName] ?? null;
        let url, project = null, auth = null;
        const transport = providerName === "huaweicloud" ? "event" : "http";
        if (transport === "event") {
          project = huaweiProject(provider, target);
          if (!project) { console.error(`${context}: no enabled project, skipping`); continue; }
          const urn = `urn:fss:${target.region}:${project}:function:${target.package ?? provider.package ?? "default"}:${target.functionName}:latest`;
          url = `https://functiongraph.${target.region}.myhuaweicloud.com/v2/${project}/fgs/functions/${encodeURIComponent(urn)}/invocations`;
          if (!source) throw new Error(`${context}: select a Huawei AK/SK source row with --source-id`);
        } else {
          const env = mergedEnv(providerName, provider, target);
          if (!source) auth = httpAuth(env, context);
          const base = await invocationUrl(providerName, provider, target, temp);
          if (base === null) continue;
          url = httpUrl(base, env.MTR_HTTP_PATH_PREFIX, context);
        }
        if (source) guards.set(source, transport === "event" ? "huaweicloud_aksk" : "http");
        rows.push(`    (${[literal(target.id), literal(url), literal(transport), project ? literal(project) : "NULL::text", json(auth), source ? literal(source) : "NULL::text", json(target.labels), json(target.tls)].join(", ")})`);
      }
    }
    if (!rows.length) throw new Error("No targets resolved; no SQL file generated");
    const guardSql = [...guards].map(([source, type]) => `  IF NOT EXISTS (SELECT 1 FROM outbound_agents WHERE id = ${literal(source)} AND ${type === "http" ? "auth->>'type' IN ('header', 'bearer')" : "auth->>'type' = 'huaweicloud_aksk' AND COALESCE(auth->>'access_key', '') <> ''"} AND COALESCE(auth->>'secret', '') <> '') THEN
      RAISE EXCEPTION 'Missing or invalid source agent: %', ${literal(source)};
    END IF;`).join("\n");
    const sql = `-- Generated from ${resolve(options.config).replace(/[\r\n]/g, " ")}
  -- Contains HTTP tokens when read from the targets file. Keep this file private.
  -- Existing IDs are skipped. Cloud resources and the database are not modified by the generator.
  BEGIN;
  ${guards.size ? `DO $mtr_guard$\nBEGIN\n${guardSql}\nEND;\n$mtr_guard$;\n` : ""}
  WITH targets(id, url, transport, project_id, auth, source_id, labels, tls) AS (
    VALUES
  ${rows.join(",\n")}
  )
  INSERT INTO outbound_agents (
    id, transport, enabled, url, headers, auth, event_response, labels, tls, created_at, updated_at
  )
  SELECT target.id, target.transport, TRUE, target.url,
    CASE WHEN target.transport = 'event' THEN '{"X-CFF-Request-Version":"v1"}'::jsonb ELSE '{}'::jsonb END,
    CASE WHEN target.transport = 'event' THEN jsonb_build_object(
      'type', 'huaweicloud_aksk', 'access_key', source.auth->>'access_key',
      'secret', source.auth->>'secret', 'project_id', target.project_id
    ) ELSE COALESCE(target.auth, source.auth) END,
    CASE WHEN target.transport = 'event' THEN 'result_field' ELSE '' END,
    COALESCE(target.labels, source.labels, '[]'::jsonb), COALESCE(target.tls, source.tls, '{}'::jsonb), NOW(), NOW()
  FROM targets AS target LEFT JOIN outbound_agents AS source ON source.id = target.source_id
  WHERE TRUE
  ON CONFLICT (id) DO NOTHING
  RETURNING id;
  COMMIT;
  `;
    await mkdir(dirname(resolve(options.output)), { recursive: true });
    await writeFile(resolve(options.output), sql, { mode: 0o600, flag: "wx" });
    console.error(`Generated ${rows.length} rows: ${resolve(options.output)} (existing IDs will be skipped)`);
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
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
    value.forEach((item, index) => {
      const nextPath = [...path, String(index)];
      if (
        typeof item === "string" &&
        isSensitivePublicPath(nextPath) &&
        !isAllowedPublicPlaceholder(item, nextPath)
      ) {
        findings.push(nextPath.join("."));
        return;
      }
      collectSensitiveValues(item, nextPath, findings);
    });
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
      !isAllowedPublicPlaceholder(item, nextPath)
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
  if (key === "projectId" || key === "agencyName") {
    return true;
  }
  if (path[0] === "providers" && path[1] === "aws" && ["roleArn", "imageUrl", "agentUrl", "profile"].includes(key)) return true;
  if (path[0] === "providers" && path[1] === "azure" && ["subscription", "agentUrl"].includes(key)) return true;
  if (isHuaweicloudPublicPath(path)) {
    const parentKey = path[path.length - 2] ?? "";
    if (
      huaweicloudSensitivePublicKeys.has(key) ||
      huaweicloudSensitivePublicKeys.has(parentKey)
    ) {
      return true;
    }
  }
  if (key === "logProjectId" || key === "logProjectID") {
    return true;
  }
  return false;
}

function isHuaweicloudPublicPath(path) {
  const providersIndex = path.indexOf("providers");
  return providersIndex >= 0 && path[providersIndex + 1] === "huaweicloud";
}

function isAllowedPublicPlaceholder(value, path) {
  if (isPlaceholderValue(value)) {
    return true;
  }
  if (path[0] === "providers" && path[1] === "aws" && /<[^<>]+>/.test(value)) return true;
  if (path[0] === "providers" && path[1] === "azure" && /<[^<>]+>/.test(value)) return true;
  const key = path[path.length - 1] ?? "";
  return (
    isHuaweicloudPublicPath(path) &&
    key === "imageUrl" &&
    /\/<[^<>/]+>\//.test(value)
  );
}

function isPlaceholderValue(value) {
  const text = value.trim();
  return text === "" || /^\/?<[^<>]+>$/.test(text);
}
