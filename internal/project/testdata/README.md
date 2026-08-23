# project.json 格式基线夹具(M1 启动条件 S1-5)

`project_json_baseline/` 里的 8 份 `<code>.json` 是从课题库在研项目的 `project.json` 摘出的代表性样本,
用来机械验证「M1 的 Go 读写代码不破坏四个外部 Python 脚本(`project_sync.py` / `establish_project.py` /
`assign_project.py` / `reline_project.py`)的兼容性」。基线统计取自 **27 份原文件的原始字节**(2026-08-23)。

> 同目录的 `bucket_cases.json` 属于 **S1-4**(`bucket.Classify` 用例表),不在本文范围。

## 1. 27 份原文件的实测统计

| 项 | 结论 |
|---|---|
| 文件数 | 27(C 线 22 份 + B 线 5 份;**没有 G 线**) |
| 字段全集 | `code` / `short_name` / `full_name` / `student` / `needs_outsourcing` / `stage` / `line` = **27/27**;`assignee` = **16/27**(其中 11 份是对象、5 份是 `null`;另 11 份**整个键都不存在**) |
| `assignee` 子结构 | 出现时一律 `{"type","id","name"}` 三键、顺序一致;`type` ∈ {`student`(8), `org`(3)} |
| 字段顺序 | **不一致 —— 全库 4 种键序**(见下表)。不能假设固定顺序,只能"按原键序回写" |
| 缩进 | **2 空格**,27/27;无 Tab |
| 分隔符 | `": "`(冒号后一个空格)、`",\n"` —— 即 Python `json.dumps` 的默认 `separators` |
| `ensure_ascii` | **False** —— 中文是明文,27 份里 **0 处** `\uXXXX` 转义 |
| 编码 / BOM | UTF-8,**无 BOM** |
| 行尾 | **LF**,27/27,无 CRLF |
| 文件尾换行 | **26/27 有**;`P26C-028` 唯一没有(夹具原样保留这一条) |
| 逐字节可复现 | 27/27 **完全等于** `json.dumps(obj, ensure_ascii=False, indent=2)`(+ 上一行的尾换行) |
| `stage` 取值 | 3(17)、4(7)、6(2)、8(1);`line` 取值 `C`(22)、`B`(5) |

### 四种键序

| 键序 | 份数 | 顺序 |
|---|---|---|
| O1 | 11 | `code, short_name, full_name, student, needs_outsourcing, stage, line`(无 `assignee`) |
| O2 | 2 | `code, short_name, full_name, student, needs_outsourcing, stage, line, assignee` |
| O3 | 5 | `code, short_name, full_name, student, needs_outsourcing, stage, assignee, line` |
| O4 | 9 | `code, short_name, full_name, student, line, assignee, needs_outsourcing, stage` |

## 2. 8 份夹具的覆盖矩阵

| 文件 | 键序 | stage | line | assignee | 备注 |
|---|---|---|---|---|---|
| `P26-010.json` | O1 | 4 | C | 键缺失 | **字段最少/体积最小** |
| `P26-015.json` | O1 | 6 | C | 键缺失 | stage 6 |
| `P26-003.json` | O2 | 3 | C | 对象 `student` | |
| `P26-009.json` | O3 | 6 | C | 对象 `student` | |
| `P26B-030.json` | O4 | 3 | B | 对象 `org` | **字段最多/体积最大** |
| `P26B-026.json` | O4 | 3 | B | `null` | B 线未分配 |
| `P26C-028.json` | O4 | 8 | C | `null` | **文件尾无换行**(唯一) |
| `P26C-020.json` | O4 | 3 | C | 对象 `student` | C 线 + O4 |

## 3. 匿名化

夹具**已匿名**:`student` 与 `assignee.name` 全部换成 `学生A`…`学生E` / `机构A` / `00_未指派`,`assignee.id` 换成
`card_test_01`…`card_test_04`。替换后重新用与原文件**完全相同的序列化风格**写回(`json.dumps(obj, ensure_ascii=False, indent=2)`,
尾换行按原文件),因此键序、缩进、`ensure_ascii` 行为、行尾、尾换行全部与原文件一致,只有名字字符串本身不同(字节长度可能变)。
已对 27 份原文件的 `student` / `assignee.name` / `assignee.id` 全部取值 grep 本目录与 `internal/topic/testdata`,**命中 0 条**。

## 4. M1 写码约束清单(以实测为准)

1. **回写必须保持该文件原有的键序**,不许统一成某一种顺序 —— 全库有 4 种,任何"规范化键序"的写法都会把 27 份里至少 16 份改脏。
   Go 侧不能用普通 `struct` + `encoding/json`(它按 struct 字段顺序输出),要么保留原文顺序做定点改写,要么用有序表示。
2. **缩进 2 空格**,分隔符 `": "` / `","`,即 `json.MarshalIndent(v, "", "  ")` 的形状。
3. **中文必须明文**:Go 的 `encoding/json` 默认会把 `<`、`>`、`&` 转义(不转义中文),需要 `json.Encoder` + `SetEscapeHTML(false)` 才与 Python `ensure_ascii=False` 完全对齐。
4. **UTF-8 无 BOM,LF 行尾**。
5. **尾换行**:按原文件保留(`json.Encoder.Encode` 会自动补一个 `\n`,`json.MarshalIndent` 不会)。改 `P26C-028` 那种无尾换行的文件时不要顺手补上。
6. **`assignee` 三态必须分辨**:键缺失 / `null` / 对象 —— Go 里 `*struct` 只能分出 `null` 与对象,键是否存在要另外记(否则"未分配"会被写成"键缺失",反之亦然)。
7. **不得新增/删除字段**,也不得把 `needs_outsourcing` / `stage` / `line` 的类型改掉(`bool` / `int` / `string`)。

## bucket_cases.json

M1 启动条件 **S1-4**:`课题/01_在研项目/README.md` §三(七目录判例表)+ §五(机制目录白名单)转成的表驱动测试用例,
供未来 `bucket.Classify(materialType, relpath) (bucket string, err error)` 直接消费。90 条,每条
`{material_type, relpath, want_bucket, want_note}`:`material_type` 是 `workflow_registry.json` v17 的 25 种
材料 `type` 之一或 `null`(无材料语境的纯路径判断);`want_bucket` 取值七目录名之一或 `REJECT`(拒绝改投/不进任何桶,
含"项目根合法散文件不进桶"这种语义);`want_note` 写判例出处(README 第几条 / registry 字段 / 边界用例说明)。

**来源**:25 种材料各 ≥1 条正例(按 `outputs.dir` 对齐,产题/评题类材料的正例取"立项后作为孵化阶段原始材料转入
01_立项定题"的场景,而非 registry 里立项前的题库存放位置 `00_产题评题/`);七目录"不应放"反例逐条覆盖;README§三
"常见拿不准判例"5 条逐条给出正确/易错两侧;README§五机制目录白名单(`App产物/`、`平台/`、`_未分类/`、`投放/`
`交稿/`、`平台素材包/`)合法位置与非法位置;项目根规则(`project.json` 合法散文件、非法散文件、自造第八桶);
路径穿越 / 绝对路径 / 隐藏文件 / `.onecreat/` 自身目录等边界用例。

**M1 使用方式**:先让 `Classify` 跑通本文件全部用例(`want_bucket`/`REJECT` 逐条断言),再扩展新用例——本文件是
回归基线,不是穷举规范;`README.md`(`01_在研项目/`)判例表本身若变,先改判例表再补用例,不得反过来靠用例反推规则。
