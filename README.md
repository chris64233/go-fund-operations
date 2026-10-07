# go-fund-operations

开放式基金交易与估值服务的 Go 项目基础模块。

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 基金终止清算分配（liquidation 包）

`liquidation` 包实现基金终止后的清算分配全流程：先保留待支付费用准备金与
争议准备金，再按投资者最终（登记日冻结）份额发放首笔及后续补充分配。

### 核心流程

1. **清算方案**：`CreatePlan` 固定基金、登记日、可分配现金、费用准备金、
   争议准备金；`ConfirmPlan` 确认时冻结登记日份额快照（按投资者 ID 排序），
   并停止该基金新的份额交易（`RecordShareTrade` 返回 `ErrTradingHalted`）。
   登记日之后的持仓变化不会进入本次清算。
2. **首笔分配**：`CreateFirstDistribution` 以可分配现金为分配池，按冻结份额
   采用确定性舍入规则计算每名投资者应付金额：
   `应付 = floor(分配池 × 份额 / 总份额)`，尾差 = 分配池 − Σ应付。
   任意时刻满足：Σ各批次应付 + 未释放准备金 + 累计尾差 = 清算资产总额。
3. **补充分配**：`ReleaseReserve` 释放部分/全部准备金并形成补充分配批次，
   仍按原冻结份额计算。已退出或账户状态变化的投资者不会被静默跳过，
   其明细进入明确的 `ON_HOLD` 待处理状态，人工处理后经 `ResolveOnHold`
   置回待付款并随下次执行完成付款。
4. **幂等执行**：`ExecuteBatch` 扫描批次明细付款，仅处理 `PENDING`/`FAILED`
   明细；中断后重试只补齐未完成明细。每条明细携带幂等键
   `批次ID/投资者ID`，付款通道按幂等键去重，保证不重复付款。
   方案取消、批次创建、准备金释放、付款确认等并发操作在互斥锁内完成
   状态校验与迁移，保证唯一结果（如取消与首笔分配并发时仅一方成功，
   并发释放不会超额）。

### 查询接口

- `GetPlan` / `GetSnapshot`：清算方案与份额快照
- `ListBatches`：分配批次列表（首笔 + 补充分配）
- `GetBatchProgress`：付款进度（已付/待付/待处理/失败及金额）
- `GetRemainingAssets`：剩余资产（未释放准备金 + 尾差）

### 状态机

- 方案：`DRAFT → CONFIRMED → IN_PROCESS → COMPLETED`，或 `→ CANCELLED`
- 批次：`OPEN → EXECUTING → DONE`
- 明细：`PENDING → PAID / FAILED(可重试) / ON_HOLD(人工处理)`

### 测试

`liquidation/service_test.go` 覆盖：快照冻结与交易停止、确定性舍入与
资产守恒不变量、执行中断恢复与幂等不重复付款、失败重试、补充分配与
退出投资者 `ON_HOLD` 处理、取消/释放并发竞态（`-race` 下运行）。
