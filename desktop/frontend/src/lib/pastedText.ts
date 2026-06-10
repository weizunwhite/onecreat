export const LONG_PASTE_MIN_CHARS = 2000;
export const LONG_PASTE_MIN_LINES = 20;

export type PastedTextBlock = {
  label: string;
  lines: number;
  remark: string;
  text: string;
};

export function lineCount(s: string): number {
  if (s === "") return 0;
  return s.split(/\r\n|\r|\n/).length;
}

export function shouldFoldPastedText(s: string): boolean {
  return s.length >= LONG_PASTE_MIN_CHARS || lineCount(s) >= LONG_PASTE_MIN_LINES;
}

export function createPastedTextBlock(id: number, text: string, remark: string): PastedTextBlock {
  return {
    label: `pasted_text_${String(id).padStart(2, "0")}.md`,
    lines: lineCount(text),
    remark,
    text,
  };
}

export function renderPastedTextBlock(block: PastedTextBlock, lineMeta = `${block.lines} lines`): string {
  return `${block.label} · ${block.remark} · ${lineMeta}\n\n--- Begin ${block.label} ---\n${block.text}\n--- End ${block.label} ---`;
}
