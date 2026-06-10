# Codex 借鉴清单(对照 onecreat)

> 来源:openai/codex(Apache-2.0,Rust),浅克隆在 `~/Desktop/codex`。
> 原则:**不换底座、不整抄**——Codex 当"开源参考答案",按教培场景挑着抄。
> 盘点日期:2026-06-08。四路并行盘点 + 人工复核(剔除了 2 处 agent 误判)。

## 一句话结论

Codex 最值得抄的不是某个功能,而是**「简化哲学」**:把专业旋钮折叠成少数语义化档位
(approval_policy × sandbox_mode),默认值即安全。这正是"老师零配置"该有的形态。

---

## 第一批:高价值 + 低成本(建议先做)

### 1. 安全档位语义化(把 allow/deny 专家规则折叠成三档)⭐ 核心
- **Codex 做法**:`approval_policy`(untrusted/on-request/never)× `sandbox_mode`
  (read-only/workspace-write/danger-full-access)二维组合,语义固定,不需要用户写规则。
  - 证据:`codex-rs/protocol/src/protocol.rs`(AskForApproval)、
    `codex-rs/protocol/src/config_types.rs:86`(SandboxMode)
- **onecreat 对照**:有 allow/deny 规则 + YOLO,但那是"配