# golden_keys.json — 题名归一化 key 黄金样本(M1 启动条件 S1-3)

**这是什么**:129 条「题名原文 → 历史归一化 key」的对照表。历史 key 现有三份实现,迁移必须逐字节一致,
否则历史题卡会全部当成新题(见 `docs/科创工作流迁移/调研/01_NASApp科创工作流现状.md` §2.1② / §6.4①)。

**怎么生成的**:用 python3 直接 `import` 参考实现 `课题/99_工作区/scripts/topic_index.py` 的
`normalize_topic_key()`(第 25–31 行:`NFKC → lower → 丢弃 str.isspace() 与 unicodedata.category 首字母为 P/S 的字符`),
对真实题名逐条求 key,**未重写算法**。参考实现版本:非 git 目录,以 mtime `2026-08-20T23:18:02-0700`、
size 35051、sha256 `d6d96e88…83ee54a7` 为准。生成时间 2026-08-23。

**样本来源**(`source` 字段):`产题文件名`/`评题文件名` = `00_产题评题/{产题,评题}/*.md` 文件名的 `<简要说明>` 段;
`产题frontmatter`/`评题frontmatter` = 同批文件 frontmatter `topics[].title`;`未立项目录` = `01_在研项目/_未立项/` 目录名的 `<题名>` 段;
`project.json` = 27 个 `P26*` 的 `full_name`/`short_name`;`synthetic` = 人工边界用例(全半角、中英混排、各类标点、emoji、
前后空白、繁体、大小写、超长、空串与纯标点)。真实语料按「含非汉字优先 + 等距」确定性抽样压到 129 条。

**碰撞**:`collides_with` 标注「不同 input 归一到同一 key」的对端(多于一个时是数组)。当前 6 组 / 12 对,
其中 3 组来自空 key(空串、纯空白、纯标点都归一成 `""`)。真实语料内部**零碰撞**,碰撞对全部由 synthetic 变体构造。

**M1 的硬约束**:`internal/topic` 的 Go 归一化实现必须先让本文件全部 129 条逐字节吻合(含 `key == ""` 的三条),
再做扫描器、题卡合并、状态反查中的任何一件。Go 侧尤其注意两处易错:① 必须是 **NFKC**(不是 NFC/NFD);
② 丢弃条件是 `unicode.IsSpace` **或** Unicode 通用类别首字母为 `P`/`S`(`Symbol` 也丢,所以 emoji、`+`、`~` 全没了)。
**隐私**:题名不含学生姓名;已对 27 份 `project.json` 的 `student`/`assignee.name`/`assignee.id` 全量 grep,命中 0 条。
