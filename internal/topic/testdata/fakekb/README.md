# fakekb — Scan 用的小型假课题树

**这是什么**：`internal/topic.Scan` 的目录夹具。三态全覆盖（candidate / unestablished /
established）、同题多来源合并（`校园噪声监测仪` 在产题、评题、project.json 三处出现）、
非法 `source` 降级成「存量」、文件名不合命名契约的 warning、short_name 别名提升已有题卡。

**隐私**：全部题名、项目编号、目录名都是为测试编的，**不含任何真实学生姓名或真实项目**。
真实数据只在 `TestScanRealKeti` 里只读跑一遍，那个测试不做任何含姓名的断言。

**编号**：`P26C-998` / `P26C-999` 是夹具专用假号，不占真实发号池。
