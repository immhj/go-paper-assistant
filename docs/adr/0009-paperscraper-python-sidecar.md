# 使用 paperscraper Python sidecar 统一论文抓取

项目采用 [`jannisborn/paperscraper`](https://github.com/jannisborn/paperscraper) v1.0.0（MIT）作为论文抓取实现。该包要求 Python 3.9–3.13，支持 arXiv、PubMed、medRxiv、bioRxiv、chemRxiv，并原生使用“外层 AND、内层同义词 OR”的嵌套关键词结构。

不把 Python 和大量科学计算依赖塞进 Go 应用镜像。Docker Compose 单独运行 `paperscraper` sidecar，暴露三个内部接口：`POST /v1/search`、`GET /v1/papers/{source}/{external_id}` 和 `GET /healthz`。Go 的 `PaperSource` interface 保持不变，只把 adapter 从 Semantic Scholar 换成 sidecar HTTP client。

第一版只启用 arXiv API 后端。bioRxiv/medRxiv/chemRxiv 依赖本地 dump，体积与更新策略不同；PubMed 面向生物医学，等 CV+医学场景的 arXiv 链路稳定后再开。新增来源不得改变 service 的调用方式。

paperscraper 的 arXiv 返回标题、作者、日期、摘要、期刊引用和 arXiv DOI，但不直接提供 TLDR，引用补全还会调用 Semantic Scholar。因此默认关闭 `PAPERSCRAPER_CITATIONS`，引用未知必须展示“引用数据暂未获取”，不能伪装成 0。需要引用信号时可打开该开关，接受更高延迟与上游限速。

Sidecar 串行 arXiv 查询并强制至少 3 秒间隔。Go adapter 仍会校验结构化规格、稳定身份、重复记录与排除词，并对 sidecar 的 429/5xx 做有界重试。只有具备 `(source, external_id)` 的候选可以入库。

依赖固定为 `paperscraper==1.0.0`，不跟随 main 分支或 `latest`，避免上游更新让 API envelope 和抓取行为静默变化。

本文对上游 README 和包元数据做了摘要改写；未复制其实现代码。
