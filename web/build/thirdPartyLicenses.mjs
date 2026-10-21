// 收集实际打入前端产物的 npm 依赖许可文本。
import { existsSync, readFileSync, readdirSync, realpathSync, statSync } from 'node:fs';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const licenseName = /^(license|licence|copying|notice|patents)(?:[._-].*)?$/i;
const nodeModulesPath = /(?:^|[\\/])node_modules(?:[\\/]|$)/i;

// 在模块路径中定位最近的 npm 包清单。
function packageForModule(moduleId) {
  const cleanId = moduleId.split('?')[0];
  if (!cleanId || cleanId.startsWith('\0') || !nodeModulesPath.test(cleanId)) return null;

  let modulePath = cleanId.startsWith('file:') ? fileURLToPath(cleanId) : cleanId;
  try {
    modulePath = realpathSync(modulePath);
  } catch {
    modulePath = resolve(modulePath);
  }

  let directory = dirname(modulePath);
  while (directory !== dirname(directory)) {
    const manifestPath = join(directory, 'package.json');
    if (existsSync(manifestPath)) {
      const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
      if (typeof manifest.name === 'string' && typeof manifest.version === 'string') {
        return { directory, manifest };
      }
    }
    directory = dirname(directory);
  }

  throw new Error(`找不到 npm 包清单：${moduleId}`);
}

// 确认每个进入浏览器产物的依赖都带有许可文本。
function licenseSources(packageInfo) {
  const { directory } = packageInfo;
  const sources = readdirSync(directory)
    .filter((name) => licenseName.test(name))
    .filter((name) => statSync(join(directory, name)).isFile())
    .sort()
    .map((name) => ({ name, path: join(directory, name) }));

  if (sources.length > 0) return sources;
  throw new Error(`npm 包缺少许可文本：${packageInfo.key}`);
}

// 兼容 npm 清单中的字符串和旧式许可对象。
function packageLicense(manifest) {
  if (typeof manifest.license === 'string') return manifest.license.trim();
  if (typeof manifest.license?.type === 'string') return manifest.license.type.trim();
  if (Array.isArray(manifest.licenses)) {
    return manifest.licenses.map((item) => item.type).filter(Boolean).join(' OR ');
  }
  return '';
}

// 生成只包含实际出现在浏览器构建产物中的依赖清单。
export default function thirdPartyLicenses() {
  return {
    name: 'filesage-third-party-licenses',
    apply: 'build',
    generateBundle(_options, bundle) {
      const packages = new Map();
      for (const output of Object.values(bundle)) {
        const moduleIds = output.type === 'chunk'
          ? Object.keys(output.modules)
          : output.originalFileNames ?? [];
        for (const moduleId of moduleIds) {
          const packageInfo = packageForModule(moduleId);
          if (!packageInfo) continue;

          const { manifest } = packageInfo;
          const key = `${manifest.name}@${manifest.version}`;
          if (!packages.has(key)) packages.set(key, { ...packageInfo, key });
        }
      }

      if (packages.size === 0) throw new Error('未找到前端产物中的 npm 依赖');

      const entries = [...packages.values()].sort((left, right) => (
        left.key < right.key ? -1 : left.key > right.key ? 1 : 0
      ));
      const notices = [
        '# Bundled npm dependencies',
        '',
        'This list covers npm packages included in the generated browser assets. License files are in `LICENSES/npm/`.',
        '',
      ];

      for (const packageInfo of entries) {
        const { manifest, key } = packageInfo;
        const license = packageLicense(manifest);
        if (!license) throw new Error(`npm 包缺少许可标识：${key}`);

        const directoryName = key.replaceAll('/', '+').replace(/[^a-zA-Z0-9@._+-]/g, '_');
        const destination = `LICENSES/npm/${directoryName}`;
        const sources = licenseSources(packageInfo);
        for (const source of sources) {
          if (!existsSync(source.path)) throw new Error(`许可文本不存在：${source.path}`);
          this.emitFile({
            type: 'asset',
            fileName: `${destination}/${basename(source.name)}`,
            source: readFileSync(source.path),
          });
        }

        notices.push(`- \`${key}\` — \`${license}\`; files: \`${destination}/\`.`);
      }

      this.emitFile({
        type: 'asset',
        fileName: 'THIRD_PARTY_NOTICES.md',
        source: `${notices.join('\n')}\n`,
      });
    },
  };
}
