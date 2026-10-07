# go-fund-operations

开放式基金交易与估值服务的 Go 项目基础模块。

开发环境：Go 1.23.0。

运行测试：

    go test ./...

## 代销机构汇总交易分配（omnibus.go）

机构可先提交一笔汇总订单，再在截止前陆续补录客户明细；客户分配合计必须始终与机构订单严格一致。

### 核心概念

- `OmnibusOrder`：汇总订单，记录机构、基金、交易方向（`DirectionSubscribe` 按总金额、`DirectionRedeem` 按总份额）、估值日、外部编号、状态与版本号。
- `Allocation`：客户分配明细（客户号 + 数量）。同一客户重复提交视为修改，不会形成重复有效分配。
- `AllocationSnapshot`：冻结时保存的按客户号升序排序的稳定快照。
- `Position`：订单确认后按快照生成的客户持仓。

### 主要接口（`OmnibusService`）

- `SubmitOrder`：登记汇总订单，初始状态 `OPEN`。
- `UpsertAllocation` / `RemoveAllocation`：补录或修改未冻结的客户明细；仅合格客户可参与分配。
- `Freeze`：校验明细总和与订单总量严格一致后冻结并保存排序快照；存在缺口、超额或不合格客户时整笔拒绝，不允许把未分配部分挂到虚拟客户。
- `Confirm`：按冻结快照生成客户持仓，重复确认幂等。
- `Fail`：订单失败，客户明细保留可查询，但不生成任何持仓。
- 查询：`GetOrder`、`ListAllocations`、`GetSnapshot`、`Positions`、`GapReport`（差额查询，`Gap` 为正表示缺口、为负表示超额）。

### 并发与幂等

所有变更操作携带 `expectedVersion`，以订单版本裁决并发冲突（`ErrVersionConflict`）。冻结后迟到的明细修改被拒绝（`ErrOrderNotOpen`），不影响已确认持仓。冻结与确认在相同版本下重复调用幂等，快照与插入顺序无关。
