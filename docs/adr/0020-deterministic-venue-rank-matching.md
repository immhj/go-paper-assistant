# 发表场所等级只做确定性匹配，并固定第三方数据版本

> 状态：accepted
> 关系：落实 ADR 0005 中“CCF 等级匹配与别名归并”的确定性步骤；取代 `docs/schema.sql` 中对 `config/ccf-2026.json` 的设想。术语见 [`CONTEXT.md`](../../CONTEXT.md) 中的“质量信号”“发表场所等级”。

## 背景

IEEE 论文需要展示 CCF 等级、JCR 分区与影响因子、中科院分区。可用的开放数据：

- [`WenyanLiu/CCFrank4dblp`](https://github.com/WenyanLiu/CCFrank4dblp)（MIT）：CCF 2026 目录，数据最后更新 2026-04-06，以 DBLP venue 路径（如 `/conf/cvpr/cvpr`）为主键，并有全称、简称，没有 ISSN。
- [`suqingdong/impact_factor`](https://github.com/suqingdong/impact_factor) 1.1.4（MIT 代码）：自带 SQLite，约 2.2 万本期刊，字段为 IF、JCR 分区、ISSN/eISSN 与中科院分区；中科院分区仅为 `Q1`–`Q4`，无年份、版本、学科大类或 TOP 标记，准确性无法独立核验。

OpenAlex 给出的会议名常为冗长全称，不能直接等值匹配 CCF 名单。

## 决策

1. **只做确定性匹配，禁止模糊匹配。** 错误等级比没有等级更误导。匹配不到显示“未匹配”。
2. **CCF**：先用 IEEE DOI 内嵌的场所代码（`10.1109/cvpr52733.…` 中的 `cvpr`、`10.1109/tnnls.…` 中的 `tnnls`）与 CCF 简称或 DBLP 路径末段精确比对，并要求期刊/会议类型与 OpenAlex 一致、IEEE 期刊只能对应名称含 IEEE 的期刊（防止 IEEE Micro 的 `mm` 误配 ACM MM）；代码与两者都不同的少数场所用人工别名表（如 `jiot`→IoT-J、`tnet`→ToN）。代码不命中时，再用规范化全称与名单精确比对。候选等级不唯一即视为未匹配。

   > 修订（实现时）：原定“用 DOI 查询 DBLP 得到 venue 路径”不可行——DBLP 搜索 API 现由 Anubis 反爬页保护，程序化请求得到 429 或挑战页。DOI 场所代码是 IEEE 自己分配的确定性标识，满足“只做确定性匹配”的同一要求。
3. **JCR / 中科院 / IF**：用 ISSN 或 eISSN 精确匹配 impact_factor 的 SQLite。中科院分区展示为“中科院 N区*”，并注明“第三方整理数据，未标注版本与学科，仅供参考”。

   > 修订（2026-10-16）：每篇论文都重复一行注释，显得冗余。注释改为不再占用正文：Web 论文卡片在“中科院 N区*”上提供可聚焦的悬停提示和可访问名称，内容就是这条注释；QQ 只有文字，没有承载提示的地方，所以去掉 `*`，只显示“中科院 N区”。注释文本仍保存在展示记录中。
4. **固定数据版本**：CCF 数据固定到上游提交 `86e59cc`，impact_factor 固定为 1.1.4 自带的 SQLite。两者由 `services/paperscraper/tools/build_rank_data.py` 生成并随仓库提交到 `services/paperscraper/data/`（构建环境不保证能访问 GitHub），`manifest.json` 记录来源与 SHA-256，sidecar 启动时校验，不一致则整体停用等级而不是猜测。sidecar 只读查询，不引入该包运行时依赖。
5. **入库时匹配并冗余保存**：等级与数据版本标识（如 `ccf-2026-04-06`、`impact_factor-1.1.4`）写入 `papers`；数据版本变化时重新匹配，展示记录据此说明依据。
6. **质量信号**：匹配到任意一种等级即进入“发表场所等级”档位，并同时列出所有匹配到的等级；仍只用于展示和加权，绝不硬过滤。
7. 首版只对 IEEE 论文匹配；arXiv 论文的 DOI 无法经 DBLP 定位正式发表场所，`journal_ref` 又是自由文本，留待未来的版本对齐工作。

## 后果

等级结论可追溯到具体数据版本，且不会出现模型或模糊算法臆造的等级。代价是覆盖率受 DBLP、别名表和第三方数据收录限制，部分论文显示“未匹配”；中科院分区的可信度有限，需要在界面上明确标注。数据更新需要人工升级版本并重新匹配。
