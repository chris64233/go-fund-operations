# go-fund-operations

开放式基金交易与估值服务的 Go 项目基础模块。本包实现基金单位净值（NAV）的
**草稿 → 发布 → 冻结 → 更正** 生命周期，以及基于版本的交易确认与差额追溯。

开发环境：Go 1.23.0。

运行测试（含竞态检测）：

    go test -race -count=1 ./...

## 设计原则

净值一旦被交易确认采用即**永久冻结**：不能覆盖原记录，不能改原确认金额，
也不能只把基金当前余额改成一个新数字。错误净值通过**递增新版本**更正，
受影响的交易逐笔生成差额处理记录，全程可从当前版本追溯到原版本与每笔确认。

## 版本关系与状态机

同一 `(基金 FundID, 估值日 ValueDate)` 构成一条独立的版本序列：

```
 草稿 draft ──发布──▶ published(v1, 当前)
     │                   │ 更正 CorrectNAV(BasedOn=v1)
     └──撤回──▶ withdrawn │
                         ▼
              superseded(v1) ──▶ published(v2, 当前, BasedOn=v1, Reason, CorrectedBy)
                                                 │ 更正 BasedOn=v2
                                                 ▼
                                  superseded(v2) ──▶ published(v3, 当前, BasedOn=v2)
```

- 任何时刻**只有一个当前有效版本**（`published`）；被更正的版本转为 `superseded`，数值不再改变。
- 版本号在序列内从 1 递增；草稿不占版本号，发布时才分配。
- 每个更正版本通过 `BasedOn` 指向原版本，并记录 `Reason`、`CorrectedBy`、发布时间，
  沿 `BasedOn` 链可从当前版本逐级追溯到首发原版本。
- 未被任何交易采用的草稿可撤回（`withdrawn`，保留审计痕迹）；已发布版本不可撤回、不可修改。

## 发布与冲突

- 首次发布：草稿成为 v1。
- 重复发布**相同内容**（单位净值与计算依据均相同）：幂等返回原版本，不产生新版本。
- 内容不同的发布：返回 `ErrVersionConflict`（`*ConflictError` 携带当前版本号），
  修正必须走 `CorrectNAV` 并说明原因与更正人。
- 对已被取代的版本发起更正：返回 `ErrVersionConflict`，新版本不会被降回旧版本。

## 交易确认与差额

- 确认（`ConfirmTx`）记录确认时的净值版本号与净值快照，金额 = 份额 × 净值，
  按统一精度舍入；确认一经生成不可变，相同确认号重复请求幂等返回原确认。
- 基于旧版本的迟到确认返回 `ErrStaleNAVReference`（含 current/seen 版本号），
  不产生确认、不产生差额。
- 更正发布时，所有**引用被更正版本**的确认逐笔生成 `Adjustment`：
  - 申购：`差额 = 份额 × (新净值 − 原净值)`；正数应补收（collect），负数应退回（refund）。
  - 赎回：方向相反（净值上浮需向投资人补付退款，下浮需追回）。
  - 每条差额关联 `ConfirmID / FromNAVVersion / ToNAVVersion`，不存在无确认的孤立差额；
    原确认的净值与金额保持不变。
- 仅计算依据变化、净值未变的更正只产生新版本，不产生差额。
- 差额状态 `pending → settled`，结算登记结算参考号；重复结算返回 `ErrAlreadySettled`。

## 精度

全部金额使用定点十进制（`Decimal`，`math/big.Int` 非标度值 + 标度），
不使用 float64。统一精度：单位净值 4 位小数、份额/金额 2 位小数，
舍入规则为四舍五入（绝对值半数进位，负数向远离零方向进位）。

## 并发保证

每个 `(基金, 估值日)` 序列由独立互斥锁保护，发布、更正、确认引用、差额登记
在同一临界区内原子完成。因此并发下：只有一个版本成为当前版本；
更正与确认并发时，引用旧版本的确认要么在更正前完成（必然被差额覆盖），
要么被明确冲突拒绝——不会回退版本，也不会留下孤立或缺失的差额记录
（见 `concurrency_test.go`，`go test -race` 验证）。

## 主要 API

| 操作 | 方法 |
| --- | --- |
| 创建草稿 | `Service.CreateDraft` |
| 撤回草稿 | `Service.WithdrawDraft` |
| 发布草稿（幂等/冲突） | `Service.PublishDraft` |
| 登记更正 | `Service.CorrectNAV` |
| 交易确认（版本引用） | `Service.ConfirmTx` |
| 结算差额 | `Service.SettleAdjustment` |
| 当前版本 / 指定版本 / 版本列表 / 草稿 | `CurrentNAV` / `NAVVersion` / `ListNAVVersions` / `ListDrafts` |
| 确认查询 | `Confirmation` / `ListConfirmations` |
| 差额查询 | `Adjustment` / `ListAdjustments` / `AdjustmentsForConfirmation` |
| 版本→原版本→交易→差额 追溯 | `TraceVersion` |

### 快速示例

```go
svc := fundoperations.NewService()
nav, _ := fundoperations.ParseDecimal("1.0000", 4)

_, draftID, _ := svc.CreateDraft(fundoperations.DraftInput{
    FundID: "F001", ValueDate: "2026-09-30", UnitNAV: nav, Basis: "pricing-1",
})
svc.PublishDraft("F001", "2026-09-30", draftID) // v1

svc.ConfirmTx(fundoperations.ConfirmTxInput{
    ConfirmID: "SUB1", FundID: "F001", ValueDate: "2026-09-30",
    TxType: fundoperations.TxSubscription,
    Shares: func() fundoperations.Decimal {
        d, _ := fundoperations.ParseDecimal("100", 2)
        return d
    }(),
})

newNAV, _ := fundoperations.ParseDecimal("1.0200", 4)
r, _ := svc.CorrectNAV(fundoperations.CorrectionInput{
    FundID: "F001", ValueDate: "2026-09-30", BasedOn: 1,
    NewUnitNAV: newNAV, NewBasis: "pricing-2",
    Reason: "估值差错更正", CorrectedBy: "alice",
})
// r.Version.Version == 2；r.Adjustments[0].ConfirmID == "SUB1"，应补收 2.00

trail, _ := svc.TraceVersion("F001", "2026-09-30", 0)
// trail.Chain: v2(当前) -> v1(原版本)；trail.Adjustments: SUB1 的差额及处理状态
```
