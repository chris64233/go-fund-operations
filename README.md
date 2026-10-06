# go-fund-operations

开放式基金交易与估值服务的 Go 项目基础模块。

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## dividend：分红与红利再投资

`dividend` 包实现开放式基金的现金分红与红利再投资全流程。

### 核心概念

- **方案（Plan）**：保存基金、登记日、除息日、每份分红金额、除息净值与执行状态
  （`DRAFT → CONFIRMED → EXECUTING → EXECUTED`，执行前可 `CANCELLED`），
  每次状态变化均持久化一条 `StatusChange`。
- **登记快照（Snapshot）**：方案确认时按"交易日 ≤ 登记日"冻结持有人份额；
  登记日之后的交易不改变本次分红资格，冻结后拒绝补登登记日（含）之前的交易。
- **领取方式（Choice）**：持有人可选现金（默认）或红利再投资，
  执行开始前可反复修改，执行开始后锁定。
- **分配明细（Allocation）**：每名持有人在同一方案内唯一；
  已执行方案的差错只能通过独立 `Correction` 更正，不得删除或修改明细。

### 精度与舍入（定点整数，全局固定规则）

- 金额单位为分，份额单位为 0.01 份，单价（每份分红、除息净值）单位为 1e-6 元。
- 现金红利 = 登记份额 × 每份分红，四舍五入到分。
- 再投资份额 = 现金红利 ÷ 除息净值，向下取整到 0.01 份；
  折算差额以现金补齐，保证 Σ明细应发 = Σ实发现金 + Σ新增份额折算金额 = 方案总额。

### 幂等与并发

- 同一方案号重复执行返回原执行结果；基金、登记日、分红金额或除息净值
  不一致时返回 `ErrExecutionConflict`。
- 执行中断后方案停留在 `EXECUTING`，再次执行只补齐未完成明细；
  入账键幂等，不会重复发放现金或重复增加份额。
- 方案操作、持仓变更与执行扫描并发时，每名持有人只生成一份最终分配；
  取消与执行竞态时恰有一个成功，取消后快照与选择记录仍可查询。

### 主要接口

`PostTrade` / `CreatePlan` / `SetChoice` / `ConfirmPlan` / `Execute` /
`CancelPlan` / `PostCorrection`，以及查询接口 `GetPlan` / `Snapshot` /
`Choices` / `Allocations` / `Execution` / `StatusHistory` / `Corrections` /
`Holdings` / `CashBalance`。持久化由 `Store` 抽象，当前为线程安全的内存实现。
