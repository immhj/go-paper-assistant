# 通过 OpenAlex 提供 IEEE 数据源，以 DOI 作为外部身份

> 状态：accepted
> 关系：**amends ADR 0007、ADR 0009**。术语见 [`CONTEXT.md`](../../CONTEXT.md) 中的“数据源”“论文”“全文”。

## 背景

用户需要检索 IEEE 出版的期刊与会议论文，并获得题目、摘要、年份、DOI、期刊/会议名和 ISSN。现有唯一数据源是 arXiv（ADR 0009），paperscraper 不覆盖 IEEE。

候选做法与取舍：

- IEEE Xplore 官方 Metadata API：数据最权威，但需要人工审批的 API key（仅美东工作日发放），且条款面向 IEEE 客户。
- 爬取 IEEE Xplore 网页或用浏览器自动化：脆弱，且违反站点条款，可能牵连机构订阅。
- Crossref：免 key，但实测 IEEE 记录基本没有摘要。
- OpenAlex：免 key 可用，数据 CC0；实测 IEEE 期刊与会议记录有摘要、DOI、期刊/会议名与 ISSN。免 key 每日约 $0.10 预算（约 100 次关键词搜索），免费 key 为 $1（约 1000 次），按 DOI 查单篇免费。
- Semantic Scholar：免 key 可补摘要，但公共限流池容易 429，不适合作主检索源。

## 决策

1. 新增 `ieee` 数据源，由 paperscraper sidecar 内的 OpenAlex 后端实现，对 Go 侧仍是同一个 `PaperSource` 接口和 sidecar 端点（保持 ADR 0009 “新增来源不改变 service 调用方式”）。
2. 只按出版商过滤（OpenAlex `primary_location.source.host_organization:P4310319808`），不按 source type 过滤，避免会议被误分类而漏掉。
3. 论文身份为 `source='ieee'`、`external_id=` 小写 DOI，且只接受 `10.1109/` 前缀；无 DOI 的记录直接跳过，不入库、不分配序号。论文链接统一为 `https://doi.org/{doi}`。
4. 检索规格新增数据源字段，默认 arXiv；用户明确提到 IEEE 时切换。一次检索只查一个数据源，不做跨源合并排序。continue/refine 沿用原数据源，restart 重新判断。
5. 不跨数据源对齐同一研究的不同版本：arXiv 预印本与 IEEE 正式版是两条独立记录。这延续 ADR 0007 的“出现真实需求再建对齐关系”，宁可重复也不错误合并。
6. OpenAlex 缺摘要时，用一次 Semantic Scholar 批量 DOI 查询补齐；429 或失败不重试，显示暂无摘要，且不对无摘要论文调用模型做速览。
7. `OPENALEX_API_KEY`、`SEMANTIC_SCHOLAR_API_KEY` 均为可选配置；留空时按免 key 运行，预算耗尽时明确提示当日 IEEE 检索额度已用完。只有 OpenAlex 的剩余预算响应头为 0 才算“额度用完”，持续的每秒限流按临时失败处理。每次 IEEE 检索都消耗预算，因此 Go 端对 IEEE 检索失败不自动重试。
8. OpenAlex 标记为开放获取且给出 PDF 地址的 IEEE 论文，沿用共享全文流程下载；下载失败（包括出版商拦截）或 OpenAlex 查询失败都回退为提示全文上传，不视为系统错误。
9. 数据源只依据用户本轮原话判断（不读模型生成的工具查询），并识别“不要 IEEE”这类否定说法。

## 后果

IEEE 检索不依赖 IEEE 审批或订阅凭证，也不访问 IEEE 网站页面。代价是元数据完整度取决于 OpenAlex 与 Semantic Scholar 的收录，免 key 额度较小；同一研究可能以两个版本分别出现。未来若拿到 IEEE 官方 key，可作为同一 `ieee` 数据源的另一后端替换 OpenAlex，身份（DOI）不变。
