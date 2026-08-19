/**
 * onecreat-gateway —— OneCreat 自命名的 OpenAI 兼容 provider 适配器。
 *
 * 为什么需要它(网关红线):dsh 内置的 `dsh-llm-deepseek` 把路由名硬编码成
 * `deepseek-official`,凡日志/事件/错误体出现 provider 字段就暴露"底层是
 * DeepSeek"这一品牌事实 —— 而 OneCreat 对老师只暴露档位(标准/高级/旗舰)。
 * 纯配置藏不住,所以这里注册一个自己命名的路由 `onecreat-gateway`。
 *
 * 实现上**复用**官方适配器的传输实现(DeepSeekAdapter 是一个 OpenAI 兼容
 * chat-completions 客户端),只换掉路由名与连接事实来源:
 *   - base URL 从环境变量读(网关地址是机密,绝不写进 profile 文件);
 *   - api key 从环境变量读(网关 token / 直连 key,同样不落盘);
 *   - model catalog 关掉(`models: []`),不向任何 UI 广播模型目录。
 * wire 上的 model 由 Go 驱动层每会话下发(网关模式下是档位占位符)。
 *
 * @module onecreat-dsh/plugins/gateway
 */

import { DeepSeekAdapter, resolveAdapterOptions } from '@deepseek-ai/dsh-llm-deepseek'

/** OneCreat 对外唯一的 provider 路由名。真实厂商名绝不出现在这里。 */
export const PROVIDER = 'onecreat-gateway'

/**
 * 连接事实的默认环境变量名。control 插件轮换凭证(onecreat/credentials.set)时写的
 * 就是它们,Go 侧 internal/engine/dsh 的 envDSHBaseURL / envDSHAPIKey 也是这两个 ——
 * 三处必须一致,所以在这里给出唯一定义。
 */
export const DEFAULT_BASE_URL_ENV = 'ONECREAT_DSH_BASE_URL'
/** 见 DEFAULT_BASE_URL_ENV。 */
export const DEFAULT_API_KEY_ENV = 'ONECREAT_DSH_API_KEY'

export const name = 'onecreat-gateway'
export const inject = ['llm']

/**
 * 解析连接事实用的环境变量名。baseURL/apiKey 的**值**一律在请求期才读,取不到就让
 * 请求期报错(而不是启动期崩),这样"没配凭证"不会让整个 sidecar 起不来,也让
 * Go 侧能在会话中途轮换 token / base URL 而不重启 sidecar。
 * @param {{baseURLEnv?: string, apiKeyEnv?: string}} config - profile 里的插件配置。
 * @returns {{baseURLEnv: string, apiKeyEnv: string}} 解析后的环境变量名。
 */
function readEnvFacts(config) {
  const baseURLEnv = config.baseURLEnv ?? DEFAULT_BASE_URL_ENV
  const apiKeyEnv = config.apiKeyEnv ?? DEFAULT_API_KEY_ENV
  return { baseURLEnv, apiKeyEnv }
}

/**
 * 注册 onecreat-gateway 路由。
 * @param {import('@deepseek-ai/cordis').Context} ctx - 载入本插件的 cordis 上下文。
 * @param {{baseURLEnv?: string, apiKeyEnv?: string}} config - 插件配置。
 * @returns {void}
 */
export function apply(ctx, config) {
  const facts = readEnvFacts(config ?? {})
  // 每次请求都重新读环境(允许 Go 侧在会话中途轮换 token / base URL 而不重启 sidecar)。
  const options = () => resolveAdapterOptions({
    ...((process.env[facts.baseURLEnv] ?? '') === '' ? {} : { baseURL: process.env[facts.baseURLEnv] }),
    // 关闭 model catalog 广播:任何 UI/事件都拿不到模型目录。
    models: [],
    thinking: 'enabled',
    reasoningEffort: 'max',
  })
  const resolveApiKey = async () => {
    const key = process.env[facts.apiKeyEnv] ?? ''
    if (key === '') {
      // 错误体里只出现 OneCreat 自己的名字与环境变量名,不带厂商品牌。
      throw new Error(`onecreat-gateway: 缺少凭证,请在环境变量 ${facts.apiKeyEnv} 中提供`)
    }
    return key
  }
  // 归因用的匿名用户 id:固定值,避免官方实现往磁盘写 anonymous-user-id 文件。
  const resolveUserId = () => /** @type {any} */ ('onecreat')
  const adapter = new DeepSeekAdapter({ options, resolveApiKey, resolveUserId })
  ctx.llm.registerAdapter([PROVIDER], adapter)
}
