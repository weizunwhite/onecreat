// streamingDisplayText 把「流式生成中」的 Markdown 源码去掉标记符号,显示成干净文字:
// 隐藏 **加粗** *斜体* `代码` # 标题 > 引用 [链接](url) 以及 ``` 代码围栏,
// 代码块内容原样保留(不剥离里面的符号)。
//
// 为什么这样做:完整 Markdown 渲染(react-markdown + 代码高亮 + KaTeX)边流式边跑会
// 每个 token 重解析+重高亮,卡顿抖动。所以流式过程中只做这一层轻量「去标记」,
// 让学生/老师看到的是干净文字而不是一串 ** # ` 符号;生成结束后再由 <Markdown>
// 一次性排成正式格式。因为是临时显示,这里对半截标记(只来了开头的 ** 还没闭合)
// 也尽量抹掉,避免边打字边冒符号。
export function streamingDisplayText(raw: string): string {
  const lines = raw.split("\n");
  let inFence = false;
  const out: string[] = [];
  for (const line of lines) {
    if (/^\s*(```|~~~)/.test(line)) {
      inFence = !inFence; // 进入/离开代码围栏
      continue; // 隐藏围栏标记行本身
    }
    out.push(inFence ? line : stripInlineMarks(line));
  }
  return out.join("\n");
}

function stripInlineMarks(line: string): string {
  let s = line;
  s = s.replace(/^\s{0,3}#{1,6}\s+/, ""); // 标题 #### 文本 -> 文本
  s = s.replace(/^\s{0,3}>\s?/, ""); // 引用 > 文本 -> 文本
  s = s.replace(/^(\s*)[-*+]\s+/, "$1• "); // 无序列表 - / * / + -> 圆点
  s = s.replace(/\[([^\]]+)\]\([^)]*\)/g, "$1"); // 链接 [文字](url) -> 文字
  s = s.replace(/\*\*([^*]+)\*\*/g, "$1"); // **加粗** -> 加粗
  s = s.replace(/__([^_]+)__/g, "$1"); // __加粗__ -> 加粗
  s = s.replace(/\*([^*\n]+)\*/g, "$1"); // *斜体* -> 斜体
  s = s.replace(/`([^`]+)`/g, "$1"); // `行内代码` -> 行内代码
  s = s.replace(/\*\*?|`/g, ""); // 半截/残留的孤立标记
  return s;
}
