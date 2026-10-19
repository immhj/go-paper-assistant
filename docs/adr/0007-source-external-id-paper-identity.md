# 论文身份改为来源与外部 ID 的组合

切换到 `jannisborn/paperscraper` 后，Semantic Scholar `corpusId` 不再是论文主身份。论文仍使用本地自增 ID 作为数据库主键，对外身份改为 `(source, external_id)` 唯一组合。

第一版只启用 arXiv，因此 `source='arxiv'`，`external_id` 使用不带版本号的 arXiv ID，例如 `2207.03928`。这同时满足稳定去重和全文回查：paperscraper sidecar 可以根据该标识返回 `https://arxiv.org/pdf/{id}`。候选若没有稳定来源或外部 ID，直接跳过，不入库、不分配会话序号。

不能退化为标题哈希。标题可能因版本更新而变化，也可能存在极相似的不同论文；错误合并比少收一篇更危险。

代价是当前模型按来源区分同一项研究工作：未来若同时启用 PubMed、bioRxiv、正式出版物，同一论文可能出现多条来源记录。那时再引入 DOI/arXiv 对齐规则或外部标识关系，不在只有一个来源时提前建表。
