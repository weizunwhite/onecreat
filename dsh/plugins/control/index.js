/**
 * onecreat-control —— OneCreat 在 dsh SDK JSON-RPC wire 上的控制面插件。
 *
 * 官方 `@deepseek-ai/dsh-sdk-jsonrpc-server` 只有 initialize / session/prompt /
 * shutdown 三个方法,**没有取消、审批(dead capability)、计划模式、resume**
 * (见 docs/dsh调研/01 §3/§6)。而 JsonRpcLineTransport 的 onRequest/onNotification
 * 都是"后注册覆盖前一个"的单一处理器,所以插件没法往官方 server 上"追加方法"。
 *
 * 因此本插件**自己拥有 stdio 传输**:内部实例化官方的 HarnessSdkJsonRpcServer
 * (复用它的 initialize/prompt/shutdown 与四种通知),再在同一条 wire 上补上
 * OneCreat 自己的方法与通知。profile 里因此**不再加载** dsh-sdk-jsonrpc-server
 * 这一行(否则两个 transport 抢 stdin)。
 *
 * 补上的能力:
 *   - `onecreat/session.cancel`   取消当前 turn(agent.cancel,不再靠杀进程)
 *   - `onecreat/planMode.set`     驱动 dsh-plan-mode
 *   - `onecreat/session.load`     从持久化恢复会话(resume)并回传消息投影
 *   - `onecreat/session.history`  取某会话的消息投影(供前端 History)
 *   - `onecreat/inject`           每轮运行时状态注入(板卡事实/人设/记忆)
 *   - 审批桥:`onecreat/approval.request`(通知)↔ `onecreat/approval.resolve`(通知)
 *   - 工具桥:`onecreat/tool.invoke`(通知)↔ `onecreat/tool.result`(通知),
 *     把 Go 侧内置工具(complete_step —— 证据引擎的诚实性闸门)暴露给模型
 *   - 预执行钩子:`onecreat/tool.preExecute` ↔ `onecreat/tool.preExecute.done`,
 *     让 Go 侧在写操作前做文件快照(checkpoint/rewind)
 *   - 可选按需挂载 OneCreat 硬件 MCP(env ONECREAT_HARDWARE_MCP)
 *
 * stdout 只走协议帧,诊断一律 stderr。
 *
 * @module onecreat-dsh/plugins/control
 */

import { existsSync } from 'node:fs'
import { randomUUID } from 'node:crypto'
import { JsonRpcLineTransport } from '@deepseek-ai/dsh-sdk-protocol'
import { HarnessSdkJsonRpcServer } from '@deepseek-ai/dsh-sdk-jsonrpc-server'
import { createUserMessage } from '@deepseek-ai/dsh-llm'
import { SessionId } from '@deepseek-ai/dsh-session'
import { defineTool } from '@deepseek-ai/dsh-tools'
import * as McpClient from '@deepseek-ai/dsh-mcp-client'
// 只为把 Context/Events 的类型增补并进本模块的类型图(approval/planMode 服务与事件)。
/** @typedef {import('@deepseek-ai/dsh-user-approval').ApprovalOutcome} ApprovalOutcome */
/** @typedef {import('@deepseek-ai/dsh-plan-mode')} PlanModeModule */

export const name = 'onecreat-control'
// agents 是硬依赖;tools/approval/planMode 用 ctx.inject 可选挂载。
export const inject = ['agents']

/** Go 侧要求审批的工具名前缀/全名(与 native 的写操作口径对齐)。 */
const APPROVAL_TOOLS = new Set([
  'write', 'edit', 'create', 'str_replace_editor', 'bash', 'run_command',
])

/**
 * 判断一个工具调用是否需要过审批。写文件、执行命令、以及硬件 MCP 里的
 * 烧录/上传类操作都要问用户;只读工具直接放行。
 * @param {string} toolName - dsh 侧的工具名。
 * @returns {boolean} 需要审批为 true。
 */
function needsApproval(toolName) {
  if (APPROVAL_TOOLS.has(toolName)) return true
  if (toolName.startsWith('mcp__')) {
    return /upload|flash|write|install|erase|deploy|ota/i.test(toolName)
  }
  return false
}

/**
 * 把一条 dsh 消息压成纯文本(前端 History 只需要文本)。
 * @param {{content?: any[]}} message - dsh 消息。
 * @returns {string} 拼接后的文本。
 */
function messageText(message) {
  const blocks = message?.content ?? []
  let out = ''
  for (const block of blocks) {
    if (block && block.type === 'text' && typeof block.text === 'string') out += block.text
  }
  return out
}

/**
 * 从会话事件日志投影出"角色 + 文本"的消息列表,供 Go 侧 History 用。
 * 只取 user/message 与 assistant/message,不含工具噪声,也不含
 * request/header、request/context(那两个含真实 provider/model,红线)。
 * @param {{events?: readonly any[]}} session - dsh 会话。
 * @returns {{role: string, content: string}[]} 消息投影。
 */
function projectMessages(session) {
  const out = []
  for (const event of session?.events ?? []) {
    if (event.type === 'user/message') {
      const text = messageText(event.data?.message)
      if (text !== '') out.push({ role: 'user', content: text })
    } else if (event.type === 'assistant/message') {
      const text = messageText(event.data?.message)
      if (text !== '') out.push({ role: 'assistant', content: text })
    }
  }
  return out
}

/**
 * 装配 OneCreat 控制面。
 * @param {import('@deepseek-ai/cordis').Context} ctx - cordis 上下文。
 * @param {{maxTokensAsSuccess?: boolean}} config - 插件配置。
 * @returns {void}
 */
export function apply(ctx, config) {
  const resolved = config ?? {}
  const rootFiber = ctx.root.fiber
  const transport = new JsonRpcLineTransport(process.stdin, process.stdout)
  const sdk = new HarnessSdkJsonRpcServer(ctx, transport, {
    maxTokensAsSuccess: resolved.maxTokensAsSuccess === true,
  })

  /** 由 onecreat/session.load 恢复出来的会话(SDK server 不认识它们)。 */
  const resumedSessions = new Map()
  /** 等待 Go 侧应答的桥接请求:id → { resolve, reject }。 */
  const waiters = new Map()

  /**
   * 发一条通知给 Go 并等它的应答通知。Go 侧按 id 配对回复。
   * @param {string} method - 出站通知名。
   * @param {object} params - 通知载荷(会自动补 id)。
   * @param {number} timeoutMs - 超时毫秒;0 表示不超时。
   * @param {AbortSignal=} signal - 取消信号(turn 被取消时撤回问题)。
   * @returns {Promise<any>} Go 侧回传的载荷。
   */
  function ask(method, params, timeoutMs, signal) {
    const id = `oc_${randomUUID().replaceAll('-', '')}`
    return new Promise((resolve, reject) => {
      /** @type {ReturnType<typeof setTimeout> | undefined} */
      let timer
      /**
       * @param {(value: any) => void} fn - 最终要调用的 resolve/reject。
       * @returns {(value: any) => void} 清理计时器与监听后再落地的包装。
       */
      const settle = (fn) => (value) => {
        waiters.delete(id)
        if (timer !== undefined) clearTimeout(timer)
        if (signal !== undefined) signal.removeEventListener('abort', onAbort)
        fn(value)
      }
      const done = settle(resolve)
      const fail = settle(reject)
      const onAbort = () => { fail(new Error('onecreat: 请求已被取消')) }
      waiters.set(id, { resolve: done, reject: fail })
      if (signal !== undefined) {
        if (signal.aborted) { fail(new Error('onecreat: 请求已被取消')); return }
        signal.addEventListener('abort', onAbort, { once: true })
      }
      if (timeoutMs > 0) {
        timer = setTimeout(() => { fail(new Error(`onecreat: ${method} 等待 Go 侧应答超时`)) }, timeoutMs)
      }
      transport.notify(method, { id, ...params })
    })
  }

  /**
   * 取一个活着的 agent(SDK 建的或我们 resume 出来的)。
   * @param {string} sessionId - 会话 id。
   * @returns {any} agent,或 undefined。
   */
  function agentOf(sessionId) {
    const resumedHandle = resumedSessions.get(sessionId)
    if (resumedHandle !== undefined) return resumedHandle.agent
    return ctx.agents.get(SessionId(sessionId))
  }

  // ---- 审批桥:成为终端 answerer,把问题转给 Go 侧前端 ----
  ctx.inject(['approval'], (approvalCtx) => {
    approvalCtx.on('approval/request', async (/** @type {any} */ req, /** @type {any} */ _next) => {
      try {
        const reply = await ask('onecreat/approval.request', {
          sessionId: String(req.agent.session.id),
          toolName: req.toolName,
          ...(req.callId === undefined ? {} : { callId: String(req.callId) }),
          ...(req.reason === undefined ? {} : { reason: req.reason }),
        }, 0, req.signal)
        return reply?.allow === true ? 'allowed-once' : 'rejected'
      } catch {
        // 取消或通道断开:按 dsh 语义settle 成 cancelled(失败关闭)。
        return 'cancelled'
      }
    })
  })

  // ---- 工具桥:把 Go 侧的 complete_step(证据引擎闸门)暴露给模型 ----
  ctx.inject(['tools'], (toolCtx) => {
    toolCtx.tools.register(defineTool({
      name: 'complete_step',
      description: 'Record the evidence-backed completion of ONE step of an approved plan. '
        + 'Call it as you finish each step instead of silently moving on: it signs the step off with '
        + 'PROOF it is done — the verification you ran (command + result), the diff/files you changed, '
        + 'or a manual check. A completion with no evidence is REJECTED, so do not claim a step is done '
        + 'until you can show why. Keep the task list moving with todo_write; use complete_step for the '
        + 'formal, evidenced sign-off of the finished one.',
      parameters: {
        step: {
          type: 'string',
          required: true,
          description: 'Which plan step this completes — its title or number, matching the task list.',
        },
        result: {
          type: 'string',
          required: true,
          description: 'What is now true or changed as a result of finishing this step.',
        },
        evidence: {
          type: 'array',
          required: true,
          description: 'Proof the step is done. At least one item is required.',
          items: {
            type: 'object',
            additionalProperties: false,
            properties: {
              kind: {
                type: 'string',
                required: true,
                enum: ['verification', 'diff', 'files', 'manual'],
                description: 'verification = a command/test was run; diff = a concrete code change; '
                  + 'files = files created/edited/inspected; manual = a manual check.',
              },
              summary: {
                type: 'string',
                required: true,
                description: 'The evidence itself: the test result, what the diff does, or what was confirmed.',
              },
              command: { type: 'string', description: 'The command run, for verification evidence.' },
              paths: { type: 'array', items: { type: 'string' }, description: 'Files this evidence refers to.' },
            },
          },
        },
        notes: { type: 'string', description: 'Optional caveats, follow-ups, or anything deferred.' },
      },
      output: {
        schema: {
          type: 'object',
          additionalProperties: false,
          properties: { text: { type: 'string', required: true } },
        },
        render: (_args, value) => [{ type: 'text', text: value.text }],
      },
      async execute(args, exec) {
        const reply = await ask('onecreat/tool.invoke', {
          sessionId: exec.agent === undefined ? '' : String(exec.agent.session.id),
          name: 'complete_step',
          arguments: JSON.stringify(args),
        }, 0, exec.signal)
        if (typeof reply?.error === 'string' && reply.error !== '') throw new Error(reply.error)
        return { text: typeof reply?.output === 'string' ? reply.output : '' }
      },
      presentCall: args => ({ card: 'generic', title: '签收步骤', kind: 'other', rawInput: args.step }),
    }))

    // 预执行钩子:写操作前让 Go 侧做文件快照(checkpoint/rewind 保留 Go 实现)。
    toolCtx.on('tools/pre-execute', async (exec, next) => {
      try {
        await ask('onecreat/tool.preExecute', {
          sessionId: exec.agent === undefined ? '' : String(exec.agent.session.id),
          name: exec.name,
          arguments: JSON.stringify(exec.arguments ?? {}),
        }, 5000, exec.signal)
      } catch {
        // 快照失败/超时不阻塞工具执行(fail-open):checkpoint 是增值能力,
        // 不是安全闸门;安全闸门是下面的审批。
      }
      if (needsApproval(exec.name)) return { kind: 'ask', reason: `工具 ${exec.name} 会修改环境,需要用户确认` }
      return next()
    })
  })

  // ---- 按需挂载 OneCreat 硬件 MCP(工具名 mcp__hardware__*) ----
  const hardwareBin = process.env.ONECREAT_HARDWARE_MCP ?? ''
  if (hardwareBin !== '' && existsSync(hardwareBin)) {
    void ctx.plugin(McpClient, {
      serverName: 'hardware',
      transport: 'stdio',
      command: hardwareBin,
      args: [],
      env: {},
      toolCallTimeoutMs: 600000,
      failOnStartupError: false,
      cwd: process.env.DSH_CWD ?? process.cwd(),
    })
  }

  // ---- 请求分派:先看 OneCreat 自己的方法,再落到官方 SDK server ----
  let exitTask
  const disposeAndExit = () => {
    exitTask ??= (async () => {
      await Promise.allSettled([Promise.resolve().then(() => transport.flush())])
      await Promise.allSettled([Promise.resolve().then(() => rootFiber.dispose())])
      process.exit(0)
    })()
    return exitTask
  }

  transport.onRequest(async (method, params) => {
    switch (method) {
      case 'onecreat/session.cancel': {
        const agent = agentOf(String(params.sessionId ?? ''))
        if (agent === undefined) return { cancelled: false }
        agent.cancel({ kind: 'user' })
        return { cancelled: true }
      }
      case 'onecreat/planMode.set': {
        const agent = agentOf(String(params.sessionId ?? ''))
        if (agent === undefined) throw new Error('planMode.set: 会话不存在')
        const planMode = ctx.get('planMode')
        if (planMode === undefined) throw new Error('planMode.set: 未组合 dsh-plan-mode')
        const outcome = planMode.set(agent, params.active === true)
        return { outcome: String(outcome) }
      }
      case 'onecreat/inject': {
        const agent = agentOf(String(params.sessionId ?? ''))
        if (agent === undefined) throw new Error('inject: 会话不存在')
        const text = String(params.text ?? '')
        if (text !== '') {
          agent.inject(createUserMessage({
            content: [{ type: 'text', text }],
            source: { kind: 'user' },
          }))
        }
        return { injected: text !== '' }
      }
      case 'onecreat/session.load': {
        const sessionId = String(params.sessionId ?? '')
        const existing = agentOf(sessionId)
        if (existing !== undefined) return { messages: projectMessages(existing.session) }
        const handle = await ctx.agents.resume({ resumeSessionId: SessionId(sessionId) })
        resumedSessions.set(sessionId, handle)
        return { messages: projectMessages(handle.agent.session) }
      }
      case 'onecreat/session.history': {
        const agent = agentOf(String(params.sessionId ?? ''))
        if (agent === undefined) return { messages: [] }
        return { messages: projectMessages(agent.session) }
      }
      case 'session/prompt': {
        const sessionId = String(params.sessionId ?? '')
        const handle = resumedSessions.get(sessionId)
        if (handle !== undefined) {
          const message = createUserMessage({
            content: /** @type {any} */ (params.contentBlocks ?? []),
            source: { kind: 'user' },
          })
          handle.agent.followup(message)
          return { messageId: String(message.id) }
        }
        return sdk.handleRequest(method, params)
      }
      case 'shutdown': {
        const result = await sdk.handleRequest(method, params)
        setImmediate(() => { void disposeAndExit() })
        return result
      }
      default:
        return sdk.handleRequest(method, params)
    }
  })

  // ---- 通知分派:Go 侧对桥接请求的应答 ----
  transport.onNotification((method, params) => {
    switch (method) {
      case 'onecreat/approval.resolve':
      case 'onecreat/tool.result':
      case 'onecreat/tool.preExecute.done': {
        const waiter = waiters.get(String(params.id ?? ''))
        if (waiter !== undefined) waiter.resolve(params)
        break
      }
      default:
        break
    }
  })

  ctx.effect(() => {
    transport.start()
    return async () => {
      for (const waiter of waiters.values()) waiter.reject(new Error('onecreat: 控制面已关闭'))
      waiters.clear()
      for (const handle of resumedSessions.values()) {
        await Promise.resolve().then(() => handle.dispose()).catch(() => {})
      }
      resumedSessions.clear()
      await sdk.shutdown()
      transport.close()
    }
  }, 'onecreat.control.serve')
}
