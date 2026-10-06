# go-fund-operations

开放式基金交易与估值服务的 Go 项目基础模块。

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 净值版本模型

净值记录（`NAVRecord`）按基金和估值日保存单位净值、计算依据版本
（`BasisVersion`）、发布时间和状态。同一基金同一估值日任意时刻只有
一个当前有效版本（`PUBLISHED`）。

### 状态机

    DRAFT --发布--> PUBLISHED --更正--> SUPERSEDED
      |
      +--撤回--> WITHDRAWN

- 草稿（`DRAFT`）发布前可撤回；一旦被交易确认引用即不可撤回、不可修改。
- 重复发布相同内容（单位净值 + 计算依据版本一致）返回原版本；
  内容不同返回 `ErrVersionConflict`，必须走更正流程。
- 更正（`CorrectNAV`）必须携带原因和更正人，生成 `Version` 递增的新版本，
  新版本通过 `CorrectionOf` 引用原版本，原版本转为 `SUPERSEDED` 并冻结。

### 版本链与可追溯性

更正形成单向版本链：

    v3 (PUBLISHED, 当前有效) --> v2 (SUPERSEDED) --> v1 (SUPERSEDED, 原始)

- `NAVHistory` 从当前版本沿 `CorrectionOf` 回溯到原始版本。
- `ConfirmationsByNAV` 查询引用了某一版本的所有交易确认。
- `AdjustmentsByConfirmation` / `AdjustmentsByCorrection` 查询差额处理记录及状态。

### 差额处理

交易确认（`TradeConfirmation`）在确认时刻按所引用版本冻结金额，之后不再变化。
更正发布时，在同一临界区内为所有引用原版本的确认逐笔生成差额记录
（`Adjustment`）：`Delta` 为正表示应补收，为负表示应退回。差额记录逐笔关联
原确认与新旧版本，绝不直接覆盖基金余额或原确认结果。

### 精度规则

- 单位净值 `NAV`：4 位小数定点数（1.0000 表示为 10000）。
- 份额 `Shares`：2 位小数定点数。
- 金额 `Amount`：分。
- 所有金额与差额统一由 `AmountFor` 计算：先以最高精度相乘，
  再四舍五入（half-up）到分。

### 并发语义

所有状态变更由同一把互斥锁保护：发布、更正登记与交易确认并发时，
只有一个版本能成为当前有效版本；基于旧版本的迟到确认返回
`ErrStaleNAVVersion`，不会把新版本降回旧版本，也不会留下孤立的差额记录。
